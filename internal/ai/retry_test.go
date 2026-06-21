package ai

import (
	"net/http"
	"testing"
)

func TestSDKMaxRetries(t *testing.T) {
	tests := []struct {
		name         string
		input        int
		expected     int
		expectedUsed bool
	}{
		{
			name:         "zero uses provider default",
			input:        0,
			expected:     0,
			expectedUsed: false,
		},
		{
			name:         "positive value is explicit",
			input:        3,
			expected:     3,
			expectedUsed: true,
		},
		{
			name:         "negative value disables retries",
			input:        -1,
			expected:     0,
			expectedUsed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, used := sdkMaxRetries(tt.input)
			if got != tt.expected || used != tt.expectedUsed {
				t.Fatalf(
					"sdkMaxRetries(%d) = %d/%v, want %d/%v",
					tt.input,
					got,
					used,
					tt.expected,
					tt.expectedUsed,
				)
			}
		})
	}
}

func TestSDKProviderOptionsAcceptNegativeMaxRetries(t *testing.T) {
	openAIModel, ok := GetModel("", "gpt-5")
	if !ok {
		t.Fatal("missing gpt-5 model")
	}
	anthropicModel, ok := GetModel("", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("missing claude-sonnet-4-5 model")
	}

	var openAIResp *http.Response
	_ = openAIResponsesOptions(
		http.DefaultClient,
		openAIModel,
		&StreamOptions{MaxRetries: -1},
		&openAIResp,
	)

	var anthropicResp *http.Response
	_ = anthropicOptions(
		http.DefaultClient,
		anthropicModel,
		&StreamOptions{MaxRetries: -1},
		&anthropicResp,
	)
}
