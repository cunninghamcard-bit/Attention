package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
)

func TestPromptRetriesRetryableErrorUntilMaxRetriesThenSurfaces(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: retryableAssistant("initial-model", "503 service unavailable", millis("2026-01-01T00:00:01.000Z")),
		continueResults: []ai.Message{
			retryableAssistant("initial-model", "502 bad gateway", millis("2026-01-01T00:00:02.000Z")),
			retryableAssistant("initial-model", "503 still unavailable", millis("2026-01-01T00:00:03.000Z")),
		},
	}
	o := newOverflowOrchestrator(s, h)
	o.settings = retrySettingsForTest(2, 1)
	retryEvents, cancelSubscribe := subscribeAutoRetryEvents(o)
	defer cancelSubscribe()

	result, err := o.Prompt(ctx, PromptInput{Text: "retry"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h.promptCalls != 1 || h.continueCalls != 2 {
		t.Fatalf("prompt/continue calls = %d/%d, want 1/2", h.promptCalls, h.continueCalls)
	}
	if h.compactCalls != 0 {
		t.Fatalf("compact calls = %d, want 0", h.compactCalls)
	}
	if result.Message.ErrorMessage != "503 still unavailable" {
		t.Fatalf("final error = %q, want last retry error", result.Message.ErrorMessage)
	}
	if got := o.currentRetryAttempt(); got != 0 {
		t.Fatalf("retryAttempt = %d, want reset to 0", got)
	}
	assertAutoRetryStart(t, readAutoRetryEvent(t, retryEvents), 1, 2, 1, "503 service unavailable")
	assertAutoRetryStart(t, readAutoRetryEvent(t, retryEvents), 2, 2, 2, "502 bad gateway")
	assertAutoRetryEnd(t, readAutoRetryEvent(t, retryEvents), false, 2, "503 still unavailable")
	assertNoAutoRetryEvent(t, retryEvents)
}

func TestPromptRetrySuccessOnSecondAttemptResets(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: retryableAssistant("initial-model", "rate limit exceeded", millis("2026-01-01T00:00:01.000Z")),
		continueResults: []ai.Message{
			retryableAssistant("initial-model", "connection lost", millis("2026-01-01T00:00:02.000Z")),
			stopAssistant("initial-model", "ok after retry", millis("2026-01-01T00:00:03.000Z")),
		},
	}
	o := newOverflowOrchestrator(s, h)
	o.settings = retrySettingsForTest(3, 1)
	retryEvents, cancelSubscribe := subscribeAutoRetryEvents(o)
	defer cancelSubscribe()

	result, err := o.Prompt(ctx, PromptInput{Text: "retry succeeds"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h.continueCalls != 2 {
		t.Fatalf("continue calls = %d, want 2", h.continueCalls)
	}
	if textOfMessage(t, result.Message) != "ok after retry" {
		t.Fatalf("result text = %q, want ok after retry", textOfMessage(t, result.Message))
	}
	if got := o.currentRetryAttempt(); got != 0 {
		t.Fatalf("retryAttempt = %d, want reset to 0", got)
	}
	assertAutoRetryStart(t, readAutoRetryEvent(t, retryEvents), 1, 3, 1, "rate limit exceeded")
	assertAutoRetryStart(t, readAutoRetryEvent(t, retryEvents), 2, 3, 2, "connection lost")
	assertAutoRetryEnd(t, readAutoRetryEvent(t, retryEvents), true, 2, "")
	assertNoAutoRetryEvent(t, retryEvents)
}

func TestPromptDoesNotRetryNonRetryableError(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: retryableAssistant("initial-model", "invalid API key", millis("2026-01-01T00:00:01.000Z")),
	}
	o := newOverflowOrchestrator(s, h)
	o.settings = retrySettingsForTest(3, 0)

	result, err := o.Prompt(ctx, PromptInput{Text: "non-retryable"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h.continueCalls != 0 {
		t.Fatalf("continue calls = %d, want 0", h.continueCalls)
	}
	if result.Message.ErrorMessage != "invalid API key" {
		t.Fatalf("final error = %q, want invalid API key", result.Message.ErrorMessage)
	}
}

func TestPromptOverflowRecoveryTakesPrecedenceOverRetry(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:        s,
		promptResult:   overflowAssistant("initial-model", millis("2026-01-01T00:00:01.000Z")),
		continueResult: stopAssistant("initial-model", "recovered", millis("2026-01-01T00:00:02.000Z")),
	}
	o := newOverflowOrchestrator(s, h)
	o.settings = retrySettingsForTest(3, 0)

	result, err := o.Prompt(ctx, PromptInput{Text: "overflow"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if textOfMessage(t, result.Message) != "recovered" {
		t.Fatalf("result text = %q, want recovered", textOfMessage(t, result.Message))
	}
	if h.compactCalls != 1 || h.continueCalls != 1 {
		t.Fatalf("compact/continue calls = %d/%d, want overflow path only 1/1", h.compactCalls, h.continueCalls)
	}
	if got := o.currentRetryAttempt(); got != 0 {
		t.Fatalf("retryAttempt = %d, want 0", got)
	}
}

func TestPromptRetryCancelDuringBackoffAbortsPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	promptDone := make(chan struct{})
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: retryableAssistant("initial-model", "server error", millis("2026-01-01T00:00:01.000Z")),
		onPrompt: func() {
			close(promptDone)
		},
	}
	o := newOverflowOrchestrator(s, h)
	o.settings = retrySettingsForTest(1, 60_000)

	errCh := make(chan error, 1)
	go func() {
		_, err := o.Prompt(ctx, PromptInput{Text: "cancel retry"})
		errCh <- err
	}()

	select {
	case <-promptDone:
	case <-time.After(time.Second):
		t.Fatal("prompt did not reach retryable assistant")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prompt did not abort promptly after cancellation")
	}
	if h.continueCalls != 0 {
		t.Fatalf("continue calls = %d, want 0", h.continueCalls)
	}
}

func TestPromptAbortRetryDuringBackoffStopsGracefully(t *testing.T) {
	ctx := context.Background()

	promptDone := make(chan struct{})
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: retryableAssistant("initial-model", "server error", millis("2026-01-01T00:00:01.000Z")),
		onPrompt: func() {
			close(promptDone)
		},
	}
	o := newOverflowOrchestrator(s, h)
	o.settings = retrySettingsForTest(1, 60_000)
	retryEvents, cancelSubscribe := subscribeAutoRetryEvents(o)
	defer cancelSubscribe()

	type promptOutcome struct {
		result PromptResult
		err    error
	}
	outcomeCh := make(chan promptOutcome, 1)
	go func() {
		result, err := o.Prompt(ctx, PromptInput{Text: "abort retry"})
		outcomeCh <- promptOutcome{result: result, err: err}
	}()

	select {
	case <-promptDone:
	case <-time.After(time.Second):
		t.Fatal("prompt did not reach retryable assistant")
	}
	assertAutoRetryStart(t, readAutoRetryEvent(t, retryEvents), 1, 1, 60_000, "server error")

	o.AbortRetry()

	select {
	case outcome := <-outcomeCh:
		if outcome.err != nil {
			t.Fatalf("Prompt error = %v, want nil", outcome.err)
		}
		if outcome.result.Message.ErrorMessage != "server error" {
			t.Fatalf("final error = %q, want current retryable error", outcome.result.Message.ErrorMessage)
		}
	case <-time.After(time.Second):
		t.Fatal("Prompt did not stop promptly after AbortRetry")
	}
	if h.continueCalls != 0 {
		t.Fatalf("continue calls = %d, want 0", h.continueCalls)
	}
	if got := o.currentRetryAttempt(); got != 0 {
		t.Fatalf("retryAttempt = %d, want reset to 0", got)
	}
	assertAutoRetryEnd(t, readAutoRetryEvent(t, retryEvents), false, 1, "Retry cancelled")
	assertNoAutoRetryEvent(t, retryEvents)
}

func TestAbortRetryNoopWhenNoRetryInProgress(t *testing.T) {
	o := newOverflowOrchestrator(newOverflowSession(), &overflowHarness{})

	o.AbortRetry()
	o.AbortRetry()

	o.mu.Lock()
	retryAbort := o.retryAbort
	o.mu.Unlock()
	if retryAbort != nil {
		t.Fatal("retryAbort = non-nil, want nil")
	}
}

func TestAutoRetrySettingsDefaultsAndOverrides(t *testing.T) {
	defaults := autoRetrySettingsFrom(nil)
	if !defaults.enabled || defaults.maxRetries != 3 || defaults.baseDelay != 2*time.Second {
		t.Fatalf("defaults = %+v, want enabled=true maxRetries=3 baseDelay=2s", defaults)
	}

	fromPiSettings := autoRetrySettingsFrom(config.Settings{
		"retry": map[string]any{
			"enabled":     false,
			"maxRetries":  float64(5),
			"baseDelayMs": float64(250),
		},
	})
	if fromPiSettings.enabled || fromPiSettings.maxRetries != 5 || fromPiSettings.baseDelay != 250*time.Millisecond {
		t.Fatalf("pi settings = %+v, want disabled maxRetries=5 baseDelay=250ms", fromPiSettings)
	}

	fromGoSettings := autoRetrySettingsFrom(config.Settings{
		"retry": map[string]any{
			"baseDelay": 2 * time.Second,
		},
	})
	if fromGoSettings.baseDelay != 2*time.Second {
		t.Fatalf("baseDelay = %s, want 2s", fromGoSettings.baseDelay)
	}
}

func TestCompactionSettingsDefaultsAndOverrides(t *testing.T) {
	defaults := compactionSettingsFrom(nil)
	if !defaults.Enabled || defaults.ReserveTokens != 16384 || defaults.KeepRecentTokens != 20000 {
		t.Fatalf("defaults = %+v, want enabled=true reserve=16384 keep=20000", defaults)
	}

	overrides := compactionSettingsFrom(config.Settings{
		"compaction": map[string]any{
			"enabled":          false,
			"reserveTokens":    float64(4096),
			"keepRecentTokens": float64(8192),
		},
	})
	if overrides.Enabled || overrides.ReserveTokens != 4096 || overrides.KeepRecentTokens != 8192 {
		t.Fatalf("overrides = %+v, want enabled=false reserve=4096 keep=8192", overrides)
	}
}

func TestSetAutoRetryOverridesSettings(t *testing.T) {
	o := newOverflowOrchestrator(newOverflowSession(), &overflowHarness{})
	o.settings = retrySettingsForTest(3, 0)

	o.SetAutoRetry(false)
	if got := o.autoRetrySettings(); got.enabled {
		t.Fatalf("autoRetrySettings enabled = true after SetAutoRetry(false): %+v", got)
	}

	o.settings = config.Settings{
		"retry": map[string]any{
			"enabled": false,
		},
	}
	o.SetAutoRetry(true)
	if got := o.autoRetrySettings(); !got.enabled {
		t.Fatalf("autoRetrySettings enabled = false after SetAutoRetry(true): %+v", got)
	}
}

func subscribeAutoRetryEvents(o *Orchestrator) (<-chan Event, func()) {
	events := make(chan Event, 8)
	cancel := o.Subscribe(func(ev Event) {
		if ev.Type == EventAutoRetryStart || ev.Type == EventAutoRetryEnd {
			events <- ev
		}
	})
	return events, cancel
}

func readAutoRetryEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()

	select {
	case ev := <-events:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for auto retry event")
		return Event{}
	}
}

func assertNoAutoRetryEvent(t *testing.T, events <-chan Event) {
	t.Helper()

	select {
	case ev := <-events:
		t.Fatalf("unexpected auto retry event: %#v", ev)
	default:
	}
}

func assertAutoRetryStart(
	t *testing.T,
	ev Event,
	attempt int,
	maxAttempts int,
	delayMs int,
	errorMessage string,
) {
	t.Helper()

	if ev.Type != EventAutoRetryStart ||
		ev.Attempt != attempt ||
		ev.MaxAttempts != maxAttempts ||
		ev.DelayMs != delayMs ||
		ev.ErrorMessage != errorMessage {
		t.Fatalf(
			"auto_retry_start = %#v, want attempt=%d maxAttempts=%d delayMs=%d errorMessage=%q",
			ev,
			attempt,
			maxAttempts,
			delayMs,
			errorMessage,
		)
	}
}

func assertAutoRetryEnd(
	t *testing.T,
	ev Event,
	success bool,
	attempt int,
	finalError string,
) {
	t.Helper()

	if ev.Type != EventAutoRetryEnd ||
		ev.Success != success ||
		ev.Attempt != attempt ||
		ev.FinalError != finalError {
		t.Fatalf(
			"auto_retry_end = %#v, want success=%t attempt=%d finalError=%q",
			ev,
			success,
			attempt,
			finalError,
		)
	}
}

func retrySettingsForTest(maxRetries int, baseDelayMs int) config.Settings {
	return config.Settings{
		"retry": map[string]any{
			"enabled":     true,
			"maxRetries":  maxRetries,
			"baseDelayMs": baseDelayMs,
		},
	}
}

func retryableAssistant(modelID string, errorMessage string, timestamp int64) ai.Message {
	return ai.Message{
		Role:         ai.RoleAssistant,
		Provider:     "test-provider",
		Model:        modelID,
		StopReason:   ai.StopReasonError,
		ErrorMessage: errorMessage,
		Content:      []ai.ContentBlock{{Type: ai.ContentText, Text: "Error: " + errorMessage}},
		Timestamp:    timestamp,
	}
}
