package execenv

import (
	"context"
	"io"
	"time"
)

type FileInfo struct {
	Name    string
	Path    string
	IsDir   bool
	Size    int64
	MtimeMs int64
}

type FileSystem interface {
	Cwd() string

	AbsolutePath(ctx context.Context, path string) (string, error)
	JoinPath(ctx context.Context, parts []string) (string, error)
	ReadTextFile(ctx context.Context, path string) (string, error)
	ReadTextLines(ctx context.Context, path string, maxLines int) ([]string, error)
	ReadBinaryFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, content []byte) error
	AppendFile(ctx context.Context, path string, content []byte) error
	FileInfo(ctx context.Context, path string) (FileInfo, error)
	ListDir(ctx context.Context, path string) ([]FileInfo, error)
	CanonicalPath(ctx context.Context, path string) (string, error)
	Exists(ctx context.Context, path string) (bool, error)
	CreateDir(ctx context.Context, path string, recursive bool) error
	Remove(ctx context.Context, path string, recursive, force bool) error
	CreateTempDir(ctx context.Context, prefix string) (string, error)
	CreateTempFile(ctx context.Context, prefix, suffix string) (string, error)
	Cleanup(ctx context.Context) error
}

type ExecOptions struct {
	Cwd      string
	Env      map[string]string
	Timeout  time.Duration
	OnStdout func(string)
	OnStderr func(string)
	// Stdout/Stderr, when set, receive decoded output and own its retention; the
	// matching ExecResult field is left empty. When nil, output is buffered into
	// ExecResult as usual. This lets a streaming caller (e.g. bash) accumulate
	// output itself without the env double-buffering it in memory.
	Stdout io.Writer
	Stderr io.Writer
}

type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Shell interface {
	Exec(ctx context.Context, command string, opts ExecOptions) (ExecResult, error)
	Cleanup(ctx context.Context) error
}

type ExecutionEnv interface {
	FileSystem
	Shell
}
