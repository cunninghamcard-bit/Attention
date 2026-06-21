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

func TestLoginAnthropicCallbackExchange(t *testing.T) {
	requireLocalListen(t)

	now := time.Unix(1_700_000_000, 0)
	var tokenRequest map[string]string
	tokenClient := newTokenTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content-type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&tokenRequest); err != nil {
			t.Fatalf("decode token request: %v", err)
		}
		_, _ = io.WriteString(w, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":3600}`)
	})

	callback := &asyncCallback{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	credentials, err := LoginAnthropic(ctx, LoginCallbacks{
		OnAuth: func(info AuthInfo) {
			authURL, err := url.Parse(info.URL)
			if err != nil {
				t.Fatalf("parse auth URL: %v", err)
			}
			query := authURL.Query()
			if authURL.String() == "" || query.Get("client_id") != anthropicClientID {
				t.Fatalf("auth URL = %q", info.URL)
			}
			if query.Get("code_challenge_method") != "S256" {
				t.Fatalf("code_challenge_method = %q", query.Get("code_challenge_method"))
			}
			redirectURI := query.Get("redirect_uri")
			state := query.Get("state")
			if redirectURI == "" || state == "" {
				t.Fatalf("redirect/state missing in %q", info.URL)
			}
			callback.get(redirectURI + "?code=code-1&state=" + url.QueryEscape(state))
		},
	}, WithHTTPClient(tokenClient), WithCallbackAddress("127.0.0.1", 0), withNow(func() time.Time {
		return now
	}))
	if err != nil {
		t.Fatal(err)
	}
	callback.assert(t)

	if credentials.Access != "access-1" || credentials.Refresh != "refresh-1" {
		t.Fatalf("credentials = %+v", credentials)
	}
	wantExpires := now.Add(3600*time.Second - anthropicExpirySkew).UnixMilli()
	if credentials.Expires != wantExpires {
		t.Fatalf("expires = %d, want %d", credentials.Expires, wantExpires)
	}
	if tokenRequest["grant_type"] != "authorization_code" ||
		tokenRequest["code"] != "code-1" ||
		tokenRequest["state"] == "" ||
		tokenRequest["code_verifier"] != tokenRequest["state"] {
		t.Fatalf("token request = %+v", tokenRequest)
	}
	if !strings.HasPrefix(tokenRequest["redirect_uri"], "http://localhost:") {
		t.Fatalf("redirect_uri = %q", tokenRequest["redirect_uri"])
	}
}

func TestRefreshAnthropicToken(t *testing.T) {
	tokenClient := newTokenTestClient(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload["grant_type"] != "refresh_token" || payload["refresh_token"] != "old-refresh" {
			t.Fatalf("payload = %+v", payload)
		}
		_, _ = io.WriteString(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":60}`)
	})

	credentials, err := RefreshAnthropicToken(
		context.Background(),
		"old-refresh",
		WithHTTPClient(tokenClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Access != "access-2" || credentials.Refresh != "refresh-2" {
		t.Fatalf("credentials = %+v", credentials)
	}
}

func TestAnthropicTokenEndpointErrorRedactsRawBodySecrets(t *testing.T) {
	tokenClient := newTokenTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(
			w,
			`{"error":{"type":"invalid_grant","message":"expired"},`+
				`"access_token":"secret-access","refresh_token":"secret-refresh"}`,
		)
	})

	_, err := RefreshAnthropicToken(context.Background(), "old-refresh", WithHTTPClient(tokenClient))
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	if strings.Contains(text, "secret-access") || strings.Contains(text, "secret-refresh") {
		t.Fatalf("error leaked token body: %s", text)
	}
	if !strings.Contains(text, "invalid_grant") || !strings.Contains(text, "expired") {
		t.Fatalf("error = %q, want safe error fields", text)
	}
}
