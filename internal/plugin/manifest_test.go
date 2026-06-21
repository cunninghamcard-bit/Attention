package plugin

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseManifestValidatesCapabilities(t *testing.T) {
	t.Run("legal token set passes", func(t *testing.T) {
		manifest, err := ParseManifest([]byte(manifestWithCapabilities(`[
			"sessions.read",
			"sessions.prompt",
			"spawn",
			"envs",
			"secrets:api-key"
		]`)))
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		if len(manifest.Capabilities) != 5 {
			t.Fatalf("capabilities = %v, want 5 tokens", manifest.Capabilities)
		}
	})

	tests := []struct {
		name         string
		capabilities string
		wantError    string
	}{
		{
			name:         "unknown word",
			capabilities: `["files.read"]`,
			wantError:    `unknown capability "files.read"`,
		},
		{
			name:         "secrets with no name",
			capabilities: `["secrets:"]`,
			wantError:    "secret name is required",
		},
		{
			name:         "uppercase",
			capabilities: `["secrets:Api"]`,
			wantError:    "must match",
		},
		{
			name:         "empty string",
			capabilities: `[""]`,
			wantError:    "must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(manifestWithCapabilities(tt.capabilities)))
			if err == nil {
				t.Fatal("ParseManifest succeeded, want capability validation error")
			}
			got := err.Error()
			if !strings.Contains(got, "manifest capabilities[0]") || !strings.Contains(got, tt.wantError) {
				t.Fatalf("error = %q, want field and %q", got, tt.wantError)
			}
		})
	}
}

func manifestWithCapabilities(capabilities string) string {
	return fmt.Sprintf(`{
		"id":"demo",
		"name":"Demo",
		"version":"1.0.0",
		"minAppVersion":"1.0.0",
		"main":"main.js",
		"capabilities":%s
	}`, capabilities)
}
