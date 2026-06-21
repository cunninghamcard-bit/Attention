package oauth

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type callbackResult struct {
	code  string
	state string
}

type callbackServer struct {
	server      *http.Server
	redirectURI string
	result      chan callbackResult
	serveDone   chan struct{}
	canceled    chan struct{}
}

func startCallbackServer(cfg config, expectedState string, providerName string) (*callbackServer, error) {
	host := cfg.callbackListenHost
	if host == "" {
		host = "127.0.0.1"
	}
	path := cfg.callbackPath
	if path == "" {
		path = "/callback"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(cfg.callbackPort)))
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback server: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	publicHost := cfg.callbackPublicHost
	if publicHost == "" {
		publicHost = "localhost"
	}

	callback := &callbackServer{
		redirectURI: fmt.Sprintf("http://%s:%d%s", publicHost, port, path),
		result:      make(chan callbackResult, 1),
		serveDone:   make(chan struct{}),
		canceled:    make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		callback.handle(w, r, expectedState, providerName)
	})
	callback.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		defer close(callback.serveDone)
		if err := callback.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			callback.cancelWait()
		}
	}()

	return callback, nil
}

func (s *callbackServer) handle(
	w http.ResponseWriter,
	r *http.Request,
	expectedState string,
	providerName string,
) {
	query := r.URL.Query()
	if value := query.Get("error"); value != "" {
		writeOAuthError(w, http.StatusBadRequest, providerName+" authentication did not complete.", value)
		return
	}

	code := query.Get("code")
	state := query.Get("state")
	if code == "" || state == "" {
		writeOAuthError(w, http.StatusBadRequest, "Missing code or state parameter.", "")
		return
	}
	if state != expectedState {
		writeOAuthError(w, http.StatusBadRequest, "State mismatch.", "")
		return
	}

	writeOAuthSuccess(w, providerName+" authentication completed. You can close this window.")
	result := callbackResult{code: code, state: state}
	select {
	case s.result <- result:
	default:
	}
}

func (s *callbackServer) wait(ctx context.Context) (*callbackResult, error) {
	select {
	case result := <-s.result:
		return &result, nil
	default:
	}

	select {
	case result := <-s.result:
		return &result, nil
	case <-s.canceled:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *callbackServer) cancelWait() {
	select {
	case s.canceled <- struct{}{}:
	default:
	}
}

func (s *callbackServer) close(ctx context.Context) error {
	shutdownErr := s.server.Shutdown(ctx)
	if shutdownErr != nil {
		_ = s.server.Close()
	}

	select {
	case <-s.serveDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	return shutdownErr
}

func waitForAuthorizationCode(
	ctx context.Context,
	server *callbackServer,
	callbacks LoginCallbacks,
	expectedState string,
	prompt Prompt,
) (authorizationInput, error) {
	if callbacks.OnManualCodeInput != nil {
		manualCtx, cancelManual := context.WithCancel(ctx)
		defer cancelManual()

		manual := make(chan manualResult, 1)
		go func() {
			input, err := callbacks.OnManualCodeInput(manualCtx)
			manual <- manualResult{input: input, err: err}
		}()

		result, err := waitForCallbackOrManual(ctx, server, manual)
		if err != nil {
			return authorizationInput{}, err
		}
		if result.callback != nil {
			return authorizationInput{Code: result.callback.code, State: result.callback.state}, nil
		}
		if result.manual != nil {
			result := *result.manual
			if result.err != nil {
				return authorizationInput{}, result.err
			}
			parsed, err := parseAuthorizationCode(result.input, expectedState)
			if err != nil {
				return authorizationInput{}, err
			}
			if parsed.Code != "" {
				return parsed, nil
			}
		}
	} else {
		result, err := server.wait(ctx)
		if err != nil {
			return authorizationInput{}, err
		}
		if result != nil {
			return authorizationInput{Code: result.code, State: result.state}, nil
		}
	}

	if callbacks.OnPrompt == nil {
		return authorizationInput{}, fmt.Errorf("missing authorization code")
	}
	input, err := callbacks.OnPrompt(ctx, prompt)
	if err != nil {
		return authorizationInput{}, err
	}
	return parseAuthorizationCode(input, expectedState)
}

func waitForManualAuthorizationCode(
	ctx context.Context,
	callbacks LoginCallbacks,
	expectedState string,
	prompt Prompt,
) (authorizationInput, error) {
	if callbacks.OnManualCodeInput != nil {
		input, err := callbacks.OnManualCodeInput(ctx)
		if err != nil {
			return authorizationInput{}, err
		}
		parsed, err := parseAuthorizationCode(input, expectedState)
		if err != nil {
			return authorizationInput{}, err
		}
		if parsed.Code != "" {
			return parsed, nil
		}
	}
	if callbacks.OnPrompt == nil {
		return authorizationInput{}, fmt.Errorf("missing authorization code")
	}
	input, err := callbacks.OnPrompt(ctx, prompt)
	if err != nil {
		return authorizationInput{}, err
	}
	return parseAuthorizationCode(input, expectedState)
}

type waitResult struct {
	callback *callbackResult
	manual   *manualResult
}

type manualResult struct {
	input string
	err   error
}

func waitForCallbackOrManual(
	ctx context.Context,
	server *callbackServer,
	manual <-chan manualResult,
) (waitResult, error) {
	select {
	case result := <-server.result:
		return waitResult{callback: &result}, nil
	default:
	}

	select {
	case result := <-server.result:
		return waitResult{callback: &result}, nil
	case result := <-manual:
		return waitResult{manual: &result}, nil
	case <-server.canceled:
		select {
		case result := <-server.result:
			return waitResult{callback: &result}, nil
		default:
			return waitResult{}, nil
		}
	case <-ctx.Done():
		return waitResult{}, ctx.Err()
	}
}

func writeOAuthSuccess(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "<!doctype html><html><body><h1>%s</h1></body></html>", html.EscapeString(message))
}

func writeOAuthError(w http.ResponseWriter, status int, message string, details string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if details != "" {
		message += " " + details
	}
	_, _ = fmt.Fprintf(w, "<!doctype html><html><body><h1>%s</h1></body></html>", html.EscapeString(message))
}
