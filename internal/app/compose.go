// Package app is the single in-process assembly point for the P0 engine.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/backend/local"
	localenv "github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/exporthtml"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/mode/enginefacade"
	"github.com/cunninghamcard-bit/Attention/internal/pipeline"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	pluginhost "github.com/cunninghamcard-bit/Attention/internal/plugin/host"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
	"github.com/cunninghamcard-bit/Attention/internal/worker"
)

type Composition struct {
	Store            backend.EventStore
	Bus              backend.NotifyBus
	Queue            backend.JobQueue
	Repo             session.JsonlSessionRepoAPI
	Host             *worker.Host
	ExtCommands      *PluginHostBridge
	NewSessionFacade func(sessionID string) *enginefacade.Facade
	Stop             func()
}

type ComposeOptions struct {
	DataDir            string
	SessionsDir        string
	CWD                string
	Model              ai.Model
	ThinkingLevel      agentloop.ThinkingLevel
	Provider           *provider.Registry
	Plugins            *plugin.Registry
	FakeStream         agentloop.StreamFunc
	ShellPath          string
	ShellCommandPrefix string
}

// PluginHostBridge exposes plugin host RPCs to control-plane adapters without
// leaking the host.Manager type across package boundaries.
type PluginHostBridge struct {
	manager                     *pluginhost.Manager
	sessionCommandsCrossProcess bool
	mu                          sync.Mutex
	sessionMounts               map[string]bool
}

// SetSessionCommandsCrossProcess makes missing local session mounts route
// through JobExtCommand instead of direct in-process DispatchCommand.
func (b *PluginHostBridge) SetSessionCommandsCrossProcess(enabled bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.sessionCommandsCrossProcess = enabled
	b.mu.Unlock()
}

// call 是三个 host RPC 的共形：host 缺席快速失败、错误统一包装，
// 唤醒/分发本身由 fn 决定（host.Manager 不越包泄漏）。
func (b *PluginHostBridge) call(fn func(*pluginhost.Manager) ([]byte, error)) ([]byte, error) {
	if b == nil || b.manager == nil {
		return nil, pluginHostDownError{err: pluginhost.ErrHostDown}
	}
	result, err := fn(b.manager)
	if err != nil {
		return nil, wrapPluginHostError(err)
	}
	return result, nil
}

func (b *PluginHostBridge) DispatchCommand(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	envID string,
	name string,
	payload []byte,
) ([]byte, error) {
	return b.call(func(m *pluginhost.Manager) ([]byte, error) {
		return m.DispatchCommand(ctx, pluginID, owner, sessionID, envID, name, payload)
	})
}

func (b *PluginHostBridge) dispatchQueuedCommand(
	ctx context.Context,
	p protocol.ExtCommandJobPayload,
) ([]byte, error) {
	return b.DispatchCommand(ctx, p.PluginID, p.Owner, p.SessionID, p.EnvID, p.Name, p.Payload)
}

func (b *PluginHostBridge) Route(
	owner string,
	sessionID string,
	envID string,
) (backend.ExtCommandRouteMode, error) {
	switch owner {
	case "engine":
		return backend.ExtCommandRouteInProcess, nil
	case "session":
		if b == nil || !b.routesSessionCommandsCrossProcess() || b.hasSessionMount(sessionID) {
			return backend.ExtCommandRouteInProcess, nil
		}
		return backend.ExtCommandRouteCrossProcess, nil
	case "environment":
		return backend.ExtCommandRouteUnsupported, nil
	default:
		return backend.ExtCommandRouteUnsupported, nil
	}
}

func (b *PluginHostBridge) routesSessionCommandsCrossProcess() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionCommandsCrossProcess
}

func (b *PluginHostBridge) recordSessionMount(sessionID string) {
	if b == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	if b.sessionMounts == nil {
		b.sessionMounts = map[string]bool{}
	}
	b.sessionMounts[sessionID] = true
	b.mu.Unlock()
}

func (b *PluginHostBridge) hasSessionMount(sessionID string) bool {
	if b == nil || sessionID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionMounts[sessionID]
}

func (b *PluginHostBridge) ExecuteTool(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	name string,
	args []byte,
) ([]byte, error) {
	return b.call(func(m *pluginhost.Manager) ([]byte, error) {
		return m.ExecuteTool(ctx, pluginID, owner, sessionID, name, args)
	})
}

func (b *PluginHostBridge) DispatchHook(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	point string,
	index int,
	eventJSON []byte,
) ([]byte, error) {
	return b.call(func(m *pluginhost.Manager) ([]byte, error) {
		return m.DispatchHook(ctx, pluginID, owner, sessionID, point, index, eventJSON)
	})
}

const defaultUITimeout = 60 * time.Second

type pluginUIBridge struct {
	store   backend.EventStore
	bus     backend.NotifyBus
	logger  *slog.Logger
	manager *pluginhost.Manager

	mu     sync.Mutex
	timers map[string]context.CancelFunc
}

type hostUIRequestPayload struct {
	RequestID string          `json:"requestId,omitempty"`
	Kind      string          `json:"kind"`
	Title     string          `json:"title"`
	Body      string          `json:"body,omitempty"`
	Options   []string        `json:"options,omitempty"`
	Default   json.RawMessage `json:"default,omitempty"`
	TimeoutMs int             `json:"timeoutMs,omitempty"`
}

func newPluginUIBridge(store backend.EventStore, bus backend.NotifyBus, logger *slog.Logger) *pluginUIBridge {
	if logger == nil {
		logger = slog.Default()
	}
	return &pluginUIBridge{
		store:  store,
		bus:    bus,
		logger: logger,
		timers: map[string]context.CancelFunc{},
	}
}

func (b *pluginUIBridge) setManager(manager *pluginhost.Manager) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.manager = manager
}

func (b *pluginUIBridge) handleRequest(req pluginhost.UIRequest) (string, error) {
	if req.PluginID == "" {
		return "", errors.New("app: plugin ui.request missing plugin id")
	}
	if req.Owner != "session" || req.SessionID == "" {
		return "", fmt.Errorf("app: plugin %q ui.request requires session owner", req.PluginID)
	}
	if req.Kind == "notify" {
		return "", b.appendUIEnvelope(context.Background(), req.SessionID, protocol.KindUIRequest, json.RawMessage(req.Payload))
	}
	if !isInteractiveUIKind(req.Kind) {
		return "", fmt.Errorf("app: plugin %q ui.request kind %q is not supported", req.PluginID, req.Kind)
	}

	var payload hostUIRequestPayload
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return "", fmt.Errorf("app: decode plugin %q ui.request payload: %w", req.PluginID, err)
	}
	if payload.Kind == "" {
		payload.Kind = req.Kind
	}
	if payload.Kind != req.Kind {
		return "", fmt.Errorf("app: plugin %q ui.request kind mismatch frame=%q payload=%q", req.PluginID, req.Kind, payload.Kind)
	}
	if payload.Title == "" {
		return "", fmt.Errorf("app: plugin %q ui.request %q missing title", req.PluginID, req.Kind)
	}
	if req.Kind == protocol.UIKindSelect && len(payload.Options) == 0 {
		return "", fmt.Errorf("app: plugin %q ui.select missing options", req.PluginID)
	}
	payload.RequestID = protocol.NewUIRequestID()
	if payload.TimeoutMs <= 0 {
		payload.TimeoutMs = int(defaultUITimeout / time.Millisecond)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("app: marshal plugin %q ui.request %q: %w", req.PluginID, req.Kind, err)
	}
	if err := b.appendUIEnvelope(context.Background(), req.SessionID, protocol.KindUIRequest, raw); err != nil {
		return "", err
	}
	b.armTimeout(req.SessionID, payload)
	return payload.RequestID, nil
}

func isInteractiveUIKind(kind string) bool {
	switch kind {
	case protocol.UIKindConfirm, protocol.UIKindSelect, protocol.UIKindInput, protocol.UIKindEditor:
		return true
	default:
		return false
	}
}

func (b *pluginUIBridge) armTimeout(sessionID string, payload hostUIRequestPayload) {
	ctx, cancel := context.WithCancel(context.Background())
	b.mu.Lock()
	b.timers[payload.RequestID] = cancel
	b.mu.Unlock()

	timeout := time.Duration(payload.TimeoutMs) * time.Millisecond
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			value := json.RawMessage(`null`)
			if len(payload.Default) > 0 {
				value = cloneRawMessage(payload.Default)
			}
			err := b.resolve(ctx, sessionID, protocol.UIResolvedPayload{
				RequestID:  payload.RequestID,
				Value:      value,
				ResolvedBy: "timeout",
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				b.logger.Error("app: ui timeout resolve failed", "session", sessionID, "requestId", payload.RequestID, "error", err)
			}
		case <-ctx.Done():
		}
	}()
}

func (b *pluginUIBridge) resolveFromClient(
	ctx context.Context,
	sessionID string,
	req protocol.UIResolveRequest,
) error {
	if req.RequestID == "" {
		return errors.New("app: ui.resolve requestId is required")
	}
	if len(req.Value) == 0 {
		return errors.New("app: ui.resolve value is required")
	}
	return b.resolve(ctx, sessionID, protocol.UIResolvedPayload{
		RequestID:  req.RequestID,
		Value:      cloneRawMessage(req.Value),
		ResolvedBy: "client",
	})
}

func (b *pluginUIBridge) resolve(
	ctx context.Context,
	sessionID string,
	payload protocol.UIResolvedPayload,
) error {
	if payload.RequestID == "" {
		return errors.New("app: ui.resolved requestId is required")
	}
	if payload.ResolvedBy == "" {
		return errors.New("app: ui.resolved resolvedBy is required")
	}
	if len(payload.Value) == 0 {
		payload.Value = json.RawMessage(`null`)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("app: marshal ui.resolved %q: %w", payload.RequestID, err)
	}

	// 记录决议（first-resolve-wins）走加锁临界区；唤醒 host 的 ResolveUI 在锁外，
	// 故临界区单独成方法用 defer 解锁，免去每个早返回手动解锁的特例。
	manager, proceed, err := b.recordResolution(ctx, sessionID, payload.RequestID, raw)
	if err != nil || !proceed {
		return err
	}
	if manager == nil {
		return pluginHostDownError{err: pluginhost.ErrHostDown}
	}
	if err := manager.ResolveUI(ctx, sessionID, raw); err != nil {
		return wrapPluginHostError(err)
	}
	return nil
}

// recordResolution 把 ui.resolved 落日志（first-resolve-wins 投影裁决）并停超时计时器，
// 返回该会话的 host manager 与是否继续唤醒。已决议/无匹配 request → proceed=false（丢弃）。
func (b *pluginUIBridge) recordResolution(
	ctx context.Context,
	sessionID string,
	requestID string,
	raw json.RawMessage,
) (manager *pluginhost.Manager, proceed bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	hasRequest, hasResolved, err := b.uiLogStateLocked(ctx, sessionID, requestID)
	if err != nil {
		return nil, false, err
	}
	if hasResolved {
		b.cancelTimerLocked(requestID)
		b.logger.Warn("app: ui.resolve dropped after first resolution", "session", sessionID, "requestId", requestID)
		return nil, false, nil
	}
	if !hasRequest {
		b.logger.Warn("app: ui.resolve dropped without matching request", "session", sessionID, "requestId", requestID)
		return nil, false, nil
	}
	if err := b.appendUIEnvelopeLocked(ctx, sessionID, protocol.KindUIResolved, raw); err != nil {
		return nil, false, err
	}
	b.cancelTimerLocked(requestID)
	return b.manager, true, nil
}

func (b *pluginUIBridge) uiLogStateLocked(
	ctx context.Context,
	sessionID string,
	requestID string,
) (hasRequest bool, hasResolved bool, err error) {
	envelopes, err := b.store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		return false, false, fmt.Errorf("app: read ui log for session %q: %w", sessionID, err)
	}
	for _, env := range envelopes {
		switch env.Kind {
		case protocol.KindUIRequest:
			if uiRequestPayloadID(env.Payload) == requestID {
				hasRequest = true
			}
		case protocol.KindUIResolved:
			var payload protocol.UIResolvedPayload
			if err := json.Unmarshal(env.Payload, &payload); err == nil && payload.RequestID == requestID {
				hasResolved = true
			}
		}
	}
	return hasRequest, hasResolved, nil
}

func uiRequestPayloadID(raw json.RawMessage) string {
	var payload struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.RequestID
}

func (b *pluginUIBridge) appendUIEnvelope(
	ctx context.Context,
	sessionID string,
	kind string,
	payload json.RawMessage,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.appendUIEnvelopeLocked(ctx, sessionID, kind, payload)
}

func (b *pluginUIBridge) appendUIEnvelopeLocked(
	ctx context.Context,
	sessionID string,
	kind string,
	payload json.RawMessage,
) error {
	if b.store == nil {
		return errors.New("app: ui bridge store is nil")
	}
	if b.bus == nil {
		return errors.New("app: ui bridge bus is nil")
	}
	if !json.Valid(payload) {
		return fmt.Errorf("app: ui envelope %q payload is not valid JSON", kind)
	}

	env := &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     sessionID,
		Kind:          kind,
		Actor:         protocol.ActorSystem,
		Payload:       cloneRawMessage(payload),
		OccurredAt:    time.Now().UTC(),
		SchemaVersion: protocol.SchemaVersion,
	}
	if err := b.store.Append(ctx, env); err != nil {
		return fmt.Errorf("app: append ui envelope %q for session %q: %w", kind, sessionID, err)
	}
	b.bus.Publish(sessionID)
	return nil
}

func (b *pluginUIBridge) cancelTimerLocked(requestID string) {
	cancel, ok := b.timers[requestID]
	if !ok {
		return
	}
	delete(b.timers, requestID)
	cancel()
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

type pluginHostDownError struct {
	err error
}

func (e pluginHostDownError) Error() string { return e.err.Error() }

func (e pluginHostDownError) Unwrap() error { return e.err }

func (e pluginHostDownError) ExtHostDown() bool { return true }

func wrapPluginHostError(err error) error {
	if errors.Is(err, pluginhost.ErrHostDown) {
		return pluginHostDownError{err: err}
	}
	return err
}

func Compose(ctx context.Context, opts ComposeOptions) (*Composition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dataDir, err := resolveDataDir(opts.DataDir)
	if err != nil {
		return nil, err
	}
	cwd, err := resolveCWD(opts.CWD)
	if err != nil {
		return nil, err
	}
	if opts.Model.ID == "" {
		return nil, errors.New("app: resolved model is required")
	}
	thinkingLevel := opts.ThinkingLevel
	if thinkingLevel == "" {
		thinkingLevel = agentloop.ThinkingMedium
	}

	store := local.NewEventStore(filepath.Join(dataDir, "events"))
	bus := local.NewNotifyBus()
	queue := local.NewJobQueue(64)
	facadeEnv := localenv.New(cwd, localenv.WithShell(opts.ShellPath))
	facadeToolDefs := builtin.NewCodingTools(facadeEnv, opts.ShellCommandPrefix)
	sessionsDir, err := resolveSessionsDir(dataDir, opts.SessionsDir)
	if err != nil {
		return nil, err
	}
	repo := session.NewJsonlSessionRepo(sessionsDir)
	emit := worker.NewEmitter(store, bus)
	uiBridge := newPluginUIBridge(store, bus, slog.Default())

	stream := opts.FakeStream
	if stream == nil {
		if opts.Provider == nil {
			return nil, errors.New("app: provider registry is required for production stream")
		}
		stream = productionStream(opts.Provider)
	}

	pluginManager, err := startPluginManager(ctx, opts.Plugins, store, bus, cwd, uiBridge.handleRequest)
	if err != nil {
		return nil, err
	}
	uiBridge.setManager(pluginManager)
	if pluginManager != nil {
		if err := activateEnginePlugins(ctx, pluginManager, opts.Plugins); err != nil {
			stopPluginManager(pluginManager)
			return nil, err
		}
	}
	var pluginBridge *PluginHostBridge
	if pluginManager != nil {
		pluginBridge = &PluginHostBridge{
			manager:       pluginManager,
			sessionMounts: map[string]bool{},
		}
	}

	workerHost := worker.New(worker.Options{
		Queue:            queue,
		Store:            store,
		Bus:              bus,
		ExtCommandRunner: pluginBridge.dispatchQueuedCommand,
		UIResolve: func(ctx context.Context, sessionID string, req protocol.UIResolveRequest) error {
			return uiBridge.resolveFromClient(ctx, sessionID, req)
		},
		Factory: func(sessionID string) (backend.Agent, error) {
			sess, ok, err := repo.Get(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("app: session %q not found", sessionID)
			}
			env := localenv.New(cwd, localenv.WithShell(opts.ShellPath))
			tools, err := wrapBuiltinTools(env, opts.ShellCommandPrefix, opts.Model)
			if err != nil {
				return nil, err
			}
			var hooks *hook.Registry
			var middleware []pipeline.RunMiddleware
			if pluginManager != nil {
				hooks = hook.NewRegistry()
				mounts, err := activateSessionPlugins(ctx, pluginManager, opts.Plugins, sess.GetMetadata())
				if err != nil {
					return nil, err
				}
				pluginBridge.recordSessionMount(sessionID)
				hostTools, err := buildHostTools(pluginBridge, sessionID, mounts, tools)
				if err != nil {
					return nil, err
				}
				tools = append(tools, hostTools...)
				hasHooks, err := registerHostHooks(hooks, pluginBridge, sessionID, mounts)
				if err != nil {
					return nil, err
				}
				if !hasHooks {
					hooks = nil
				}
				if hooksNeedRunContextMiddleware(hooks) {
					middleware = append(middleware, pipeline.MWContext(pipeline.ContextConfig{
						Hooks: hooks,
						Tools: promptTools(tools),
					}))
				}
				if err := emitSessionCapabilities(emit, sessionID, tools); err != nil {
					return nil, err
				}
			}
			view := &sessionView{ctx: ctx, session: sess}
			agent := worker.NewNative(worker.NativeOptions{
				SessionID: sessionID,
				Snapshot: func() pipeline.AgentSnapshot {
					return pipeline.AgentSnapshot{
						Model:         opts.Model,
						ThinkingLevel: thinkingLevel,
					}
				},
				Env: pipeline.EnvView{
					ID:           "local",
					CWD:          cwd,
					Capabilities: []string{"filesystem", "shell"},
				},
				Session:    view,
				Emit:       emit,
				Stream:     stream,
				Tools:      tools,
				Middleware: middleware,
				Hooks:      hooks,
				ResolveModel: func(spec string) (ai.Model, error) {
					if opts.Provider == nil {
						return ai.Model{}, errors.New("app: provider registry is required for model resolution")
					}
					return resolveModelSpec(opts.Provider, spec)
				},
				OnModelChange: func(ctx context.Context, m ai.Model) error {
					_, err := sess.AppendModelChange(ctx, m.Provider, m.ID)
					return err
				},
			})
			return agent, nil
		},
	})

	hostCtx, cancelHost := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := workerHost.Run(hostCtx); err != nil {
			fmt.Fprintf(os.Stderr, "app: worker host stopped: %v\n", err)
		}
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancelHost()
			<-done
			if pluginManager != nil {
				stopPluginManager(pluginManager)
			}
		})
	}

	return &Composition{
		Store:       store,
		Bus:         bus,
		Queue:       queue,
		Repo:        repo,
		Host:        workerHost,
		ExtCommands: pluginBridge,
		NewSessionFacade: func(sessionID string) *enginefacade.Facade {
			return enginefacade.New(enginefacade.Options{
				SessionID:          sessionID,
				Repo:               repo,
				CWD:                cwd,
				Model:              opts.Model,
				ThinkingLevel:      string(thinkingLevel),
				Provider:           opts.Provider,
				ShellPath:          opts.ShellPath,
				ShellCommandPrefix: opts.ShellCommandPrefix,
				// Metadata 签名无错误位：取不到时只报 ID（不伪造其余字段）。
				Metadata: func() session.Metadata {
					s, ok, err := repo.Get(ctx, sessionID)
					if err != nil || !ok {
						return session.Metadata{ID: sessionID}
					}
					return s.GetMetadata()
				},
				Store:       store,
				Bus:         bus,
				Queue:       queue,
				ExportTools: exportToolDefinitions(facadeToolDefs),
			})
		},
		Stop: stop,
	}, nil
}

func startPluginManager(
	ctx context.Context,
	registry *plugin.Registry,
	store backend.EventStore,
	bus backend.NotifyBus,
	cwd string,
	onUIRequest func(pluginhost.UIRequest) (string, error),
) (*pluginhost.Manager, error) {
	if registry == nil {
		return nil, nil
	}
	if !hasEnabledContributions(registry) {
		return nil, nil
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("app: plugin host requires node for enabled plugin contributions: %w", err)
	}

	manager := pluginhost.New(pluginhost.Options{
		NodePath:    nodePath,
		WorkDir:     cwd,
		OnEvent:     pluginEventSink(store, bus),
		OnUIRequest: onUIRequest,
	})
	if err := manager.Start(ctx); err != nil {
		return nil, fmt.Errorf("app: start plugin host: %w", err)
	}
	return manager, nil
}

func hasEnabledContributions(registry *plugin.Registry) bool {
	for _, owner := range []string{"session", "engine", "environment"} {
		if len(registry.EnabledWithContribution(owner)) > 0 {
			return true
		}
	}
	return false
}

func activateEnginePlugins(
	ctx context.Context,
	manager *pluginhost.Manager,
	registry *plugin.Registry,
) error {
	for _, info := range registry.EnabledWithContribution("engine") {
		modulePath, ok := registry.ContributionPath(info.ID, "engine")
		if !ok {
			return fmt.Errorf("app: enabled plugin %q lost engine contribution", info.ID)
		}
		_, err := manager.Activate(ctx, pluginhost.ActivateSpec{
			PluginID:   info.ID,
			Owner:      "engine",
			ModulePath: modulePath,
		})
		if err != nil {
			return fmt.Errorf("app: activate engine plugin %q: %w", info.ID, err)
		}
	}
	return nil
}

func activateSessionPlugins(
	ctx context.Context,
	manager *pluginhost.Manager,
	registry *plugin.Registry,
	metadata session.Metadata,
) ([]sessionPluginMount, error) {
	ctxSeed, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("app: marshal plugin session metadata for %q: %w", metadata.ID, err)
	}

	mounts := []sessionPluginMount{}
	for _, info := range registry.EnabledWithContribution("session") {
		modulePath, ok := registry.ContributionPath(info.ID, "session")
		if !ok {
			return nil, fmt.Errorf("app: enabled plugin %q lost session contribution", info.ID)
		}
		registered, err := manager.Activate(ctx, pluginhost.ActivateSpec{
			PluginID:   info.ID,
			Owner:      "session",
			SessionID:  metadata.ID,
			ModulePath: modulePath,
			CtxSeed:    ctxSeed,
		})
		if err != nil {
			return nil, fmt.Errorf("app: activate session plugin %q for %q: %w", info.ID, metadata.ID, err)
		}
		mounts = append(mounts, sessionPluginMount{
			PluginID:   info.ID,
			Registered: registered,
		})
	}
	return mounts, nil
}

type sessionPluginMount struct {
	PluginID   string
	Registered pluginhost.Registered
}

func registerHostHooks(
	registry *hook.Registry,
	bridge *PluginHostBridge,
	sessionID string,
	mounts []sessionPluginMount,
) (bool, error) {
	if registry == nil {
		return false, nil
	}
	if bridge == nil {
		return false, pluginHostDownError{err: pluginhost.ErrHostDown}
	}

	hasHooks := false
	for _, mount := range mounts {
		for _, reg := range mount.Registered.Hooks {
			if reg.Point == "" {
				return false, fmt.Errorf("app: plugin %q registered hook with empty point", mount.PluginID)
			}
			if reg.Index < 0 {
				return false, fmt.Errorf(
					"app: plugin %q hook %q registered negative index %d",
					mount.PluginID,
					reg.Point,
					reg.Index,
				)
			}
			hasHooks = true
			pluginID := mount.PluginID
			point := reg.Point
			index := reg.Index
			registry.On(point, func(ctx context.Context, event any) (any, error) {
				eventJSON, err := json.Marshal(event)
				if err != nil {
					return nil, fmt.Errorf("app: marshal hook %q event for plugin %q: %w", point, pluginID, err)
				}
				raw, err := bridge.DispatchHook(
					ctx,
					pluginID,
					"session",
					sessionID,
					point,
					index,
					eventJSON,
				)
				if err != nil {
					return nil, err
				}
				return decodeHostHookResult(point, raw)
			})
		}
	}
	return hasHooks, nil
}

func decodeHostHookResult(point string, raw []byte) (any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	switch point {
	case hook.EventInput:
		var result hook.InputResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode input hook result: %w", err)
		}
		return result, nil
	case hook.EventToolCall:
		var result hook.ToolCallResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode tool_call hook result: %w", err)
		}
		return result, nil
	case hook.EventToolResult:
		var result hook.ToolResultPatch
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode tool_result hook result: %w", err)
		}
		return result, nil
	case hook.EventContext:
		var result hook.ContextResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode context hook result: %w", err)
		}
		return result, nil
	case hook.EventBeforeAgentStart:
		var result hook.BeforeAgentStartResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode before_agent_start hook result: %w", err)
		}
		return result, nil
	case hook.EventBeforeProviderRequest:
		var result hook.BeforeProviderRequestResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode before_provider_request hook result: %w", err)
		}
		return result, nil
	case hook.EventBeforeProviderPayload:
		var result hook.BeforeProviderPayloadResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode before_provider_payload hook result: %w", err)
		}
		return result, nil
	case hook.EventMessageEnd:
		var result hook.MessageEndResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode message_end hook result: %w", err)
		}
		return result, nil
	case hook.EventResourcesDiscover:
		var result hook.ResourcesDiscoverResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode resources_discover hook result: %w", err)
		}
		return result, nil
	case hook.EventSessionBeforeSwitch:
		var result hook.SessionBeforeSwitchResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode session_before_switch hook result: %w", err)
		}
		return result, nil
	case hook.EventSessionBeforeFork:
		var result hook.SessionBeforeForkResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode session_before_fork hook result: %w", err)
		}
		return result, nil
	case hook.EventSessionBeforeCompact:
		var result hook.SessionBeforeCompactResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode session_before_compact hook result: %w", err)
		}
		return result, nil
	case hook.EventSessionBeforeTree:
		var result hook.SessionBeforeTreeResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode session_before_tree hook result: %w", err)
		}
		return result, nil
	case hook.EventUserBash:
		var result hook.UserBashEventResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("app: decode user_bash hook result: %w", err)
		}
		return result, nil
	case hook.EventAfterProviderResponse:
		return nil, nil
	default:
		return nil, fmt.Errorf("app: unsupported hook point %q", point)
	}
}

func hooksNeedRunContextMiddleware(registry *hook.Registry) bool {
	if registry == nil {
		return false
	}
	return registry.HasHandlers(hook.EventBeforeAgentStart) ||
		registry.HasHandlers(hook.EventContext)
}

func promptTools(tools []tool.Tool) []pipeline.PromptTool {
	out := make([]pipeline.PromptTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, pipeline.PromptTool{
			Name:              t.Name,
			AvailableToPrompt: true,
		})
	}
	return out
}

type sessionCapabilitiesPayload struct {
	Tools []string `json:"tools"`
}

type hostToolResultPayload struct {
	Content json.RawMessage `json:"content,omitempty"`
	Details any             `json:"details,omitempty"`
	IsError bool            `json:"isError,omitempty"`
}

func buildHostTools(
	bridge *PluginHostBridge,
	sessionID string,
	mounts []sessionPluginMount,
	existing []tool.Tool,
) ([]tool.Tool, error) {
	if bridge == nil {
		return nil, pluginHostDownError{err: pluginhost.ErrHostDown}
	}

	seen := make(map[string]string, len(existing))
	for _, existingTool := range existing {
		if existingTool.Name == "" {
			return nil, errors.New("app: existing tool with empty name")
		}
		seen[existingTool.Name] = "native"
	}

	hostTools := []tool.Tool{}
	for _, mount := range mounts {
		for _, reg := range mount.Registered.Tools {
			if reg.Name == "" {
				return nil, fmt.Errorf("app: plugin %q registered tool with empty name", mount.PluginID)
			}
			if owner, ok := seen[reg.Name]; ok {
				return nil, fmt.Errorf("app: plugin %q tool %q duplicates %s tool", mount.PluginID, reg.Name, owner)
			}
			wrapped, err := buildHostTool(bridge, sessionID, mount.PluginID, reg)
			if err != nil {
				return nil, err
			}
			hostTools = append(hostTools, wrapped)
			seen[reg.Name] = "plugin " + mount.PluginID
		}
	}
	return hostTools, nil
}

func buildHostTool(
	bridge *PluginHostBridge,
	sessionID string,
	pluginID string,
	reg pluginhost.ToolReg,
) (tool.Tool, error) {
	parameters, err := decodeHostToolSchema(pluginID, reg)
	if err != nil {
		return tool.Tool{}, err
	}

	return tool.Tool{
		Tool: ai.Tool{
			Name:        reg.Name,
			Description: reg.Description,
			Parameters:  parameters,
		},
		Execute: func(
			ctx context.Context,
			toolCallID string,
			args map[string]any,
			onUpdate tool.UpdateCallback,
		) (tool.Result, error) {
			_ = toolCallID
			_ = onUpdate
			if args == nil {
				args = map[string]any{}
			}
			payload, err := json.Marshal(args)
			if err != nil {
				return tool.Result{}, fmt.Errorf("app: marshal host tool %q args: %w", reg.Name, err)
			}
			raw, err := bridge.ExecuteTool(ctx, pluginID, "session", sessionID, reg.Name, payload)
			if err != nil {
				var hostDown interface{ ExtHostDown() bool }
				if errors.As(err, &hostDown) && hostDown.ExtHostDown() {
					return tool.Result{
						Content: []ai.ContentBlock{{
							Type: ai.ContentText,
							Text: "ext_host_down: " + err.Error(),
						}},
						IsError: true,
					}, nil
				}
				return tool.Result{}, err
			}
			return decodeHostToolResult(raw)
		},
	}, nil
}

func decodeHostToolSchema(pluginID string, reg pluginhost.ToolReg) (map[string]any, error) {
	if len(reg.Schema) == 0 {
		return nil, nil
	}
	var parameters map[string]any
	if err := json.Unmarshal(reg.Schema, &parameters); err != nil {
		return nil, fmt.Errorf("app: plugin %q tool %q schema: %w", pluginID, reg.Name, err)
	}
	if parameters == nil {
		return nil, fmt.Errorf("app: plugin %q tool %q schema must be a JSON object", pluginID, reg.Name)
	}
	return parameters, nil
}

func decodeHostToolResult(raw []byte) (tool.Result, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return tool.Result{}, errors.New("app: host tool result is empty")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return tool.Result{}, errors.New("app: host tool result must be a JSON object")
	}

	var payload hostToolResultPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return tool.Result{}, fmt.Errorf("app: decode host tool result: %w", err)
	}
	content, err := decodeHostToolContent(payload.Content)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content: content,
		Details: payload.Details,
		IsError: payload.IsError,
	}, nil
}

func decodeHostToolContent(raw json.RawMessage) ([]ai.ContentBlock, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, fmt.Errorf("app: decode host tool text content: %w", err)
		}
		return []ai.ContentBlock{{Type: ai.ContentText, Text: text}}, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var blocks []ai.ContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, fmt.Errorf("app: decode host tool block content: %w", err)
		}
		for i, block := range blocks {
			if block.Type == "" {
				return nil, fmt.Errorf("app: host tool content block %d missing type", i)
			}
		}
		return blocks, nil
	}
	return nil, errors.New("app: host tool content must be a string or []ai.ContentBlock")
}

func emitSessionCapabilities(
	emit pipeline.Emitter,
	sessionID string,
	tools []tool.Tool,
) error {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return emit(&pipeline.RunContext{SessionID: sessionID},
		protocol.KindSessionCapabilities,
		protocol.ActorSystem,
		sessionCapabilitiesPayload{Tools: names})
}

func pluginEventSink(
	store backend.EventStore,
	bus backend.NotifyBus,
) func(pluginID, owner, sessionID, name string, payload []byte) error {
	return func(pluginID, owner, sessionID, name string, payload []byte) error {
		if pluginID == "" {
			return errors.New("app: plugin event missing plugin id")
		}
		if name == "" {
			return fmt.Errorf("app: plugin %q emitted event with empty name", pluginID)
		}
		if sessionID == "" {
			if owner == "engine" {
				return fmt.Errorf(
					"app: plugin %q events.emit(%q) rejected: engine scope has no downlink (c3)",
					pluginID,
					name,
				)
			}
			return fmt.Errorf(
				"app: plugin %q events.emit(%q) rejected: owner %q has no session downlink",
				pluginID,
				name,
				owner,
			)
		}
		if owner != "session" {
			return fmt.Errorf(
				"app: plugin %q events.emit(%q) rejected: owner %q is not session-scoped",
				pluginID,
				name,
				owner,
			)
		}
		if payload != nil && !json.Valid(payload) {
			return fmt.Errorf("app: plugin %q event %q payload is not valid JSON", pluginID, name)
		}

		raw := json.RawMessage(nil)
		if payload != nil {
			raw = append(json.RawMessage(nil), payload...)
		}
		env := &protocol.Envelope{
			ID:            protocol.NewEventID(),
			SessionID:     sessionID,
			Kind:          "ext." + pluginID + "." + name,
			Actor:         protocol.ActorSystem, // review: plugin actor identity
			Payload:       raw,
			OccurredAt:    time.Now().UTC(),
			SchemaVersion: protocol.SchemaVersion,
		}
		if err := store.Append(context.Background(), env); err != nil {
			return fmt.Errorf("app: append plugin event %q for session %q: %w", env.Kind, sessionID, err)
		}
		bus.Publish(sessionID)
		return nil
	}
}

func stopPluginManager(manager *pluginhost.Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "app: plugin host stopped with error: %v\n", err)
	}
}

func resolveDataDir(dataDir string) (string, error) {
	if dataDir != "" {
		return filepath.Abs(dataDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".along"), nil
}

func resolveSessionsDir(dataDir string, sessionsDir string) (string, error) {
	if sessionsDir != "" {
		return filepath.Abs(sessionsDir)
	}
	return filepath.Join(dataDir, "sessions"), nil
}

func resolveCWD(cwd string) (string, error) {
	if cwd != "" {
		return filepath.Abs(cwd)
	}
	return os.Getwd()
}

func productionStream(prov *provider.Registry) agentloop.StreamFunc {
	return func(
		ctx context.Context,
		model ai.Model,
		llmCtx ai.Context,
		opts ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		auth, err := prov.ResolveAuth(ctx, model)
		if err != nil {
			return streamError(err)
		}
		if auth.APIKey != "" {
			opts.APIKey = auth.APIKey
		}
		if len(auth.Headers) > 0 {
			headers := maps.Clone(opts.Headers)
			if headers == nil {
				headers = map[string]string{}
			}
			maps.Copy(headers, auth.Headers)
			opts.Headers = headers
		}
		return ai.StreamSimple(ctx, model, llmCtx, opts)
	}
}

func streamError(err error) *ai.AssistantMessageEventStream {
	return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
		yield(nil, err)
	})
}

func wrapBuiltinTools(
	env *localenv.Env,
	shellCommandPrefix string,
	model ai.Model,
) ([]tool.Tool, error) {
	ctxFactory := func(context.Context) extension.ExtensionContext {
		return extension.ExtensionContext{
			Cwd:   env.Cwd(),
			Model: func() ai.Model { return model },
		}
	}
	defs := builtin.NewCodingTools(env, shellCommandPrefix)
	out := make([]tool.Tool, 0, len(defs))
	for _, def := range defs {
		wrapped, err := builtin.Wrap(def, ctxFactory)
		if err != nil {
			return nil, err
		}
		out = append(out, wrapped)
	}
	return out, nil
}

func exportToolDefinitions(defs []extension.ToolDefinition) []exporthtml.ToolDefinition {
	out := make([]exporthtml.ToolDefinition, 0, len(defs))
	for _, def := range defs {
		out = append(out, exporthtml.ToolDefinition{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  def.Parameters,
		})
	}
	return out
}

func resolveModelSpec(prov *provider.Registry, spec string) (ai.Model, error) {
	if spec == "" {
		return ai.Model{}, errors.New("app: model spec is empty")
	}
	if providerName, modelID, ok := strings.Cut(spec, "/"); ok {
		if providerName == "" || modelID == "" {
			return ai.Model{}, fmt.Errorf("app: invalid model spec %q", spec)
		}
		model, found := prov.ResolveByProvider(providerName, modelID)
		if !found {
			return ai.Model{}, fmt.Errorf("app: unknown model %q", spec)
		}
		return model, nil
	}
	model, found := prov.Resolve(spec)
	if !found {
		return ai.Model{}, fmt.Errorf("app: unknown model %q", spec)
	}
	return model, nil
}

type sessionView struct {
	ctx     context.Context
	session *session.Session
}

func (v *sessionView) Messages() []message.AgentMessage {
	ctx, err := v.session.BuildContext(v.ctx)
	if err != nil {
		panic(err)
	}
	return ctx.Messages
}

func (v *sessionView) AppendMessage(m message.AgentMessage) error {
	_, err := v.session.AppendMessage(v.ctx, m)
	return err
}
