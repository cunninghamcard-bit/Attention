package oauth

import (
	"context"
	"net/http"
	"time"
)

const tokenRequestTimeout = 30 * time.Second

type Credentials struct {
	Refresh   string `json:"refresh"`
	Access    string `json:"access"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId,omitempty"`
}

type AuthInfo struct {
	URL          string
	Instructions string
}

type Prompt struct {
	Message     string
	Placeholder string
	AllowEmpty  bool
}

// DeviceCodeInfo mirrors pi's OAuthDeviceCodeInfo for device-code login flows.
// See .agents/references/pi/packages/ai/src/utils/oauth/types.ts (OAuthDeviceCodeInfo).
type DeviceCodeInfo struct {
	UserCode         string
	VerificationURI  string
	IntervalSeconds  int
	ExpiresInSeconds int
}

// SelectOption mirrors pi's OAuthSelectOption.
type SelectOption struct {
	ID    string
	Label string
}

// SelectPrompt mirrors pi's OAuthSelectPrompt: an interactive single choice.
type SelectPrompt struct {
	Message string
	Options []SelectOption
}

// LoginCallbacks mirrors pi's OAuthLoginCallbacks.
// See .agents/references/pi/packages/ai/src/utils/oauth/types.ts:41-52.
type LoginCallbacks struct {
	OnAuth            func(AuthInfo)
	OnDeviceCode      func(DeviceCodeInfo)
	OnPrompt          func(context.Context, Prompt) (string, error)
	OnProgress        func(string)
	OnManualCodeInput func(context.Context) (string, error)
	// OnSelect shows an interactive selector and returns the chosen option id,
	// or an empty string when the user cancels.
	OnSelect func(context.Context, SelectPrompt) (string, error)
}

type Option func(*config)

type config struct {
	clientID           string
	authorizeURL       string
	tokenURL           string
	scope              string
	callbackListenHost string
	callbackPublicHost string
	callbackPort       int
	callbackPath       string
	originator         string
	httpClient         *http.Client
	now                func() time.Time
}

func WithHTTPClient(client *http.Client) Option {
	return func(cfg *config) {
		cfg.httpClient = client
	}
}

func WithAuthorizeURL(rawURL string) Option {
	return func(cfg *config) {
		cfg.authorizeURL = rawURL
	}
}

func WithTokenURL(rawURL string) Option {
	return func(cfg *config) {
		cfg.tokenURL = rawURL
	}
}

func WithCallbackAddress(host string, port int) Option {
	return func(cfg *config) {
		cfg.callbackListenHost = host
		cfg.callbackPort = port
	}
}

func WithCallbackPublicHost(host string) Option {
	return func(cfg *config) {
		cfg.callbackPublicHost = host
	}
}

func WithCallbackPath(path string) Option {
	return func(cfg *config) {
		cfg.callbackPath = path
	}
}

func WithOriginator(originator string) Option {
	return func(cfg *config) {
		cfg.originator = originator
	}
}

func withNow(now func() time.Time) Option {
	return func(cfg *config) {
		cfg.now = now
	}
}

func applyOptions(defaults config, options []Option) config {
	cfg := defaults
	for _, option := range options {
		option(&cfg)
	}
	if cfg.httpClient == nil {
		cfg.httpClient = http.DefaultClient
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return cfg
}

func expiresUnixMilli(now func() time.Time, expiresInSeconds int, skew time.Duration) int64 {
	return now().Add(time.Duration(expiresInSeconds)*time.Second - skew).UnixMilli()
}
