package ai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnthropicAuthHeaderUsesBearer(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	requestSeen := false
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requestSeen = true
			if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Fatalf("Authorization = %q, want bearer", got)
			}
			if got := req.Header.Get("X-Api-Key"); got != "" {
				t.Fatalf("X-Api-Key = %q, want empty when AuthHeader is set", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"content-type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(strings.Join([]string{
					`event: message_start`,
					`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}`,
					"",
					`event: message_delta`,
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
					"",
					`event: message_stop`,
					`data: {"type":"message_stop"}`,
					"",
				}, "\n"))),
				Request: req,
			}, nil
		}),
	}

	model, _ := GetModel("", "claude-sonnet-4-5")
	model.BaseURL = "https://example.test"
	model.AuthHeader = true
	got := collectProviderEvents(t, streamAnthropicSDK(context.Background(), client, model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}))
	if !requestSeen {
		t.Fatal("request was not sent")
	}
	if got[len(got)-1].Message.StopReason != StopReasonStop {
		t.Fatalf("final stop reason = %q", got[len(got)-1].Message.StopReason)
	}
}
