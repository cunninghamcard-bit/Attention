//go:build unix

package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
)

func TestEnvExecTimeoutKillsProcessTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "grandchild-marker")
	env := local.New(root)

	_, err := env.Exec(
		context.Background(),
		`(sleep 0.2; printf marker > "$MARKER") & wait`,
		execenv.ExecOptions{
			Env:     map[string]string{"MARKER": marker},
			Timeout: 50 * time.Millisecond,
		},
	)
	assertExecutionCode(t, err, execenv.ExecutionErrorTimeout)

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("background child wrote marker after Exec timeout")
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat marker: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
