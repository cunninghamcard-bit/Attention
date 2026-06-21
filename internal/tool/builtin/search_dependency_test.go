package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/execenv"
)

func TestResolveSearchToolUsesManagedBinBeforeSystem(t *testing.T) {
	binDir := t.TempDir()
	managedPath := filepath.Join(binDir, binaryFileName("rg"))
	if err := writeExecutableFixture(managedPath); err != nil {
		t.Fatalf("write managed fixture: %v", err)
	}
	restore := stubSearchToolResolverDeps(t, binDir, func(context.Context, searchToolDependency) (string, error) {
		return "", errors.New("downloader should not run")
	})
	defer restore()

	env := &searchToolResolveEnv{}
	command, err := resolveSearchTool(context.Background(), env, ripgrepSearchToolDependency())
	if err != nil {
		t.Fatalf("resolveSearchTool err = %v", err)
	}
	if command != managedPath {
		t.Fatalf("command = %q, want managed path %q", command, managedPath)
	}
	if len(env.commands) != 0 {
		t.Fatalf("commands = %#v, want no system lookup", env.commands)
	}
}

func TestResolveSearchToolDownloadsWhenSystemMissing(t *testing.T) {
	binDir := t.TempDir()
	restore := stubSearchToolResolverDeps(t, binDir, func(_ context.Context, dependency searchToolDependency) (string, error) {
		return filepath.Join(binDir, binaryFileName(dependency.binaryName)), nil
	})
	defer restore()

	env := &searchToolResolveEnv{commandLookupExitCode: 1}
	command, err := resolveSearchTool(context.Background(), env, ripgrepSearchToolDependency())
	if err != nil {
		t.Fatalf("resolveSearchTool err = %v", err)
	}
	if !strings.HasSuffix(command, binaryFileName("rg")) {
		t.Fatalf("command = %q, want downloaded rg path", command)
	}
	if len(env.commands) != 1 || env.commands[0] != "command -v rg" {
		t.Fatalf("commands = %#v, want rg system lookup", env.commands)
	}
}

func TestResolveSearchToolOfflineSkipsDownload(t *testing.T) {
	t.Setenv("ALONG_OFFLINE", "1")
	var downloaded bool
	restore := stubSearchToolResolverDeps(t, t.TempDir(), func(context.Context, searchToolDependency) (string, error) {
		downloaded = true
		return "", nil
	})
	defer restore()

	env := &searchToolResolveEnv{commandLookupExitCode: 1}
	_, err := resolveSearchTool(context.Background(), env, ripgrepSearchToolDependency())
	if err == nil {
		t.Fatal("resolveSearchTool err = nil, want offline failure")
	}
	if downloaded {
		t.Fatal("downloader ran in offline mode")
	}
	if got := err.Error(); !strings.Contains(got, "Offline mode enabled") {
		t.Fatalf("offline error = %q, want offline guidance", got)
	}
}

func TestSearchToolAssetNames(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "fd linux amd64",
			got:  fdAssetName("10.3.0", "linux", "amd64"),
			want: "fd-v10.3.0-x86_64-unknown-linux-gnu.tar.gz",
		},
		{
			name: "ripgrep linux amd64",
			got:  ripgrepAssetName("14.1.1", "linux", "amd64"),
			want: "ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz",
		},
		{
			name: "ripgrep linux arm64",
			got:  ripgrepAssetName("14.1.1", "linux", "arm64"),
			want: "ripgrep-14.1.1-aarch64-unknown-linux-gnu.tar.gz",
		},
		{
			name: "fd windows arm64",
			got:  fdAssetName("10.3.0", "windows", "arm64"),
			want: "fd-v10.3.0-aarch64-pc-windows-msvc.zip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("asset name = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func stubSearchToolResolverDeps(
	t *testing.T,
	binDir string,
	downloader func(context.Context, searchToolDependency) (string, error),
) func() {
	t.Helper()
	previousBinDir := searchToolBinDir
	previousDownloader := managedSearchToolDownloader
	searchToolBinDir = func() (string, error) {
		return binDir, nil
	}
	managedSearchToolDownloader = downloader
	return func() {
		searchToolBinDir = previousBinDir
		managedSearchToolDownloader = previousDownloader
	}
}

func writeExecutableFixture(path string) error {
	return os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)
}

type searchToolResolveEnv struct {
	execenv.ExecutionEnv
	commands              []string
	commandLookupExitCode int
}

func (e *searchToolResolveEnv) Exec(
	_ context.Context,
	command string,
	_ execenv.ExecOptions,
) (execenv.ExecResult, error) {
	e.commands = append(e.commands, command)
	return execenv.ExecResult{
		Stdout:   "/usr/bin/rg\n",
		ExitCode: e.commandLookupExitCode,
	}, nil
}
