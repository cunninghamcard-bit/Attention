// Package resource contains shared resource metadata and frontmatter helpers.
package resource

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type SourceKind string

const (
	SourceGlobal   SourceKind = "global"
	SourceProject  SourceKind = "project"
	SourceExplicit SourceKind = "explicit"
	SourceUser     SourceKind = "user"
	SourcePath     SourceKind = "path"
)

// SourceInfo records where a loaded resource came from.
type SourceInfo struct {
	Kind    SourceKind
	Path    string
	BaseDir string
}

func NewSourceInfo(kind SourceKind, path string, baseDir string) SourceInfo {
	return SourceInfo{
		Kind:    kind,
		Path:    path,
		BaseDir: baseDir,
	}
}

// ParseFrontmatter parses leading YAML frontmatter and returns the remaining
// trimmed body.
//
// pi: .agents/references/pi/packages/coding-agent/src/utils/frontmatter.ts:8-37.
func ParseFrontmatter(content string) (map[string]string, string, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	if !strings.HasPrefix(normalized, "---") {
		return map[string]string{}, normalized, nil
	}

	endIndex := strings.Index(normalized[3:], "\n---")
	if endIndex == -1 {
		return map[string]string{}, normalized, nil
	}
	endIndex += 3

	// An empty frontmatter block ("---\n---") puts endIndex before the yaml
	// start; pi's slice(4, endIndex) yields "" there, so mirror that instead
	// of slicing out of range.
	yamlPart := ""
	if endIndex > 4 {
		yamlPart = normalized[4:endIndex]
	}

	var raw map[string]any
	if err := yaml.Unmarshal([]byte(yamlPart), &raw); err != nil {
		return nil, "", err
	}

	frontmatter := map[string]string{}
	for key, value := range raw {
		if value == nil {
			continue
		}
		frontmatter[key] = fmt.Sprint(value)
	}
	return frontmatter, strings.TrimSpace(normalized[endIndex+4:]), nil
}
