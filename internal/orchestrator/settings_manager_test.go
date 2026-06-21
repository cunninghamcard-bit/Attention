package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestSettingsManagerSettersPersistGlobalSettings(t *testing.T) {
	o, _, globalPath := newSettingsManagerTestOrchestrator(t, `{
  "retry": {"enabled": true},
  "steeringMode": "one-at-a-time"
}`)

	if !o.autoRetrySettings().enabled {
		t.Fatal("autoRetrySettings enabled = false, want manager settings to override fallback Settings")
	}

	o.SetAutoRetry(false)
	settings := readSettingsFile(t, globalPath)
	retry := requireSettingsObject(t, settings["retry"], "retry")
	if retry["enabled"] != false {
		t.Fatalf("retry.enabled = %v, want false", retry["enabled"])
	}
	if o.autoRetrySettings().enabled {
		t.Fatal("autoRetrySettings enabled = true after SetAutoRetry(false)")
	}

	o.SetSteeringMode(QueueModeAll)
	settings = readSettingsFile(t, globalPath)
	if settings["steeringMode"] != string(QueueModeAll) {
		t.Fatalf("steeringMode = %v, want %q", settings["steeringMode"], QueueModeAll)
	}
	if got := o.Snapshot().SteeringMode; got != string(QueueModeAll) {
		t.Fatalf("Snapshot SteeringMode = %q, want %q", got, QueueModeAll)
	}
}

func TestSetEnableSkillCommandsUpdatesCacheAndPersists(t *testing.T) {
	o, _, globalPath := newSettingsManagerTestOrchestrator(t, `{}`)
	o.skills = []resource.Skill{
		{
			Name:        "review",
			Description: "Review changes",
		},
	}
	if !hasSlashCommandNamed(o.SlashCommands(), "skill:review") {
		t.Fatalf("SlashCommands missing default skill command: %#v", o.SlashCommands())
	}

	o.SetEnableSkillCommands(false)
	settings := readSettingsFile(t, globalPath)
	if settings["enableSkillCommands"] != false {
		t.Fatalf("enableSkillCommands = %v, want false", settings["enableSkillCommands"])
	}
	if hasSlashCommandNamed(o.SlashCommands(), "skill:review") {
		t.Fatalf("SlashCommands still include disabled skill command: %#v", o.SlashCommands())
	}
}

func TestReloadSettingsRefreshesManagerSettings(t *testing.T) {
	o, _, globalPath := newSettingsManagerTestOrchestrator(t, `{
  "retry": {"enabled": true},
  "compaction": {"enabled": true},
  "steeringMode": "one-at-a-time",
  "followUpMode": "one-at-a-time"
}`)

	o.SetAutoRetry(false)
	o.SetAutoCompaction(false)
	if o.autoRetrySettings().enabled {
		t.Fatal("autoRetrySettings enabled = true after runtime override")
	}
	if o.Snapshot().AutoCompactionEnabled {
		t.Fatal("AutoCompactionEnabled = true after runtime update")
	}

	writeSettingsFile(t, globalPath, `{
  "retry": {"enabled": true},
  "compaction": {"enabled": true},
  "steeringMode": "all",
  "followUpMode": "all"
}`)

	if err := o.ReloadSettings(context.Background()); err != nil {
		t.Fatalf("ReloadSettings: %v", err)
	}
	if !o.autoRetrySettings().enabled {
		t.Fatal("autoRetrySettings enabled = false after reloading retry.enabled=true")
	}
	snap := o.Snapshot()
	if !snap.AutoCompactionEnabled {
		t.Fatal("AutoCompactionEnabled = false after reloading compaction.enabled=true")
	}
	if snap.SteeringMode != string(QueueModeAll) || snap.FollowUpMode != string(QueueModeAll) {
		t.Fatalf(
			"queue modes = %q/%q, want %q/%q",
			snap.SteeringMode,
			snap.FollowUpMode,
			QueueModeAll,
			QueueModeAll,
		)
	}
}

func newSettingsManagerTestOrchestrator(
	t *testing.T,
	globalSettings string,
) (*Orchestrator, *config.Manager, string) {
	t.Helper()

	agentDir := t.TempDir()
	cwd := t.TempDir()
	globalPath := filepath.Join(agentDir, "settings.json")
	writeSettingsFile(t, globalPath, globalSettings)

	manager, err := config.NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(context.Background(), NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Settings: config.Settings{
			"retry": map[string]any{
				"enabled": false,
			},
			"compaction": map[string]any{
				"enabled": false,
			},
			"steeringMode": string(QueueModeAll),
			"followUpMode": string(QueueModeAll),
		},
		SettingsManager: manager,
		AgentDir:        agentDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, manager, globalPath
}

func readSettingsFile(t *testing.T, path string) config.Settings {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings file: %v", err)
	}
	settings := config.Settings{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings file: %v", err)
	}
	return settings
}

func writeSettingsFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write settings file: %v", err)
	}
}

func requireSettingsObject(t *testing.T, value any, label string) map[string]any {
	t.Helper()

	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want object", label, value)
	}
	return object
}
