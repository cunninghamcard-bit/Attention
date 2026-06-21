package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifestRejectsBadManifests(t *testing.T) {
	tests := []struct {
		name      string
		manifest  string
		wantField string
	}{
		{
			name:      "missing id",
			manifest:  `{"name":"Demo","version":"1.0.0","minAppVersion":"1.0.0","main":"main.js"}`,
			wantField: "id",
		},
		{
			name:      "bad id",
			manifest:  `{"id":"Demo","name":"Demo","version":"1.0.0","minAppVersion":"1.0.0","main":"main.js"}`,
			wantField: "id",
		},
		{
			name:      "missing name",
			manifest:  `{"id":"demo","version":"1.0.0","minAppVersion":"1.0.0","main":"main.js"}`,
			wantField: "name",
		},
		{
			name:      "bad version",
			manifest:  `{"id":"demo","name":"Demo","version":"1","minAppVersion":"1.0.0","main":"main.js"}`,
			wantField: "version",
		},
		{
			name:      "bad min app version",
			manifest:  `{"id":"demo","name":"Demo","version":"1.0.0","minAppVersion":"01.0.0","main":"main.js"}`,
			wantField: "minAppVersion",
		},
		{
			name:      "missing main and contributions",
			manifest:  `{"id":"demo","name":"Demo","version":"1.0.0","minAppVersion":"1.0.0"}`,
			wantField: "main/contributions",
		},
		{
			name:      "command missing id",
			manifest:  `{"id":"demo","name":"Demo","version":"1.0.0","minAppVersion":"1.0.0","main":"main.js","commands":[{"name":"Run"}]}`,
			wantField: "commands[0].id",
		},
		{
			name:      "command missing name",
			manifest:  `{"id":"demo","name":"Demo","version":"1.0.0","minAppVersion":"1.0.0","main":"main.js","commands":[{"id":"run"}]}`,
			wantField: "commands[0].name",
		},
		{
			name:      "view missing type",
			manifest:  `{"id":"demo","name":"Demo","version":"1.0.0","minAppVersion":"1.0.0","main":"main.js","views":[{"name":"Panel"}]}`,
			wantField: "views[0].type",
		},
		{
			name:      "view missing name",
			manifest:  `{"id":"demo","name":"Demo","version":"1.0.0","minAppVersion":"1.0.0","main":"main.js","views":[{"type":"panel"}]}`,
			wantField: "views[0].name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tt.manifest))
			if err == nil {
				t.Fatal("ParseManifest succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantField) {
				t.Fatalf("error = %q, want field %q", err.Error(), tt.wantField)
			}
		})
	}
}

func TestParseManifestAcceptsContributionsOnly(t *testing.T) {
	manifest, err := ParseManifest([]byte(`{
		"id":"demo",
		"name":"Demo",
		"version":"1.0.0",
		"minAppVersion":"1.0.0",
		"contributions":{"session":"session.js","engine":"engine.js"},
		"commands":[{"id":"run","name":"Run"}],
		"views":[{"type":"panel","name":"Panel"}],
		"activation":["onCommand:run"],
		"capabilities":["sessions.read"]
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.Main != "" || manifest.Contributions.Session != "session.js" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestRegistryUserDirWinsOverBundled(t *testing.T) {
	root := t.TempDir()
	bundledDir := filepath.Join(root, "bundled")
	agentDir := filepath.Join(root, "agent")

	writePluginManifest(t, filepath.Join(bundledDir, "demo"), "demo", "1.0.0", "main.js", "")
	userPluginDir := filepath.Join(agentDir, "plugins", "demo")
	writePluginManifest(t, userPluginDir, "demo", "2.0.0", "main.js", "")

	registry, err := NewRegistry(bundledDir, agentDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	plugins := registry.List()
	if len(plugins) != 1 {
		t.Fatalf("plugins = %+v, want 1", plugins)
	}
	got := plugins[0]
	if got.ID != "demo" || got.Version != "2.0.0" || got.Dir != userPluginDir || !got.Enabled || !got.HasApp {
		t.Fatalf("plugin = %+v", got)
	}
}

func TestRegistryLoadErrorsAreIsolated(t *testing.T) {
	root := t.TempDir()
	bundledDir := filepath.Join(root, "bundled")
	agentDir := filepath.Join(root, "agent")

	writePluginManifest(t, filepath.Join(bundledDir, "good"), "good", "1.0.0", "main.js", "")
	badDir := filepath.Join(bundledDir, "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("mkdir bad plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "manifest.json"), []byte(`{"id":"Bad"}`), 0o644); err != nil {
		t.Fatalf("write bad manifest: %v", err)
	}

	registry, err := NewRegistry(bundledDir, agentDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	good, ok := registry.Get("good")
	if !ok || good.LoadError != "" {
		t.Fatalf("good plugin = %+v, ok = %v", good, ok)
	}
	bad, ok := registry.Get("bad")
	if !ok {
		t.Fatal("bad plugin load error was not listed")
	}
	if !strings.Contains(bad.LoadError, "manifest id") {
		t.Fatalf("bad LoadError = %q", bad.LoadError)
	}
}

func TestRegistryEnabledPersistenceRoundTrip(t *testing.T) {
	root := t.TempDir()
	bundledDir := filepath.Join(root, "bundled")
	agentDir := filepath.Join(root, "agent")
	writePluginManifest(t, filepath.Join(bundledDir, "demo"), "demo", "1.0.0", "main.js", "")

	registry, err := NewRegistry(bundledDir, agentDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.SetEnabled("demo", false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}

	reloaded, err := NewRegistry(bundledDir, agentDir)
	if err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	info, ok := reloaded.Get("demo")
	if !ok {
		t.Fatal("demo plugin missing after reload")
	}
	if info.Enabled {
		t.Fatalf("Enabled = true, want false")
	}

	if err := reloaded.SetEnabled("demo", true); err != nil {
		t.Fatalf("SetEnabled true: %v", err)
	}
	rereloaded, err := NewRegistry(bundledDir, agentDir)
	if err != nil {
		t.Fatalf("second reload registry: %v", err)
	}
	info, ok = rereloaded.Get("demo")
	if !ok || !info.Enabled {
		t.Fatalf("after enable plugin = %+v, ok = %v", info, ok)
	}

	if err := registry.SetEnabled("missing", false); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("SetEnabled missing error = %v, want ErrPluginNotFound", err)
	}
}

func TestRegistryContributionAccessors(t *testing.T) {
	root := t.TempDir()
	bundledDir := filepath.Join(root, "bundled")
	agentDir := filepath.Join(root, "agent")
	pluginDir := filepath.Join(bundledDir, "demo")
	writePluginManifest(
		t,
		pluginDir,
		"demo",
		"1.0.0",
		"",
		`{"session":"session.mjs","engine":"engine.mjs","environment":"environment.mjs"}`,
	)
	writePluginManifest(t, filepath.Join(bundledDir, "app-only"), "app-only", "1.0.0", "main.js", "")

	registry, err := NewRegistry(bundledDir, agentDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tests := []struct {
		name  string
		owner string
		file  string
	}{
		{name: "session", owner: "session", file: "session.mjs"},
		{name: "engine", owner: "engine", file: "engine.mjs"},
		{name: "environment", owner: "environment", file: "environment.mjs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := registry.ContributionPath("demo", tt.owner)
			if !ok {
				t.Fatalf("ContributionPath(%q) ok = false", tt.owner)
			}
			want, err := filepath.Abs(filepath.Join(pluginDir, tt.file))
			if err != nil {
				t.Fatalf("abs want path: %v", err)
			}
			if got != want {
				t.Fatalf("ContributionPath(%q) = %q, want %q", tt.owner, got, want)
			}

			infos := registry.EnabledWithContribution(tt.owner)
			if len(infos) != 1 || infos[0].ID != "demo" {
				t.Fatalf("EnabledWithContribution(%q) = %+v, want demo only", tt.owner, infos)
			}
		})
	}

	if _, ok := registry.ContributionPath("demo", "missing"); ok {
		t.Fatal("ContributionPath unknown owner ok = true, want false")
	}
	if _, ok := registry.ContributionPath("app-only", "session"); ok {
		t.Fatal("ContributionPath app-only session ok = true, want false")
	}
	if _, ok := registry.ContributionPath("missing", "session"); ok {
		t.Fatal("ContributionPath missing plugin ok = true, want false")
	}

	if err := registry.SetEnabled("demo", false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}
	if _, ok := registry.ContributionPath("demo", "session"); ok {
		t.Fatal("ContributionPath disabled plugin ok = true, want false")
	}
	if infos := registry.EnabledWithContribution("session"); len(infos) != 0 {
		t.Fatalf("EnabledWithContribution disabled = %+v, want empty", infos)
	}
}

func TestManifestCompatibleWith(t *testing.T) {
	tests := []struct {
		name          string
		minAppVersion string
		appVersion    string
		want          bool
	}{
		{name: "same version", minAppVersion: "1.2.3", appVersion: "1.2.3", want: true},
		{name: "newer minor", minAppVersion: "1.2.3", appVersion: "1.3.0", want: true},
		{name: "older patch", minAppVersion: "1.2.3", appVersion: "1.2.2", want: false},
		{name: "release satisfies prerelease min", minAppVersion: "1.2.3-beta.1", appVersion: "1.2.3", want: true},
		{name: "prerelease below release min", minAppVersion: "1.2.3", appVersion: "1.2.3-beta.1", want: false},
		{name: "build metadata ignored", minAppVersion: "1.2.3+build.1", appVersion: "1.2.3+build.2", want: true},
		{name: "invalid app version", minAppVersion: "1.2.3", appVersion: "1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := Manifest{MinAppVersion: tt.minAppVersion}
			if got := manifest.CompatibleWith(tt.appVersion); got != tt.want {
				t.Fatalf("CompatibleWith(%q) = %v, want %v", tt.appVersion, got, tt.want)
			}
		})
	}
}

func TestCompatibleWithMinAppVersionRejectsBadMinVersion(t *testing.T) {
	_, err := CompatibleWithMinAppVersion("1", "1.0.0")
	if err == nil {
		t.Fatal("CompatibleWithMinAppVersion succeeded, want error")
	}
	if !strings.Contains(err.Error(), "minAppVersion") {
		t.Fatalf("error = %q, want minAppVersion", err.Error())
	}
}

func writePluginManifest(
	t *testing.T,
	dir string,
	id string,
	version string,
	main string,
	contributions string,
) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	contributionField := ""
	if contributions != "" {
		contributionField = fmt.Sprintf(`,"contributions":%s`, contributions)
	}
	data := fmt.Sprintf(
		`{"id":%q,"name":"%s","version":%q,"minAppVersion":"1.0.0","main":%q%s}`,
		id,
		id,
		version,
		main,
		contributionField,
	)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
