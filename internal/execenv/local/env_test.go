package local_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
)

func TestEnvPathResolution(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	env := local.New(root)

	got, err := env.AbsolutePath(ctx, filepath.Join("dir", "..", "file.txt"))
	if err != nil {
		t.Fatalf("AbsolutePath returned error: %v", err)
	}
	want := filepath.Join(root, "file.txt")
	if got != want {
		t.Fatalf("AbsolutePath relative = %q, want %q", got, want)
	}

	absolute := filepath.Join(root, "abs.txt")
	got, err = env.AbsolutePath(ctx, absolute)
	if err != nil {
		t.Fatalf("AbsolutePath absolute returned error: %v", err)
	}
	if got != absolute {
		t.Fatalf("AbsolutePath absolute = %q, want %q", got, absolute)
	}

	got, err = env.JoinPath(ctx, []string{root, "child", "file.txt"})
	if err != nil {
		t.Fatalf("JoinPath returned error: %v", err)
	}
	want = filepath.Join(root, "child", "file.txt")
	if got != want {
		t.Fatalf("JoinPath = %q, want %q", got, want)
	}
}

func TestEnvFileSystemOperations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	env := local.New(root)

	if err := env.WriteFile(ctx, filepath.Join("dir", "file.txt"), []byte("one\ntwo\nthree\n")); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := env.AppendFile(ctx, filepath.Join("dir", "file.txt"), []byte("four\n")); err != nil {
		t.Fatalf("AppendFile returned error: %v", err)
	}

	text, err := env.ReadTextFile(ctx, filepath.Join("dir", "file.txt"))
	if err != nil {
		t.Fatalf("ReadTextFile returned error: %v", err)
	}
	if text != "one\ntwo\nthree\nfour\n" {
		t.Fatalf("ReadTextFile = %q", text)
	}

	binary, err := env.ReadBinaryFile(ctx, filepath.Join("dir", "file.txt"))
	if err != nil {
		t.Fatalf("ReadBinaryFile returned error: %v", err)
	}
	if string(binary) != text {
		t.Fatalf("ReadBinaryFile = %q, want %q", string(binary), text)
	}

	lines, err := env.ReadTextLines(ctx, filepath.Join("dir", "file.txt"), 2)
	if err != nil {
		t.Fatalf("ReadTextLines returned error: %v", err)
	}
	if !slices.Equal(lines, []string{"one", "two"}) {
		t.Fatalf("ReadTextLines = %#v, want first two lines", lines)
	}

	info, err := env.FileInfo(ctx, filepath.Join("dir", "file.txt"))
	if err != nil {
		t.Fatalf("FileInfo returned error: %v", err)
	}
	if info.Name != "file.txt" || info.Path != filepath.Join(root, "dir", "file.txt") || info.IsDir {
		t.Fatalf("FileInfo returned unexpected metadata: %#v", info)
	}
	if info.Size <= 0 || info.MtimeMs <= 0 {
		t.Fatalf("FileInfo size/mtime not populated: %#v", info)
	}

	entries, err := env.ListDir(ctx, "dir")
	if err != nil {
		t.Fatalf("ListDir returned error: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "file.txt" {
		t.Fatalf("ListDir = %#v, want file.txt", entries)
	}

	canonical, err := env.CanonicalPath(ctx, filepath.Join("dir", "file.txt"))
	if err != nil {
		t.Fatalf("CanonicalPath returned error: %v", err)
	}
	if canonical != filepath.Join(root, "dir", "file.txt") {
		t.Fatalf("CanonicalPath = %q", canonical)
	}

	exists, err := env.Exists(ctx, "missing.txt")
	if err != nil {
		t.Fatalf("Exists missing returned error: %v", err)
	}
	if exists {
		t.Fatal("Exists missing = true, want false")
	}

	if err := env.CreateDir(ctx, filepath.Join("made", "nested"), true); err != nil {
		t.Fatalf("CreateDir returned error: %v", err)
	}
	exists, err = env.Exists(ctx, filepath.Join("made", "nested"))
	if err != nil {
		t.Fatalf("Exists created dir returned error: %v", err)
	}
	if !exists {
		t.Fatal("created directory does not exist")
	}

	tempDir, err := env.CreateTempDir(ctx, "along-test-")
	if err != nil {
		t.Fatalf("CreateTempDir returned error: %v", err)
	}
	defer os.RemoveAll(tempDir)
	tempInfo, err := os.Stat(tempDir)
	if err != nil {
		t.Fatalf("stat temp dir: %v", err)
	}
	if !tempInfo.IsDir() {
		t.Fatalf("temp dir path is not a directory: %s", tempDir)
	}

	tempFile, err := env.CreateTempFile(ctx, "along-test-", ".txt")
	if err != nil {
		t.Fatalf("CreateTempFile returned error: %v", err)
	}
	defer os.Remove(tempFile)
	tempInfo, err = os.Stat(tempFile)
	if err != nil {
		t.Fatalf("stat temp file: %v", err)
	}
	if tempInfo.IsDir() {
		t.Fatalf("temp file path is a directory: %s", tempFile)
	}

	if err := env.Remove(ctx, filepath.Join("dir", "file.txt"), false, false); err != nil {
		t.Fatalf("Remove file returned error: %v", err)
	}
	exists, err = env.Exists(ctx, filepath.Join("dir", "file.txt"))
	if err != nil {
		t.Fatalf("Exists removed file returned error: %v", err)
	}
	if exists {
		t.Fatal("removed file still exists")
	}

	if err := env.Remove(ctx, "made", true, false); err != nil {
		t.Fatalf("Remove directory returned error: %v", err)
	}
	exists, err = env.Exists(ctx, "made")
	if err != nil {
		t.Fatalf("Exists removed directory returned error: %v", err)
	}
	if exists {
		t.Fatal("removed directory still exists")
	}

	if err := env.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}
	if _, err := os.Stat(tempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp dir after Cleanup stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(tempFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file after Cleanup stat error = %v, want not exist", err)
	}
}

func TestEnvReadMissingFileErrorCode(t *testing.T) {
	t.Parallel()

	env := local.New(t.TempDir())
	_, err := env.ReadTextFile(context.Background(), "missing.txt")
	if err == nil {
		t.Fatal("ReadTextFile missing returned nil error")
	}

	var fileErr *execenv.FileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("ReadTextFile missing error type = %T, want *execenv.FileError", err)
	}
	if fileErr.Code != execenv.FileErrorNotFound {
		t.Fatalf("ReadTextFile missing code = %q, want %q", fileErr.Code, execenv.FileErrorNotFound)
	}
}

func TestEnvExecSuccess(t *testing.T) {
	t.Parallel()

	env := local.New(t.TempDir())
	result, err := env.Exec(context.Background(), stdoutCommand(), execenv.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec success returned error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("Exec success exit code = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Fatalf("Exec success stdout = %q, want hello", result.Stdout)
	}
}

func TestEnvExecUsesCustomShell(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("custom bash shell test uses a POSIX shell path")
	}
	const shellPath = "/bin/bash"
	if _, err := os.Stat(shellPath); err != nil {
		t.Skipf("%s is absent: %v", shellPath, err)
	}

	env := local.New(t.TempDir(), local.WithShell(shellPath))
	result, err := env.Exec(
		context.Background(),
		`printf '%s' "${BASH_VERSION:+bash}"`,
		execenv.ExecOptions{},
	)
	if err != nil {
		t.Fatalf("Exec with custom shell returned error: %v", err)
	}
	if result.Stdout != "bash" {
		t.Fatalf("Exec custom shell stdout = %q, want bash", result.Stdout)
	}
}

func TestEnvExecNonZeroExitIsResult(t *testing.T) {
	t.Parallel()

	env := local.New(t.TempDir())
	result, err := env.Exec(context.Background(), nonZeroCommand(), execenv.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec non-zero returned error: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("Exec non-zero exit code = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "failed") {
		t.Fatalf("Exec non-zero stderr = %q, want failed", result.Stderr)
	}
}

func TestEnvExecTimeout(t *testing.T) {
	t.Parallel()

	env := local.New(t.TempDir())
	_, err := env.Exec(
		context.Background(),
		sleepCommand(),
		execenv.ExecOptions{Timeout: 50 * time.Millisecond},
	)
	assertExecutionCode(t, err, execenv.ExecutionErrorTimeout)
}

func TestEnvExecContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	env := local.New(t.TempDir())
	_, err := env.Exec(ctx, sleepCommand(), execenv.ExecOptions{})
	assertExecutionCode(t, err, execenv.ExecutionErrorAborted)
}

func TestEnvExecStreamsStdoutChunks(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	chunks := []string{}
	env := local.New(t.TempDir())

	result, err := env.Exec(
		context.Background(),
		streamingCommand(),
		execenv.ExecOptions{
			OnStdout: func(chunk string) {
				mu.Lock()
				defer mu.Unlock()
				chunks = append(chunks, chunk)
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec streaming returned error: %v", err)
	}
	if !strings.Contains(result.Stdout, "first") || !strings.Contains(result.Stdout, "second") {
		t.Fatalf("Exec streaming stdout = %q, want first and second", result.Stdout)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) == 0 {
		t.Fatal("OnStdout was not called")
	}
	if strings.Join(chunks, "") != result.Stdout {
		t.Fatalf("OnStdout chunks = %#v, result stdout = %q", chunks, result.Stdout)
	}
}

func TestEnvExecSinkOwnsOutputWhileStreaming(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	chunks := []string{}
	var sink bytes.Buffer
	env := local.New(t.TempDir())

	result, err := env.Exec(
		context.Background(),
		streamingCommand(),
		execenv.ExecOptions{
			Stdout: &sink,
			OnStdout: func(chunk string) {
				mu.Lock()
				defer mu.Unlock()
				chunks = append(chunks, chunk)
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec with sink returned error: %v", err)
	}
	// The sink owns retention, so ExecResult is left empty.
	if result.Stdout != "" || result.Stderr != "" {
		t.Fatalf("Exec sink result = %#v, want empty stdout/stderr", result)
	}
	if got := sink.String(); !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("sink = %q, want streamed output retained", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(chunks, ""); !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("OnStdout chunks = %#v, want streamed output", chunks)
	}
}

func TestEnvExecStreamsCompleteUTF8Runes(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("split UTF-8 fixture uses POSIX printf byte escapes")
	}

	var mu sync.Mutex
	chunks := []string{}
	env := local.New(t.TempDir())

	result, err := env.Exec(
		context.Background(),
		splitUTF8Command(),
		execenv.ExecOptions{
			OnStdout: func(chunk string) {
				mu.Lock()
				defer mu.Unlock()
				chunks = append(chunks, chunk)
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec split UTF-8 returned error: %v", err)
	}
	if result.Stdout != "你\n" {
		t.Fatalf("Exec split UTF-8 stdout = %q, want complete rune", result.Stdout)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(chunks, ""); got != "你\n" {
		t.Fatalf("OnStdout chunks = %#v, joined = %q, want complete rune", chunks, got)
	}
	for _, chunk := range chunks {
		if strings.ContainsRune(chunk, '\ufffd') {
			t.Fatalf("OnStdout chunk = %q, contains replacement rune", chunk)
		}
	}
}

func assertExecutionCode(t *testing.T, err error, want execenv.ExecutionErrorCode) {
	t.Helper()

	if err == nil {
		t.Fatalf("Exec error = nil, want code %q", want)
	}
	var execErr *execenv.ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("Exec error type = %T, want *execenv.ExecutionError", err)
	}
	if execErr.Code != want {
		t.Fatalf("Exec error code = %q, want %q", execErr.Code, want)
	}
}

func stdoutCommand() string {
	if runtime.GOOS == "windows" {
		return "echo hello"
	}
	return "printf hello"
}

func nonZeroCommand() string {
	if runtime.GOOS == "windows" {
		return "echo failed 1>&2 & exit /b 7"
	}
	return "printf failed >&2; exit 7"
}

func sleepCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 3 127.0.0.1 > nul"
	}
	return "sleep 2"
}

func streamingCommand() string {
	if runtime.GOOS == "windows" {
		return "echo first & echo second"
	}
	return "printf first; sleep 0.05; printf second"
}

func splitUTF8Command() string {
	return "printf '\\344'; sleep 0.05; printf '\\275\\240\\n'"
}
