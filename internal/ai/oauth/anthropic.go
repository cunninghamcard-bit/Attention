package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	anthropicClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	anthropicAuthorizeURL = "https://claude.ai/oauth/authorize"
	anthropicTokenURL     = "https://platform.claude.com/v1/oauth/token"
	anthropicCallbackPort = 53692
	anthropicCallbackPath = "/callback"
	anthropicExpirySkew   = 5 * time.Minute
)

const anthropicScope = "org:create_api_key user:profile user:inference " +
	"user:sessions:claude_code user:mcp_servers user:file_upload"

func LoginAnthropic(
	ctx context.Context,
	callbacks LoginCallbacks,
	options ...Option,
) (Credentials, error) {
	cfg := applyOptions(defaultAnthropicConfig(), options)
	pkce, err := GeneratePKCE()
	if err != nil {
		return Credentials{}, err
	}

	server, err := startCallbackServer(cfg, pkce.Verifier, "Anthropic")
	if err != nil {
		return Credentials{}, err
	}
	defer closeCallbackServer(ctx, server)

	authURL, err := anthropicAuthorizationURL(cfg, pkce, server.redirectURI)
	if err != nil {
		return Credentials{}, err
	}
	if callbacks.OnAuth != nil {
		callbacks.OnAuth(AuthInfo{
			URL:          authURL,
			Instructions: "Complete login in your browser. If needed, paste the final redirect URL.",
		})
	}

	auth, err := waitForAuthorizationCode(ctx, server, callbacks, pkce.Verifier, Prompt{
		Message:     "Paste the authorization code or full redirect URL:",
		Placeholder: server.redirectURI,
	})
	if err != nil {
		return Credentials{}, err
	}
	if auth.Code == "" {
		return Credentials{}, fmt.Errorf("missing authorization code")
	}
	if auth.State == "" {
		return Credentials{}, fmt.Errorf("missing OAuth state")
	}

	if callbacks.OnProgress != nil {
		callbacks.OnProgress("Exchanging authorization code for tokens...")
	}
	return exchangeAnthropicAuthorizationCode(ctx, cfg, auth.Code, auth.State, pkce.Verifier, server.redirectURI)
}

func RefreshAnthropicToken(
	ctx context.Context,
	refreshToken string,
	options ...Option,
) (Credentials, error) {
	cfg := applyOptions(defaultAnthropicConfig(), options)
	body, status, err := postJSON(ctx, cfg.httpClient, cfg.tokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     cfg.clientID,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return Credentials{}, tokenEndpointError("Anthropic token refresh", status, body, err)
	}
	return decodeAnthropicTokenResponse(cfg, body, "Anthropic token refresh")
}

func defaultAnthropicConfig() config {
	return config{
		clientID:           anthropicClientID,
		authorizeURL:       anthropicAuthorizeURL,
		tokenURL:           anthropicTokenURL,
		scope:              anthropicScope,
		callbackListenHost: defaultCallbackListenHost(),
		callbackPublicHost: "localhost",
		callbackPort:       anthropicCallbackPort,
		callbackPath:       anthropicCallbackPath,
	}
}

func anthropicAuthorizationURL(cfg config, pkce PKCE, redirectURI string) (string, error) {
	authURL, err := url.Parse(cfg.authorizeURL)
	if err != nil {
		return "", err
	}
	params := authURL.Query()
	params.Set("code", "true")
	params.Set("client_id", cfg.clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", cfg.scope)
	params.Set("code_challenge", pkce.Challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", pkce.Verifier)
	authURL.RawQuery = params.Encode()
	return authURL.String(), nil
}

func exchangeAnthropicAuthorizationCode(
	ctx context.Context,
	cfg config,
	code string,
	state string,
	verifier string,
	redirectURI string,
) (Credentials, error) {
	body, status, err := postJSON(ctx, cfg.httpClient, cfg.tokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     cfg.clientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return Credentials{}, tokenEndpointError("Anthropic token exchange", status, body, err)
	}
	return decodeAnthropicTokenResponse(cfg, body, "Anthropic token exchange")
}

func decodeAnthropicTokenResponse(cfg config, body []byte, operation string) (Credentials, error) {
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return Credentials{}, fmt.Errorf("%s returned invalid JSON: %w", operation, err)
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 {
		return Credentials{}, fmt.Errorf("%s response missing required fields", operation)
	}
	return Credentials{
		Access:  token.AccessToken,
		Refresh: token.RefreshToken,
		Expires: expiresUnixMilli(cfg.now, token.ExpiresIn, anthropicExpirySkew),
	}, nil
}

func tokenEndpointError(operation string, status int, body []byte, err error) error {
	message := safeTokenEndpointErrorMessage(body)
	if message != "" {
		return fmt.Errorf("%s failed (%d): %s", operation, status, message)
	}
	if status > 0 {
		return fmt.Errorf("%s failed (%d): %w", operation, status, err)
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}

func safeTokenEndpointErrorMessage(body []byte) string {
	if len(strings.TrimSpace(string(body))) == 0 {
		return ""
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "token endpoint returned an error response"
	}

	parts := []string{}
	appendString := func(value any) {
		text, ok := value.(string)
		if ok && text != "" {
			parts = append(parts, text)
		}
	}

	if errValue, ok := payload["error"].(map[string]any); ok {
		appendString(errValue["code"])
		appendString(errValue["type"])
		appendString(errValue["message"])
	} else {
		appendString(payload["error"])
	}
	appendString(payload["error_description"])
	appendString(payload["message"])

	if len(parts) == 0 {
		return "token endpoint returned an error response"
	}
	return strings.Join(parts, ": ")
}

func defaultCallbackListenHost() string {
	if host := os.Getenv("PI_OAUTH_CALLBACK_HOST"); host != "" {
		return host
	}
	return "127.0.0.1"
}

func closeCallbackServer(ctx context.Context, server *callbackServer) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = server.close(closeCtx)
}
