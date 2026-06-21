package ai

import "testing"

func TestIsContextOverflow_ErrorPatterns(t *testing.T) {
	tests := []struct {
		name         string
		errorMessage string
	}{
		{
			name:         "anthropic prompt too long",
			errorMessage: "prompt is too long: 213462 tokens > 200000 maximum",
		},
		{
			name:         "anthropic request too large",
			errorMessage: `413 {"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`,
		},
		{
			name:         "bedrock input too long",
			errorMessage: "Input is too long for requested model",
		},
		{
			name:         "openai context window",
			errorMessage: "Your input exceeds the context window of this model",
		},
		{
			name:         "openai compatible maximum context length",
			errorMessage: "Requested token count exceeds the model's maximum context length of 131,072 tokens",
		},
		{
			name: "google input token count",
			errorMessage: "The input token count (1196265) exceeds the maximum number of tokens allowed " +
				"(1048575)",
		},
		{
			name: "xai maximum prompt length",
			errorMessage: "This model's maximum prompt length is 131072 but the request contains " +
				"537812 tokens",
		},
		{
			name:         "groq reduce messages",
			errorMessage: "Please reduce the length of the messages or completion",
		},
		{
			name: "openrouter maximum context length",
			errorMessage: "This endpoint's maximum context length is 131072 tokens. However, you requested " +
				"about 537812 tokens",
		},
		{
			name: "together input longer than context",
			errorMessage: "The input (537812 tokens) is longer than the model's context length " +
				"(131072 tokens).",
		},
		{
			name:         "github copilot token count",
			errorMessage: "prompt token count of 537812 exceeds the limit of 131072",
		},
		{
			name:         "llama cpp context size",
			errorMessage: "the request exceeds the available context size, try increasing it",
		},
		{
			name:         "lm studio context length",
			errorMessage: "tokens to keep from the initial prompt is greater than the context length",
		},
		{
			name:         "minimax context window",
			errorMessage: "invalid params, context window exceeds limit",
		},
		{
			name:         "kimi model token limit",
			errorMessage: "Your request exceeded model token limit: 131072 (requested: 537812)",
		},
		{
			name: "mistral maximum context length",
			errorMessage: "Prompt contains 537812 tokens and is too large for model with 131072 " +
				"maximum context length",
		},
		{
			name:         "zai model context window",
			errorMessage: "model_context_window_exceeded",
		},
		{
			name:         "ollama prompt too long",
			errorMessage: "prompt too long; exceeded max context length by 123 tokens",
		},
		{
			name:         "generic context length exceeded",
			errorMessage: "context_length_exceeded",
		},
		{
			name:         "generic too many tokens",
			errorMessage: "too many tokens in request",
		},
		{
			name:         "generic token limit exceeded",
			errorMessage: "token limit exceeded for this model",
		},
		{
			name:         "cerebras status no body",
			errorMessage: "413 status code (no body)",
		},
	}

	if len(tests) != len(OverflowPatterns) {
		t.Fatalf("test cases = %d, want %d overflow patterns", len(tests), len(OverflowPatterns))
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !OverflowPatterns[i].MatchString(tt.errorMessage) {
				t.Fatalf("OverflowPatterns[%d] did not match %q", i, tt.errorMessage)
			}

			msg := Message{
				StopReason:   StopReasonError,
				ErrorMessage: tt.errorMessage,
			}
			if !IsContextOverflow(msg, 0) {
				t.Fatalf("IsContextOverflow(%q) = false, want true", tt.errorMessage)
			}
		})
	}
}

func TestIsContextOverflow_NonOverflowExclusions(t *testing.T) {
	tests := []struct {
		name         string
		errorMessage string
	}{
		{
			name:         "throttling prefix",
			errorMessage: "Throttling error: Too many tokens, please wait before trying again.",
		},
		{
			name:         "rate limit",
			errorMessage: "rate limit: token limit exceeded",
		},
		{
			name:         "too many requests",
			errorMessage: "too many requests: prompt is too long",
		},
	}

	if len(tests) != len(NonOverflowPatterns) {
		t.Fatalf("test cases = %d, want %d non-overflow patterns", len(tests), len(NonOverflowPatterns))
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !NonOverflowPatterns[i].MatchString(tt.errorMessage) {
				t.Fatalf("NonOverflowPatterns[%d] did not match %q", i, tt.errorMessage)
			}

			msg := Message{
				StopReason:   StopReasonError,
				ErrorMessage: tt.errorMessage,
			}
			if IsContextOverflow(msg, 0) {
				t.Fatalf("IsContextOverflow(%q) = true, want false", tt.errorMessage)
			}
		})
	}
}

func TestIsContextOverflow_UnrelatedError(t *testing.T) {
	msg := Message{
		StopReason:   StopReasonError,
		ErrorMessage: "upstream returned invalid credentials",
	}

	if IsContextOverflow(msg, 0) {
		t.Fatal("IsContextOverflow(unrelated error) = true, want false")
	}
}

func TestIsContextOverflow_SilentStopUsage(t *testing.T) {
	tests := []struct {
		name          string
		contextWindow int
		usage         *Usage
		want          bool
	}{
		{
			name:          "over context window",
			contextWindow: 1000,
			usage:         &Usage{Input: 900, CacheRead: 101},
			want:          true,
		},
		{
			name:          "under context window",
			contextWindow: 1000,
			usage:         &Usage{Input: 900, CacheRead: 99},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := Message{
				StopReason: StopReasonStop,
				Usage:      tt.usage,
			}
			if got := IsContextOverflow(msg, tt.contextWindow); got != tt.want {
				t.Fatalf("IsContextOverflow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsContextOverflow_LengthStopZeroOutput(t *testing.T) {
	msg := Message{
		StopReason: StopReasonLength,
		Usage: &Usage{
			Input:  990,
			Output: 0,
		},
	}

	if !IsContextOverflow(msg, 1000) {
		t.Fatal("IsContextOverflow(length stop at 99% with zero output) = false, want true")
	}
}
