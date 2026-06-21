package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
)

const (
	defaultAutoRetryEnabled = true
	defaultAutoRetryMax     = 3
	// pi defaults retry.baseDelayMs to 2000ms when unset:
	// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:716-721.
	defaultAutoRetryBaseDelay = 2 * time.Second

	// pi defaults compaction settings when keys are absent:
	// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:663-690.
	defaultCompactionEnabled          = true
	defaultCompactionReserveTokens    = 16384
	defaultCompactionKeepRecentTokens = 20000
)

var errRetryAborted = errors.New("retry aborted")

type autoRetrySettings struct {
	enabled    bool
	maxRetries int
	baseDelay  time.Duration
}

// willRetryAfterAgentEnd mirrors pi's _willRetryAfterAgentEnd
// (agent-session.ts:542-555): retry enabled, attempts left, and the last
// assistant message carries a retryable error.
func (o *Orchestrator) willRetryAfterAgentEnd(messages []ai.Message) bool {
	settings := o.autoRetrySettings()
	if !settings.enabled || o.currentRetryAttempt() >= settings.maxRetries {
		return false
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ai.RoleAssistant {
			return ai.IsRetryableError(messages[i], o.currentModel().ContextWindow)
		}
	}
	return false
}

func (o *Orchestrator) retryTransientError(
	ctx context.Context,
	state harness.TurnState,
	msg ai.Message,
) (ai.Message, error) {
	abortCh := o.startRetryAbort()
	defer o.clearRetryAbort(abortCh)

	settings := o.autoRetrySettings()
	for settings.enabled &&
		ai.IsRetryableError(msg, state.Model.ContextWindow) &&
		o.currentRetryAttempt() < settings.maxRetries {
		attempt := o.incrementRetryAttempt()
		delay := retryBackoffDelay(settings.baseDelay, attempt)
		// pi emits auto_retry_start after computing attempt/delay and before
		// sleeping: .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2463-2472.
		o.publish(Event{
			Type:         EventAutoRetryStart,
			Attempt:      attempt,
			MaxAttempts:  settings.maxRetries,
			DelayMs:      int(delay.Milliseconds()),
			ErrorMessage: retryErrorMessage(msg),
		})
		if err := waitRetryBackoff(ctx, delay, abortCh); err != nil {
			if errors.Is(err, errRetryAborted) {
				// pi _prepareRetry catches aborted sleep, resets the attempt, and
				// returns false so the turn stops without surfacing a ctx error:
				// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2463-2490.
				o.resetRetryAttempt()
				o.publish(Event{
					Type:       EventAutoRetryEnd,
					Success:    false,
					Attempt:    attempt,
					FinalError: "Retry cancelled",
				})
				return msg, nil
			}
			o.resetRetryAttempt()
			return msg, err
		}
		if err := o.prepareRetryAssistant(ctx, msg); err != nil {
			return msg, err
		}

		continued, err := o.harness.Continue(ctx, state)
		if err != nil {
			return continued, err
		}
		msg = continued
		if msg.StopReason != ai.StopReasonError {
			// pi emits successful auto_retry_end before resetting retry state:
			// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:532.
			o.publish(Event{
				Type:    EventAutoRetryEnd,
				Success: true,
				Attempt: attempt,
			})
			o.resetRetryAttempt()
			return msg, nil
		}
	}

	if lastAttempt := o.currentRetryAttempt(); msg.StopReason == ai.StopReasonError && lastAttempt > 0 {
		// pi emits terminal failed auto_retry_end before resetting retry state:
		// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:942.
		o.publish(Event{
			Type:       EventAutoRetryEnd,
			Success:    false,
			Attempt:    lastAttempt,
			FinalError: retryErrorMessage(msg),
		})
		o.resetRetryAttempt()
	}
	return msg, nil
}

func retryErrorMessage(msg ai.Message) string {
	if msg.ErrorMessage != "" {
		return msg.ErrorMessage
	}
	return "Unknown error"
}

func (o *Orchestrator) prepareRetryAssistant(ctx context.Context, msg ai.Message) error {
	entry, err := assistantEntryForMessage(o.session, msg)
	if err != nil || !entry.ok {
		return err
	}
	return moveToAssistantParent(ctx, o.session, entry)
}

func moveToAssistantParent(
	ctx context.Context,
	s harness.Session,
	assistant assistantEntryResult,
) error {
	parentID := copyEntryIDPtr(assistant.entry.ParentID)
	_, err := s.MoveTo(ctx, parentID, nil)
	return err
}

func (o *Orchestrator) autoRetrySettings() autoRetrySettings {
	o.mu.Lock()
	settings := cloneSettings(o.settings)
	var override bool
	hasOverride := false
	if o.autoRetryEnabledOverride != nil {
		override = *o.autoRetryEnabledOverride
		hasOverride = true
	}
	o.mu.Unlock()

	result := autoRetrySettingsFrom(settings)
	if hasOverride {
		result.enabled = override
	}
	return result
}

// SetAutoRetry mirrors pi set_auto_retry -> setAutoRetryEnabled:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:535-538
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2519-2521.
// pi writes through settingsManager:
// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:703-714.
func (o *Orchestrator) SetAutoRetry(enabled bool) {
	value := enabled
	o.mu.Lock()
	o.autoRetryEnabledOverride = &value
	o.mu.Unlock()

	o.persistGlobalSetting([]string{"retry", "enabled"}, enabled)
}

func autoRetrySettingsFrom(settings config.Settings) autoRetrySettings {
	result := autoRetrySettings{
		enabled:    defaultAutoRetryEnabled,
		maxRetries: defaultAutoRetryMax,
		baseDelay:  defaultAutoRetryBaseDelay,
	}

	retry, ok := retrySettingsObject(settings)
	if !ok {
		return result
	}
	if enabled, ok := retry["enabled"].(bool); ok {
		result.enabled = enabled
	}
	if maxRetries, ok := intSetting(retry["maxRetries"]); ok {
		result.maxRetries = max(maxRetries, 0)
	}
	if baseDelayMs, ok := intSetting(retry["baseDelayMs"]); ok {
		if baseDelayMs < 0 {
			baseDelayMs = 0
		}
		result.baseDelay = time.Duration(baseDelayMs) * time.Millisecond
	}
	if baseDelay, ok := durationSetting(retry["baseDelay"]); ok {
		result.baseDelay = baseDelay
	}
	return result
}

func compactionSettingsFrom(settings config.Settings) hook.CompactionSettings {
	result := hook.CompactionSettings{
		Enabled:          defaultCompactionEnabled,
		ReserveTokens:    defaultCompactionReserveTokens,
		KeepRecentTokens: defaultCompactionKeepRecentTokens,
	}

	compaction, ok := compactionSettingsObject(settings)
	if !ok {
		return result
	}
	if enabled, ok := compaction["enabled"].(bool); ok {
		result.Enabled = enabled
	}
	if reserveTokens, ok := intSetting(compaction["reserveTokens"]); ok {
		result.ReserveTokens = reserveTokens
	}
	if keepRecentTokens, ok := intSetting(compaction["keepRecentTokens"]); ok {
		result.KeepRecentTokens = keepRecentTokens
	}
	return result
}

func retrySettingsObject(settings config.Settings) (map[string]any, bool) {
	return settingsObject(settings, "retry")
}

func compactionSettingsObject(settings config.Settings) (map[string]any, bool) {
	return settingsObject(settings, "compaction")
}

func intSetting(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return int(n), true
		}
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return int(f), true
	default:
		return 0, false
	}
}

func durationSetting(value any) (time.Duration, bool) {
	switch v := value.(type) {
	case time.Duration:
		if v < 0 {
			return 0, true
		}
		return v, true
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, false
		}
		if d < 0 {
			return 0, true
		}
		return d, true
	default:
		ms, ok := intSetting(value)
		if !ok {
			return 0, false
		}
		if ms < 0 {
			ms = 0
		}
		return time.Duration(ms) * time.Millisecond, true
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

func waitRetryBackoff(ctx context.Context, delay time.Duration, retryAbort <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-retryAbort:
		if err := ctx.Err(); err != nil {
			return err
		}
		return errRetryAborted
	default:
	}
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-retryAbort:
		if err := ctx.Err(); err != nil {
			return err
		}
		return errRetryAborted
	case <-timer.C:
		return nil
	}
}

func (o *Orchestrator) startRetryAbort() chan struct{} {
	ch := make(chan struct{})
	o.mu.Lock()
	o.retryAbort = ch
	o.mu.Unlock()
	return ch
}

func (o *Orchestrator) clearRetryAbort(ch chan struct{}) {
	o.mu.Lock()
	if o.retryAbort == ch {
		o.retryAbort = nil
	}
	o.mu.Unlock()
}

// AbortRetry mirrors pi abort_retry -> abortRetry:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:540-543
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2502-2504.
// It closes only the retry backoff signal, leaving the active run context intact.
func (o *Orchestrator) AbortRetry() {
	o.mu.Lock()
	ch := o.retryAbort
	if ch != nil {
		close(ch)
		o.retryAbort = nil
	}
	o.mu.Unlock()
}

func (o *Orchestrator) incrementRetryAttempt() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retryAttempt++
	return o.retryAttempt
}

func (o *Orchestrator) currentRetryAttempt() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.retryAttempt
}

func (o *Orchestrator) resetRetryAttempt() {
	o.mu.Lock()
	o.retryAttempt = 0
	o.mu.Unlock()
}

func cloneSettings(settings config.Settings) config.Settings {
	if len(settings) == 0 {
		return nil
	}
	cloned := make(config.Settings, len(settings))
	maps.Copy(cloned, settings)
	return cloned
}
