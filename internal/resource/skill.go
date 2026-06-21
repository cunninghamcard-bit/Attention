package resource

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cunninghamcard-bit/Attention/internal/config"

	gitignore "github.com/sabhiram/go-gitignore"
)

// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:10-14.
const (
	maxSkillNameLength        = 64
	maxSkillDescriptionLength = 1024
)

// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:16.
var ignoreFileNames = [...]string{".gitignore", ".ignore", ".fdignore"}

// Skill is a loaded agent skill.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:74-81.
type Skill struct {
	Name                   string
	Description            string
	FilePath               string
	BaseDir                string
	Source                 SourceInfo
	DisableModelInvocation bool
}

// LoadSkillsOptions configures skill discovery.
type LoadSkillsOptions struct {
	CWD             string
	AgentDir        string
	Paths           []string
	IncludeDefaults bool
}

// LoadSkills discovers skills from default locations and explicit paths.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:387-487.
func LoadSkills(opts LoadSkillsOptions) ([]Skill, []ResourceDiagnostic, error) {
	cwd := opts.CWD
	if cwd == "" {
		resolved, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		cwd = resolved
	}
	agentDir := opts.AgentDir
	if agentDir == "" {
		resolved, err := config.AgentDir()
		if err != nil {
			return nil, nil, err
		}
		agentDir = resolved
	}

	resolvedCWD, err := resolvePath(cwd, "")
	if err != nil {
		return nil, nil, err
	}
	resolvedAgentDir, err := resolvePath(agentDir, "")
	if err != nil {
		return nil, nil, err
	}

	loader := skillLoader{
		byName:  map[string]Skill{},
		byPath:  map[string]struct{}{},
		ordered: []Skill{},
	}
	diagnostics := []ResourceDiagnostic{}
	userSkillsDir := filepath.Join(resolvedAgentDir, "skills")
	projectSkillsDir := filepath.Join(resolvedCWD, config.ConfigDirName, "skills")

	if opts.IncludeDefaults {
		loaded, loadDiagnostics := loadSkillsFromDir(userSkillsDir, SourceUser)
		loader.add(loaded)
		diagnostics = append(diagnostics, loadDiagnostics...)

		loaded, loadDiagnostics = loadSkillsFromDir(projectSkillsDir, SourceProject)
		loader.add(loaded)
		diagnostics = append(diagnostics, loadDiagnostics...)
	}

	for _, rawPath := range opts.Paths {
		resolvedPath, err := resolvePath(strings.TrimSpace(rawPath), resolvedCWD)
		if err != nil {
			return loader.ordered, diagnostics, err
		}

		info, err := os.Stat(resolvedPath)
		if err != nil {
			message := "skill path does not exist"
			if !os.IsNotExist(err) {
				message = err.Error()
			}
			diagnostics = append(diagnostics, skillWarning(message, resolvedPath))
			continue
		}
		source := sourceForExplicitPath(resolvedPath, userSkillsDir, projectSkillsDir, opts.IncludeDefaults)
		if info.IsDir() {
			loaded, loadDiagnostics := loadSkillsFromDir(resolvedPath, source)
			loader.add(loaded)
			diagnostics = append(diagnostics, loadDiagnostics...)
			continue
		}
		if info.Mode().IsRegular() && strings.HasSuffix(resolvedPath, ".md") {
			loaded, loadDiagnostics, ok := loadSkillFromFile(resolvedPath, source)
			diagnostics = append(diagnostics, loadDiagnostics...)
			if ok {
				loader.add([]Skill{loaded})
			}
			continue
		}
		diagnostics = append(diagnostics, skillWarning("skill path is not a markdown file", resolvedPath))
	}

	return loader.ordered, append(diagnostics, loader.collisions...), nil
}

// FormatSkillsForPrompt formats visible skills for system-prompt injection.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:335-360.
func FormatSkillsForPrompt(skills []Skill) string {
	visible := make([]Skill, 0, len(skills))
	for _, s := range skills {
		if !s.DisableModelInvocation {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}

	lines := []string{
		"",
		"",
		"The following skills provide specialized instructions for specific tasks.",
		"Use the read tool to load a skill's file when the task matches its description.",
		"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.",
		"",
		"<available_skills>",
	}
	for _, s := range visible {
		lines = append(
			lines,
			"  <skill>",
			"    <name>"+escapeXML(s.Name)+"</name>",
			"    <description>"+escapeXML(s.Description)+"</description>",
			"    <location>"+escapeXML(s.FilePath)+"</location>",
			"  </skill>",
		)
	}
	lines = append(lines, "</available_skills>")
	return strings.Join(lines, "\n")
}

// ExpandSkillCommand expands "/skill:name args" to the referenced skill body.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1148-1171.
func ExpandSkillCommand(
	text string,
	skills []Skill,
	readFile func(string) ([]byte, error),
) string {
	if !strings.HasPrefix(text, "/skill:") {
		return text
	}
	if readFile == nil {
		return text
	}

	spaceIndex := strings.Index(text, " ")
	skillName := ""
	args := ""
	if spaceIndex == -1 {
		skillName = text[7:]
	} else {
		skillName = text[7:spaceIndex]
		args = strings.TrimSpace(text[spaceIndex+1:])
	}

	var found Skill
	ok := false
	for _, s := range skills {
		if s.Name == skillName {
			found = s
			ok = true
			break
		}
	}
	if !ok {
		return text
	}

	raw, err := readFile(found.FilePath)
	if err != nil {
		return text
	}
	_, body, err := ParseFrontmatter(string(raw))
	if err != nil {
		return text
	}
	body = strings.TrimSpace(body)

	block := "<skill name=\"" + found.Name + "\" location=\"" + found.FilePath + "\">\n" +
		"References are relative to " + found.BaseDir + ".\n\n" +
		body + "\n</skill>"
	if args != "" {
		return block + "\n\n" + args
	}
	return block
}

// DedupeSkills keeps the first skill per name and reports each loser as a
// collision diagnostic; duplicate file paths (reached via symlinks) are
// dropped silently.
//
// pi funnels every skill source through a single loadSkills call, so
// cross-source duplicates flow through the same map (skills.ts:393-428);
// along loads base and extension skills separately and dedupes at the merge.
func DedupeSkills(skills []Skill) ([]Skill, []ResourceDiagnostic) {
	loader := skillLoader{
		byName:  map[string]Skill{},
		byPath:  map[string]struct{}{},
		ordered: []Skill{},
	}
	loader.add(skills)
	return loader.ordered, loader.collisions
}

type skillLoader struct {
	byName  map[string]Skill
	byPath  map[string]struct{}
	ordered []Skill
	// collisions are appended after all other diagnostics, matching pi's
	// [...allDiagnostics, ...collisionDiagnostics] ordering (skills.ts:485).
	collisions []ResourceDiagnostic
}

func (l *skillLoader) add(skills []Skill) {
	for _, s := range skills {
		realPath := canonicalPath(s.FilePath)
		if _, ok := l.byPath[realPath]; ok {
			continue
		}
		if existing, ok := l.byName[s.Name]; ok {
			l.collisions = append(l.collisions, ResourceDiagnostic{
				Type:    DiagnosticCollision,
				Message: fmt.Sprintf("name %q collision", s.Name),
				Path:    s.FilePath,
				Collision: &ResourceCollision{
					ResourceType: ResourceTypeSkill,
					Name:         s.Name,
					WinnerPath:   existing.FilePath,
					LoserPath:    s.FilePath,
				},
			})
			continue
		}
		l.byName[s.Name] = s
		l.byPath[realPath] = struct{}{}
		l.ordered = append(l.ordered, s)
	}
}

// loadSkillsFromDir mirrors pi's two-pass skill discovery: a directory-local
// SKILL.md wins, otherwise the root may load loose Markdown and subdirectories
// recurse for their own SKILL.md only.
//
// Ignore-file handling mirrors pi's IGNORE_FILE_NAMES, addIgnoreRules, and
// per-entry matcher checks.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:174-274.
// pi ignore matcher: .agents/references/pi/packages/coding-agent/src/core/skills.ts:16,47-65,209-258.
func loadSkillsFromDir(dir string, source SourceKind) ([]Skill, []ResourceDiagnostic) {
	return loadSkillsFromDirInternal(dir, source, true, skillIgnoreState{rootDir: dir})
}

func loadSkillsFromDirInternal(
	dir string,
	source SourceKind,
	includeRootFiles bool,
	ignoreState skillIgnoreState,
) ([]Skill, []ResourceDiagnostic) {
	skills := []Skill{}
	diagnostics := []ResourceDiagnostic{}
	ignoreState = ignoreState.withDirRules(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return skills, diagnostics
	}

	// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:194-220.
	for _, entry := range entries {
		if entry.Name() != "SKILL.md" {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		_, isFile := skillEntryKind(fullPath, entry)
		if !isFile {
			continue
		}
		if ignoreState.ignores(fullPath, false) {
			continue
		}
		skills, diagnostics = appendLoadedSkill(skills, diagnostics, fullPath, source)
		return skills, diagnostics
	}

	// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:223-271.
	for _, entry := range entries {
		if shouldSkipDirEntry(entry.Name()) {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		isDir, isFile := skillEntryKind(fullPath, entry)

		if ignoreState.ignores(fullPath, isDir) {
			continue
		}
		if isDir {
			loaded, loadDiagnostics := loadSkillsFromDirInternal(fullPath, source, false, ignoreState)
			skills = append(skills, loaded...)
			diagnostics = append(diagnostics, loadDiagnostics...)
			continue
		}
		if !isFile || !includeRootFiles || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		skills, diagnostics = appendLoadedSkill(skills, diagnostics, fullPath, source)
	}
	return skills, diagnostics
}

type skillIgnoreState struct {
	rootDir  string
	patterns []string
	matcher  *gitignore.GitIgnore
}

func (s skillIgnoreState) withDirRules(dir string) skillIgnoreState {
	rootDir := s.rootDir
	if rootDir == "" {
		rootDir = dir
	}

	prefix := skillIgnorePrefix(rootDir, dir)
	patterns := append([]string{}, s.patterns...)
	for _, filename := range ignoreFileNames {
		patterns = append(patterns, readSkillIgnorePatterns(filepath.Join(dir, filename), prefix)...)
	}

	return skillIgnoreState{
		rootDir:  rootDir,
		patterns: patterns,
		matcher:  compileSkillIgnoreMatcher(patterns),
	}
}

func (s skillIgnoreState) ignores(path string, dir bool) bool {
	if s.matcher == nil {
		return false
	}

	relPath, ok := skillRelativePath(s.rootDir, path)
	if !ok {
		return false
	}
	if dir {
		relPath += "/"
	}
	return s.matcher.MatchesPath(relPath)
}

func readSkillIgnorePatterns(path string, prefix string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}

	patterns := []string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		pattern, ok := prefixSkillIgnorePattern(line, prefix)
		if ok {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

func prefixSkillIgnorePattern(line string, prefix string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, `\#`) {
		return "", false
	}

	pattern := line
	negated := false
	switch {
	case strings.HasPrefix(pattern, "!"):
		negated = true
		pattern = pattern[1:]
	case strings.HasPrefix(pattern, `\!`):
		pattern = pattern[1:]
	}
	pattern = strings.TrimPrefix(pattern, "/")

	pattern = prefix + pattern
	if negated {
		return "!" + pattern, true
	}
	return pattern, true
}

func skillIgnorePrefix(rootDir string, dir string) string {
	relPath, ok := skillRelativePath(rootDir, dir)
	if !ok || relPath == "" {
		return ""
	}
	return relPath + "/"
}

func skillRelativePath(rootDir string, path string) (string, bool) {
	relPath, err := filepath.Rel(rootDir, path)
	if err != nil {
		return "", false
	}
	if relPath == "." {
		return "", true
	}
	return filepath.ToSlash(relPath), true
}

func compileSkillIgnoreMatcher(patterns []string) (matcher *gitignore.GitIgnore) {
	if len(patterns) == 0 {
		return nil
	}
	defer func() {
		if recover() != nil {
			matcher = nil
		}
	}()
	return gitignore.CompileIgnoreLines(patterns...)
}

func appendLoadedSkill(
	skills []Skill,
	diagnostics []ResourceDiagnostic,
	filePath string,
	source SourceKind,
) ([]Skill, []ResourceDiagnostic) {
	loaded, loadDiagnostics, ok := loadSkillFromFile(filePath, source)
	diagnostics = append(diagnostics, loadDiagnostics...)
	if ok {
		skills = append(skills, loaded)
	}
	return skills, diagnostics
}

func skillEntryKind(path string, entry os.DirEntry) (bool, bool) {
	if entry.Type()&os.ModeSymlink == 0 {
		return entry.IsDir(), entry.Type().IsRegular()
	}

	info, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	return info.IsDir(), info.Mode().IsRegular()
}

// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:276-326.
func loadSkillFromFile(filePath string, source SourceKind) (Skill, []ResourceDiagnostic, bool) {
	rawContent, err := os.ReadFile(filePath)
	if err != nil {
		return Skill{}, []ResourceDiagnostic{skillWarning("load skill: "+err.Error(), filePath)}, false
	}
	frontmatter, _, err := ParseFrontmatter(string(rawContent))
	if err != nil {
		return Skill{}, []ResourceDiagnostic{skillWarning("parse skill frontmatter: "+err.Error(), filePath)}, false
	}

	baseDir := filepath.Dir(filePath)
	// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:286-296.
	name := frontmatter["name"]
	if name == "" {
		name = filepath.Base(baseDir)
	}
	description := frontmatter["description"]

	diagnostics := skillWarnings(validateDescription(description), filePath)
	diagnostics = append(diagnostics, skillWarnings(validateName(name), filePath)...)
	if strings.TrimSpace(description) == "" {
		return Skill{}, diagnostics, false
	}

	return Skill{
		Name:                   name,
		Description:            description,
		FilePath:               filePath,
		BaseDir:                baseDir,
		Source:                 NewSourceInfo(source, filePath, baseDir),
		DisableModelInvocation: frontmatter["disable-model-invocation"] == "true",
	}, diagnostics, true
}

func skillWarnings(messages []string, path string) []ResourceDiagnostic {
	diagnostics := make([]ResourceDiagnostic, 0, len(messages))
	for _, message := range messages {
		diagnostics = append(diagnostics, skillWarning(message, path))
	}
	return diagnostics
}

func skillWarning(message string, path string) ResourceDiagnostic {
	return ResourceDiagnostic{
		Type:    DiagnosticWarning,
		Message: message,
		Path:    path,
	}
}

// pi: .agents/references/pi/packages/coding-agent/src/core/skills.ts:92-124.
func validateName(name string) []string {
	errors := []string{}
	nameLength := utf8.RuneCountInString(name)
	if nameLength > maxSkillNameLength {
		errors = append(
			errors,
			"name exceeds "+strconv.Itoa(maxSkillNameLength)+" characters ("+strconv.Itoa(nameLength)+")",
		)
	}
	if !isValidSkillNameCharacters(name) {
		errors = append(errors, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errors = append(errors, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errors = append(errors, "name must not contain consecutive hyphens")
	}
	return errors
}

func validateDescription(description string) []string {
	descriptionLength := utf8.RuneCountInString(description)
	switch {
	case strings.TrimSpace(description) == "":
		return []string{"description is required"}
	case descriptionLength > maxSkillDescriptionLength:
		return []string{
			"description exceeds " + strconv.Itoa(maxSkillDescriptionLength) +
				" characters (" + strconv.Itoa(descriptionLength) + ")",
		}
	default:
		return []string{}
	}
}

func isValidSkillNameCharacters(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		isLowerAlpha := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if !isLowerAlpha && !isDigit && r != '-' {
			return false
		}
	}
	return true
}

func shouldSkipDirEntry(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules":
		return true
	default:
		return false
	}
}

func canonicalPath(path string) string {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return realPath
}

func sourceForExplicitPath(
	path string,
	userSkillsDir string,
	projectSkillsDir string,
	includeDefaults bool,
) SourceKind {
	if includeDefaults {
		return SourcePath
	}
	switch {
	case isUnderPath(path, userSkillsDir):
		return SourceUser
	case isUnderPath(path, projectSkillsDir):
		return SourceProject
	default:
		return SourcePath
	}
}

func isUnderPath(path string, root string) bool {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	if cleanPath == cleanRoot {
		return true
	}
	prefix := cleanRoot + string(filepath.Separator)
	return strings.HasPrefix(cleanPath, prefix)
}

func escapeXML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	).Replace(s)
}
