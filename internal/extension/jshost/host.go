package jshost

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/render"
)

//go:embed host.ts
var embeddedHostScript []byte

var (
	errAlreadyStarted = errors.New("jshost: host already started")
	errHostClosed     = errors.New("jshost: host closed")
	errHostStopped    = errors.New("jshost: host stopped")
	errLoadInFlight   = errors.New("jshost: load already in flight")
	errNotStarted     = errors.New("jshost: host not started")
)

// Host runs the embedded Bun TypeScript extension host as a subprocess and
// speaks the Phase 1 JSON-line protocol over stdin/stdout.
type Host struct {
	mu sync.Mutex

	cmd      *exec.Cmd
	stdin    io.WriteCloser
	writer   *jsonLineWriter
	cancel   context.CancelFunc
	tempPath string
	done     chan struct{}

	pendingLoad        chan loadedMessage
	pendingInvocations map[string]*invocationWaiter
	nextInvocationID   uint64

	closed   bool
	stopping bool
	err      error
}

type invocationWaiter struct {
	hookResultCh    chan hookResultMessage
	commandResultCh chan commandResultMessage
	toolResultCh    chan toolResultMessage
	toolRenderCh    chan toolRenderMessage
	oauthResultCh   chan oauthResultMessage
	toolUpdateCh    chan toolUpdateMessage
	uiCh            chan UIRequest
	ctxCh           chan ContextRequest
}

// Start writes the embedded host.ts to a temp file and starts `bun <host.ts>`.
func (h *Host) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("jshost: nil context")
	}

	h.mu.Lock()
	if h.cmd != nil {
		h.mu.Unlock()
		return errAlreadyStarted
	}
	h.mu.Unlock()

	bunPath, err := exec.LookPath("bun")
	if err != nil {
		return fmt.Errorf("find bun: %w", err)
	}

	tempPath, err := writeEmbeddedHostScript()
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, bunPath, tempPath)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		_ = os.Remove(tempPath)
		return fmt.Errorf("open host stdin: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = os.Remove(tempPath)
		return fmt.Errorf("open host stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.Remove(tempPath)
		return fmt.Errorf("start bun host: %w", err)
	}

	done := make(chan struct{})
	h.mu.Lock()
	h.cmd = cmd
	h.stdin = stdin
	h.writer = newJSONLineWriter(stdin)
	h.cancel = cancel
	h.tempPath = tempPath
	h.done = done
	h.pendingInvocations = map[string]*invocationWaiter{}
	h.mu.Unlock()

	go h.readLoop(stdout)
	go h.waitLoop(cmd, tempPath, done)

	return nil
}

// Stop terminates the Bun subprocess and waits for it to exit.
func (h *Host) Stop() error {
	h.mu.Lock()
	if h.cmd == nil {
		h.mu.Unlock()
		return nil
	}
	if h.stopping {
		done := h.done
		h.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}

	h.stopping = true
	cancel := h.cancel
	stdin := h.stdin
	process := h.cmd.Process
	done := h.done
	h.mu.Unlock()

	h.closeWithError(errHostStopped)

	if stdin != nil {
		_ = stdin.Close()
	}
	if cancel != nil {
		cancel()
	}
	if process != nil {
		_ = process.Kill()
	}
	if done != nil {
		<-done
	}
	return nil
}

// Load sends a load request and waits for the extension's registered event list.
func (h *Host) Load(path string) ([]string, error) {
	loaded, err := h.load(context.TODO(), path)
	if err != nil {
		return nil, err
	}
	return loaded.Events, nil
}

// LoadContext is Load with caller-controlled cancellation.
func (h *Host) LoadContext(ctx context.Context, path string) ([]string, error) {
	if ctx == nil {
		return nil, errors.New("jshost: nil context")
	}
	loaded, err := h.load(ctx, path)
	if err != nil {
		return nil, err
	}
	return loaded.Events, nil
}

// LoadExtension sends a load request and returns all JS-registered capabilities.
func (h *Host) LoadExtension(path string) (LoadedExtension, error) {
	return h.load(context.TODO(), path)
}

// LoadExtensionContext is LoadExtension with caller-controlled cancellation.
func (h *Host) LoadExtensionContext(ctx context.Context, path string) (LoadedExtension, error) {
	if ctx == nil {
		return LoadedExtension{}, errors.New("jshost: nil context")
	}
	return h.load(ctx, path)
}

// drainPending services any UI/ctx requests already buffered for this
// invocation before the caller returns on a result message. The reader enqueues
// stream messages in order, so by the time a *_result message is received every
// earlier UI/ctx request has already been buffered. Without this, the result
// select() can race a still-buffered trailing request (e.g. a fire-and-forget
// notify issued just before the handler returns) and drop it. Best-effort:
// responses here are for already-issued requests, so errors are ignored.
func (h *Host) drainPending(
	ctx context.Context,
	waiter *invocationWaiter,
	uiResponder UIResponder,
	ctxResponder ContextResponder,
) {
	for {
		select {
		case request, ok := <-waiter.uiCh:
			if !ok {
				return
			}
			_ = h.respondToUI(ctx, request, uiResponder)
		case request, ok := <-waiter.ctxCh:
			if !ok {
				return
			}
			_ = h.respondToContext(ctx, request, ctxResponder)
		default:
			return
		}
	}
}

// FireHook emits one hook event to the loaded extension and services UI requests
// and ctx requests until the matching hook_result arrives.
func (h *Host) FireHook(
	ctx context.Context,
	eventType string,
	payload map[string]any,
	uiResponder UIResponder,
	ctxResponder ContextResponder,
) (HookResult, error) {
	if ctx == nil {
		return HookResult{}, errors.New("jshost: nil context")
	}
	if eventType == "" {
		return HookResult{}, errors.New("jshost: empty event type")
	}
	if uiResponder == nil {
		return HookResult{}, errors.New("jshost: nil UI responder")
	}
	if ctxResponder == nil {
		return HookResult{}, errors.New("jshost: nil ctx responder")
	}

	id, waiter, err := h.registerInvocation()
	if err != nil {
		return HookResult{}, err
	}
	defer h.removeInvocation(id)

	event := make(map[string]any, len(payload)+1)
	maps.Copy(event, payload)
	event["type"] = eventType

	if err := h.write(hookRequest{Type: "hook", ID: id, Event: event}); err != nil {
		h.closeWithError(err)
		return HookResult{}, err
	}

	for {
		select {
		case msg, ok := <-waiter.hookResultCh:
			if !ok {
				return HookResult{}, h.closedErr()
			}
			h.drainPending(ctx, waiter, uiResponder, ctxResponder)
			return msg.Result, nil
		case request, ok := <-waiter.uiCh:
			if !ok {
				return HookResult{}, h.closedErr()
			}
			if err := h.respondToUI(ctx, request, uiResponder); err != nil {
				return HookResult{}, err
			}
		case request, ok := <-waiter.ctxCh:
			if !ok {
				return HookResult{}, h.closedErr()
			}
			if err := h.respondToContext(ctx, request, ctxResponder); err != nil {
				return HookResult{}, err
			}
		case <-ctx.Done():
			return HookResult{}, ctx.Err()
		}
	}
}

// InvokeCommand invokes a JS slash command handler and services extension UI/ctx
// requests emitted while the command is in flight.
func (h *Host) InvokeCommand(
	ctx context.Context,
	name string,
	args []string,
	uiResponder UIResponder,
	ctxResponder ContextResponder,
) error {
	if ctx == nil {
		return errors.New("jshost: nil context")
	}
	if name == "" {
		return errors.New("jshost: empty command name")
	}
	if uiResponder == nil {
		return errors.New("jshost: nil UI responder")
	}
	if ctxResponder == nil {
		return errors.New("jshost: nil ctx responder")
	}

	id, waiter, err := h.registerInvocation()
	if err != nil {
		return err
	}
	defer h.removeInvocation(id)

	if err := h.write(commandInvokeRequest{
		Type: "command_invoke",
		ID:   id,
		Name: name,
		Args: append([]string(nil), args...),
	}); err != nil {
		h.closeWithError(err)
		return err
	}

	for {
		select {
		case msg, ok := <-waiter.commandResultCh:
			if !ok {
				return h.closedErr()
			}
			h.drainPending(ctx, waiter, uiResponder, ctxResponder)
			if msg.Error != "" {
				return fmt.Errorf("jshost: command %q: %s", name, msg.Error)
			}
			return nil
		case request, ok := <-waiter.uiCh:
			if !ok {
				return h.closedErr()
			}
			if err := h.respondToUI(ctx, request, uiResponder); err != nil {
				return err
			}
		case request, ok := <-waiter.ctxCh:
			if !ok {
				return h.closedErr()
			}
			if err := h.respondToContext(ctx, request, ctxResponder); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// InvokeTool invokes a JS extension tool and services extension UI/ctx requests
// and optional tool_update messages until the matching tool_result arrives.
func (h *Host) InvokeTool(
	ctx context.Context,
	name string,
	toolCall extension.ToolCall,
	onUpdate extension.ToolUpdateCallback,
	uiResponder UIResponder,
	ctxResponder ContextResponder,
) (extension.ToolResult, error) {
	if ctx == nil {
		return extension.ToolResult{}, errors.New("jshost: nil context")
	}
	if name == "" {
		return extension.ToolResult{}, errors.New("jshost: empty tool name")
	}
	if uiResponder == nil {
		return extension.ToolResult{}, errors.New("jshost: nil UI responder")
	}
	if ctxResponder == nil {
		return extension.ToolResult{}, errors.New("jshost: nil ctx responder")
	}

	id, waiter, err := h.registerInvocation()
	if err != nil {
		return extension.ToolResult{}, err
	}
	defer h.removeInvocation(id)

	if err := h.write(toolInvokeRequest{
		Type:       "tool_invoke",
		ID:         id,
		Name:       name,
		ToolCallID: toolCall.ID,
		Args:       copyArgs(toolCall.Args),
	}); err != nil {
		h.closeWithError(err)
		return extension.ToolResult{}, err
	}

	for {
		select {
		case msg, ok := <-waiter.toolResultCh:
			if !ok {
				return extension.ToolResult{}, h.closedErr()
			}
			h.drainPending(ctx, waiter, uiResponder, ctxResponder)
			return convertToolResultPayload(msg.toolResultPayload)
		case update, ok := <-waiter.toolUpdateCh:
			if !ok {
				return extension.ToolResult{}, h.closedErr()
			}
			if onUpdate == nil {
				continue
			}
			result, err := convertToolResultPayload(update.PartialResult)
			if err != nil {
				return extension.ToolResult{}, err
			}
			onUpdate(result)
		case request, ok := <-waiter.uiCh:
			if !ok {
				return extension.ToolResult{}, h.closedErr()
			}
			if err := h.respondToUI(ctx, request, uiResponder); err != nil {
				return extension.ToolResult{}, err
			}
		case request, ok := <-waiter.ctxCh:
			if !ok {
				return extension.ToolResult{}, h.closedErr()
			}
			if err := h.respondToContext(ctx, request, ctxResponder); err != nil {
				return extension.ToolResult{}, err
			}
		case <-ctx.Done():
			return extension.ToolResult{}, ctx.Err()
		}
	}
}

// InvokeToolRenderCall invokes a JS extension tool's renderCall function.
func (h *Host) InvokeToolRenderCall(
	ctx context.Context,
	name string,
	input extension.ToolCallRenderInput,
) ([]render.Block, error) {
	return h.invokeToolRender(ctx, name, toolRenderRequest{
		Phase:            "call",
		Args:             copyArgs(input.Args),
		Expanded:         input.Expanded,
		ToolCallID:       input.ToolCallID,
		CWD:              input.CWD,
		ExecutionStarted: input.ExecutionStarted,
		ArgsComplete:     input.ArgsComplete,
		ShowImages:       input.ShowImages,
		IsError:          input.IsError,
		State:            input.State,
		LastBlocks:       input.LastBlocks,
	})
}

// InvokeToolRenderResult invokes a JS extension tool's renderResult function.
func (h *Host) InvokeToolRenderResult(
	ctx context.Context,
	name string,
	input extension.ToolResultRenderInput,
) ([]render.Block, error) {
	return h.invokeToolRender(ctx, name, toolRenderRequest{
		Phase:            "result",
		Args:             copyArgs(input.Args),
		Expanded:         input.Expanded,
		Partial:          input.Partial,
		ToolCallID:       input.ToolCallID,
		CWD:              input.CWD,
		ExecutionStarted: input.ExecutionStarted,
		ArgsComplete:     input.ArgsComplete,
		ShowImages:       input.ShowImages,
		IsError:          input.IsError || input.Result.IsError,
		State:            input.State,
		LastBlocks:       input.LastBlocks,
		Result: &toolRenderResultInput{
			Content: input.Result.Content,
			Details: input.Result.Details,
			IsError: input.Result.IsError,
		},
	})
}

// invokeToolRender sends a tool_render request and waits for the result.
// Rendering is pure: this bridge only waits for tool_render_result and does not
// service UI/ctx requests emitted during render.
func (h *Host) invokeToolRender(
	ctx context.Context,
	name string,
	req toolRenderRequest,
) ([]render.Block, error) {
	if ctx == nil {
		return nil, errors.New("jshost: nil context")
	}
	if name == "" {
		return nil, errors.New("jshost: empty tool name")
	}

	id, waiter, err := h.registerInvocation()
	if err != nil {
		return nil, err
	}
	defer h.removeInvocation(id)

	req.Type = "tool_render"
	req.ID = id
	req.Name = name
	if err := h.write(req); err != nil {
		h.closeWithError(err)
		return nil, err
	}

	select {
	case msg, ok := <-waiter.toolRenderCh:
		if !ok {
			return nil, h.closedErr()
		}
		if msg.Error != "" {
			return nil, errors.New(msg.Error)
		}
		return msg.Blocks, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// InvokeOAuth invokes an extension-provided OAuth callback and services
// extension UI/ctx requests until the matching oauth_result arrives.
func (h *Host) InvokeOAuth(
	ctx context.Context,
	provider string,
	method string,
	args map[string]any,
	uiResponder UIResponder,
	ctxResponder ContextResponder,
) (oauthResult, error) {
	if ctx == nil {
		return oauthResult{}, errors.New("jshost: nil context")
	}
	if provider == "" {
		return oauthResult{}, errors.New("jshost: empty oauth provider")
	}
	if method == "" {
		return oauthResult{}, errors.New("jshost: empty oauth method")
	}
	if uiResponder == nil {
		return oauthResult{}, errors.New("jshost: nil UI responder")
	}
	if ctxResponder == nil {
		return oauthResult{}, errors.New("jshost: nil ctx responder")
	}

	id, waiter, err := h.registerInvocation()
	if err != nil {
		return oauthResult{}, err
	}
	defer h.removeInvocation(id)

	if err := h.write(oauthInvokeRequest{
		Type:     "oauth_invoke",
		ID:       id,
		Provider: provider,
		Method:   method,
		Args:     copyArgs(args),
	}); err != nil {
		h.closeWithError(err)
		return oauthResult{}, err
	}

	for {
		select {
		case msg, ok := <-waiter.oauthResultCh:
			if !ok {
				return oauthResult{}, h.closedErr()
			}
			h.drainPending(ctx, waiter, uiResponder, ctxResponder)
			return msg.oauthResult, nil
		case request, ok := <-waiter.uiCh:
			if !ok {
				return oauthResult{}, h.closedErr()
			}
			if err := h.respondToUI(ctx, request, uiResponder); err != nil {
				return oauthResult{}, err
			}
		case request, ok := <-waiter.ctxCh:
			if !ok {
				return oauthResult{}, h.closedErr()
			}
			if err := h.respondToContext(ctx, request, ctxResponder); err != nil {
				return oauthResult{}, err
			}
		case <-ctx.Done():
			return oauthResult{}, ctx.Err()
		}
	}
}

func (h *Host) load(ctx context.Context, path string) (LoadedExtension, error) {
	if path == "" {
		return LoadedExtension{}, errors.New("jshost: empty extension path")
	}

	ch := make(chan loadedMessage, 1)
	if err := h.registerLoad(ch); err != nil {
		return LoadedExtension{}, err
	}
	defer h.removeLoad(ch)

	if err := h.write(loadRequest{Type: "load", Path: path}); err != nil {
		h.closeWithError(err)
		return LoadedExtension{}, err
	}

	if ctx == nil {
		msg, ok := <-ch
		if !ok {
			return LoadedExtension{}, h.closedErr()
		}
		return msg.loadedExtension(), nil
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return LoadedExtension{}, h.closedErr()
		}
		return msg.loadedExtension(), nil
	case <-ctx.Done():
		return LoadedExtension{}, ctx.Err()
	}
}

func (h *Host) respondToUI(ctx context.Context, request UIRequest, uiResponder UIResponder) error {
	response, err := uiResponder(ctx, request)
	if err != nil {
		if request.Method != "notify" {
			_ = h.write(UIResponse{
				Type:      "ui_response",
				ID:        request.ID,
				Cancelled: true,
			})
		}
		return err
	}
	if request.Method == "notify" {
		return nil
	}

	response = normalizeUIResponse(request.ID, response)
	if err := h.write(response); err != nil {
		h.closeWithError(err)
		return err
	}
	return nil
}

func (h *Host) respondToContext(
	ctx context.Context,
	request ContextRequest,
	ctxResponder ContextResponder,
) error {
	response, err := ctxResponder(ctx, request)
	if err != nil {
		response = ContextResponse{Error: err.Error()}
	}
	response = normalizeContextResponse(request.ID, response)
	if writeErr := h.write(response); writeErr != nil {
		h.closeWithError(writeErr)
		return writeErr
	}
	return nil
}

func normalizeUIResponse(id string, response UIResponse) UIResponse {
	if response.Type == "" {
		response.Type = "ui_response"
	}
	if response.ID == "" {
		response.ID = id
	}
	return response
}

func normalizeContextResponse(id string, response ContextResponse) ContextResponse {
	if response.Type == "" {
		response.Type = "ctx_response"
	}
	if response.ID == "" {
		response.ID = id
	}
	return response
}

func (h *Host) registerLoad(ch chan loadedMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.writer == nil {
		return errNotStarted
	}
	if h.closed {
		return h.err
	}
	if h.pendingLoad != nil {
		return errLoadInFlight
	}
	h.pendingLoad = ch
	return nil
}

func (h *Host) removeLoad(ch chan loadedMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.pendingLoad == ch {
		h.pendingLoad = nil
	}
}

func (h *Host) registerInvocation() (string, *invocationWaiter, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.writer == nil {
		return "", nil, errNotStarted
	}
	if h.closed {
		return "", nil, h.err
	}

	h.nextInvocationID++
	id := strconv.FormatUint(h.nextInvocationID, 10)
	waiter := &invocationWaiter{
		hookResultCh:    make(chan hookResultMessage, 1),
		commandResultCh: make(chan commandResultMessage, 1),
		toolResultCh:    make(chan toolResultMessage, 1),
		toolRenderCh:    make(chan toolRenderMessage, 1),
		oauthResultCh:   make(chan oauthResultMessage, 1),
		toolUpdateCh:    make(chan toolUpdateMessage, 32),
		uiCh:            make(chan UIRequest, 32),
		ctxCh:           make(chan ContextRequest, 32),
	}
	h.pendingInvocations[id] = waiter
	return id, waiter, nil
}

func (h *Host) removeInvocation(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pendingInvocations, id)
}

func (h *Host) write(value any) error {
	h.mu.Lock()
	writer := h.writer
	closed := h.closed
	err := h.err
	h.mu.Unlock()

	if writer == nil {
		return errNotStarted
	}
	if closed {
		if err != nil {
			return err
		}
		return errHostClosed
	}
	return writer.WriteJSON(value)
}

func (h *Host) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		h.dispatchLine(scanner.Bytes())
	}

	if err := scanner.Err(); err != nil {
		h.closeWithError(fmt.Errorf("read host stdout: %w", err))
	}
}

func (h *Host) dispatchLine(line []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		h.closeWithError(fmt.Errorf("parse host line: %w", err))
		return
	}

	switch envelope.Type {
	case "loaded":
		var msg loadedMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse loaded message: %w", err))
			return
		}
		h.deliverLoaded(msg)
	case "ui_request":
		var msg UIRequest
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse UI request: %w", err))
			return
		}
		h.deliverUIRequest(msg)
	case "ctx_request":
		var msg ContextRequest
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse ctx request: %w", err))
			return
		}
		h.deliverContextRequest(msg)
	case "hook_result":
		var msg hookResultMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse hook result: %w", err))
			return
		}
		h.deliverHookResult(msg)
	case "command_result":
		var msg commandResultMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse command result: %w", err))
			return
		}
		h.deliverCommandResult(msg)
	case "tool_result":
		var msg toolResultMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse tool result: %w", err))
			return
		}
		h.deliverToolResult(msg)
	case "tool_render_result":
		var msg toolRenderMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse tool render result: %w", err))
			return
		}
		h.deliverToolRender(msg)
	case "oauth_result":
		var msg oauthResultMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse oauth result: %w", err))
			return
		}
		h.deliverOAuthResult(msg)
	case "tool_update":
		var msg toolUpdateMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse tool update: %w", err))
			return
		}
		h.deliverToolUpdate(msg)
	case "error":
		var msg errorMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.closeWithError(fmt.Errorf("parse host error: %w", err))
			return
		}
		h.closeWithError(fmt.Errorf("jshost: %s", msg.Message))
	default:
		h.closeWithError(fmt.Errorf("jshost: unknown host message type %q", envelope.Type))
	}
}

func (h *Host) deliverLoaded(msg loadedMessage) {
	h.mu.Lock()
	ch := h.pendingLoad
	h.mu.Unlock()

	if ch == nil {
		return
	}
	select {
	case ch <- msg:
	default:
		h.closeWithError(errors.New("jshost: loaded waiter blocked"))
	}
}

func (h *Host) deliverUIRequest(msg UIRequest) {
	invocationID := invocationIDFromRequestID(msg.ID)
	if invocationID == "" {
		return
	}

	h.mu.Lock()
	waiter := h.pendingInvocations[invocationID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.uiCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: UI waiter for invocation %s blocked", invocationID))
	}
}

func (h *Host) deliverContextRequest(msg ContextRequest) {
	invocationID := invocationIDFromRequestID(msg.ID)
	if invocationID == "" {
		return
	}

	h.mu.Lock()
	waiter := h.pendingInvocations[invocationID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.ctxCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: ctx waiter for invocation %s blocked", invocationID))
	}
}

func (h *Host) deliverHookResult(msg hookResultMessage) {
	h.mu.Lock()
	waiter := h.pendingInvocations[msg.ID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.hookResultCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: hook waiter %s blocked", msg.ID))
	}
}

func (h *Host) deliverCommandResult(msg commandResultMessage) {
	h.mu.Lock()
	waiter := h.pendingInvocations[msg.ID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.commandResultCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: command waiter %s blocked", msg.ID))
	}
}

func (h *Host) deliverToolResult(msg toolResultMessage) {
	h.mu.Lock()
	waiter := h.pendingInvocations[msg.ID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.toolResultCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: tool waiter %s blocked", msg.ID))
	}
}

func (h *Host) deliverToolRender(msg toolRenderMessage) {
	h.mu.Lock()
	waiter := h.pendingInvocations[msg.ID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.toolRenderCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: tool render waiter %s blocked", msg.ID))
	}
}

func (h *Host) deliverOAuthResult(msg oauthResultMessage) {
	h.mu.Lock()
	waiter := h.pendingInvocations[msg.ID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.oauthResultCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: oauth waiter %s blocked", msg.ID))
	}
}

func (h *Host) deliverToolUpdate(msg toolUpdateMessage) {
	h.mu.Lock()
	waiter := h.pendingInvocations[msg.ID]
	h.mu.Unlock()

	if waiter == nil {
		return
	}
	select {
	case waiter.toolUpdateCh <- msg:
	default:
		h.closeWithError(fmt.Errorf("jshost: tool update waiter %s blocked", msg.ID))
	}
}

func invocationIDFromRequestID(id string) string {
	index := strings.Index(id, ":")
	if index <= 0 {
		return ""
	}
	return id[:index]
}

func (h *Host) waitLoop(cmd *exec.Cmd, tempPath string, done chan struct{}) {
	err := cmd.Wait()
	_ = os.Remove(tempPath)

	h.mu.Lock()
	stopping := h.stopping
	h.mu.Unlock()

	switch {
	case stopping:
		h.closeWithError(errHostStopped)
	case err != nil:
		h.closeWithError(fmt.Errorf("jshost: host process exited: %w", err))
	default:
		h.closeWithError(errHostClosed)
	}

	close(done)
}

func (h *Host) closeWithError(err error) {
	if err == nil {
		err = errHostClosed
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}

	h.closed = true
	h.err = err
	loadCh := h.pendingLoad
	h.pendingLoad = nil
	pendingInvocations := h.pendingInvocations
	h.pendingInvocations = map[string]*invocationWaiter{}
	h.mu.Unlock()

	if loadCh != nil {
		close(loadCh)
	}
	for _, waiter := range pendingInvocations {
		close(waiter.hookResultCh)
		close(waiter.commandResultCh)
		close(waiter.toolResultCh)
		close(waiter.toolRenderCh)
		close(waiter.oauthResultCh)
		close(waiter.toolUpdateCh)
		close(waiter.uiCh)
		close(waiter.ctxCh)
	}
}

func (h *Host) closedErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.err != nil {
		return h.err
	}
	return errHostClosed
}

func writeEmbeddedHostScript() (string, error) {
	file, err := os.CreateTemp("", "along-jshost-*.ts")
	if err != nil {
		return "", fmt.Errorf("create host temp file: %w", err)
	}

	path := file.Name()
	if _, err := file.Write(embeddedHostScript); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write host temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close host temp file: %w", err)
	}
	return path, nil
}
