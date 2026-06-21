package resource

const (
	ResourceTypeExtension = "extension"
	ResourceTypeSkill     = "skill"
	ResourceTypePrompt    = "prompt"
	ResourceTypeTheme     = "theme"

	DiagnosticWarning   = "warning"
	DiagnosticError     = "error"
	DiagnosticCollision = "collision"
)

// ResourceCollision describes a resource name collision and which definition
// won. The string values mirror pi's diagnostic shape.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/diagnostics.ts:1-8.
type ResourceCollision struct {
	ResourceType string
	Name         string
	WinnerPath   string
	LoserPath    string
	WinnerSource string
	LoserSource  string
}

// ResourceDiagnostic is emitted by resource loaders when a resource cannot be
// loaded but startup can continue.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/diagnostics.ts:10-15.
type ResourceDiagnostic struct {
	Type      string
	Message   string
	Path      string
	Collision *ResourceCollision
}
