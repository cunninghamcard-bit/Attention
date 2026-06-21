package pipeline

import (
	"context"
	"math"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

const (
	defaultRetryEnabled   = true
	defaultRetryMax       = 3
	defaultRetryBaseDelay = 2 * time.Second
)

type RetryConfig struct {
	Enabled    *bool
	MaxRetries *int
	BaseDelay  *time.Duration
	Wait       func(ctx context.Context, delay time.Duration) error
}

type RetryPayload struct {
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	DelayMs      int    `json:"delayMs,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	FinalError   string `json:"finalError,omitempty"`
}

type retrySettings struct {
	enabled    bool
	maxRetries int
	baseDelay  time.Duration
	wait       func(ctx context.Context, delay time.Duration) error
}

func MWRetry(cfg RetryConfig, emit Emitter) RunMiddleware {
	settings := retrySettingsFromConfig(cfg)

	return func(ctx context.Context, tc *RunContext, next RunHandler) error {
		if err := next(ctx, tc); err != nil {
			return err
		}

		msg, ok := lastAssistantMessage(tc)
		if !ok || !ai.IsRetryableError(msg, tc.Agent.Model.ContextWindow) {
			return nil
		}
		if !settings.enabled || settings.maxRetries <= 0 {
			return nil
		}

		attempt := 0
		for attempt < settings.maxRetries && ai.IsRetryableError(msg, tc.Agent.Model.ContextWindow) {
			attempt++
			delay := retryBackoffDelay(settings.baseDelay, attempt)
			if err := emitRetry(emit, tc, protocol.KindRetryAttempted, RetryPayload{
				Attempt:      attempt,
				MaxAttempts:  settings.maxRetries,
				DelayMs:      int(delay.Milliseconds()),
				ErrorMessage: retryErrorMessage(msg),
			}); err != nil {
				return err
			}
			if err := settings.wait(ctx, delay); err != nil {
				return err
			}
			if err := next(ctx, tc); err != nil {
				return err
			}
			var found bool
			msg, found = lastAssistantMessage(tc)
			if !found {
				return nil
			}
		}

		if ai.IsRetryableError(msg, tc.Agent.Model.ContextWindow) {
			return emitRetry(emit, tc, protocol.KindRetryExhausted, RetryPayload{
				Attempt:     attempt,
				MaxAttempts: settings.maxRetries,
				FinalError:  retryErrorMessage(msg),
			})
		}
		return nil
	}
}

func retrySettingsFromConfig(cfg RetryConfig) retrySettings {
	enabled := defaultRetryEnabled
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}
	maxRetries := defaultRetryMax
	if cfg.MaxRetries != nil {
		maxRetries = *cfg.MaxRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
	}
	baseDelay := defaultRetryBaseDelay
	if cfg.BaseDelay != nil {
		baseDelay = *cfg.BaseDelay
		if baseDelay < 0 {
			baseDelay = 0
		}
	}
	wait := cfg.Wait
	if wait == nil {
		wait = waitRetryBackoff
	}
	return retrySettings{
		enabled:    enabled,
		maxRetries: maxRetries,
		baseDelay:  baseDelay,
		wait:       wait,
	}
}

func retryBackoffDelay(base time.Duration, attempt int) time.Duration {
	if attempt <= 1 || base <= 0 {
		return base
	}
	if attempt > 62 {
		return time.Duration(math.MaxInt64)
	}
	multiplier := int64(1) << uint(attempt-1)
	if base > time.Duration(math.MaxInt64/multiplier) {
		return time.Duration(math.MaxInt64)
	}
	return base * time.Duration(multiplier)
}

func waitRetryBackoff(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func emitRetry(emit Emitter, tc *RunContext, kind string, payload RetryPayload) error {
	if emit == nil {
		return nil
	}
	return emit(tc, kind, protocol.ActorSystem, payload)
}

func retryErrorMessage(msg ai.Message) string {
	if msg.ErrorMessage != "" {
		return msg.ErrorMessage
	}
	return "Unknown error"
}

func lastAssistantMessage(tc *RunContext) (ai.Message, bool) {
	if tc == nil || tc.Session == nil {
		return ai.Message{}, false
	}
	messages := tc.Session.Messages()
	return lastAssistantFromMessages(messages)
}

func lastAssistantFromMessages(messages []message.AgentMessage) (ai.Message, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := message.AsAIMessage(messages[i])
		if !ok || msg.Role != ai.RoleAssistant {
			continue
		}
		return msg, true
	}
	return ai.Message{}, false
}
