package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

const overflowCompactionReason = "overflow"

var errContextOverflowRecovery = errors.New("context overflow recovery failed")

type OverflowConfig struct {
	Enabled *bool
	Reason  string
	Compact CompactionFunc
}

func MWOverflow(cfg OverflowConfig, emit Emitter) RunMiddleware {
	return func(ctx context.Context, tc *RunContext, next RunHandler) error {
		if err := next(ctx, tc); err != nil {
			return err
		}
		msg, ok := lastAssistantMessage(tc)
		if !ok || !shouldRecoverOverflow(tc, msg, cfg.Enabled) {
			return nil
		}

		reason := cfg.Reason
		if reason == "" {
			reason = overflowCompactionReason
		}
		if _, err := runCompaction(
			ctx,
			tc,
			CompactionConfig{Enabled: boolPtrValue(true), Compact: cfg.Compact},
			emit,
			reason,
			true,
		); err != nil {
			return fmt.Errorf("context overflow recovery compact: %w", err)
		}

		if err := next(ctx, tc); err != nil {
			return fmt.Errorf("context overflow recovery continue: %w", err)
		}
		continued, ok := lastAssistantMessage(tc)
		if !ok {
			return nil
		}
		if ai.IsContextOverflow(continued, tc.Agent.Model.ContextWindow) {
			return contextOverflowRecoveryError(continued)
		}
		if continued.StopReason == ai.StopReasonError {
			return continueAssistantError(continued)
		}
		return nil
	}
}

func shouldRecoverOverflow(tc *RunContext, msg ai.Message, enabled *bool) bool {
	if !enabledValue(enabled, true) {
		return false
	}
	if !ai.IsContextOverflow(msg, tc.Agent.Model.ContextWindow) {
		return false
	}
	return sameAssistantModel(msg, tc.Agent.Model)
}

func sameAssistantModel(msg ai.Message, model ai.Model) bool {
	return msg.Provider == model.Provider && msg.Model == model.ID
}

func contextOverflowRecoveryError(msg ai.Message) error {
	if msg.ErrorMessage != "" {
		return fmt.Errorf("%w: %s", errContextOverflowRecovery, msg.ErrorMessage)
	}
	return errContextOverflowRecovery
}

func continueAssistantError(msg ai.Message) error {
	if msg.ErrorMessage != "" {
		return fmt.Errorf("context overflow recovery continue failed: %s", msg.ErrorMessage)
	}
	return errors.New("context overflow recovery continue failed")
}

func boolPtrValue(value bool) *bool {
	return &value
}
