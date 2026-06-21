package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewManagerMergesGlobalAndProjectSettings(t *testing.T) {
	agentDir, cwd := managerDirs(t)
	writeFile(t, filepath.Join(agentDir, settingsFile), `{
		"transport": "sse",
		"retry": {"enabled": true, "maxRetries": 3},
		"x": "global",
		"globalOnly": 1
	}`)
	writeFile(t, filepath.Join(cwd, ConfigDirName, settingsFile), `{
		"retry": {"maxRetries": 5},
		"x": "project",
		"projectOnly": true
	}`)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	settings := manager.Settings()
	if settings["transport"] != "sse" || settings["x"] != "project" || settings["projectOnly"] != true {
		t.Fatalf("settings = %+v", settings)
	}
	if settings["globalOnly"] != float64(1) {
		t.Fatalf("globalOnly = %v, want 1", settings["globalOnly"])
	}
	retry := requireObject(t, settings["retry"], "retry")
	if retry["enabled"] != true || retry["maxRetries"] != float64(5) {
		t.Fatalf("retry = %+v, want enabled=true maxRetries=5", retry)
	}
}

func TestManagerSetGlobalWritesFileAndPreservesExistingKeys(t *testing.T) {
	agentDir, cwd := managerDirs(t)
	globalPath := filepath.Join(agentDir, settingsFile)
	writeFile(t, globalPath, `{
		"retry": {"enabled": true, "maxRetries": 3},
		"unrelated": "keep"
	}`)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Set(ScopeGlobal, []string{"retry", "enabled"}, false); err != nil {
		t.Fatalf("Set global retry.enabled: %v", err)
	}

	settings := manager.Settings()
	retry := requireObject(t, settings["retry"], "merged retry")
	if retry["enabled"] != false {
		t.Fatalf("merged retry.enabled = %v, want false", retry["enabled"])
	}

	fileSettings, err := readSettings(globalPath)
	if err != nil {
		t.Fatalf("read global settings: %v", err)
	}
	if fileSettings["unrelated"] != "keep" {
		t.Fatalf("unrelated = %v, want keep", fileSettings["unrelated"])
	}
	fileRetry := requireObject(t, fileSettings["retry"], "file retry")
	if fileRetry["enabled"] != false || fileRetry["maxRetries"] != float64(3) {
		t.Fatalf("file retry = %+v, want enabled=false maxRetries=3", fileRetry)
	}
	assertFileMode(t, globalPath, 0o600)
}

func TestManagerSetProjectOverridesGlobal(t *testing.T) {
	agentDir, cwd := managerDirs(t)
	writeFile(t, filepath.Join(agentDir, settingsFile), `{"x": "global"}`)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Set(ScopeProject, []string{"x"}, 1); err != nil {
		t.Fatalf("Set project x: %v", err)
	}

	if got := manager.Settings()["x"]; got != 1 {
		t.Fatalf("Settings()[x] = %#v, want 1", got)
	}
}

func TestManagerSetNestedCreatesIntermediateObjects(t *testing.T) {
	agentDir, cwd := managerDirs(t)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Set(ScopeProject, []string{"a", "b", "c"}, "value"); err != nil {
		t.Fatalf("Set project a.b.c: %v", err)
	}

	settings := manager.Settings()
	a := requireObject(t, settings["a"], "a")
	b := requireObject(t, a["b"], "a.b")
	if b["c"] != "value" {
		t.Fatalf("a.b.c = %v, want value", b["c"])
	}

	fileSettings, err := readSettings(filepath.Join(cwd, ConfigDirName, settingsFile))
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	fileA := requireObject(t, fileSettings["a"], "file a")
	fileB := requireObject(t, fileA["b"], "file a.b")
	if fileB["c"] != "value" {
		t.Fatalf("file a.b.c = %v, want value", fileB["c"])
	}
}

func TestManagerReloadPicksUpExternalEdit(t *testing.T) {
	agentDir, cwd := managerDirs(t)
	globalPath := filepath.Join(agentDir, settingsFile)
	writeFile(t, globalPath, `{"x": 1}`)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	writeFile(t, globalPath, `{"x": 2}`)

	if err := manager.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := manager.Settings()["x"]; got != float64(2) {
		t.Fatalf("Settings()[x] = %#v, want 2", got)
	}
}

func TestManagerSetEmptyPathReturnsError(t *testing.T) {
	agentDir, cwd := managerDirs(t)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Set(ScopeGlobal, []string{}, true); err == nil {
		t.Fatal("Set empty path error = nil, want error")
	}
}

func TestManagerSettingsReturnsDeepClone(t *testing.T) {
	agentDir, cwd := managerDirs(t)
	writeFile(t, filepath.Join(agentDir, settingsFile), `{"retry": {"enabled": true}}`)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	settings := manager.Settings()
	retry := requireObject(t, settings["retry"], "retry")
	retry["enabled"] = false

	fresh := manager.Settings()
	freshRetry := requireObject(t, fresh["retry"], "fresh retry")
	if freshRetry["enabled"] != true {
		t.Fatalf("fresh retry.enabled = %v, want true", freshRetry["enabled"])
	}
}

func managerDirs(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	mkdir(t, agentDir)
	mkdir(t, cwd)
	return agentDir, cwd
}

func requireObject(t *testing.T, value any, label string) map[string]any {
	t.Helper()

	object, ok := asObject(value)
	if !ok {
		t.Fatalf("%s = %T, want object", label, value)
	}
	return object
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
}

func TestNewManagerDegradesCorruptSettings(t *testing.T) {
	agentDir, cwd := managerDirs(t)
	globalPath := filepath.Join(agentDir, settingsFile)
	corrupt := `{"transport": "sse",` // truncated JSON
	writeFile(t, globalPath, corrupt)
	writeFile(t, filepath.Join(cwd, ConfigDirName, settingsFile), `{"projectOnly": true}`)

	manager, err := NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	settings := manager.Settings()
	if _, exists := settings["transport"]; exists {
		t.Fatalf("settings = %+v, want corrupt global scope empty", settings)
	}
	if settings["projectOnly"] != true {
		t.Fatalf("settings = %+v, want project scope loaded", settings)
	}

	drained := manager.DrainErrors()
	if len(drained) != 1 || drained[0].Scope != ScopeGlobal || drained[0].Err == nil {
		t.Fatalf("drained = %+v, want one global error", drained)
	}
	if again := manager.DrainErrors(); len(again) != 0 {
		t.Fatalf("second drain = %+v, want empty", again)
	}

	// Set must apply in memory but never clobber the corrupt file.
	if err := manager.Set(ScopeGlobal, []string{"theme"}, "dark"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if manager.Settings()["theme"] != "dark" {
		t.Fatalf("settings = %+v, want in-memory theme applied", manager.Settings())
	}
	data, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("read global settings: %v", err)
	}
	if string(data) != corrupt {
		t.Fatalf("global settings rewritten to %q, want corrupt content preserved", data)
	}

	// Reload keeps previous in-memory state while the file stays corrupt.
	if err := manager.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if manager.Settings()["theme"] != "dark" {
		t.Fatalf("settings after reload = %+v, want previous in-memory state kept", manager.Settings())
	}
}
