package local

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/cunninghamcard-bit/Attention/internal/execenv"
)

var _ execenv.ExecutionEnv = (*Env)(nil)

type Env struct {
	cwd string

	shellPath string

	mu        sync.Mutex
	tempPaths []string
}

// Option customizes a local execution environment.
type Option func(*Env)

// WithShell configures the shell path used by Exec.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2542-2551.
func WithShell(shellPath string) Option {
	return func(e *Env) {
		e.shellPath = shellPath
	}
}

func New(cwd string, opts ...Option) *Env {
	resolved := cwd
	if resolved == "" {
		if wd, err := os.Getwd(); err == nil {
			resolved = wd
		} else {
			resolved = "."
		}
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	env := &Env{
		cwd:       filepath.Clean(resolved),
		tempPaths: []string{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(env)
		}
	}
	return env
}

func (e *Env) Cwd() string {
	return e.cwd
}

func (e *Env) AbsolutePath(ctx context.Context, path string) (string, error) {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func (e *Env) JoinPath(ctx context.Context, parts []string) (string, error) {
	joined := filepath.Join(parts...)
	if err := fileContextError(ctx, joined); err != nil {
		return "", err
	}
	return joined, nil
}

func (e *Env) ReadTextFile(ctx context.Context, path string) (string, error) {
	data, err := e.ReadBinaryFile(ctx, path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (e *Env) ReadTextLines(ctx context.Context, path string, maxLines int) ([]string, error) {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return nil, err
	}
	if maxLines <= 0 {
		return []string{}, nil
	}

	file, err := os.Open(resolved)
	if err != nil {
		return nil, mapFileError(err, resolved)
	}

	lines, readErr := readLines(ctx, file, resolved, maxLines)
	closeErr := file.Close()
	if readErr != nil {
		return nil, mapFileError(readErr, resolved)
	}
	if closeErr != nil {
		return nil, mapFileError(closeErr, resolved)
	}
	return lines, nil
}

func (e *Env) ReadBinaryFile(ctx context.Context, path string) ([]byte, error) {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, mapFileError(err, resolved)
	}
	return data, nil
}

func (e *Env) WriteFile(ctx context.Context, path string, content []byte) error {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return mapFileError(err, resolved)
	}
	if err := fileContextError(ctx, resolved); err != nil {
		return err
	}
	if err := os.WriteFile(resolved, content, 0o644); err != nil {
		return mapFileError(err, resolved)
	}
	return nil
}

func (e *Env) AppendFile(ctx context.Context, path string, content []byte) error {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return mapFileError(err, resolved)
	}
	if err := fileContextError(ctx, resolved); err != nil {
		return err
	}

	file, err := os.OpenFile(resolved, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return mapFileError(err, resolved)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return mapFileError(err, resolved)
	}
	if err := file.Close(); err != nil {
		return mapFileError(err, resolved)
	}
	return nil
}

func (e *Env) FileInfo(ctx context.Context, path string) (execenv.FileInfo, error) {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return execenv.FileInfo{}, err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return execenv.FileInfo{}, mapFileError(err, resolved)
	}
	return fileInfo(resolved, info), nil
}

func (e *Env) ListDir(ctx context.Context, path string) ([]execenv.FileInfo, error) {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, mapFileError(err, resolved)
	}

	infos := make([]execenv.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if err := fileContextError(ctx, resolved); err != nil {
			return nil, err
		}
		entryPath := filepath.Join(resolved, entry.Name())
		// pi stats each entry following symlinks and skips entries that
		// cannot be stat'd — broken symlinks, permission holes, races — so a
		// single bad entry never fails the whole listing, and a symlink to a
		// directory gets the directory suffix (tools/ls.ts:165-173).
		info, err := os.Stat(entryPath)
		if err != nil {
			continue
		}
		infos = append(infos, fileInfo(entryPath, info))
	}
	return infos, nil
}

func (e *Env) CanonicalPath(ctx context.Context, path string) (string, error) {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", mapFileError(err, resolved)
	}
	return canonical, nil
}

func (e *Env) Exists(ctx context.Context, path string) (bool, error) {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return false, err
	}
	if _, err := os.Lstat(resolved); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, mapFileError(err, resolved)
	}
	return true, nil
}

func (e *Env) CreateDir(ctx context.Context, path string, recursive bool) error {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return err
	}
	var err error
	if recursive {
		err = os.MkdirAll(resolved, 0o755)
	} else {
		err = os.Mkdir(resolved, 0o755)
	}
	if err != nil {
		return mapFileError(err, resolved)
	}
	return nil
}

func (e *Env) Remove(ctx context.Context, path string, recursive, force bool) error {
	resolved := e.resolvePath(path)
	if err := fileContextError(ctx, resolved); err != nil {
		return err
	}
	if !force {
		if _, err := os.Lstat(resolved); err != nil {
			return mapFileError(err, resolved)
		}
	}

	var err error
	if recursive {
		err = os.RemoveAll(resolved)
	} else {
		err = os.Remove(resolved)
	}
	if err != nil {
		if force && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return mapFileError(err, resolved)
	}
	return nil
}

func (e *Env) CreateTempDir(ctx context.Context, prefix string) (string, error) {
	if err := fileContextError(ctx, ""); err != nil {
		return "", err
	}
	path, err := os.MkdirTemp("", prefix+"*")
	if err != nil {
		return "", mapFileError(err, "")
	}
	e.trackTemp(path)
	return path, nil
}

func (e *Env) CreateTempFile(ctx context.Context, prefix, suffix string) (string, error) {
	if err := fileContextError(ctx, ""); err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", prefix+"*"+suffix)
	if err != nil {
		return "", mapFileError(err, "")
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", mapFileError(err, path)
	}
	e.trackTemp(path)
	return path, nil
}

func (e *Env) Exec(
	ctx context.Context,
	command string,
	opts execenv.ExecOptions,
) (execenv.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return execenv.ExecResult{}, executionError(execenv.ExecutionErrorAborted, err)
	}

	shell, args, err := shellConfig(e.shellPath)
	if err != nil {
		return execenv.ExecResult{}, executionError(execenv.ExecutionErrorShellUnavailable, err)
	}
	resolvedCommand := command

	runCtx := ctx
	cancelTimeout := func() {}
	if opts.Timeout > 0 {
		runCtx, cancelTimeout = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancelTimeout()

	runCtx, cancelRun := context.WithCancel(runCtx)
	defer cancelRun()

	cmd := exec.CommandContext(runCtx, shell, append(args, resolvedCommand)...)
	cmd.Dir = e.cwd
	if opts.Cwd != "" {
		cmd.Dir = e.resolvePath(opts.Cwd)
	}
	cmd.Env = mergedEnv(opts.Env)
	configureProcessTreeCancel(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return execenv.ExecResult{}, executionError(execenv.ExecutionErrorSpawnError, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return execenv.ExecResult{}, executionError(execenv.ExecutionErrorSpawnError, err)
	}

	if err := cmd.Start(); err != nil {
		return execenv.ExecResult{}, mapStartError(ctx, runCtx, opts, err)
	}

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	// Buffering into ExecResult is just the default sink; a caller can supply its
	// own sink to own retention, in which case the ExecResult field stays empty.
	var stdoutSink io.Writer = &stdoutBuf
	var stderrSink io.Writer = &stderrBuf
	if opts.Stdout != nil {
		stdoutSink = opts.Stdout
	}
	if opts.Stderr != nil {
		stderrSink = opts.Stderr
	}
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var callbackErr error
	var readErr error

	setCallbackErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if callbackErr == nil {
			callbackErr = err
		}
	}
	setReadErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if readErr == nil {
			readErr = err
		}
	}

	wg.Add(2)
	go readOutput(stdout, stdoutSink, opts.OnStdout, "stdout", cancelRun, setCallbackErr, setReadErr, &wg)
	go readOutput(stderr, stderrSink, opts.OnStderr, "stderr", cancelRun, setCallbackErr, setReadErr, &wg)

	// Drain both pipes to completion BEFORE Wait: cmd.Wait closes the
	// StdoutPipe/StderrPipe once the process exits, so calling it while a
	// reader is still draining is the documented anti-pattern. Reading first
	// removes the race at the source (no os.ErrClosed to swallow).
	wg.Wait()
	waitErr := cmd.Wait()

	errMu.Lock()
	callbackFailure := callbackErr
	readFailure := readErr
	errMu.Unlock()

	if callbackFailure != nil {
		return execenv.ExecResult{}, executionError(execenv.ExecutionErrorCallbackError, callbackFailure)
	}

	result := execenv.ExecResult{
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
		ExitCode: exitCode(cmd),
	}

	// Error paths return the result alongside the error: pi's execCommand
	// always resolves with whatever stdout/stderr was captured before the
	// kill (exec.ts:91-105), so buffered callers keep partial output.
	if waitErr != nil {
		if ctx.Err() != nil {
			return result, executionError(execenv.ExecutionErrorAborted, ctx.Err())
		}
		if opts.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return result, executionError(execenv.ExecutionErrorTimeout, runCtx.Err())
		}

		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, executionError(execenv.ExecutionErrorUnknown, waitErr)
	}

	if readFailure != nil {
		return execenv.ExecResult{}, executionError(execenv.ExecutionErrorUnknown, readFailure)
	}
	return result, nil
}

func (e *Env) Cleanup(ctx context.Context) error {
	e.mu.Lock()
	paths := append([]string(nil), e.tempPaths...)
	e.tempPaths = []string{}
	e.mu.Unlock()

	for _, path := range paths {
		if ctx.Err() != nil {
			return nil
		}
		_ = os.RemoveAll(path)
	}
	return nil
}

func (e *Env) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(e.cwd, path))
}

func (e *Env) trackTemp(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tempPaths = append(e.tempPaths, path)
}

func readLines(ctx context.Context, reader io.Reader, path string, maxLines int) ([]string, error) {
	lines := []string{}
	buffered := bufio.NewReader(reader)
	for len(lines) < maxLines {
		if err := fileContextError(ctx, path); err != nil {
			return nil, err
		}

		line, err := buffered.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			lines = append(lines, line)
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		return nil, err
	}
	return lines, nil
}

func fileInfo(path string, info os.FileInfo) execenv.FileInfo {
	return execenv.FileInfo{
		Name:    filepath.Base(filepath.Clean(path)),
		Path:    filepath.Clean(path),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		MtimeMs: info.ModTime().UnixMilli(),
	}
}

func fileContextError(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return &execenv.FileError{
			Code: execenv.FileErrorAborted,
			Path: path,
			Err:  err,
		}
	}
	return nil
}

func mapFileError(err error, path string) error {
	if err == nil {
		return nil
	}
	if fileErr, ok := errors.AsType[*execenv.FileError](err); ok {
		return fileErr
	}
	if path == "" {
		if pathErr, ok := errors.AsType[*os.PathError](err); ok {
			path = pathErr.Path
		}
	}

	code := execenv.FileErrorUnknown
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = execenv.FileErrorAborted
	case errors.Is(err, os.ErrNotExist):
		code = execenv.FileErrorNotFound
	case errors.Is(err, os.ErrPermission):
		code = execenv.FileErrorPermissionDenied
	case errors.Is(err, syscall.ENOTDIR):
		code = execenv.FileErrorNotDirectory
	case errors.Is(err, syscall.EISDIR):
		code = execenv.FileErrorIsDirectory
	case errors.Is(err, os.ErrInvalid), errors.Is(err, syscall.EINVAL), errors.Is(err, os.ErrExist):
		code = execenv.FileErrorInvalid
	case errors.Is(err, syscall.EOPNOTSUPP):
		code = execenv.FileErrorNotSupported
	}

	return &execenv.FileError{
		Code: code,
		Path: path,
		Err:  err,
	}
}

// shellConfig mirrors pi's getShellConfig resolution order: the explicit
// shellPath, on Windows Git Bash then bash.exe on PATH (loud error when
// absent), on Unix /bin/bash then bash on PATH then sh
// (utils/shell.ts:57-110). The "bash" tool advertises bash semantics, so sh
// (dash on Debian) is a last resort, never the default.
func shellConfig(shellPath string) (string, []string, error) {
	if shellPath != "" {
		if _, err := os.Stat(shellPath); err != nil {
			return "", nil, fmt.Errorf("custom shell path not found: %s", shellPath)
		}
		return shellPath, []string{"-c"}, nil
	}

	if runtime.GOOS == "windows" {
		paths := []string{}
		if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
			paths = append(paths, filepath.Join(programFiles, "Git", "bin", "bash.exe"))
		}
		if programFilesX86 := os.Getenv("ProgramFiles(x86)"); programFilesX86 != "" {
			paths = append(paths, filepath.Join(programFilesX86, "Git", "bin", "bash.exe"))
		}
		for _, path := range paths {
			if _, err := os.Stat(path); err == nil {
				return path, []string{"-c"}, nil
			}
		}
		if path, err := exec.LookPath("bash.exe"); err == nil {
			return path, []string{"-c"}, nil
		}
		searched := make([]string, 0, len(paths))
		for _, path := range paths {
			searched = append(searched, "  "+path)
		}
		return "", nil, fmt.Errorf("no bash shell found. Options:\n"+
			"  1. Install Git for Windows: https://git-scm.com/download/win\n"+
			"  2. Add your bash to PATH (Cygwin, MSYS2, etc.)\n"+
			"  3. Set shellPath in settings.json\n\n"+
			"Searched Git Bash in:\n%s", strings.Join(searched, "\n"))
	}

	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash", []string{"-c"}, nil
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return path, []string{"-c"}, nil
	}
	return "sh", []string{"-c"}, nil
}

func mergedEnv(overrides map[string]string) []string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	maps.Copy(env, overrides)

	merged := make([]string, 0, len(env))
	for key, value := range env {
		merged = append(merged, key+"="+value)
	}
	return merged
}

func readOutput(
	reader io.Reader,
	sink io.Writer,
	callback func(string),
	name string,
	cancel context.CancelFunc,
	setCallbackErr func(error),
	setReadErr func(error),
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	decoder := utf8StreamDecoder{}
	chunk := make([]byte, 32*1024)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			text := decoder.decode(chunk[:n], false)
			if !emitOutputText(text, sink, callback, name, cancel, setCallbackErr) {
				return
			}
		}
		if err == nil {
			continue
		}
		text := decoder.decode(nil, true)
		if !emitOutputText(text, sink, callback, name, cancel, setCallbackErr) {
			return
		}
		if !errors.Is(err, io.EOF) {
			setReadErr(err)
		}
		return
	}
}

type utf8StreamDecoder struct {
	pending []byte
}

func (d *utf8StreamDecoder) decode(chunk []byte, final bool) string {
	data := chunk
	if len(d.pending) > 0 {
		combined := make([]byte, 0, len(d.pending)+len(chunk))
		combined = append(combined, d.pending...)
		combined = append(combined, chunk...)
		d.pending = d.pending[:0]
		data = combined
	}
	if len(data) == 0 {
		return ""
	}

	if !final {
		prefixLen := utf8CompletePrefixLen(data)
		if prefixLen < len(data) {
			d.pending = append(d.pending[:0], data[prefixLen:]...)
			data = data[:prefixLen]
		}
	}
	return string(data)
}

func utf8CompletePrefixLen(data []byte) int {
	start := max(0, len(data)-utf8.UTFMax)
	for i := len(data) - 1; i >= start; i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.FullRune(data[i:]) {
			return i
		}
		return len(data)
	}
	return len(data)
}

func emitOutputText(
	text string,
	sink io.Writer,
	callback func(string),
	name string,
	cancel context.CancelFunc,
	setCallbackErr func(error),
) bool {
	if text == "" {
		return true
	}
	_, _ = sink.Write([]byte(text))
	if callback == nil {
		return true
	}
	if err := callCallback(name, callback, text); err != nil {
		setCallbackErr(err)
		cancel()
		return false
	}
	return true
}

func callCallback(name string, callback func(string), chunk string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s callback panic: %w", name, panicError(recovered))
		}
	}()

	callback(chunk)
	return nil
}

func panicError(value any) error {
	if err, ok := value.(error); ok {
		return err
	}
	return fmt.Errorf("%v", value)
}

func mapStartError(
	ctx context.Context,
	runCtx context.Context,
	opts execenv.ExecOptions,
	err error,
) error {
	if ctx.Err() != nil {
		return executionError(execenv.ExecutionErrorAborted, ctx.Err())
	}
	if opts.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return executionError(execenv.ExecutionErrorTimeout, runCtx.Err())
	}
	return executionError(execenv.ExecutionErrorSpawnError, err)
}

func executionError(code execenv.ExecutionErrorCode, err error) error {
	return &execenv.ExecutionError{
		Code: code,
		Err:  err,
	}
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 0
	}
	return cmd.ProcessState.ExitCode()
}
