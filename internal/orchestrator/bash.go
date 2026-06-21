package orchestrator

import (
	"context"
	"errors"

	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
)

type bashAbortHandle struct {
	cancel context.CancelFunc
}

type userBashDecision struct {
	Result     *BashResult
	Operations hook.BashOperations
}

type bashOperationsEnv struct {
	execenv.FileSystem

	operations hook.BashOperations
	cwd        string

	exitCodeSet bool
	exitCode    *int
}

func (o *Orchestrator) ExecuteBash(ctx context.Context, command string) (BashResult, error) {
	decision, err := o.emitUserBash(ctx, command)
	if err != nil {
		return BashResult{}, err
	}
	if decision.Result != nil {
		return *decision.Result, nil
	}

	o.mu.Lock()
	execEnv := o.execEnv
	o.mu.Unlock()

	var runEnv execenv.ExecutionEnv
	var operationsEnv *bashOperationsEnv
	if decision.Operations != nil {
		operationsEnv = newBashOperationsEnv(
			execEnv,
			decision.Operations,
			o.bashCWD(),
		)
		runEnv = operationsEnv
	} else if execEnv != nil {
		runEnv = execEnv
	} else {
		return BashResult{}, errors.New("bash execution is not available")
	}

	// pi creates one AbortController for executeBash and clears it in finally:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2535-2562.
	runCtx, cancel := context.WithCancel(ctx)
	handle := &bashAbortHandle{cancel: cancel}
	o.mu.Lock()
	o.bashAbort = handle
	o.mu.Unlock()
	defer func() {
		o.clearBashAbort(handle)
		cancel()
	}()

	// pi applies the shell command prefix on its own line before handing the
	// command to operations (agent-session.ts:2543-2545).
	if prefix := o.shellCommandPrefix(); prefix != "" {
		command = prefix + "\n" + command
	}
	run := builtin.RunBash(runCtx, runEnv, command)
	if operationsEnv != nil {
		operationsEnv.applyExitCode(&run)
	}
	return BashResult{
		Output:         run.Output,
		ExitCode:       run.ExitCode,
		Cancelled:      run.Cancelled,
		Truncated:      run.Truncated,
		FullOutputPath: run.FullOutputPath,
	}, nil
}

// shellCommandPrefix reads the live setting, mirroring pi's
// settingsManager.getShellCommandPrefix() (agent-session.ts:2543).
func (o *Orchestrator) shellCommandPrefix() string {
	o.mu.Lock()
	manager := o.settingsManager
	settings := o.settings
	o.mu.Unlock()
	if manager != nil {
		settings = manager.Settings()
	}
	if value, ok := settings["shellCommandPrefix"].(string); ok {
		return value
	}
	return ""
}

func (o *Orchestrator) emitUserBash(ctx context.Context, command string) (userBashDecision, error) {
	registry := o.hookRegistry()
	if registry == nil || !registry.HasHandlers(hook.EventUserBash) {
		return userBashDecision{}, nil
	}

	// pi returns the FIRST extension's user_bash result and skips the rest,
	// so two interceptors never both execute the command (runner.ts:829-856).
	result, err := registry.EmitFirst(ctx, hook.UserBashEvent{
		Type:    hook.EventUserBash,
		Command: command,
		// pi's !! prefix maps to excludeFromContext. along RPC bash has no !!
		// prefix concept, so it always emits false.
		ExcludeFromContext: false,
		CWD:                o.bashCWD(),
	})
	if err != nil {
		return userBashDecision{}, err
	}

	switch r := result.(type) {
	case hook.UserBashEventResult:
		return userBashDecision{
			Result:     r.Result,
			Operations: r.Operations,
		}, nil
	case *hook.UserBashEventResult:
		if r == nil {
			return userBashDecision{}, nil
		}
		return userBashDecision{
			Result:     r.Result,
			Operations: r.Operations,
		}, nil
	default:
		return userBashDecision{}, nil
	}
}

func newBashOperationsEnv(
	fs execenv.FileSystem,
	operations hook.BashOperations,
	cwd string,
) *bashOperationsEnv {
	if fs == nil {
		fs = local.New(cwd)
	}
	return &bashOperationsEnv{
		FileSystem: fs,
		operations: operations,
		cwd:        cwd,
	}
}

func (e *bashOperationsEnv) Cwd() string {
	if e.cwd != "" {
		return e.cwd
	}
	return e.FileSystem.Cwd()
}

func (e *bashOperationsEnv) Exec(
	ctx context.Context,
	command string,
	opts execenv.ExecOptions,
) (execenv.ExecResult, error) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = e.Cwd()
	}

	// operations delivers combined output through a single OnData callback. Route
	// it to whichever output sink/callback the caller wired, honoring the same
	// ExecOptions contract as the local env (sink owns retention; callback streams).
	var onData func(string)
	if opts.Stdout != nil || opts.Stderr != nil || opts.OnStdout != nil || opts.OnStderr != nil {
		onData = func(chunk string) {
			if opts.Stdout != nil {
				_, _ = opts.Stdout.Write([]byte(chunk))
			} else if opts.Stderr != nil {
				_, _ = opts.Stderr.Write([]byte(chunk))
			}
			if opts.OnStdout != nil {
				opts.OnStdout(chunk)
			} else if opts.OnStderr != nil {
				opts.OnStderr(chunk)
			}
		}
	}

	result, err := e.operations.Exec(ctx, command, cwd, hook.BashExecOptions{
		OnData:  onData,
		Timeout: opts.Timeout,
		Env:     opts.Env,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return execenv.ExecResult{}, bashExecutionAborted(ctxErr)
		}
		return execenv.ExecResult{}, err
	}

	e.exitCodeSet = true
	e.exitCode = result.ExitCode
	if ctxErr := ctx.Err(); ctxErr != nil {
		return execenv.ExecResult{}, bashExecutionAborted(ctxErr)
	}
	if result.ExitCode == nil {
		return execenv.ExecResult{}, nil
	}
	return execenv.ExecResult{ExitCode: *result.ExitCode}, nil
}

func (e *bashOperationsEnv) applyExitCode(run *builtin.BashRun) {
	if e.exitCodeSet && e.exitCode == nil {
		run.ExitCode = nil
	}
}

func bashExecutionAborted(err error) error {
	return &execenv.ExecutionError{
		Code: execenv.ExecutionErrorAborted,
		Err:  err,
	}
}

func (o *Orchestrator) bashCWD() string {
	o.mu.Lock()
	cwd := o.cwd
	execEnv := o.execEnv
	current := o.session
	o.mu.Unlock()

	if execEnv != nil {
		return execEnv.Cwd()
	}
	if cwd != "" {
		return cwd
	}
	if current != nil {
		return current.GetMetadata().CWD
	}
	return ""
}

func (o *Orchestrator) AbortBash() {
	// pi abort_bash delegates to abortBash, which aborts the active controller:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2598-2600.
	o.mu.Lock()
	handle := o.bashAbort
	if handle != nil {
		handle.cancel()
	}
	o.mu.Unlock()
}

func (o *Orchestrator) clearBashAbort(handle *bashAbortHandle) {
	o.mu.Lock()
	if o.bashAbort == handle {
		o.bashAbort = nil
	}
	o.mu.Unlock()
}
