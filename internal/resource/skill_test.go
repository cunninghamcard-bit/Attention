package resource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDiscoversNestedSkills(t *testing.T) {
	root := t.TempDir()
	firstDir := filepath.Join(root, "sub")
	secondDir := filepath.Join(root, "nested", "deep")
	for _, dir := range []string{firstDir, secondDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	firstPath := filepath.Join(firstDir, "SKILL.md")
	secondPath := filepath.Join(secondDir, "SKILL.md")
	if err := os.WriteFile(firstPath, []byte(`---
name: foo
description: Use foo
---
Foo body
`), 0o600); err != nil {
		t.Fatalf("write first skill: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte(`---
name: bar
description: Use bar
---
Bar body
`), 0o600); err != nil {
		t.Fatalf("write second skill: %v", err)
	}

	got, _, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{root},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("skills len = %d, want 2: %#v", len(got), got)
	}
	if got[0].Name != "bar" || got[0].BaseDir != secondDir || got[0].FilePath != secondPath {
		t.Fatalf("first skill = %#v, want nested bar skill", got[0])
	}
	if got[1].Name != "foo" || got[1].BaseDir != firstDir || got[1].FilePath != firstPath {
		t.Fatalf("second skill = %#v, want sub foo skill", got[1])
	}
}

func TestLoadSkillsFromDirMirrorsPiDiscoveryRules(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root-skill")
	fooDir := filepath.Join(root, "foo")
	looseDir := filepath.Join(root, "loose")
	blankDir := filepath.Join(root, "blank")
	dotDir := filepath.Join(root, ".hidden")
	nodeModulesDir := filepath.Join(root, "node_modules", "dependency")
	for _, dir := range []string{root, fooDir, looseDir, blankDir, dotDir, nodeModulesDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	rootLoosePath := filepath.Join(root, "a.md")
	if err := os.WriteFile(rootLoosePath, []byte(`---
description: Root loose skill
---
Root loose body
`), 0o600); err != nil {
		t.Fatalf("write root loose skill: %v", err)
	}

	fooPath := filepath.Join(fooDir, "SKILL.md")
	if err := os.WriteFile(fooPath, []byte(`---
description: Foo skill
---
Foo body
`), 0o600); err != nil {
		t.Fatalf("write foo skill: %v", err)
	}

	if err := os.WriteFile(filepath.Join(looseDir, "bar.md"), []byte(`---
name: bar
description: Loose nested skill
---
Bar body
`), 0o600); err != nil {
		t.Fatalf("write nested loose skill: %v", err)
	}

	blankPath := filepath.Join(blankDir, "SKILL.md")
	if err := os.WriteFile(blankPath, []byte(`---
description: "   "
---
Blank body
`), 0o600); err != nil {
		t.Fatalf("write blank-description skill: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dotDir, "SKILL.md"), []byte(`---
name: hidden
description: Hidden skill
---
Hidden body
`), 0o600); err != nil {
		t.Fatalf("write dot-dir skill: %v", err)
	}

	if err := os.WriteFile(filepath.Join(nodeModulesDir, "SKILL.md"), []byte(`---
name: dependency
description: Dependency skill
---
Dependency body
`), 0o600); err != nil {
		t.Fatalf("write node_modules skill: %v", err)
	}

	got, diagnostics, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{root},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byName := map[string]Skill{}
	for _, skill := range got {
		byName[skill.Name] = skill
	}
	if len(byName) != 2 {
		t.Fatalf("loaded skills = %#v, want root-skill and foo only", got)
	}
	if skill := byName["root-skill"]; skill.FilePath != rootLoosePath || skill.BaseDir != root {
		t.Fatalf("root loose skill = %#v, want name fallback to parent root dir", skill)
	}
	if skill := byName["foo"]; skill.FilePath != fooPath || skill.BaseDir != fooDir {
		t.Fatalf("foo skill = %#v, want parent-dir name fallback", skill)
	}
	for _, skippedName := range []string{"bar", "hidden", "dependency"} {
		if _, ok := byName[skippedName]; ok {
			t.Fatalf("loaded skipped skill %q: %#v", skippedName, got)
		}
	}
	assertWarningDiagnostic(t, diagnostics, blankPath, "description is required")
}

func TestLoadSkillsHonorsGitignoreFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, ".gitignore"),
		[]byte("ignored/\nignored-file/SKILL.md\nblocked.md\n"),
		0o600,
	); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	writeTestSkill(t, filepath.Join(root, "ignored"), "ignored")
	writeTestSkill(t, filepath.Join(root, "ignored-file"), "ignored-file")
	writeTestSkillMarkdown(t, filepath.Join(root, "blocked.md"), "blocked")
	keptPath := writeTestSkill(t, filepath.Join(root, "kept"), "kept")

	byName := loadTestSkillsByName(t, root)
	if len(byName) != 1 {
		t.Fatalf("loaded skills = %#v, want kept only", byName)
	}
	if skill := byName["kept"]; skill.FilePath != keptPath {
		t.Fatalf("kept skill = %#v, want %s", skill, keptPath)
	}
	for _, skippedName := range []string{"ignored", "ignored-file", "blocked"} {
		if _, ok := byName[skippedName]; ok {
			t.Fatalf("loaded ignored skill %q: %#v", skippedName, byName)
		}
	}
}

func TestLoadSkillsHonorsGitignoreNegation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*\n!keep-me/\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	writeTestSkill(t, filepath.Join(root, "drop-me"), "drop-me")
	keptPath := writeTestSkill(t, filepath.Join(root, "keep-me"), "keep-me")

	byName := loadTestSkillsByName(t, root)
	if len(byName) != 1 {
		t.Fatalf("loaded skills = %#v, want keep-me only", byName)
	}
	if skill := byName["keep-me"]; skill.FilePath != keptPath {
		t.Fatalf("keep-me skill = %#v, want %s", skill, keptPath)
	}
	if _, ok := byName["drop-me"]; ok {
		t.Fatalf("loaded negation-excluded skill: %#v", byName)
	}
}

func TestLoadSkillsScopesNestedGitignorePatterns(t *testing.T) {
	root := t.TempDir()
	scopedDir := filepath.Join(root, "scoped")
	if err := os.MkdirAll(scopedDir, 0o700); err != nil {
		t.Fatalf("mkdir scoped dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scopedDir, ".gitignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write nested .gitignore: %v", err)
	}

	writeTestSkill(t, filepath.Join(scopedDir, "ignored"), "scoped-ignored")
	scopedKeptPath := writeTestSkill(t, filepath.Join(scopedDir, "kept"), "scoped-kept")
	outsideIgnoredPath := writeTestSkill(t, filepath.Join(root, "ignored"), "outside-ignored")

	byName := loadTestSkillsByName(t, root)
	if len(byName) != 2 {
		t.Fatalf("loaded skills = %#v, want scoped-kept and outside-ignored", byName)
	}
	if skill := byName["scoped-kept"]; skill.FilePath != scopedKeptPath {
		t.Fatalf("scoped-kept skill = %#v, want %s", skill, scopedKeptPath)
	}
	if skill := byName["outside-ignored"]; skill.FilePath != outsideIgnoredPath {
		t.Fatalf("outside-ignored skill = %#v, want %s", skill, outsideIgnoredPath)
	}
	if _, ok := byName["scoped-ignored"]; ok {
		t.Fatalf("loaded nested ignored skill: %#v", byName)
	}
}

func TestLoadSkillsHonorsIgnoreAndFdignoreFiles(t *testing.T) {
	for _, filename := range []string{".ignore", ".fdignore"} {
		t.Run(filename, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, filename), []byte("blocked/\n"), 0o600); err != nil {
				t.Fatalf("write %s: %v", filename, err)
			}
			writeTestSkill(t, filepath.Join(root, "blocked"), "blocked")
			keptPath := writeTestSkill(t, filepath.Join(root, "kept"), "kept")

			byName := loadTestSkillsByName(t, root)
			if len(byName) != 1 {
				t.Fatalf("loaded skills = %#v, want kept only", byName)
			}
			if skill := byName["kept"]; skill.FilePath != keptPath {
				t.Fatalf("kept skill = %#v, want %s", skill, keptPath)
			}
			if _, ok := byName["blocked"]; ok {
				t.Fatalf("loaded %s-blocked skill: %#v", filename, byName)
			}
		})
	}
}

func TestLoadSkillsFromDirStopsAtDirectorySkill(t *testing.T) {
	root := t.TempDir()
	nestedDir := filepath.Join(root, "nested")
	if err := os.MkdirAll(nestedDir, 0o700); err != nil {
		t.Fatalf("mkdir nested dir: %v", err)
	}

	rootSkillPath := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(rootSkillPath, []byte(`---
name: root
description: Root skill
---
Root body
`), 0o600); err != nil {
		t.Fatalf("write root skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.md"), []byte(`---
name: loose
description: Loose skill
---
Loose body
`), 0o600); err != nil {
		t.Fatalf("write loose skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "SKILL.md"), []byte(`---
name: nested
description: Nested skill
---
Nested body
`), 0o600); err != nil {
		t.Fatalf("write nested skill: %v", err)
	}

	got, diagnostics, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{root},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", diagnostics)
	}
	if len(got) != 1 || got[0].Name != "root" || got[0].FilePath != rootSkillPath {
		t.Fatalf("skills = %#v, want only root SKILL.md", got)
	}
}

func TestLoadMissingDirIsEmpty(t *testing.T) {
	got, _, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{filepath.Join(t.TempDir(), "missing")},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("skills len = %d, want 0: %#v", len(got), got)
	}
}

func TestLoadFlagsDisableModelInvocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(`---
name: hidden
description: Hidden skill
disable-model-invocation: true
---
Hidden body
`), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	got, _, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{dir},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("skills len = %d, want 1: %#v", len(got), got)
	}
	if !got[0].DisableModelInvocation {
		t.Fatalf("DisableModelInvocation = false, want true: %#v", got[0])
	}
}

func TestLoadReturnsDiagnosticsForMalformedSkills(t *testing.T) {
	root := t.TempDir()
	goodDir := filepath.Join(root, "good")
	badDir := filepath.Join(root, "bad")
	missingDescriptionDir := filepath.Join(root, "missing-description")
	for _, dir := range []string{goodDir, badDir, missingDescriptionDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	goodPath := filepath.Join(goodDir, "SKILL.md")
	if err := os.WriteFile(goodPath, []byte(`---
name: good
description: Good skill
---
Good body
`), 0o600); err != nil {
		t.Fatalf("write good skill: %v", err)
	}

	badPath := filepath.Join(badDir, "SKILL.md")
	if err := os.WriteFile(badPath, []byte(`---
name: [bad
description: Bad skill
---
Bad body
`), 0o600); err != nil {
		t.Fatalf("write bad skill: %v", err)
	}

	missingDescriptionPath := filepath.Join(missingDescriptionDir, "SKILL.md")
	if err := os.WriteFile(missingDescriptionPath, []byte(`---
name: missing-description
---
Missing description body
`), 0o600); err != nil {
		t.Fatalf("write missing-description skill: %v", err)
	}

	got, diagnostics, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{root},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("skills = %#v, want only good skill", got)
	}
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics len = %d, want 2: %#v", len(diagnostics), diagnostics)
	}
	assertWarningDiagnostic(t, diagnostics, badPath, "parse skill frontmatter")
	assertWarningDiagnostic(t, diagnostics, missingDescriptionPath, "description is required")
}

func TestLoadWarnsForOverLongMetadata(t *testing.T) {
	root := t.TempDir()
	goodDir := filepath.Join(root, "good")
	longNameDir := filepath.Join(root, "long-name")
	longDescriptionDir := filepath.Join(root, "long-description")
	for _, dir := range []string{goodDir, longNameDir, longDescriptionDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	goodPath := filepath.Join(goodDir, "SKILL.md")
	if err := os.WriteFile(goodPath, []byte(`---
name: good
description: Good skill
---
Good body
`), 0o600); err != nil {
		t.Fatalf("write good skill: %v", err)
	}

	longNamePath := filepath.Join(longNameDir, "SKILL.md")
	longName := strings.Repeat("a", maxSkillNameLength+1)
	if err := os.WriteFile(longNamePath, []byte(`---
name: `+longName+`
description: Long name skill
---
Long name body
`), 0o600); err != nil {
		t.Fatalf("write long-name skill: %v", err)
	}

	longDescriptionPath := filepath.Join(longDescriptionDir, "SKILL.md")
	longDescription := strings.Repeat("a", maxSkillDescriptionLength+1)
	if err := os.WriteFile(longDescriptionPath, []byte(`---
name: long-description
description: `+longDescription+`
---
Long description body
`), 0o600); err != nil {
		t.Fatalf("write long-description skill: %v", err)
	}

	got, diagnostics, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{root},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byName := map[string]Skill{}
	for _, skill := range got {
		byName[skill.Name] = skill
	}
	if len(byName) != 3 {
		t.Fatalf("skills = %#v, want all skills loaded with warnings", got)
	}
	for _, name := range []string{"good", longName, "long-description"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("missing loaded skill %q from %#v", name, got)
		}
	}
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics len = %d, want 2: %#v", len(diagnostics), diagnostics)
	}
	assertWarningDiagnostic(t, diagnostics, longNamePath, "name exceeds 64 characters")
	assertWarningDiagnostic(t, diagnostics, longDescriptionPath, "description exceeds 1024 characters")
}

func TestFormatSkillsForPrompt(t *testing.T) {
	if got := FormatSkillsForPrompt(nil); got != "" {
		t.Fatalf("FormatSkillsForPrompt(nil) = %q, want empty", got)
	}
	if got := FormatSkillsForPrompt([]Skill{{Name: "hidden", DisableModelInvocation: true}}); got != "" {
		t.Fatalf("FormatSkillsForPrompt(disabled) = %q, want empty", got)
	}

	got := FormatSkillsForPrompt([]Skill{
		{
			Name:        "foo<&\"'",
			Description: "desc<&\"'",
			FilePath:    "/tmp/foo<&\"'.md",
		},
		{
			Name:                   "hidden",
			Description:            "Hidden",
			FilePath:               "/tmp/hidden/SKILL.md",
			DisableModelInvocation: true,
		},
	})
	for _, want := range []string{
		"The following skills provide specialized instructions for specific tasks.",
		"Use the read tool to load a skill's file when the task matches its description.",
		"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.",
		"<available_skills>",
		"<name>foo&lt;&amp;&quot;&apos;</name>",
		"<description>desc&lt;&amp;&quot;&apos;</description>",
		"<location>/tmp/foo&lt;&amp;&quot;&apos;.md</location>",
		"</available_skills>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hidden") {
		t.Fatalf("formatted prompt includes disabled skill:\n%s", got)
	}
}

func assertWarningDiagnostic(
	t *testing.T,
	diagnostics []ResourceDiagnostic,
	path string,
	messagePart string,
) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Path != path {
			continue
		}
		if diagnostic.Type != DiagnosticWarning {
			t.Fatalf("diagnostic for %s type = %q, want warning", path, diagnostic.Type)
		}
		if !strings.Contains(diagnostic.Message, messagePart) {
			t.Fatalf("diagnostic for %s message = %q, want %q", path, diagnostic.Message, messagePart)
		}
		return
	}
	t.Fatalf("diagnostic for %s not found in %#v", path, diagnostics)
}

func writeTestSkill(t *testing.T, dir string, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir skill dir %s: %v", dir, err)
	}

	path := filepath.Join(dir, "SKILL.md")
	writeTestSkillMarkdown(t, path, name)
	return path
}

func writeTestSkillMarkdown(t *testing.T, path string, name string) {
	t.Helper()
	content := `---
name: ` + name + `
description: Use ` + name + `
---
` + name + ` body
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write skill %s: %v", path, err)
	}
}

func loadTestSkillsByName(t *testing.T, root string) map[string]Skill {
	t.Helper()
	got, _, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{root},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byName := map[string]Skill{}
	for _, skill := range got {
		byName[skill.Name] = skill
	}
	return byName
}

func TestExpandSkillCommand(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "foo")
	filePath := filepath.Join(baseDir, "SKILL.md")
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte(`---
name: foo
description: Use foo
---
Foo body
`), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	skills := []Skill{{
		Name:     "foo",
		FilePath: filePath,
		BaseDir:  baseDir,
	}}
	wantBlock := "<skill name=\"foo\" location=\"" + filePath + "\">\n" +
		"References are relative to " + baseDir + ".\n\n" +
		"Foo body\n</skill>"

	tests := []struct {
		name     string
		text     string
		readFile func(string) ([]byte, error)
		want     string
	}{
		{
			name:     "non skill command unchanged",
			text:     "hello /skill:foo",
			readFile: os.ReadFile,
			want:     "hello /skill:foo",
		},
		{
			name:     "known skill expands",
			text:     "/skill:foo",
			readFile: os.ReadFile,
			want:     wantBlock,
		},
		{
			name:     "known skill appends args",
			text:     "/skill:foo extra args",
			readFile: os.ReadFile,
			want:     wantBlock + "\n\nextra args",
		},
		{
			name:     "unknown skill unchanged",
			text:     "/skill:missing",
			readFile: os.ReadFile,
			want:     "/skill:missing",
		},
		{
			name: "read error unchanged",
			text: "/skill:foo",
			readFile: func(string) ([]byte, error) {
				return nil, errors.New("boom")
			},
			want: "/skill:foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandSkillCommand(tt.text, skills, tt.readFile)
			if got != tt.want {
				t.Fatalf("ExpandSkillCommand(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestLoadSkillsEmitsCollisionDiagnostics(t *testing.T) {
	winnerDir := filepath.Join(t.TempDir(), "winner")
	loserDir := filepath.Join(t.TempDir(), "loser")
	for _, dir := range []string{winnerDir, loserDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(`---
name: shared
description: Shared skill
---
Body
`), 0o600); err != nil {
			t.Fatalf("write skill: %v", err)
		}
	}

	skills, diagnostics, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{winnerDir, loserDir},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("skills len = %d, want 1", len(skills))
	}
	winnerPath := filepath.Join(winnerDir, "SKILL.md")
	loserPath := filepath.Join(loserDir, "SKILL.md")
	if skills[0].FilePath != winnerPath {
		t.Fatalf("winner = %q, want %q", skills[0].FilePath, winnerPath)
	}

	var collision *ResourceDiagnostic
	for i := range diagnostics {
		if diagnostics[i].Type == DiagnosticCollision {
			collision = &diagnostics[i]
			break
		}
	}
	if collision == nil {
		t.Fatalf("no collision diagnostic in %#v", diagnostics)
	}
	if collision.Message != `name "shared" collision` {
		t.Errorf("message = %q", collision.Message)
	}
	if collision.Collision == nil ||
		collision.Collision.ResourceType != ResourceTypeSkill ||
		collision.Collision.Name != "shared" ||
		collision.Collision.WinnerPath != winnerPath ||
		collision.Collision.LoserPath != loserPath {
		t.Errorf("collision payload = %#v", collision.Collision)
	}
}

func TestLoadSkillsWarnsForBadExplicitPaths(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "does-not-exist")
	notMarkdown := filepath.Join(base, "notes.txt")
	if err := os.WriteFile(notMarkdown, []byte("plain"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, diagnostics, err := LoadSkills(LoadSkillsOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{missing, notMarkdown},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	want := map[string]string{
		missing:     "skill path does not exist",
		notMarkdown: "skill path is not a markdown file",
	}
	for path, message := range want {
		found := false
		for _, d := range diagnostics {
			if d.Type == DiagnosticWarning && d.Path == path && d.Message == message {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing warning %q for %s in %#v", message, path, diagnostics)
		}
	}
}
