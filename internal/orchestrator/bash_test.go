package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type fakeBashOperations struct {
	exec func(
		ctx context.Context,
		command string,
		cwd string,
		opts hook.BashExecOptions,
	) (hook.BashOpResult, error)
}

func (f fakeBashOperations) Exec(
	ctx context.Context,
	command string,
	cwd string,
	opts hook.BashExecOptions,
) (hook.BashOpResult, error) {
	if f.exec == nil {
		return hook.BashOpResult{}, nil
	}
	return f.exec(ctx, command, cwd, opts)
}

func TestExecuteBashNilEnvErrors(t *testing.T) {
	t.Parallel()

	o, _ := newTestOrchestrator(t, nil)

	_, err := o.ExecuteBash(context.Background(), "printf 'hello\\n'")
	if err == nil {
		t.Fatal("ExecuteBash error = nil, want unavailable error")
	}
	if !strings.Contains(err.Error(), "bash execution is not available") {
		t.Fatalf("ExecuteBash error = %v, want unavailable error", err)
	}
}

func TestExecuteBashUsesUserBashReplacement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	exitCode := 42

	o.hooks.On(hook.EventUserBash, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.UserBashEvent)
		if !ok {
			t.Fatalf("event type = %T, want UserBashEvent", event)
		}
		if e.Command != "definitely-not-a-real-command" {
			t.Fatalf("Command = %q, want definitely-not-a-real-command", e.Command)
		}
		if e.ExcludeFromContext {
			t.Fatal("ExcludeFromContext = true, want false for rpc bash")
		}
		if e.CWD == "" {
			t.Fatal("CWD is empty")
		}
		return hook.UserBashEventResult{
			Result: &hook.BashResult{
				Output:    "from extension\n",
				ExitCode:  &exitCode,
				Cancelled: true,
			},
			Operations: fakeBashOperations{
				exec: func(
					context.Context,
					string,
					string,
					hook.BashExecOptions,
				) (hook.BashOpResult, error) {
					t.Fatal("operations.Exec called for full result replacement")
					return hook.BashOpResult{}, nil
				},
			},
		}, nil
	})

	result, err := o.ExecuteBash(ctx, "definitely-not-a-real-command")
	if err != nil {
		t.Fatalf("ExecuteBash: %v", err)
	}
	if result.Output != "from extension\n" {
		t.Fatalf("Output = %q, want replacement output", result.Output)
	}
	if result.ExitCode == nil || *result.ExitCode != exitCode {
		t.Fatalf("ExitCode = %v, want %d", result.ExitCode, exitCode)
	}
	if !result.Cancelled {
		t.Fatal("Cancelled = false, want replacement value true")
	}
}

func TestExecuteBashUsesUserBashOperations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	exitCode := 7
	var capturedCommand string
	var capturedCWD string

	o.hooks.On(hook.EventUserBash, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.UserBashEvent)
		if !ok {
			t.Fatalf("event type = %T, want UserBashEvent", event)
		}
		if e.Command != "definitely-not-a-real-command" {
			t.Fatalf("Command = %q, want definitely-not-a-real-command", e.Command)
		}
		return hook.UserBashEventResult{
			Operations: fakeBashOperations{
				exec: func(
					_ context.Context,
					command string,
					cwd string,
					opts hook.BashExecOptions,
				) (hook.BashOpResult, error) {
					capturedCommand = command
					capturedCWD = cwd
					if opts.OnData == nil {
						t.Fatal("OnData = nil")
					}
					opts.OnData("from operations\n")
					return hook.BashOpResult{ExitCode: &exitCode}, nil
				},
			},
		}, nil
	})

	result, err := o.ExecuteBash(ctx, "definitely-not-a-real-command")
	if err != nil {
		t.Fatalf("ExecuteBash: %v", err)
	}
	if capturedCommand != "definitely-not-a-real-command" {
		t.Fatalf("operations command = %q, want requested command", capturedCommand)
	}
	if capturedCWD == "" {
		t.Fatal("operations cwd is empty")
	}
	if result.Output != "from operations\n" {
		t.Fatalf("Output = %q, want operations output", result.Output)
	}
	if result.ExitCode == nil || *result.ExitCode != exitCode {
		t.Fatalf("ExitCode = %v, want %d", result.ExitCode, exitCode)
	}
	if result.Cancelled || result.Truncated || result.FullOutputPath != "" {
		t.Fatalf("ExecuteBash result = %+v, want ordinary operations success", result)
	}
}

func TestExecuteBashOperationsAbortCancelsRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	started := make(chan struct{})

	o.hooks.On(hook.EventUserBash, func(_ context.Context, _ any) (any, error) {
		return hook.UserBashEventResult{
			Operations: fakeBashOperations{
				exec: func(
					ctx context.Context,
					_ string,
					_ string,
					opts hook.BashExecOptions,
				) (hook.BashOpResult, error) {
					if opts.OnData != nil {
						opts.OnData("partial\n")
					}
					close(started)
					<-ctx.Done()
					return hook.BashOpResult{}, nil
				},
			},
		}, nil
	})

	type bashOutcome struct {
		result BashResult
		err    error
	}
	outcomes := make(chan bashOutcome, 1)
	go func() {
		result, err := o.ExecuteBash(ctx, "long-running-operation")
		outcomes <- bashOutcome{result: result, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("operations Exec did not start")
	}

	o.AbortBash()

	var outcome bashOutcome
	select {
	case outcome = <-outcomes:
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteBash did not return after AbortBash")
	}
	if outcome.err != nil {
		t.Fatalf("ExecuteBash: %v", outcome.err)
	}
	if outcome.result.Output != "partial\n" {
		t.Fatalf("Output = %q, want partial output", outcome.result.Output)
	}
	if !outcome.result.Cancelled {
		t.Fatal("Cancelled = false, want true after AbortBash")
	}
	if outcome.result.ExitCode != nil {
		t.Fatalf("ExitCode = %v, want nil after cancellation", outcome.result.ExitCode)
	}
}

func TestAbortBashNoopWhenNoBashInProgress(t *testing.T) {
	t.Parallel()

	o, _ := newTestOrchestrator(t, nil)

	o.AbortBash()
	o.AbortBash()

	o.mu.Lock()
	bashAbort := o.bashAbort
	o.mu.Unlock()
	if bashAbort != nil {
		t.Fatal("bashAbort = non-nil, want nil")
	}
}

func TestExecuteBashMapsRunResult(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: env.Cwd()},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		ExecutionEnv:  env,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := o.ExecuteBash(ctx, "printf 'hello\\n'")
	if err != nil {
		t.Fatalf("ExecuteBash: %v", err)
	}
	if result.Output != "hello\n" {
		t.Fatalf("ExecuteBash output = %q, want command output", result.Output)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("ExecuteBash exitCode = %v, want 0", result.ExitCode)
	}
	if result.Cancelled || result.Truncated || result.FullOutputPath != "" {
		t.Fatalf("ExecuteBash result = %+v, want ordinary success", result)
	}

	o.mu.Lock()
	bashAbort := o.bashAbort
	o.mu.Unlock()
	if bashAbort != nil {
		t.Fatal("bashAbort = non-nil after ExecuteBash return")
	}
}
