package ai

import "testing"

func TestIsRetryableError_TransientPatterns(t *testing.T) {
	tests := []struct {
		name         string
		errorMessage string
	}{
		{name: "overloaded", errorMessage: "overloaded_error: model overloaded"},
		{name: "provider returned error", errorMessage: "provider returned error"},
		{name: "rate limit", errorMessage: "rate limit exceeded"},
		{name: "too many requests", errorMessage: "Too many requests"},
		{name: "429", errorMessage: "HTTP 429 from provider"},
		{name: "500", errorMessage: "HTTP 500 internal fault"},
		{name: "502", errorMessage: "502 bad gateway"},
		{name: "503", errorMessage: "503 unavailable"},
		{name: "504", errorMessage: "504 gateway timeout"},
		{name: "service unavailable", errorMessage: "service unavailable"},
		{name: "server unavailable", errorMessage: "server unavailable"},
		{name: "internal unavailable", errorMessage: "internal unavailable"},
		{name: "server error", errorMessage: "server error"},
		{name: "internal error", errorMessage: "internal error"},
		{name: "network error", errorMessage: "network error"},
		{name: "network refused", errorMessage: "network refused"},
		{name: "network lost", errorMessage: "network lost"},
		{name: "connection error", errorMessage: "connection error"},
		{name: "connection refused", errorMessage: "connection refused"},
		{name: "connection lost", errorMessage: "connection lost"},
		{name: "websocket closed", errorMessage: "websocket closed"},
		{name: "websocket error", errorMessage: "websocket error"},
		{name: "other side closed", errorMessage: "other side closed"},
		{name: "fetch failed", errorMessage: "fetch failed"},
		{name: "upstream connect", errorMessage: "upstream connect error"},
		{name: "reset before headers", errorMessage: "reset before headers"},
		{name: "socket hang up", errorMessage: "socket hang up"},
		{name: "ended without", errorMessage: "ended without message_stop"},
		{name: "stream ended before message stop", errorMessage: "stream ended before message_stop"},
		{name: "http2 no response", errorMessage: "http2 request did not get a response"},
		{name: "timed out", errorMessage: "request timed out"},
		{name: "time out", errorMessage: "request time out"},
		{name: "timeout", errorMessage: "timeout waiting for response"},
		{name: "terminated", errorMessage: "terminated"},
		{name: "retry delay", errorMessage: "retry delay exceeded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := Message{
				StopReason:   StopReasonError,
				ErrorMessage: tt.errorMessage,
			}
			if !IsRetryableError(msg, 0) {
				t.Fatalf("IsRetryableError(%q) = false, want true", tt.errorMessage)
			}
		})
	}
}

func TestIsRetryableError_ExcludesOverflow(t *testing.T) {
	msg := Message{
		StopReason:   StopReasonError,
		ErrorMessage: "HTTP 500: prompt is too long",
	}

	if IsRetryableError(msg, 0) {
		t.Fatal("IsRetryableError(overflow) = true, want false")
	}
}

func TestIsRetryableError_NonRetryableError(t *testing.T) {
	msg := Message{
		StopReason:   StopReasonError,
		ErrorMessage: "invalid API key",
	}

	if IsRetryableError(msg, 0) {
		t.Fatal("IsRetryableError(non-retryable error) = true, want false")
	}
}

func TestIsRetryableError_NonErrorStopReason(t *testing.T) {
	msg := Message{
		StopReason:   StopReasonStop,
		ErrorMessage: "rate limit exceeded",
	}

	if IsRetryableError(msg, 0) {
		t.Fatal("IsRetryableError(non-error stop reason) = true, want false")
	}
}
