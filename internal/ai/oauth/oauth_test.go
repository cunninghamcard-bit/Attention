package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeneratePKCE(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkce.Verifier) < 43 {
		t.Fatalf("verifier length = %d, want at least 43", len(pkce.Verifier))
	}

	sum := sha256.Sum256([]byte(pkce.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.Challenge != want {
		t.Fatalf("challenge = %q, want %q", pkce.Challenge, want)
	}
}

func TestParseAuthorizationInput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCode  string
		wantState string
	}{
		{
			name:      "redirect URL",
			input:     "http://localhost/callback?code=code-1&state=state-1",
			wantCode:  "code-1",
			wantState: "state-1",
		},
		{
			name:      "hash separator",
			input:     "code-2#state-2",
			wantCode:  "code-2",
			wantState: "state-2",
		},
		{
			name:      "query string",
			input:     "code=code-3&state=state-3",
			wantCode:  "code-3",
			wantState: "state-3",
		},
		{
			name:     "code only",
			input:    "code-4",
			wantCode: "code-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAuthorizationInput(tt.input)
			if got.Code != tt.wantCode || got.State != tt.wantState {
				t.Fatalf("parse = %+v, want code/state %q/%q", got, tt.wantCode, tt.wantState)
			}
		})
	}
}

func TestCallbackServerRejectsStateMismatch(t *testing.T) {
	requireLocalListen(t)

	server, err := startCallbackServer(config{
		callbackListenHost: "127.0.0.1",
		callbackPublicHost: "localhost",
		callbackPort:       0,
		callbackPath:       "/callback",
	}, "expected-state", "Test")
	if err != nil {
		t.Fatal(err)
	}
	defer closeCallbackServer(context.Background(), server)

	resp, err := http.Get(server.redirectURI + "?code=code-1&state=wrong-state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	server.cancelWait()
	result, err := server.wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil", result)
	}
}

func TestWaitForCallbackOrManualReturnsManualResult(t *testing.T) {
	server := &callbackServer{
		result:   make(chan callbackResult, 1),
		canceled: make(chan struct{}, 1),
	}
	manual := make(chan manualResult, 1)
	manual <- manualResult{input: "code-1#state-1"}

	result, err := waitForCallbackOrManual(t.Context(), server, manual)
	if err != nil {
		t.Fatal(err)
	}
	if result.manual == nil || result.manual.input != "code-1#state-1" {
		t.Fatalf("manual result = %+v, want code-1#state-1", result.manual)
	}
	if result.callback != nil {
		t.Fatalf("callback result = %+v, want nil", result.callback)
	}
}

func TestWaitForCallbackOrManualPrefersReadyCallback(t *testing.T) {
	server := &callbackServer{
		result:   make(chan callbackResult, 1),
		canceled: make(chan struct{}, 1),
	}
	server.result <- callbackResult{code: "code-1", state: "state-1"}
	manual := make(chan manualResult, 1)
	manual <- manualResult{input: "manual-code"}

	result, err := waitForCallbackOrManual(t.Context(), server, manual)
	if err != nil {
		t.Fatal(err)
	}
	if result.callback == nil || result.callback.code != "code-1" || result.callback.state != "state-1" {
		t.Fatalf("callback result = %+v, want code/state", result.callback)
	}
	if result.manual != nil {
		t.Fatalf("manual result = %+v, want nil", result.manual)
	}
}

func TestWaitForCallbackOrManualStopsOnCancel(t *testing.T) {
	server := &callbackServer{
		result:   make(chan callbackResult, 1),
		canceled: make(chan struct{}, 1),
	}
	server.cancelWait()
	manual := make(chan manualResult)

	result, err := waitForCallbackOrManual(t.Context(), server, manual)
	if err != nil {
		t.Fatal(err)
	}
	if result.callback != nil || result.manual != nil {
		t.Fatalf("result = %+v, want empty", result)
	}
}

func TestParseAuthorizationCodeRejectsStateMismatch(t *testing.T) {
	_, err := parseAuthorizationCode("code-1#wrong-state", "expected-state")
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("err = %v, want state mismatch", err)
	}
}

type asyncCallback struct {
	err <-chan error
}

func (c *asyncCallback) get(rawURL string) {
	errc := make(chan error, 1)
	c.err = errc
	go func() {
		errc <- callbackGET(rawURL)
	}()
}

func (c *asyncCallback) assert(t *testing.T) {
	t.Helper()
	if c.err == nil {
		t.Fatal("callback request was not started")
	}
	if err := <-c.err; err != nil {
		t.Fatal(err)
	}
}

func callbackGET(rawURL string) error {
	resp, err := http.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("callback status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func requireLocalListen(t *testing.T) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listen unavailable: %v", err)
	}
	_ = listener.Close()
}

func newTokenTestClient(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Result(), nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func fakeOpenAICodexJWT(t *testing.T, accountID string) string {
	t.Helper()

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{
		openAICodexJWTClaim: map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + claims + ".sig"
}
