package print

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

func TestRunPrintsOnlyFinalAssistantText(t *testing.T) {
	runner := &fakeRunner{}
	runner.promptFunc = func(_ context.Context, input orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		if input.Text != "prompt" {
			t.Fatalf("prompt input text = %q, want prompt", input.Text)
		}
		return orchestrator.PromptResult{Message: assistantMessage("final answer")}, nil
	}

	var out bytes.Buffer
	if err := run(context.Background(), runner, []string{"prompt"}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// pi's text mode writes nothing during streaming and prints only the last
	// assistant message after the run completes (print-mode.ts:128-144).
	if got, want := out.String(), "final answer\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunMultipleTextBlocksEachGetNewline(t *testing.T) {
	runner := &fakeRunner{}
	runner.promptFunc = func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		return orchestrator.PromptResult{Message: ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.ContentBlock{
				{Type: ai.ContentText, Text: "one"},
				{Type: ai.ContentToolCall, ToolName: "run"},
				{Type: ai.ContentText, Text: "two"},
			},
		}}, nil
	}

	var out bytes.Buffer
	if err := run(context.Background(), runner, []string{"prompt"}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := out.String(), "one\ntwo\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunErrorStopReasonFailsWithoutStdout(t *testing.T) {
	runner := &fakeRunner{}
	runner.promptFunc = func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		msg := assistantMessage("partial text")
		msg.StopReason = ai.StopReasonError
		msg.ErrorMessage = "provider exploded"
		return orchestrator.PromptResult{Message: msg}, nil
	}

	var out bytes.Buffer
	err := run(context.Background(), runner, []string{"prompt"}, &out)
	// pi writes errorMessage to stderr and exits 1; the message text never
	// reaches stdout (print-mode.ts:132-136).
	if err == nil || err.Error() != "provider exploded" {
		t.Fatalf("err = %v, want provider exploded", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

func TestRunAbortedStopReasonUsesFallbackMessage(t *testing.T) {
	runner := &fakeRunner{}
	runner.promptFunc = func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		msg := assistantMessage("")
		msg.StopReason = ai.StopReasonAborted
		return orchestrator.PromptResult{Message: msg}, nil
	}

	var out bytes.Buffer
	err := run(context.Background(), runner, []string{"prompt"}, &out)
	if err == nil || err.Error() != "Request aborted" {
		t.Fatalf("err = %v, want Request aborted", err)
	}
}

func TestRunReturnsPromptError(t *testing.T) {
	want := errors.New("prompt failed")
	runner := &fakeRunner{}
	runner.promptFunc = func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		return orchestrator.PromptResult{Message: assistantMessage("final")}, want
	}

	var out bytes.Buffer
	err := run(context.Background(), runner, []string{"prompt"}, &out)
	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got := out.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
}

func TestRunIgnoresNonAssistantResult(t *testing.T) {
	runner := &fakeRunner{}
	runner.promptFunc = func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		return orchestrator.PromptResult{Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "user text"}},
		}}, nil
	}

	var out bytes.Buffer
	if err := run(context.Background(), runner, []string{"prompt"}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

type fakeRunner struct {
	promptFunc func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error)
}

func (f *fakeRunner) Prompt(
	ctx context.Context,
	input orchestrator.PromptInput,
) (orchestrator.PromptResult, error) {
	return f.promptFunc(ctx, input)
}

func assistantMessage(text string) ai.Message {
	msg := ai.Message{Role: ai.RoleAssistant}
	if text != "" {
		msg.Content = []ai.ContentBlock{{Type: ai.ContentText, Text: text}}
	}
	return msg
}
