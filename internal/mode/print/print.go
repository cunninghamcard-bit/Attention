// Package print implements the one-shot stdout presentation mode.
package print

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

type promptRunner interface {
	Prompt(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error)
}

// Run sends each prompt sequentially through the orchestrator and writes the
// final assistant text. pi's text mode writes nothing during streaming and
// prints only the last assistant message after all prompts complete;
// error/aborted runs go to stderr with exit code 1 (print-mode.ts:120-149).
func Run(ctx context.Context, orch promptRunner, prompts []string) error {
	return run(ctx, orch, prompts, os.Stdout)
}

func run(ctx context.Context, orch promptRunner, prompts []string, stdout io.Writer) error {
	var result orchestrator.PromptResult
	for _, prompt := range prompts {
		var err error
		result, err = orch.Prompt(ctx, orchestrator.PromptInput{Text: prompt})
		if err != nil {
			return err
		}
	}

	msg := result.Message
	if msg.Role != ai.RoleAssistant {
		return nil
	}
	if msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted {
		// pi: errorMessage (or "Request <stopReason>") to stderr, exit 1, and
		// the message text never reaches stdout (print-mode.ts:132-136).
		errMsg := msg.ErrorMessage
		if errMsg == "" {
			errMsg = fmt.Sprintf("Request %s", msg.StopReason)
		}
		return errors.New(errMsg)
	}
	for _, block := range msg.Content {
		if block.Type == ai.ContentText {
			if _, err := fmt.Fprintln(stdout, block.Text); err != nil {
				return err
			}
		}
	}
	return nil
}
