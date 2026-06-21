package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMutationQueueKeyFallsBackOnlyForMissingPaths(t *testing.T) {
	tmp := t.TempDir()
	parentFile := filepath.Join(tmp, "parent.txt")
	if err := os.WriteFile(parentFile, []byte("content"), 0o600); err != nil {
		t.Fatalf("write parent fixture: %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{
			name: "missing leaf",
			path: filepath.Join(tmp, "missing.txt"),
		},
		{
			name: "parent is file",
			path: filepath.Join(parentFile, "child.txt"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mutationQueueKey(tt.path)
			if err != nil {
				t.Fatalf("mutationQueueKey() error = %v, want nil", err)
			}
			want, err := filepath.Abs(tt.path)
			if err != nil {
				t.Fatalf("Abs fixture path: %v", err)
			}
			if got != want {
				t.Fatalf("mutationQueueKey() = %q, want %q", got, want)
			}
		})
	}
}

func TestMutationQueueKeyReturnsRealpathErrors(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatalf("create symlink loop: %v", err)
	}

	if _, err := mutationQueueKey(a); err == nil {
		t.Fatal("mutationQueueKey() error = nil, want symlink-loop error")
	}
}

func TestWithFileMutationQueueCleansQueue(t *testing.T) {
	resetFileMutationQueuesForTest(t)

	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("content"), 0o600); err != nil {
		t.Fatalf("write file fixture: %v", err)
	}

	got, err := withFileMutationQueue(file, func() string {
		if count := fileMutationQueueCount(); count != 1 {
			t.Fatalf("queue count while locked = %d, want 1", count)
		}
		return "ok"
	})
	if err != nil {
		t.Fatalf("withFileMutationQueue() error = %v, want nil", err)
	}
	if got != "ok" {
		t.Fatalf("withFileMutationQueue() = %q, want ok", got)
	}
	if count := fileMutationQueueCount(); count != 0 {
		t.Fatalf("queue count after release = %d, want 0", count)
	}
}

func TestWithFileMutationQueueDoesNotRunOnRealpathError(t *testing.T) {
	resetFileMutationQueuesForTest(t)

	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatalf("create symlink loop: %v", err)
	}

	called := false
	if _, err := withFileMutationQueue(a, func() bool {
		called = true
		return true
	}); err == nil {
		t.Fatal("withFileMutationQueue() error = nil, want symlink-loop error")
	}
	if called {
		t.Fatal("withFileMutationQueue() ran callback after key resolution failed")
	}
	if count := fileMutationQueueCount(); count != 0 {
		t.Fatalf("queue count after failed key resolution = %d, want 0", count)
	}
}

func resetFileMutationQueuesForTest(t *testing.T) {
	t.Helper()

	fileMutationQueues.Lock()
	fileMutationQueues.queues = map[string]*fileMutationQueue{}
	fileMutationQueues.Unlock()

	t.Cleanup(func() {
		fileMutationQueues.Lock()
		fileMutationQueues.queues = map[string]*fileMutationQueue{}
		fileMutationQueues.Unlock()
	})
}

func fileMutationQueueCount() int {
	fileMutationQueues.Lock()
	defer fileMutationQueues.Unlock()
	return len(fileMutationQueues.queues)
}
