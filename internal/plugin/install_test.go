package plugin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/config"
)

func TestInstallLocalPluginCopiesToGlobalDirAndEnables(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, config.AgentDirName)
	cwd := filepath.Join(root, "project")
	source := filepath.Join(root, "source")
	mustMkdir(t, agentDir)
	mustMkdir(t, cwd)
	writePluginFixture(t, source)

	result, err := Install(context.Background(), InstallOptions{
		Source:   source,
		AgentDir: agentDir,
		CWD:      cwd,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantDir := filepath.Join(root, userPluginDirName, "rtk-optimizer")
	if result.Name != "rtk-optimizer" || result.Dir != wantDir || !result.Installed {
		t.Fatalf("result = %+v, want installed rtk-optimizer at %s", result, wantDir)
	}
	if !manifestExists(wantDir) {
		t.Fatalf("manifest missing at %s", wantDir)
	}

	manager, err := config.NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	global, err := manager.ScopeSettings(config.ScopeGlobal)
	if err != nil {
		t.Fatalf("ScopeSettings: %v", err)
	}
	if got := settingsStringSlice(global, settingsPluginsKey); !slices.Equal(got, []string{"rtk-optimizer"}) {
		t.Fatalf("plugins = %#v, want [rtk-optimizer]", got)
	}
}

func TestInstallGitPluginClonesSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git script uses POSIX shell")
	}

	root := t.TempDir()
	agentDir := filepath.Join(root, config.AgentDirName)
	cwd := filepath.Join(root, "project")
	bin := filepath.Join(root, "bin")
	mustMkdir(t, agentDir)
	mustMkdir(t, cwd)
	mustMkdir(t, bin)
	mustWrite(t, filepath.Join(bin, "git"), `#!/bin/sh
test "$1" = clone || exit 2
test "$2" = --depth=1 || exit 3
dest="$4"
mkdir -p "$dest/.attention-plugin"
printf '{"name":"rtk-optimizer"}' > "$dest/.attention-plugin/plugin.json"
printf '%s' "$3" > "$dest/source.txt"
`)
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	source := "https://example.test/rtk-optimizer.git"
	result, err := Install(context.Background(), InstallOptions{
		Source:   source,
		AgentDir: agentDir,
		CWD:      cwd,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.Installed {
		t.Fatalf("result = %+v, want installed", result)
	}
	data, err := os.ReadFile(filepath.Join(result.Dir, "source.txt"))
	if err != nil {
		t.Fatalf("read source marker: %v", err)
	}
	if string(data) != source {
		t.Fatalf("source marker = %q, want %q", data, source)
	}
}
