package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

type PKCE struct {
	Verifier  string
	Challenge string
}

type authorizationInput struct {
	Code  string
	State string
}

func GeneratePKCE() (PKCE, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return PKCE{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}

	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random state: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func parseAuthorizationInput(input string) authorizationInput {
	value := strings.TrimSpace(input)
	if value == "" {
		return authorizationInput{}
	}

	if strings.Contains(value, "://") {
		if parsed, err := url.Parse(value); err == nil {
			return authorizationInput{
				Code:  parsed.Query().Get("code"),
				State: parsed.Query().Get("state"),
			}
		}
	}

	if code, state, ok := strings.Cut(value, "#"); ok {
		return authorizationInput{Code: code, State: state}
	}

	if strings.Contains(value, "code=") {
		params, err := url.ParseQuery(strings.TrimPrefix(value, "?"))
		if err == nil {
			return authorizationInput{
				Code:  params.Get("code"),
				State: params.Get("state"),
			}
		}
	}

	return authorizationInput{Code: value}
}

func parseAuthorizationCode(input string, expectedState string) (authorizationInput, error) {
	parsed := parseAuthorizationInput(input)
	if parsed.State != "" && parsed.State != expectedState {
		return authorizationInput{}, fmt.Errorf("OAuth state mismatch")
	}
	if parsed.State == "" {
		parsed.State = expectedState
	}
	return parsed, nil
}
