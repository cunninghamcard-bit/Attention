package jshost

import (
	"context"
	"fmt"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
)

const jsOAuthGetAPIKeyTimeout = 10 * time.Second

type jsOAuthProvider struct {
	host *Host
	id   string
	name string
}

func (p jsOAuthProvider) ID() string {
	return p.id
}

func (p jsOAuthProvider) Name() string {
	return p.name
}

func (p jsOAuthProvider) Login(
	context.Context,
	oauth.LoginCallbacks,
) (oauth.Credentials, error) {
	return oauth.Credentials{}, fmt.Errorf(
		"extension provider %q interactive login is not yet supported",
		p.id,
	)
}

func (p jsOAuthProvider) RefreshToken(
	ctx context.Context,
	refresh string,
) (oauth.Credentials, error) {
	result, err := p.host.InvokeOAuth(
		ctx,
		p.id,
		"refreshToken",
		map[string]any{
			"credentials": oauth.Credentials{Refresh: refresh},
		},
		oauthNoopUIResponder,
		oauthNoopContextResponder,
	)
	if err != nil {
		return oauth.Credentials{}, err
	}
	if result.Error != "" {
		return oauth.Credentials{}, fmt.Errorf(
			"extension provider %q refreshToken: %s",
			p.id,
			result.Error,
		)
	}
	if result.Credentials == nil {
		return oauth.Credentials{}, nil
	}
	return *result.Credentials, nil
}

// GetAPIKey is synchronous because oauth.Provider requires it. The JS bridge is
// asynchronous, so this blocks briefly and mirrors pi's undefined-on-failure
// behavior by returning an empty key when the host call fails.
func (p jsOAuthProvider) GetAPIKey(creds oauth.Credentials) string {
	ctx, cancel := context.WithTimeout(context.Background(), jsOAuthGetAPIKeyTimeout)
	defer cancel()

	result, err := p.host.InvokeOAuth(
		ctx,
		p.id,
		"getApiKey",
		map[string]any{"credentials": creds},
		oauthNoopUIResponder,
		oauthNoopContextResponder,
	)
	if err != nil || result.Error != "" {
		return ""
	}
	return result.APIKey
}

func oauthNoopUIResponder(_ context.Context, request UIRequest) (UIResponse, error) {
	return UIResponse{
		ID:        request.ID,
		Cancelled: true,
	}, nil
}

func oauthNoopContextResponder(
	_ context.Context,
	request ContextRequest,
) (ContextResponse, error) {
	return ContextResponse{ID: request.ID}, nil
}
