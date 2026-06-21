package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/backend/local"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type fakeExtCommandDispatcher struct {
	mu     sync.Mutex
	calls  []protocol.ExtCommandRequest
	result []byte
	err    error
	route  backend.ExtCommandRouteMode
}

func (f *fakeExtCommandDispatcher) Route(
	owner string,
	sessionID string,
	envID string,
) (backend.ExtCommandRouteMode, error) {
	if f.route != "" {
		return f.route, nil
	}
	return backend.ExtCommandRouteInProcess, nil
}

func (f *fakeExtCommandDispatcher) DispatchCommand(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	envID string,
	name string,
	payload []byte,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, protocol.ExtCommandRequest{
		PluginID:  pluginID,
		Owner:     owner,
		SessionID: sessionID,
		EnvID:     envID,
		Name:      name,
		Payload:   append([]byte(nil), payload...),
	})
	if f.err != nil {
		return nil, f.err
	}
	return append([]byte(nil), f.result...), nil
}

func (f *fakeExtCommandDispatcher) lastCall(t *testing.T) protocol.ExtCommandRequest {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("dispatcher was not called")
	}
	return f.calls[len(f.calls)-1]
}

type fakeExtHostDownError struct{}

func (fakeExtHostDownError) Error() string { return "plugin host down" }

func (fakeExtHostDownError) ExtHostDown() bool { return true }

func startExtCommandTestServer(dispatcher ExtCommandDispatcher) *testServer {
	srv := &Server{
		opts: Options{
			Token:       "tok-test",
			ExtCommands: dispatcher,
		},
		sessions: map[string]*session.Session{},
		metadata: map[string]session.Metadata{},
	}
	mux := http.NewServeMux()
	srv.routes(mux)
	return &testServer{handler: srv.middleware(mux)}
}

func TestExtCommandHappyPath(t *testing.T) {
	dispatcher := &fakeExtCommandDispatcher{result: []byte(`{"ok":true}`)}
	harness := startExtCommandTestServer(dispatcher)

	var response protocol.ExtCommandResponse
	rec := doJSON(t, harness, "POST", "/v1/ext/command", protocol.ExtCommandRequest{
		PluginID:  "fixture-plugin",
		Owner:     "session",
		SessionID: "ses_1",
		Name:      "echo",
		Payload:   json.RawMessage(`{"msg":"hi"}`),
	}, &response)
	if rec.Code != http.StatusOK || string(response.Result) != `{"ok":true}` {
		t.Fatalf("response = %d %s", rec.Code, string(response.Result))
	}

	call := dispatcher.lastCall(t)
	if call.PluginID != "fixture-plugin" ||
		call.Owner != "session" ||
		call.SessionID != "ses_1" ||
		call.Name != "echo" ||
		string(call.Payload) != `{"msg":"hi"}` {
		t.Fatalf("dispatcher call = %+v payload=%s", call, string(call.Payload))
	}
}

func TestExtCommandValidation(t *testing.T) {
	harness := startExtCommandTestServer(&fakeExtCommandDispatcher{result: []byte(`null`)})
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{
			name:    "missing plugin id",
			body:    `{"owner":"engine","name":"echo"}`,
			message: "pluginId is required",
		},
		{
			name:    "missing command name",
			body:    `{"pluginId":"p","owner":"engine"}`,
			message: "name is required",
		},
		{
			name:    "unknown owner",
			body:    `{"pluginId":"p","owner":"workspace","name":"echo"}`,
			message: "owner must be engine, session, or environment",
		},
		{
			name:    "session requires session id",
			body:    `{"pluginId":"p","owner":"session","name":"echo"}`,
			message: "session owner requires sessionId",
		},
		{
			name:    "environment requires env id",
			body:    `{"pluginId":"p","owner":"environment","name":"echo"}`,
			message: "environment owner requires envId",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := rawJSON(t, harness, "POST", "/v1/ext/command", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if body.Error.Code != "bad_request" || !strings.Contains(body.Error.Message, tt.message) {
				t.Fatalf("error = %+v, want message containing %q", body.Error, tt.message)
			}
		})
	}
}

func TestExtCommandNilDispatcher(t *testing.T) {
	harness, _, _, _ := startTestServer(t)
	rec, body := rawJSON(
		t,
		harness,
		"POST",
		"/v1/ext/command",
		`{"pluginId":"p","owner":"engine","name":"echo"}`,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body.Error.Code != "plugins_unavailable" {
		t.Fatalf("error code = %q, want plugins_unavailable", body.Error.Code)
	}
}

func TestExtCommandErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		body string
	}{
		{
			name: "handler error",
			err:  errors.New("handler failed"),
			code: http.StatusUnprocessableEntity,
			body: "ext_command_failed",
		},
		{
			name: "timeout",
			err:  context.DeadlineExceeded,
			code: http.StatusGatewayTimeout,
			body: "ext_command_timeout",
		},
		{
			name: "host down",
			err:  fakeExtHostDownError{},
			code: http.StatusServiceUnavailable,
			body: "ext_host_down",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			harness := startExtCommandTestServer(&fakeExtCommandDispatcher{err: tt.err})
			rec, body := rawJSON(
				t,
				harness,
				"POST",
				"/v1/ext/command",
				`{"pluginId":"p","owner":"engine","name":"echo"}`,
			)
			if rec.Code != tt.code {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if body.Error.Code != tt.body {
				t.Fatalf("error code = %q, want %q", body.Error.Code, tt.body)
			}
		})
	}
}

func TestExtCommandCrossProcessResult(t *testing.T) {
	harness := startExtCommandXprocTestServer(t, "result")

	var response protocol.ExtCommandResponse
	rec := doJSON(t, harness, "POST", "/v1/ext/command", protocol.ExtCommandRequest{
		PluginID:  "todo",
		Owner:     "session",
		SessionID: "ses_xproc_result",
		Name:      "list",
		Payload:   json.RawMessage(`{"limit":1}`),
	}, &response)
	if rec.Code != http.StatusOK || string(response.Result) != `{"ok":true}` {
		t.Fatalf("response = %d %s", rec.Code, string(response.Result))
	}
}

func TestExtCommandCrossProcessFailed(t *testing.T) {
	harness := startExtCommandXprocTestServer(t, "failed")

	rec, body := rawJSON(
		t,
		harness,
		"POST",
		"/v1/ext/command",
		`{"pluginId":"todo","owner":"session","sessionId":"ses_xproc_failed","name":"list"}`,
	)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body.Error.Code != "dispatch_failed" || body.Error.Message != "boom" {
		t.Fatalf("error = %+v", body.Error)
	}
}

func TestExtCommandCrossProcessDeadlineReturns504(t *testing.T) {
	oldDeadline := extCommandAPIDeadline
	extCommandAPIDeadline = 10 * time.Millisecond
	t.Cleanup(func() { extCommandAPIDeadline = oldDeadline })

	harness := startExtCommandXprocTestServer(t, "none")
	rec, body := rawJSON(
		t,
		harness,
		"POST",
		"/v1/ext/command",
		`{"pluginId":"todo","owner":"session","sessionId":"ses_xproc_timeout","name":"list"}`,
	)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body.Error.Code != "ext_command_timeout" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestExtCommandUnsupportedOwnerReturns501(t *testing.T) {
	harness := startExtCommandTestServer(&fakeExtCommandDispatcher{
		route: backend.ExtCommandRouteUnsupported,
	})
	rec, body := rawJSON(
		t,
		harness,
		"POST",
		"/v1/ext/command",
		`{"pluginId":"todo","owner":"environment","envId":"env_1","name":"list"}`,
	)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body.Error.Code != "not_supported" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func startExtCommandXprocTestServer(t *testing.T, mode string) *testServer {
	t.Helper()

	store := local.NewEventStore(t.TempDir())
	bus := local.NewNotifyBus()
	queue := &xprocTestQueue{mode: mode, store: store, bus: bus}
	srv := &Server{
		opts: Options{
			Token:       "tok-test",
			Store:       store,
			Bus:         bus,
			Queue:       queue,
			ExtCommands: &fakeExtCommandDispatcher{route: backend.ExtCommandRouteCrossProcess},
		},
		sessions: map[string]*session.Session{},
		metadata: map[string]session.Metadata{},
	}
	mux := http.NewServeMux()
	srv.routes(mux)
	return &testServer{handler: srv.middleware(mux)}
}

type xprocTestQueue struct {
	mode  string
	store backend.EventStore
	bus   backend.NotifyBus
}

func (q *xprocTestQueue) Enqueue(ctx context.Context, job backend.Job) error {
	if q.mode == "none" {
		return nil
	}
	var payload protocol.ExtCommandJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	var (
		kind     string
		response any
	)
	switch q.mode {
	case "result":
		kind = protocol.KindExtCommandResult
		response = protocol.ExtCommandResultPayload{
			CorrID: payload.CorrID,
			Result: json.RawMessage(`{"ok":true}`),
		}
	case "failed":
		kind = protocol.KindExtCommandFailed
		response = protocol.ExtCommandFailedPayload{
			CorrID:  payload.CorrID,
			Code:    "dispatch_failed",
			Message: "boom",
		}
	default:
		return nil
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if err := q.store.Append(ctx, &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     job.SessionID,
		Kind:          kind,
		Actor:         protocol.ActorSystem,
		Payload:       raw,
		OccurredAt:    time.Now().UTC(),
		SchemaVersion: protocol.SchemaVersion,
	}); err != nil {
		return err
	}
	q.bus.Publish(job.SessionID)
	return nil
}

func (q *xprocTestQueue) Lease(ctx context.Context) (backend.LeasedJob, error) {
	<-ctx.Done()
	return backend.LeasedJob{}, ctx.Err()
}

func (q *xprocTestQueue) Ack(ctx context.Context, leaseToken string) error {
	return nil
}

func (q *xprocTestQueue) Nack(ctx context.Context, leaseToken string, retryAfter time.Duration) error {
	return nil
}

func (q *xprocTestQueue) Heartbeat(ctx context.Context, leaseToken string) error {
	return nil
}

func rawJSON(
	t *testing.T,
	harness *testServer,
	method string,
	target string,
	body string,
) (*httptest.ResponseRecorder, protocol.ErrorResponse) {
	t.Helper()

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-test")
	req.Header.Set("X-Along-Schema", protocol.SchemaVersion)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	harness.handler.ServeHTTP(rec, req)

	var response protocol.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	return rec, response
}
