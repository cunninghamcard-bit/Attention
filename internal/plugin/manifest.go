package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

var (
	pluginIDPattern             = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
	secretCapabilityNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

type Manifest struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	Author        string         `json:"author,omitempty"`
	Version       string         `json:"version"`
	MinAppVersion string         `json:"minAppVersion"`
	Main          string         `json:"main,omitempty"`
	Contributions *Contributions `json:"contributions,omitempty"`
	Commands      []Command      `json:"commands,omitempty"`
	Views         []View         `json:"views,omitempty"`
	Activation    []string       `json:"activation,omitempty"`
	Capabilities  []string       `json:"capabilities,omitempty"`
}

type Contributions struct {
	Session     string `json:"session,omitempty"`
	Engine      string `json:"engine,omitempty"`
	Environment string `json:"environment,omitempty"`
}

type Command struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon,omitempty"`
}

type View struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Icon string `json:"icon,omitempty"`
}

func ParseManifest(data []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Manifest{}, fmt.Errorf("manifest: trailing JSON data")
	}

	if !pluginIDPattern.MatchString(manifest.ID) {
		return Manifest{}, fmt.Errorf("manifest id: must match %s", pluginIDPattern.String())
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return Manifest{}, fmt.Errorf("manifest name: required")
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return Manifest{}, fmt.Errorf("manifest version: required")
	}
	if err := validateStrictSemver(manifest.Version); err != nil {
		return Manifest{}, fmt.Errorf("manifest version: %w", err)
	}
	if strings.TrimSpace(manifest.MinAppVersion) == "" {
		return Manifest{}, fmt.Errorf("manifest minAppVersion: required")
	}
	if err := validateStrictSemver(manifest.MinAppVersion); err != nil {
		return Manifest{}, fmt.Errorf("manifest minAppVersion: %w", err)
	}
	if strings.TrimSpace(manifest.Main) == "" && !manifest.Contributions.hasAny() {
		return Manifest{}, fmt.Errorf("manifest main/contributions: at least one must be present")
	}
	if err := validateCapabilities(manifest.Capabilities); err != nil {
		return Manifest{}, err
	}

	for i, cmd := range manifest.Commands {
		if strings.TrimSpace(cmd.ID) == "" {
			return Manifest{}, fmt.Errorf("manifest commands[%d].id: required", i)
		}
		if strings.TrimSpace(cmd.Name) == "" {
			return Manifest{}, fmt.Errorf("manifest commands[%d].name: required", i)
		}
	}
	for i, view := range manifest.Views {
		if strings.TrimSpace(view.Type) == "" {
			return Manifest{}, fmt.Errorf("manifest views[%d].type: required", i)
		}
		if strings.TrimSpace(view.Name) == "" {
			return Manifest{}, fmt.Errorf("manifest views[%d].name: required", i)
		}
	}

	return manifest, nil
}

func validateCapabilities(capabilities []string) error {
	for i, token := range capabilities {
		if err := validateCapability(token); err != nil {
			return fmt.Errorf("manifest capabilities[%d]: %w", i, err)
		}
	}
	return nil
}

func validateCapability(token string) error {
	switch token {
	case "sessions.read", "sessions.prompt", "spawn", "envs":
		return nil
	case "":
		return fmt.Errorf("must not be empty")
	}

	name, ok := strings.CutPrefix(token, "secrets:")
	if ok {
		if name == "" {
			return fmt.Errorf("secret name is required")
		}
		if !secretCapabilityNamePattern.MatchString(name) {
			return fmt.Errorf("secret name %q must match %s", name, secretCapabilityNamePattern.String())
		}
		return nil
	}

	return fmt.Errorf("unknown capability %q", token)
}

func (m Manifest) CompatibleWith(appVersion string) bool {
	ok, err := CompatibleWithMinAppVersion(m.MinAppVersion, appVersion)
	return err == nil && ok
}

func CompatibleWithMinAppVersion(minAppVersion string, appVersion string) (bool, error) {
	minVersion, err := parseStrictSemver(minAppVersion)
	if err != nil {
		return false, fmt.Errorf("minAppVersion: %w", err)
	}
	app, err := parseStrictSemver(appVersion)
	if err != nil {
		return false, fmt.Errorf("appVersion: %w", err)
	}
	return compareSemver(app, minVersion) >= 0, nil
}

func (c *Contributions) hasAny() bool {
	return c != nil &&
		(strings.TrimSpace(c.Session) != "" ||
			strings.TrimSpace(c.Engine) != "" ||
			strings.TrimSpace(c.Environment) != "")
}

type semver struct {
	major int
	minor int
	patch int
	pre   []string
}

func validateStrictSemver(raw string) error {
	_, err := parseStrictSemver(raw)
	return err
}

func parseStrictSemver(raw string) (semver, error) {
	if raw == "" {
		return semver{}, fmt.Errorf("invalid semver %q", raw)
	}

	coreAndPre, build, hasBuild := strings.Cut(raw, "+")
	if hasBuild {
		if build == "" || !validIdentifierList(build, false) {
			return semver{}, fmt.Errorf("invalid semver %q", raw)
		}
		if strings.Contains(build, "+") {
			return semver{}, fmt.Errorf("invalid semver %q", raw)
		}
	}

	core, preRaw, hasPre := strings.Cut(coreAndPre, "-")

	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid semver %q", raw)
	}

	major, err := parseNumericIdentifier(parts[0])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver %q", raw)
	}
	minor, err := parseNumericIdentifier(parts[1])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver %q", raw)
	}
	patch, err := parseNumericIdentifier(parts[2])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver %q", raw)
	}

	var pre []string
	if hasPre {
		if preRaw == "" || !validIdentifierList(preRaw, true) {
			return semver{}, fmt.Errorf("invalid semver %q", raw)
		}
		pre = strings.Split(preRaw, ".")
	}

	return semver{major: major, minor: minor, patch: patch, pre: pre}, nil
}

func parseNumericIdentifier(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty numeric identifier")
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, fmt.Errorf("numeric identifier has leading zero")
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("numeric identifier is not numeric")
		}
	}
	return strconv.Atoi(raw)
}

func validIdentifierList(raw string, rejectNumericLeadingZero bool) bool {
	for _, part := range strings.Split(raw, ".") {
		if part == "" {
			return false
		}
		if rejectNumericLeadingZero && isNumeric(part) && len(part) > 1 && part[0] == '0' {
			return false
		}
		for _, r := range part {
			if (r >= '0' && r <= '9') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= 'a' && r <= 'z') ||
				r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func compareSemver(a semver, b semver) int {
	if got := compareInt(a.major, b.major); got != 0 {
		return got
	}
	if got := compareInt(a.minor, b.minor); got != 0 {
		return got
	}
	if got := compareInt(a.patch, b.patch); got != 0 {
		return got
	}
	return comparePrerelease(a.pre, b.pre)
}

func compareInt(a int, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func comparePrerelease(a []string, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}

	for i := 0; i < len(a) && i < len(b); i++ {
		aNumeric := isNumeric(a[i])
		bNumeric := isNumeric(b[i])
		switch {
		case aNumeric && bNumeric:
			if got := compareNumericString(a[i], b[i]); got != 0 {
				return got
			}
		case aNumeric:
			return -1
		case bNumeric:
			return 1
		default:
			if got := strings.Compare(a[i], b[i]); got != 0 {
				return got
			}
		}
	}
	return compareInt(len(a), len(b))
}

func compareNumericString(a string, b string) int {
	if len(a) != len(b) {
		return compareInt(len(a), len(b))
	}
	return strings.Compare(a, b)
}

func isNumeric(raw string) bool {
	if raw == "" {
		return false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
