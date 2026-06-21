package rpc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

var errRPCUIClosed = errors.New("rpc: extension UI closed")

type rpcUIContext struct {
	ctx    context.Context
	server *server
}

// rpcUIContext mirrors pi's extension UI RPC bridge for along's serializable
// compat.UIContext surface:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-types.ts:213-258.
// Requests use fresh ids and wait for extension_ui_response like pi's
// createDialogPromise/createExtensionUIContext:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:90-270.
func newRPCUIContext(ctx context.Context, s *server) *rpcUIContext {
	if ctx == nil {
		ctx = context.Background()
	}
	return &rpcUIContext{ctx: ctx, server: s}
}

func (ui *rpcUIContext) Select(prompt string, options []string) (int, error) {
	resp, err := ui.server.requestUI(ui.ctx, func(id string) any {
		return uiSelectRequest{
			Type:    "extension_ui_request",
			ID:      id,
			Method:  "select",
			Title:   prompt,
			Options: append([]string(nil), options...),
		}
	})
	if err != nil {
		return -1, err
	}
	if resp.Cancelled {
		return -1, errors.New("rpc: extension UI select cancelled")
	}
	if resp.Value == nil {
		return -1, errors.New("rpc: extension UI select response missing value")
	}
	for i, option := range options {
		if option == *resp.Value {
			return i, nil
		}
	}
	return -1, fmt.Errorf("rpc: extension UI select response value %q not found", *resp.Value)
}

func (ui *rpcUIContext) Confirm(prompt string) (bool, error) {
	resp, err := ui.server.requestUI(ui.ctx, func(id string) any {
		return uiConfirmRequest{
			Type:   "extension_ui_request",
			ID:     id,
			Method: "confirm",
			Title:  prompt,
		}
	})
	if err != nil {
		return false, err
	}
	if resp.Cancelled {
		return false, nil
	}
	if resp.Confirmed == nil {
		return false, errors.New("rpc: extension UI confirm response missing confirmed")
	}
	return *resp.Confirmed, nil
}

func (ui *rpcUIContext) Input(prompt string) (string, error) {
	resp, err := ui.server.requestUI(ui.ctx, func(id string) any {
		return uiInputRequest{
			Type:   "extension_ui_request",
			ID:     id,
			Method: "input",
			Title:  prompt,
		}
	})
	if err != nil {
		return "", err
	}
	if resp.Cancelled {
		return "", nil
	}
	if resp.Value == nil {
		return "", errors.New("rpc: extension UI input response missing value")
	}
	return *resp.Value, nil
}

func (ui *rpcUIContext) Editor(title, prefill string) (string, error) {
	resp, err := ui.server.requestUI(ui.ctx, func(id string) any {
		return uiEditorRequest{
			Type:    "extension_ui_request",
			ID:      id,
			Method:  "editor",
			Title:   title,
			Prefill: prefill,
		}
	})
	if err != nil {
		return "", err
	}
	if resp.Cancelled {
		return "", nil
	}
	if resp.Value == nil {
		return "", errors.New("rpc: extension UI editor response missing value")
	}
	return *resp.Value, nil
}

func (ui *rpcUIContext) Notify(msg string) {
	// pi notify is fire-and-forget and emits an extension_ui_request without
	// registering a response waiter:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:151-155.
	id, err := ui.server.nextUIRequestID()
	if err != nil {
		return
	}
	ui.server.write(uiNotifyRequest{
		Type:    "extension_ui_request",
		ID:      id,
		Method:  "notify",
		Message: msg,
	})
}

func (ui *rpcUIContext) SetStatus(key, text string) {
	id, err := ui.server.nextUIRequestID()
	if err != nil {
		return
	}
	ui.server.write(uiSetStatusRequest{
		Type:       "extension_ui_request",
		ID:         id,
		Method:     "setStatus",
		StatusKey:  key,
		StatusText: text,
	})
}

func (ui *rpcUIContext) SetWidget(key string, lines []string) {
	id, err := ui.server.nextUIRequestID()
	if err != nil {
		return
	}
	ui.server.write(uiSetWidgetRequest{
		Type:        "extension_ui_request",
		ID:          id,
		Method:      "setWidget",
		WidgetKey:   key,
		WidgetLines: append([]string{}, lines...),
	})
}

func (ui *rpcUIContext) SetTitle(title string) {
	id, err := ui.server.nextUIRequestID()
	if err != nil {
		return
	}
	ui.server.write(uiSetTitleRequest{
		Type:   "extension_ui_request",
		ID:     id,
		Method: "setTitle",
		Title:  title,
	})
}

func (ui *rpcUIContext) SetEditorText(text string) {
	id, err := ui.server.nextUIRequestID()
	if err != nil {
		return
	}
	ui.server.write(uiSetEditorTextRequest{
		Type:   "extension_ui_request",
		ID:     id,
		Method: "set_editor_text",
		Text:   text,
	})
}

// Serializable extension UI request structs mirror pi's RpcExtensionUIRequest
// tagged union:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-types.ts:213-248.
// Future declarative GUI widgets can add a new method variant such as
// "component" per developer/18-extension-ui-parity.md §4; do not implement it
// until that protocol is designed.
type uiSelectRequest struct {
	Type    string   `json:"type"`
	ID      string   `json:"id"`
	Method  string   `json:"method"`
	Title   string   `json:"title"`
	Options []string `json:"options"`
}

type uiConfirmRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Title  string `json:"title"`
}

type uiInputRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Title  string `json:"title"`
}

type uiEditorRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Title   string `json:"title"`
	Prefill string `json:"prefill"`
}

type uiNotifyRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Message string `json:"message"`
}

type uiSetStatusRequest struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Method     string `json:"method"`
	StatusKey  string `json:"statusKey"`
	StatusText string `json:"statusText"`
}

type uiSetWidgetRequest struct {
	Type        string   `json:"type"`
	ID          string   `json:"id"`
	Method      string   `json:"method"`
	WidgetKey   string   `json:"widgetKey"`
	WidgetLines []string `json:"widgetLines"`
	// Along has no placement option yet, so widgetPlacement is omitted.
}

type uiSetTitleRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Title  string `json:"title"`
}

type uiSetEditorTextRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Text   string `json:"text"`
}

type uiResponse struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	Value     *string `json:"value,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
	Cancelled bool    `json:"cancelled,omitempty"`
}

func (s *server) requestUI(
	ctx context.Context,
	buildRequest func(id string) any,
) (uiResponse, error) {
	id, ch, err := s.registerUIRequest()
	if err != nil {
		return uiResponse{}, err
	}
	if err := s.writer.WriteJSON(buildRequest(id)); err != nil {
		s.removeUIRequest(id)
		return uiResponse{}, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return uiResponse{}, errRPCUIClosed
		}
		return resp, nil
	case <-ctx.Done():
		s.removeUIRequest(id)
		return uiResponse{}, ctx.Err()
	}
}

func (s *server) nextUIRequestID() (string, error) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	if s.uiClosed {
		return "", errRPCUIClosed
	}
	s.nextUIID++
	return "ui-" + strconv.FormatUint(s.nextUIID, 10), nil
}

func (s *server) registerUIRequest() (string, <-chan uiResponse, error) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	if s.uiClosed {
		return "", nil, errRPCUIClosed
	}
	s.nextUIID++
	id := "ui-" + strconv.FormatUint(s.nextUIID, 10)
	ch := make(chan uiResponse, 1)
	s.pendingUI[id] = ch
	return id, ch, nil
}

func (s *server) removeUIRequest(id string) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	delete(s.pendingUI, id)
}

func (s *server) deliverUIResponse(resp uiResponse) {
	s.uiMu.Lock()
	ch, ok := s.pendingUI[resp.ID]
	if ok {
		delete(s.pendingUI, resp.ID)
	}
	s.uiMu.Unlock()
	if ok {
		ch <- resp
	}
}

func (s *server) closeUIRequests() {
	s.uiMu.Lock()
	pending := s.pendingUI
	s.pendingUI = map[string]chan uiResponse{}
	s.uiClosed = true
	s.uiMu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
}
