package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	openAICodexClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAICodexAuthorizeURL  = "https://auth.openai.com/oauth/authorize"
	openAICodexTokenURL      = "https://auth.openai.com/oauth/token"
	openAICodexCallbackPort  = 1455
	openAICodexCallbackPath  = "/auth/callback"
	openAICodexScope         = "openid profile email offline_access"
	openAICodexJWTClaim      = "https://api.openai.com/auth"
	openAICodexDefaultOrigin = "pi"
)

func LoginOpenAICodex(
	ctx context.Context,
	callbacks LoginCallbacks,
	options ...Option,
) (Credentials, error) {
	cfg := applyOptions(defaultOpenAICodexConfig(), options)
	pkce, err := GeneratePKCE()
	if err != nil {
		return Credentials{}, err
	}
	state, err := randomHex(16)
	if err != nil {
		return Credentials{}, err
	}

	server, callbackErr := startCallbackServer(cfg, state, "OpenAI")
	redirectURI := openAICodexRedirectURI(cfg)
	if callbackErr == nil {
		redirectURI = server.redirectURI
		defer closeCallbackServer(ctx, server)
	}

	authURL, err := openAICodexAuthorizationURL(cfg, pkce, state, redirectURI)
	if err != nil {
		return Credentials{}, err
	}
	instructions := "A browser window should open. Complete login to finish."
	if callbackErr != nil {
		instructions = "Open this URL and paste the final redirect URL or authorization code."
		if callbacks.OnProgress != nil {
			callbacks.OnProgress(fmt.Sprintf("OAuth callback server unavailable: %v", callbackErr))
		}
	}
	if callbacks.OnAuth != nil {
		callbacks.OnAuth(AuthInfo{
			URL:          authURL,
			Instructions: instructions,
		})
	}

	var auth authorizationInput
	if server != nil {
		auth, err = waitForAuthorizationCode(ctx, server, callbacks, state, Prompt{
			Message: "Paste the authorization code or full redirect URL:",
		})
	} else {
		auth, err = waitForManualAuthorizationCode(ctx, callbacks, state, Prompt{
			Message:     "Paste the authorization code or full redirect URL:",
			Placeholder: redirectURI,
		})
	}
	if err != nil {
		return Credentials{}, err
	}
	if auth.Code == "" {
		return Credentials{}, fmt.Errorf("missing authorization code")
	}

	credentials, err := exchangeOpenAICodexAuthorizationCode(ctx, cfg, auth.Code, pkce.Verifier, redirectURI)
	if err != nil {
		return Credentials{}, err
	}
	accountID, err := ExtractOpenAICodexAccountID(credentials.Access)
	if err != nil {
		return Credentials{}, err
	}
	credentials.AccountID = accountID
	return credentials, nil
}

func RefreshOpenAICodexToken(
	ctx context.Context,
	refreshToken string,
	options ...Option,
) (Credentials, error) {
	cfg := applyOptions(defaultOpenAICodexConfig(), options)
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", cfg.clientID)

	body, status, err := postForm(ctx, cfg.httpClient, cfg.tokenURL, form)
	if err != nil {
		return Credentials{}, tokenEndpointError("OpenAI Codex token refresh", status, body, err)
	}

	credentials, err := decodeOpenAICodexTokenResponse(cfg, body, "OpenAI Codex token refresh")
	if err != nil {
		return Credentials{}, err
	}
	accountID, err := ExtractOpenAICodexAccountID(credentials.Access)
	if err != nil {
		return Credentials{}, err
	}
	credentials.AccountID = accountID
	return credentials, nil
}

func ExtractOpenAICodexAccountID(accessToken string) (string, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("failed to extract accountId from token")
	}

	payload, err := decodeJWTPart(parts[1])
	if err != nil {
		return "", fmt.Errorf("failed to extract accountId from token")
	}

	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("failed to extract accountId from token")
	}
	if claims.Auth.ChatGPTAccountID == "" {
		return "", fmt.Errorf("failed to extract accountId from token")
	}
	return claims.Auth.ChatGPTAccountID, nil
}

func openAICodexRedirectURI(cfg config) string {
	publicHost := cfg.callbackPublicHost
	if publicHost == "" {
		publicHost = "localhost"
	}
	path := cfg.callbackPath
	if path == "" {
		path = openAICodexCallbackPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("http://%s:%d%s", publicHost, cfg.callbackPort, path)
}

func defaultOpenAICodexConfig() config {
	return config{
		clientID:           openAICodexClientID,
		authorizeURL:       openAICodexAuthorizeURL,
		tokenURL:           openAICodexTokenURL,
		scope:              openAICodexScope,
		callbackListenHost: defaultCallbackListenHost(),
		callbackPublicHost: "localhost",
		callbackPort:       openAICodexCallbackPort,
		callbackPath:       openAICodexCallbackPath,
		originator:         openAICodexDefaultOrigin,
	}
}

func openAICodexAuthorizationURL(cfg config, pkce PKCE, state string, redirectURI string) (string, error) {
	authURL, err := url.Parse(cfg.authorizeURL)
	if err != nil {
		return "", err
	}
	params := authURL.Query()
	params.Set("response_type", "code")
	params.Set("client_id", cfg.clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", cfg.scope)
	params.Set("code_challenge", pkce.Challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	params.Set("originator", cfg.originator)
	authURL.RawQuery = params.Encode()
	return authURL.String(), nil
}

func exchangeOpenAICodexAuthorizationCode(
	ctx context.Context,
	cfg config,
	code string,
	verifier string,
	redirectURI string,
) (Credentials, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", cfg.clientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)

	body, status, err := postForm(ctx, cfg.httpClient, cfg.tokenURL, form)
	if err != nil {
		return Credentials{}, tokenEndpointError("OpenAI Codex token exchange", status, body, err)
	}
	return decodeOpenAICodexTokenResponse(cfg, body, "OpenAI Codex token exchange")
}

func decodeOpenAICodexTokenResponse(cfg config, body []byte, operation string) (Credentials, error) {
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
		Expires: expiresUnixMilli(cfg.now, token.ExpiresIn, 0),
	}, nil
}

func decodeJWTPart(part string) ([]byte, error) {
	payload, err := base64.RawURLEncoding.DecodeString(part)
	if err == nil {
		return payload, nil
	}
	return base64.URLEncoding.DecodeString(part)
}
