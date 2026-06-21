package auth

import (
	"encoding/json"
	"fmt"
)

const (
	TypeAPIKey = "api_key"
	TypeOAuth  = "oauth"
)

type Credential struct {
	Type  string
	Key   string
	OAuth *OAuthCredential
}

type OAuthCredential struct {
	Refresh   string
	Access    string
	Expires   int64
	AccountID string
}

func (c Credential) MarshalJSON() ([]byte, error) {
	switch c.Type {
	case TypeAPIKey:
		return json.Marshal(apiKeyCredential{
			Type: TypeAPIKey,
			Key:  c.Key,
		})
	case TypeOAuth:
		if c.OAuth == nil {
			return nil, fmt.Errorf("oauth credential missing OAuth fields")
		}
		return json.Marshal(oauthCredential{
			Type:      TypeOAuth,
			Refresh:   c.OAuth.Refresh,
			Access:    c.OAuth.Access,
			Expires:   c.OAuth.Expires,
			AccountID: c.OAuth.AccountID,
		})
	default:
		return nil, fmt.Errorf("unknown credential type %q", c.Type)
	}
}

func (c *Credential) UnmarshalJSON(data []byte) error {
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}

	switch typed.Type {
	case TypeAPIKey:
		var wire apiKeyCredential
		if err := json.Unmarshal(data, &wire); err != nil {
			return err
		}
		*c = Credential{
			Type: TypeAPIKey,
			Key:  wire.Key,
		}
		return nil
	case TypeOAuth:
		var wire oauthCredential
		if err := json.Unmarshal(data, &wire); err != nil {
			return err
		}
		oauth := OAuthCredential{
			Refresh:   wire.Refresh,
			Access:    wire.Access,
			Expires:   wire.Expires,
			AccountID: wire.AccountID,
		}
		*c = Credential{
			Type:  TypeOAuth,
			Key:   wire.Access,
			OAuth: &oauth,
		}
		return nil
	default:
		return fmt.Errorf("unknown credential type %q", typed.Type)
	}
}

func validateCredential(cred Credential) error {
	switch cred.Type {
	case TypeAPIKey:
		if cred.Key == "" {
			return fmt.Errorf("api_key credential missing key")
		}
		return nil
	case TypeOAuth:
		if cred.OAuth == nil {
			return fmt.Errorf("oauth credential missing OAuth fields")
		}
		if cred.OAuth.Refresh == "" || cred.OAuth.Access == "" {
			return fmt.Errorf("oauth credential missing refresh or access token")
		}
		return nil
	default:
		return fmt.Errorf("unknown credential type %q", cred.Type)
	}
}

type apiKeyCredential struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

type oauthCredential struct {
	Type      string `json:"type"`
	Refresh   string `json:"refresh"`
	Access    string `json:"access"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId,omitempty"`
}
