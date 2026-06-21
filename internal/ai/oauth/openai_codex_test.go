package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoginOpenAICodexCallbackExchange(t *testing.T) {
	requireLocalListen(t)

	access := fakeOpenAICodexJWT(t, "acct_1")
	var tokenForm url.Values
	tokenClient := newTokenTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type = %q, want form", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		tokenForm = r.PostForm
		response := map[string]any{
			"access_token":  access,
			"refresh_token": "refresh-1",
			"expires_in":    3600,
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	})

	callback := &asyncCallback{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	credentials, err := LoginOpenAICodex(ctx, LoginCallbacks{
		OnAuth: func(info AuthInfo) {
			authURL, err := url.Parse(info.URL)
			if err != nil {
				t.Fatalf("parse auth URL: %v", err)
			}
			query := authURL.Query()
			if query.Get("client_id") != openAICodexClientID ||
				query.Get("originator") != openAICodexDefaultOrigin ||
				query.Get("id_token_add_organizations") != "true" ||
				query.Get("codex_cli_simplified_flow") != "true" {
				t.Fatalf("auth query = %v", query)
			}
			redirectURI := query.Get("redirect_uri")
			state := query.Get("state")
			if !strings.Contains(redirectURI, openAICodexCallbackPath) || state == "" {
				t.Fatalf("redirect/state missing in %q", info.URL)
			}
			callback.get(redirectURI + "?code=code-1&state=" + url.QueryEscape(state))
		},
	}, WithHTTPClient(tokenClient), WithCallbackAddress("127.0.0.1", 0))
	if err != nil {
		t.Fatal(err)
	}
	callback.assert(t)

	if credentials.Access != access || credentials.Refresh != "refresh-1" || credentials.AccountID != "acct_1" {
		t.Fatalf("credentials = %+v", credentials)
	}
	if tokenForm.Get("grant_type") != "authorization_code" ||
		tokenForm.Get("code") != "code-1" ||
		tokenForm.Get("code_verifier") == "" {
		t.Fatalf("token form = %v", tokenForm)
	}
	if !strings.Contains(tokenForm.Get("redirect_uri"), openAICodexCallbackPath) {
		t.Fatalf("redirect_uri = %q", tokenForm.Get("redirect_uri"))
	}
}

func TestRefreshOpenAICodexToken(t *testing.T) {
	access := fakeOpenAICodexJWT(t, "acct_2")
	tokenClient := newTokenTestClient(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.PostForm.Get("grant_type") != "refresh_token" ||
			r.PostForm.Get("refresh_token") != "old-refresh" {
			t.Fatalf("form = %v", r.PostForm)
		}
		_, _ = io.WriteString(w, `{"access_token":`+strconvQuote(access)+`,"refresh_token":"refresh-2","expires_in":60}`)
	})

	credentials, err := RefreshOpenAICodexToken(
		context.Background(),
		"old-refresh",
		WithHTTPClient(tokenClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Access != access || credentials.Refresh != "refresh-2" || credentials.AccountID != "acct_2" {
		t.Fatalf("credentials = %+v", credentials)
	}
}

func TestLoginOpenAICodexFallsBackToManualInputWhenCallbackPortUnavailable(t *testing.T) {
	access := fakeOpenAICodexJWT(t, "acct_3")
	var tokenForm url.Values
	tokenClient := newTokenTestClient(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		tokenForm = r.PostForm
		response := map[string]any{
			"access_token":  access,
			"refresh_token": "refresh-3",
			"expires_in":    3600,
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	})

	var authCalled bool
	var progressCalled bool
	credentials, err := LoginOpenAICodex(context.Background(), LoginCallbacks{
		OnAuth: func(info AuthInfo) {
			authCalled = true
			authURL, err := url.Parse(info.URL)
			if err != nil {
				t.Fatalf("parse auth URL: %v", err)
			}
			redirectURI := authURL.Query().Get("redirect_uri")
			if !strings.Contains(redirectURI, ":1"+openAICodexCallbackPath) {
				t.Fatalf("redirect_uri = %q", redirectURI)
			}
		},
		OnProgress: func(message string) {
			progressCalled = strings.Contains(message, "callback server unavailable")
		},
		OnManualCodeInput: func(context.Context) (string, error) {
			return "code-3", nil
		},
	}, WithHTTPClient(tokenClient), WithCallbackAddress("192.0.2.1", 1))
	if err != nil {
		t.Fatal(err)
	}

	if !authCalled || !progressCalled {
		t.Fatalf("auth/progress called = %v/%v", authCalled, progressCalled)
	}
	if credentials.AccountID != "acct_3" || credentials.Refresh != "refresh-3" {
		t.Fatalf("credentials = %+v", credentials)
	}
	if tokenForm.Get("code") != "code-3" {
		t.Fatalf("token form = %v", tokenForm)
	}
}

func TestOpenAICodexTokenWithoutAccountIDFails(t *testing.T) {
	_, err := ExtractOpenAICodexAccountID("header.payload.signature")
	if err == nil || !strings.Contains(err.Error(), "extract accountId") {
		t.Fatalf("err = %v, want accountId extraction failure", err)
	}
}

func strconvQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
