package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

const manualCompactionReason = "manual"

var errMissingCompactor = errors.New("pipeline: compactor is required")

type CompactionFunc func(ctx context.Context, tc *RunContext, reason string) (CompactionResult, error)

type CompactionConfig struct {
	Enabled *bool
	Reason  string
	Compact CompactionFunc
}

type CompactionResult struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     int
	Details          any
	FromHook         bool
}

type CompactionPayload struct {
	Reason       string            `json:"reason"`
	Result       *CompactionResult `json:"result,omitempty"`
	Aborted      bool              `json:"aborted,omitempty"`
	WillRetry    bool              `json:"willRetry,omitempty"`
	ErrorMessage string            `json:"errorMessage,omitempty"`
}

func MWCompaction(cfg CompactionConfig, emit Emitter) RunMiddleware {
	return func(ctx context.Context, tc *RunContext, next RunHandler) error {
		if !enabledValue(cfg.Enabled, true) {
			return next(ctx, tc)
		}

		reason := cfg.Reason
		if reason == "" {
			reason = manualCompactionReason
		}
		if _, err := runCompaction(ctx, tc, cfg, emit, reason, false); err != nil {
			return err
		}
		return next(ctx, tc)
	}
}

func runCompaction(
	ctx context.Context,
	tc *RunContext,
	cfg CompactionConfig,
	emit Emitter,
	reason string,
	willRetry bool,
) (CompactionResult, error) {
	if cfg.Compact == nil {
		return CompactionResult{}, errMissingCompactor
	}
	if err := emitCompaction(emit, tc, protocol.KindCompactionStarted, CompactionPayload{
		Reason: reason,
	}); err != nil {
		return CompactionResult{}, err
	}

	result, compactErr := cfg.Compact(ctx, tc, reason)
	if compactErr == nil {
		compactErr = appendCompactionSummary(tc, result)
	}

	payload := CompactionPayload{
		Reason:    reason,
		Aborted:   errors.Is(compactErr, context.Canceled),
		WillRetry: willRetry,
	}
	if compactErr == nil {
		payload.Result = &result
	} else if !payload.Aborted {
		payload.ErrorMessage = compactionErrorMessage(reason, compactErr)
	}
	if err := emitCompaction(emit, tc, protocol.KindCompactionCompleted, payload); err != nil {
		return result, err
	}
	if compactErr != nil {
		return result, compactErr
	}
	return result, nil
}

func appendCompactionSummary(tc *RunContext, result CompactionResult) error {
	if result.Summary == "" || tc.Session == nil {
		return nil
	}
	return tc.Session.AppendMessage(message.CompactionSummaryMessage{
		Summary:      result.Summary,
		TokensBefore: result.TokensBefore,
		Timestamp:    time.Now().UnixMilli(),
	})
}

func emitCompaction(emit Emitter, tc *RunContext, kind string, payload CompactionPayload) error {
	if emit == nil {
		return nil
	}
	return emit(tc, kind, protocol.ActorSystem, payload)
}

func compactionErrorMessage(reason string, err error) string {
	if err == nil {
		return ""
	}
	if reason == "" {
		return err.Error()
	}
	return fmt.Sprintf("%s compaction failed: %s", reason, err.Error())
}

func enabledValue(value *bool, defaultValue bool) bool {
	if value == nil {
		return defaultValue
	}
	return *value
}
