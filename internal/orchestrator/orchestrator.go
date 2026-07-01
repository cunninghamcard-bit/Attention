package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/auth"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
)

// Orchestrator is the mode-facing runtime facade. It owns mutable run state;
// the harness remains stateless and receives snapshots per call.
type Orchestrator struct {
	session      harness.Session
	harness      agentHarness
	hooks        *hook.Registry
	provider     *provider.Registry
	baseProvider *provider.Registry
	repo         *session.JsonlSessionRepo
	cwd          string

	model               ai.Model
	thinkingLevel       agentloop.ThinkingLevel
	systemPrompt        string
	systemPromptOptions hook.SystemPromptOptions
	settings            config.Settings
	settingsManager     *config.Manager
	getAPIKey           func(ctx context.Context, provider string) (string, error)
	tools               []tool.Tool
	toolDefs            []extension.ToolDefinition
	baseToolDefs        []extension.ToolDefinition
	toolBuilder         ToolBuilder
	extensions          []ExtensionSource
	hooksPath           string
	commands            map[string]extension.CommandDefinition
	customSystemPrompt  string
	promptTemplates     []resource.PromptTemplate
	skills              []resource.Skill
	promptPaths         []string
	skillPaths          []string
	agentDir            string
	contextFiles        []resource.ContextFile
	diagnostics         []resource.ResourceDiagnostic
	execEnv             execenv.ExecutionEnv

	mu                        sync.Mutex
	phase                     phase
	idleCh                    chan struct{}
	activeCancel              context.CancelFunc
	pendingWrites             []pendingWrite
	steerQueue                []message.AgentMessage
	followUpQueue             []message.AgentMessage
	steeringMode              QueueMode
	followUpMode              QueueMode
	nextTurnQueue             []message.AgentMessage
	overflowRecoveryAttempted bool
	retryAttempt              int
	autoRetryEnabledOverride  *bool
	autoCompactionEnabled     bool
	retryAbort                chan struct{}

	subscribersMu    sync.Mutex
	subscribers      map[uint64]func(Event)
	nextSubscriberID uint64
}

// QueueMode controls how many queued messages are injected at a drain point.
// pi: .agents/references/pi/packages/agent/src/types.ts:38-44.
type QueueMode string

const (
	QueueModeAll        QueueMode = "all"
	QueueModeOneAtATime QueueMode = "one-at-a-time"
)

func steeringModeFromSettings(settings config.Settings) QueueMode {
	return queueModeFromSettings(settings, "steeringMode")
}

func followUpModeFromSettings(settings config.Settings) QueueMode {
	return queueModeFromSettings(settings, "followUpMode")
}

func queueModeFromSettings(settings config.Settings, key string) QueueMode {
	text, ok := settings[key].(string)
	if !ok {
		return QueueModeOneAtATime
	}
	mode := QueueMode(text)
	switch mode {
	case QueueModeAll, QueueModeOneAtATime:
		return mode
	default:
		return QueueModeOneAtATime
	}
}

func autoCompactionEnabledFrom(settings config.Settings) bool {
	compaction, ok := settingsObject(settings, "compaction")
	if !ok {
		return true
	}
	enabled, ok := compaction["enabled"].(bool)
	if !ok {
		return true
	}
	return enabled
}

func enableSkillCommandsFrom(settings config.Settings) bool {
	enabled, ok := settings["enableSkillCommands"].(bool)
	if ok {
		return enabled
	}
	if _, exists := settings["enableSkillCommands"]; exists {
		return true
	}

	// pi migrates legacy skills.enableSkillCommands to enableSkillCommands and
	// defaults missing enableSkillCommands to true:
	// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:359-363
	// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:908-914.
	skills, ok := settingsObject(settings, "skills")
	if !ok {
		return true
	}
	enabled, ok = skills["enableSkillCommands"].(bool)
	if !ok {
		return true
	}
	return enabled
}

func settingsObject(settings config.Settings, key string) (map[string]any, bool) {
	if settings == nil {
		return nil, false
	}
	switch value := settings[key].(type) {
	case map[string]any:
		return value, true
	case config.Settings:
		return map[string]any(value), true
	default:
		return nil, false
	}
}

func settingsStringSliceValue(settings config.Settings, key string) []string {
	if settings == nil {
		return []string{}
	}
	value, ok := settings[key]
	if !ok {
		return []string{}
	}
	switch items := value.(type) {
	case []string:
		return append([]string(nil), items...)
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return []string{}
	}
}

const manualCompactionReason = "manual"

const (
	modelSelectSourceSet     = "set"
	modelSelectSourceCycle   = "cycle"
	modelSelectSourceRestore = "restore"
)

// New creates an orchestrator for a fresh session.
func New(ctx context.Context, opts NewOptions) (*Orchestrator, error) {
	s := opts.Session
	if s == nil {
		if opts.Repo == nil {
			return nil, errors.New("orchestrator: repo or session is required")
		}
		created, err := opts.Repo.Create(ctx, opts.CreateOptions)
		if err != nil {
			return nil, err
		}
		s = created
	}

	cfg := runtimeConfig{
		model:              opts.Model,
		modelID:            opts.ModelID,
		modelProvider:      opts.ModelProvider,
		provider:           opts.Provider,
		repo:               opts.Repo,
		thinkingLevel:      opts.ThinkingLevel,
		systemPrompt:       opts.SystemPrompt,
		appendSystemPrompt: opts.AppendSystemPrompt,
		getAPIKey:          opts.GetAPIKey,
		settings:           cloneSettings(opts.Settings),
		settingsManager:    opts.SettingsManager,
		extensions:         opts.Extensions,
		hooksPath:          opts.HooksPath,
		tools:              opts.Tools,
		toolBuilder:        opts.ToolBuilder,
		promptTemplates:    opts.PromptTemplates,
		skills:             opts.Skills,
		promptPaths:        append([]string(nil), opts.PromptPaths...),
		skillPaths:         append([]string(nil), opts.SkillPaths...),
		agentDir:           opts.AgentDir,
		contextFiles:       opts.ContextFiles,
		diagnostics:        append([]resource.ResourceDiagnostic(nil), opts.Diagnostics...),
		executionEnv:       opts.ExecutionEnv,
	}
	return assemble(ctx, s, cfg)
}

// Open creates an orchestrator for an existing session and recovers current
// model/thinking configuration from its JSONL context.
func Open(ctx context.Context, opts OpenOptions) (*Orchestrator, error) {
	s := opts.Session
	if s == nil {
		if opts.Repo == nil {
			return nil, errors.New("orchestrator: repo or session is required")
		}
		opened, err := opts.Repo.Open(ctx, opts.Metadata)
		if err != nil {
			return nil, err
		}
		s = opened
	}

	cfg := runtimeConfig{
		model:              opts.Model,
		modelID:            opts.ModelID,
		modelProvider:      opts.ModelProvider,
		provider:           opts.Provider,
		repo:               opts.Repo,
		thinkingLevel:      opts.ThinkingLevel,
		systemPrompt:       opts.SystemPrompt,
		appendSystemPrompt: opts.AppendSystemPrompt,
		getAPIKey:          opts.GetAPIKey,
		settings:           cloneSettings(opts.Settings),
		settingsManager:    opts.SettingsManager,
		extensions:         opts.Extensions,
		hooksPath:          opts.HooksPath,
		tools:              opts.Tools,
		toolBuilder:        opts.ToolBuilder,
		promptTemplates:    opts.PromptTemplates,
		skills:             opts.Skills,
		promptPaths:        append([]string(nil), opts.PromptPaths...),
		skillPaths:         append([]string(nil), opts.SkillPaths...),
		agentDir:           opts.AgentDir,
		contextFiles:       opts.ContextFiles,
		diagnostics:        append([]resource.ResourceDiagnostic(nil), opts.Diagnostics...),
		executionEnv:       opts.ExecutionEnv,
		recoverState:       true,
	}
	return assemble(ctx, s, cfg)
}

func assemble(ctx context.Context, s harness.Session, cfg runtimeConfig) (*Orchestrator, error) {
	if s == nil {
		return nil, errors.New("orchestrator: session is required")
	}
	if cfg.thinkingLevel == "" {
		// pi: settings defaultThinkingLevel ?? DEFAULT_THINKING_LEVEL "medium"
		// (defaults.ts:3, agent-session.ts:1571).
		cfg.thinkingLevel = agentloop.ThinkingMedium
		if value, ok := cfg.settings["defaultThinkingLevel"].(string); ok && value != "" {
			cfg.thinkingLevel = agentloop.ThinkingLevel(value)
		}
	}
	var templateCollisions []resource.ResourceDiagnostic
	cfg.promptTemplates, templateCollisions = resource.DedupePromptTemplates(cfg.promptTemplates)
	cfg.diagnostics = append(cfg.diagnostics, templateCollisions...)
	prov := cfg.provider
	if prov == nil {
		prov = defaultProviderRegistry(cfg.model, cfg.getAPIKey)
	}
	baseProvider := prov.CloneBase()
	modelID := cfg.modelID
	modelProvider := cfg.model.Provider
	// An explicit CLI provider hint (--provider) wins over the model's recorded
	// provider so an ambiguous model id resolves under the requested provider.
	if cfg.modelProvider != "" {
		modelProvider = cfg.modelProvider
	}
	if modelID == "" {
		modelID = cfg.model.ID
	}
	if cfg.recoverState {
		recovered, err := s.BuildContext(ctx)
		if err != nil {
			return nil, err
		}
		if recovered.Model != nil && recovered.Model.ModelID != "" {
			modelID = recovered.Model.ModelID
			modelProvider = recovered.Model.Provider
		}
		if recovered.ThinkingLevel != "" {
			cfg.thinkingLevel = agentloop.ThinkingLevel(recovered.ThinkingLevel)
		}
	}
	metadata := s.GetMetadata()
	settings := cfg.settings
	steeringMode := QueueModeOneAtATime
	followUpMode := QueueModeOneAtATime
	autoCompactionEnabled := true
	if cfg.settingsManager != nil {
		settings = cfg.settingsManager.Settings()
		steeringMode = steeringModeFromSettings(settings)
		followUpMode = followUpModeFromSettings(settings)
		autoCompactionEnabled = autoCompactionEnabledFrom(settings)
	}

	// pi defaults both queue modes to one-at-a-time when settings omit them:
	// .agents/references/pi/packages/agent/src/agent.ts:212-213 and
	// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:613-624.
	o := &Orchestrator{
		session:               s,
		repo:                  cfg.repo,
		cwd:                   metadata.CWD,
		model:                 cfg.model,
		thinkingLevel:         cfg.thinkingLevel,
		settings:              cloneSettings(settings),
		settingsManager:       cfg.settingsManager,
		getAPIKey:             cfg.getAPIKey,
		execEnv:               cfg.executionEnv,
		phase:                 phaseIdle,
		steeringMode:          steeringMode,
		followUpMode:          followUpMode,
		autoCompactionEnabled: autoCompactionEnabled,
		commands:              map[string]extension.CommandDefinition{},
		baseProvider:          baseProvider,
		baseToolDefs:          append([]extension.ToolDefinition(nil), cfg.tools...),
		toolBuilder:           cfg.toolBuilder,
		extensions:            append([]ExtensionSource(nil), cfg.extensions...),
		hooksPath:             cfg.hooksPath,
		customSystemPrompt:    cfg.systemPrompt,
		promptTemplates:       append([]resource.PromptTemplate(nil), cfg.promptTemplates...),
		skills:                append([]resource.Skill(nil), cfg.skills...),
		promptPaths:           append([]string(nil), cfg.promptPaths...),
		skillPaths:            append([]string(nil), cfg.skillPaths...),
		agentDir:              cfg.agentDir,
		contextFiles:          append([]resource.ContextFile(nil), cfg.contextFiles...),
		diagnostics:           append([]resource.ResourceDiagnostic(nil), cfg.diagnostics...),
	}

	hooks := hook.NewRegistry()

	// Declarative shell hooks: a hooks.json file maps lifecycle events to shell
	// commands. A missing/empty file is a no-op (LoadShellHooks returns nil,nil).
	if runner, err := hook.LoadShellHooks(cfg.hooksPath); err != nil {
		o.diagnostics = append(o.diagnostics, resource.ResourceDiagnostic{
			Type:    resource.DiagnosticError,
			Message: fmt.Sprintf("Failed to load shell hooks: %s", err),
			Path:    cfg.hooksPath,
		})
	} else if runner != nil {
		runner.Register(hooks, s.GetMetadata().ID)
	}

	ctxFactory := o.extensionContext

	// Both builtin and extension tools are extension.ToolDefinition values,
	// wrapped uniformly into runtime tool.Tool via builtin.Wrap (pi:
	// wrapToolDefinition). Later registrations replace earlier ones by name,
	// while preserving the first insertion order, matching JS Map#set in pi.
	toolRegistry := tool.NewRegistry()
	toolDefOrder := []string{}
	toolDefsByName := map[string]extension.ToolDefinition{}
	addToolDef := func(def extension.ToolDefinition) error {
		wrapped, err := builtin.Wrap(def, ctxFactory)
		if err != nil {
			return err
		}
		if _, exists := toolDefsByName[def.Name]; !exists {
			toolDefOrder = append(toolDefOrder, def.Name)
		}
		toolDefsByName[def.Name] = def
		toolRegistry.Add(wrapped)
		return nil
	}
	for _, def := range cfg.tools {
		if err := addToolDef(def); err != nil {
			return nil, err
		}
	}

	allExtensions := append([]ExtensionSource(nil), cfg.extensions...)

	bindExtension := func(ext extension.Extension) error {
		if err := o.bindExtensionCommands(ext); err != nil {
			return err
		}
		if err := registerExtensionProviders(prov, ext.Providers); err != nil {
			return err
		}
		for _, def := range ext.Tools {
			if err := addToolDef(def); err != nil {
				return err
			}
		}
		return nil
	}

	for _, source := range allExtensions {
		loadErr := func() error {
			if source.Factory == nil {
				return fmt.Errorf("extension %q missing factory", source.Path)
			}
			ext, err := extension.Load(source.Path, hooks, ctxFactory, source.Factory)
			if err != nil {
				return err
			}
			return bindExtension(ext)
		}()
		if loadErr != nil {
			// pi records a per-extension load error and keeps loading the
			// rest; the agent starts with whatever loaded successfully
			// (loader.ts:380-438).
			o.diagnostics = append(o.diagnostics, resource.ResourceDiagnostic{
				Type:    resource.DiagnosticError,
				Message: fmt.Sprintf("Failed to load extension: %s", loadErr),
				Path:    source.Path,
			})
		}
	}

	o.registerEventHandlers(hooks)

	model, err := resolveRuntimeModel(prov, modelProvider, modelID, cfg.model, cfg.provider == nil)
	if err != nil {
		return nil, err
	}
	o.model = model
	o.hooks = hooks
	o.provider = prov
	o.tools = toolRegistry.Tools()
	toolDefs := toolDefinitionsInOrder(toolDefOrder, toolDefsByName)
	o.toolDefs = toolDefs
	reason := cfg.resourceDiscoverReason
	if reason == "" {
		reason = "startup"
	}
	if err := o.discoverExtensionResources(ctx, metadata.CWD, reason); err != nil {
		return nil, err
	}
	cfg.skills = o.skills
	o.systemPrompt, o.systemPromptOptions = buildSystemPrompt(cfg, metadata.CWD, toolDefs)
	o.harness = harness.New(harness.HarnessConfig{
		Session:            s,
		Hooks:              hooks,
		Tools:              o.tools,
		CompactionSettings: compactionSettingsFrom(settings),
		GetProviderAuth:    providerAuthResolver(prov),
	})
	return o, nil
}

// Close releases resources owned by the orchestrator. The orchestrator no
// longer owns external processes, so this is currently a no-op retained for
// API stability (callers still defer Close).
func (o *Orchestrator) Close() error {
	return nil
}

type extensionResourcePaths struct {
	skillPaths  []string
	promptPaths []string
	themePaths  []string
}

type loadedResourceSet struct {
	skills          []resource.Skill
	promptTemplates []resource.PromptTemplate
	diagnostics     []resource.ResourceDiagnostic
}

func (o *Orchestrator) discoverExtensionResources(ctx context.Context, cwd string, reason string) error {
	discovered, err := o.gatherExtensionResourcePaths(ctx, cwd, reason)
	if err != nil {
		return err
	}

	// along has no theme system yet, so themePaths are accepted for pi parity
	// and intentionally ignored.
	_ = discovered.themePaths
	if len(discovered.skillPaths) == 0 && len(discovered.promptPaths) == 0 {
		return nil
	}

	agentDir, err := o.resolveAgentDir()
	if err != nil {
		return fmt.Errorf("orchestrator: resolve agent dir for extension resources: %w", err)
	}

	extensionSkills := []resource.Skill{}
	extensionPromptTemplates := []resource.PromptTemplate{}
	extensionDiagnostics := []resource.ResourceDiagnostic{}
	if len(discovered.skillPaths) > 0 {
		skills, diagnostics, err := resource.LoadSkills(resource.LoadSkillsOptions{
			CWD:             cwd,
			AgentDir:        agentDir,
			Paths:           discovered.skillPaths,
			IncludeDefaults: false,
		})
		if err != nil {
			return fmt.Errorf("orchestrator: load extension skills: %w", err)
		}
		extensionSkills = append(extensionSkills, skills...)
		extensionDiagnostics = append(extensionDiagnostics, diagnostics...)
	}

	if len(discovered.promptPaths) > 0 {
		templates, diagnostics, err := resource.LoadPromptTemplates(resource.LoadPromptTemplatesOptions{
			CWD:             cwd,
			AgentDir:        agentDir,
			Paths:           discovered.promptPaths,
			IncludeDefaults: false,
		})
		if err != nil {
			return fmt.Errorf("orchestrator: load extension prompt templates: %w", err)
		}
		extensionPromptTemplates = append(extensionPromptTemplates, templates...)
		extensionDiagnostics = append(extensionDiagnostics, diagnostics...)
	}

	o.mu.Lock()
	// pi loads base and extension resources through one loader pass, so
	// cross-source name duplicates are deduped with collision diagnostics
	// (resource-loader.ts:533-539,800-824); along merges two loads here.
	skills, skillCollisions := resource.DedupeSkills(append(o.skills, extensionSkills...))
	o.skills = skills
	templates, templateCollisions := resource.DedupePromptTemplates(append(o.promptTemplates, extensionPromptTemplates...))
	o.promptTemplates = templates
	o.diagnostics = append(o.diagnostics, extensionDiagnostics...)
	o.diagnostics = append(o.diagnostics, skillCollisions...)
	o.diagnostics = append(o.diagnostics, templateCollisions...)
	o.mu.Unlock()
	return nil
}

func (o *Orchestrator) gatherExtensionResourcePaths(
	ctx context.Context,
	cwd string,
	reason string,
) (extensionResourcePaths, error) {
	hooks := o.hooks
	if hooks == nil || !hooks.HasHandlers(hook.EventResourcesDiscover) {
		return extensionResourcePaths{}, nil
	}

	paths := extensionResourcePaths{
		skillPaths:  []string{},
		promptPaths: []string{},
		themePaths:  []string{},
	}
	event := hook.ResourcesDiscoverEvent{
		Type:   hook.EventResourcesDiscover,
		CWD:    cwd,
		Reason: reason,
	}

	// pi's emitResourcesDiscover gathers every handler result; Registry.Emit
	// would collapse these to last non-nil.
	// .agents/references/pi/packages/coding-agent/src/core/extensions/runner.ts:990-1030.
	for _, handler := range hooks.Handlers(hook.EventResourcesDiscover) {
		result, err := handler(ctx, event)
		if err != nil {
			return extensionResourcePaths{}, err
		}
		discovered := resourcesDiscoverResult(result)
		if discovered == nil {
			continue
		}
		paths.skillPaths = append(paths.skillPaths, discovered.SkillPaths...)
		paths.promptPaths = append(paths.promptPaths, discovered.PromptPaths...)
		paths.themePaths = append(paths.themePaths, discovered.ThemePaths...)
	}

	return paths, nil
}

func resourcesDiscoverResult(result any) *hook.ResourcesDiscoverResult {
	switch r := result.(type) {
	case hook.ResourcesDiscoverResult:
		return &r
	case *hook.ResourcesDiscoverResult:
		return r
	default:
		return nil
	}
}

func (o *Orchestrator) resolveAgentDir() (string, error) {
	o.mu.Lock()
	agentDir := o.agentDir
	o.mu.Unlock()
	if agentDir != "" {
		return agentDir, nil
	}
	return config.AgentDir()
}

func resourcesSnapshot(
	skills []resource.Skill,
	promptTemplates []resource.PromptTemplate,
) ResourcesSnapshot {
	snap := ResourcesSnapshot{
		Skills:          make([]ResourceSummary, 0, len(skills)),
		PromptTemplates: make([]ResourceSummary, 0, len(promptTemplates)),
	}
	for _, skill := range skills {
		snap.Skills = append(snap.Skills, ResourceSummary{
			Name:        skill.Name,
			Description: skill.Description,
		})
	}
	for _, template := range promptTemplates {
		snap.PromptTemplates = append(snap.PromptTemplates, ResourceSummary{
			Name:        template.Name,
			Description: template.Description,
		})
	}
	return snap
}

func buildSystemPrompt(
	cfg runtimeConfig,
	cwd string,
	defs []extension.ToolDefinition,
) (string, hook.SystemPromptOptions) {
	opts := resource.SystemPromptOptions{
		CustomPrompt:       cfg.systemPrompt,
		AppendSystemPrompt: cfg.appendSystemPrompt,
		CWD:                cwd,
		Tools:              toolInfos(defs),
		ContextFiles:       cfg.contextFiles,
		Skills:             cfg.skills,
		Guidelines:         toolGuidelines(defs),
		HasReadTool:        hasTool(defs, "read"),
	}
	return resource.BuildSystemPrompt(opts), hookSystemPromptOptions(opts)
}

func hookSystemPromptOptions(opts resource.SystemPromptOptions) hook.SystemPromptOptions {
	toolSnippets := make(map[string]string, len(opts.Tools))
	selectedTools := make([]string, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		selectedTools = append(selectedTools, tool.Name)
		// Only tools with a snippet get a map entry, matching pi's
		// _toolPromptSnippets (no empty-string entries). selectedTools still
		// lists every active tool.
		if tool.Snippet != "" {
			toolSnippets[tool.Name] = tool.Snippet
		}
	}

	contextFiles := make([]hook.ContextFileInfo, 0, len(opts.ContextFiles))
	for _, file := range opts.ContextFiles {
		contextFiles = append(contextFiles, hook.ContextFileInfo{
			Path:    file.Path,
			Content: file.Content,
		})
	}

	skills := make([]hook.SkillInfo, 0, len(opts.Skills))
	for _, skill := range opts.Skills {
		skills = append(skills, hook.SkillInfo{
			Name:        skill.Name,
			Description: skill.Description,
		})
	}

	return hook.SystemPromptOptions{
		CustomPrompt:       opts.CustomPrompt,
		SelectedTools:      selectedTools,
		ToolSnippets:       toolSnippets,
		PromptGuidelines:   append([]string{}, opts.Guidelines...),
		AppendSystemPrompt: opts.AppendSystemPrompt,
		CWD:                opts.CWD,
		ContextFiles:       contextFiles,
		Skills:             skills,
	}
}

// toolInfos lists every active tool with its one-line prompt snippet. Tools
// without an explicit PromptSnippet get an empty snippet and are omitted from the
// "Available tools" section by resource.formatTools, mirroring pi (a tool appears
// there only when the caller provides a snippet; there is no description fallback).
func toolInfos(defs []extension.ToolDefinition) []resource.ToolInfo {
	infos := make([]resource.ToolInfo, 0, len(defs))
	for _, def := range defs {
		infos = append(infos, resource.ToolInfo{
			Name:    def.Name,
			Snippet: strings.TrimSpace(def.PromptSnippet),
		})
	}
	return infos
}

func toolDefinitionsInOrder(
	order []string,
	byName map[string]extension.ToolDefinition,
) []extension.ToolDefinition {
	defs := make([]extension.ToolDefinition, 0, len(order))
	for _, name := range order {
		def, ok := byName[name]
		if !ok {
			continue
		}
		defs = append(defs, def)
	}
	return defs
}

// toolGuidelines collects per-tool prompt guidelines. resource.BuildSystemPrompt
// deduplicates them, so two tools sharing a guideline contribute it once.
func toolGuidelines(defs []extension.ToolDefinition) []string {
	var guidelines []string
	for _, def := range defs {
		guidelines = append(guidelines, def.PromptGuidelines...)
	}
	return guidelines
}

func hasTool(defs []extension.ToolDefinition, name string) bool {
	for _, def := range defs {
		if def.Name == name {
			return true
		}
	}
	return false
}

func defaultProviderRegistry(
	model ai.Model,
	getAPIKey func(ctx context.Context, provider string) (string, error),
) *provider.Registry {
	models := ai.BuiltinModels()
	if model.ID != "" && !hasRuntimeModel(models, model) {
		models = append(models, model)
	}

	var authResolver provider.AuthResolver
	if getAPIKey != nil {
		authResolver = getAPIKeyResolver{getAPIKey: getAPIKey}
	}
	return provider.New(models, authResolver)
}

type getAPIKeyResolver struct {
	getAPIKey func(ctx context.Context, provider string) (string, error)
}

func (r getAPIKeyResolver) Resolve(ctx context.Context, providerName string) (auth.Credential, error) {
	key, err := r.getAPIKey(ctx, providerName)
	if err != nil {
		return auth.Credential{}, err
	}
	return auth.Credential{
		Type: auth.TypeAPIKey,
		Key:  key,
	}, nil
}

func resolveRuntimeModel(
	prov *provider.Registry,
	providerName string,
	modelID string,
	fallback ai.Model,
	allowFallback bool,
) (ai.Model, error) {
	if modelID == "" {
		return ai.Model{}, errors.New("orchestrator: model id is required")
	}
	if providerName != "" {
		if model, ok := prov.ResolveByProvider(providerName, modelID); ok {
			return model, nil
		}
		if allowFallback && fallback.Provider == providerName && fallback.ID == modelID {
			return fallback, nil
		}
		return ai.Model{}, unknownModelError(modelID, prov.All())
	}
	if model, ok := prov.Resolve(modelID); ok {
		return model, nil
	}
	if allowFallback && fallback.ID == modelID {
		return fallback, nil
	}
	if matches := matchingModels(prov.All(), modelID); len(matches) > 1 {
		return ai.Model{}, ambiguousModelError(modelID, matches)
	}
	return ai.Model{}, unknownModelError(modelID, prov.All())
}

func providerAuthResolver(
	prov *provider.Registry,
) func(ctx context.Context, model ai.Model) (harness.ProviderAuth, error) {
	return func(ctx context.Context, model ai.Model) (harness.ProviderAuth, error) {
		resolved, err := prov.ResolveAuth(ctx, model)
		if err != nil {
			return harness.ProviderAuth{}, err
		}
		return harness.ProviderAuth{
			APIKey:  resolved.APIKey,
			Headers: resolved.Headers,
		}, nil
	}
}

func unknownModelError(modelID string, models []ai.Model) error {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return fmt.Errorf("orchestrator: unknown model %q", modelID)
	}
	return fmt.Errorf("orchestrator: unknown model %q (available: %s)", modelID, strings.Join(ids, ", "))
}

func ambiguousModelError(modelID string, models []ai.Model) error {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.Provider+"/"+model.ID)
	}
	sort.Strings(ids)
	return fmt.Errorf("orchestrator: ambiguous model %q (matches: %s)", modelID, strings.Join(ids, ", "))
}

func matchingModels(models []ai.Model, id string) []ai.Model {
	matches := []ai.Model{}
	for _, model := range models {
		if model.ID == id {
			matches = append(matches, model)
		}
	}
	return matches
}

func hasModel(models []ai.Model, providerName, id string) bool {
	for _, model := range models {
		if model.Provider == providerName && model.ID == id {
			return true
		}
	}
	return false
}

func hasRuntimeModel(models []ai.Model, target ai.Model) bool {
	if target.Provider != "" {
		return hasModel(models, target.Provider, target.ID)
	}
	for _, model := range models {
		if model.ID == target.ID {
			return true
		}
	}
	return false
}

// firePreflight signals prompt acceptance once, if a callback is set. See
// PromptInput.PreflightResult.
func firePreflight(input PromptInput) {
	if input.PreflightResult != nil {
		input.PreflightResult(true)
	}
}

func (o *Orchestrator) Prompt(ctx context.Context, input PromptInput) (PromptResult, error) {
	runCtx, cancel, state, err := o.beginRun(ctx, phaseTurn, true)
	if err != nil {
		var busy *BusyError
		if input.StreamingBehavior != "" && errors.As(err, &busy) {
			// Mirrors pi streaming prompt routing:
			// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1005-1018.
			userInput := UserInput{Text: input.Text, Content: input.Content}
			if input.StreamingBehavior == "followUp" {
				if err := o.FollowUp(ctx, userInput); err != nil {
					return PromptResult{}, err
				}
			} else {
				if err := o.Steer(ctx, userInput); err != nil {
					return PromptResult{}, err
				}
			}
			firePreflight(input)
			return PromptResult{}, nil
		}
		return PromptResult{}, err
	}
	defer cancel()

	o.setOverflowRecoveryAttempted(false)
	o.resetRetryAttempt()

	var msg ai.Message
	var promptErr error
	var handled bool
	input, handled = o.emitInput(runCtx, input)
	if handled {
		// Input hook consumed the prompt: accepted. pi counts handled/queued
		// prompts as preflight success (rpc-mode.ts:391-403).
		firePreflight(input)
	} else {
		if prePromptMsg, err := o.recoverOverflowBeforePrompt(runCtx, state); err != nil {
			// Pre-prompt compaction failed before the run: preflight not fired,
			// so rpc reports the error as the prompt response (not an event).
			msg = prePromptMsg
			promptErr = err
		} else {
			// Accepted: acceptance checks passed, about to run the irreversible
			// turn. Mirrors pi firing preflightResult(true) after acceptance,
			// before _runAgentPrompt (agent-session.ts:1108).
			firePreflight(input)
			skills, promptTemplates := o.resourceExpansionSnapshot()
			input.Text = resource.ExpandSkillCommand(input.Text, skills, os.ReadFile)
			input.Text = resource.ExpandPromptTemplate(input.Text, promptTemplates)
			messages := o.promptMessages(input)
			msg, promptErr = o.harness.Prompt(runCtx, messages, state)
			if promptErr == nil {
				msg, promptErr = o.recoverOverflowAfterAssistant(runCtx, state, msg)
			}
		}
	}
	if ai.IsRetryableError(msg, state.Model.ContextWindow) {
		retried, err := o.retryTransientError(runCtx, state, msg)
		msg = retried
		if err != nil {
			promptErr = err
		} else {
			promptErr = nil
		}
	} else if promptErr == nil && msg.StopReason != ai.StopReasonError {
		o.resetRetryAttempt()
	}
	// Per-turn save_point/flush is now owned by the turn_end handler
	// (events.go), mirroring pi which flushes + emits save_point at every
	// turn_end and settled once at agent_end (agent-harness.ts:483-535). This
	// end-of-run flush is a safety net for anything queued AFTER the last
	// turn_end (e.g. at agent_end); a final save_point is emitted only if that
	// residual flush actually had pending writes, so save_point is never emitted
	// twice for the same turn.
	o.mu.Lock()
	hadResidualMutations := len(o.pendingWrites) > 0
	o.mu.Unlock()
	settleCtx := context.WithoutCancel(ctx)
	flushErr := o.flushPendingWrites(settleCtx)
	if hadResidualMutations {
		o.emitSavePoint(settleCtx, true)
	}
	o.finishRun()
	o.mu.Lock()
	nextTurnCount := len(o.nextTurnQueue)
	o.mu.Unlock()
	o.emitSettled(settleCtx, nextTurnCount)

	result := PromptResult{Message: msg, Handled: handled}
	if promptErr != nil && flushErr != nil {
		return result, errors.Join(promptErr, flushErr)
	}
	if promptErr != nil {
		return result, promptErr
	}
	if flushErr != nil {
		return result, flushErr
	}
	return result, nil
}

func (o *Orchestrator) Steer(ctx context.Context, input UserInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	msg := userInputMessage(input)
	o.mu.Lock()
	o.steerQueue = append(o.steerQueue, msg)
	ev := o.queueUpdateEventLocked()
	o.mu.Unlock()
	o.publish(ev)
	return nil
}

func (o *Orchestrator) FollowUp(ctx context.Context, input UserInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	msg := userInputMessage(input)
	o.mu.Lock()
	o.followUpQueue = append(o.followUpQueue, msg)
	ev := o.queueUpdateEventLocked()
	o.mu.Unlock()
	o.publish(ev)
	return nil
}

// SetSteeringMode updates steering queue drain behavior.
// pi: .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1588-1590
// and .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:613-621.
func (o *Orchestrator) SetSteeringMode(mode QueueMode) {
	o.mu.Lock()
	o.steeringMode = mode
	o.mu.Unlock()

	o.persistGlobalSetting([]string{"steeringMode"}, string(mode))
}

// SetFollowUpMode updates follow-up queue drain behavior.
// pi: .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1597-1599
// and .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:623-631.
func (o *Orchestrator) SetFollowUpMode(mode QueueMode) {
	o.mu.Lock()
	o.followUpMode = mode
	o.mu.Unlock()

	o.persistGlobalSetting([]string{"followUpMode"}, string(mode))
}

// SetEnableSkillCommands mirrors pi setEnableSkillCommands:
// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:912-915.
func (o *Orchestrator) SetEnableSkillCommands(enabled bool) {
	o.mu.Lock()
	if o.settings == nil {
		o.settings = config.Settings{}
	}
	o.settings["enableSkillCommands"] = enabled
	o.mu.Unlock()

	o.persistGlobalSetting([]string{"enableSkillCommands"}, enabled)
}

func (o *Orchestrator) ReloadSettings(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.mu.Lock()
	if o.phase != phaseIdle {
		currentPhase := o.phase
		o.mu.Unlock()
		return &BusyError{Phase: string(currentPhase)}
	}
	session := o.session
	cwd := o.cwd
	model := o.model
	thinkingLevel := o.thinkingLevel
	settings := cloneSettings(o.settings)
	settingsManager := o.settingsManager
	baseProvider := o.baseProvider
	if baseProvider != nil {
		baseProvider = baseProvider.CloneBase()
	}
	getAPIKey := o.getAPIKey
	repo := o.repo
	customSystemPrompt := o.customSystemPrompt
	extensions := append([]ExtensionSource(nil), o.extensions...)
	hooksPath := o.hooksPath
	baseToolDefs := append([]extension.ToolDefinition(nil), o.baseToolDefs...)
	toolBuilder := o.toolBuilder
	agentDir := o.agentDir
	contextFiles := append([]resource.ContextFile(nil), o.contextFiles...)
	execEnv := o.execEnv
	previous := resourcesSnapshot(o.skills, o.promptTemplates)
	oldRegistry := o.hooks
	o.mu.Unlock()

	// pi reloads the settings manager before rebuilding runtime resources:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2398-2404
	// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:407.
	if settingsManager != nil {
		if err := settingsManager.Reload(); err != nil {
			return err
		}
		settings = settingsManager.Settings()
	}

	promptPaths := settingsStringSliceValue(settings, "prompts")
	skillPaths := settingsStringSliceValue(settings, "skills")
	agentDirForLoad := agentDir
	if agentDirForLoad == "" {
		resolved, err := config.AgentDir()
		if err != nil {
			return fmt.Errorf("orchestrator: resolve agent dir for reload: %w", err)
		}
		agentDirForLoad = resolved
	}
	base, err := loadBaseResources(cwd, agentDirForLoad, promptPaths, skillPaths)
	if err != nil {
		return err
	}
	plugins := plugin.Load(settings, agentDirForLoad, cwd)
	extensions = reloadPluginExtensions(extensions, plugins.Sources)
	if toolBuilder != nil {
		reloadedTools, err := toolBuilder(plugins.BinDirs)
		if err != nil {
			return fmt.Errorf("orchestrator: reload tools: %w", err)
		}
		baseToolDefs = reloadedTools
	}
	if baseProvider == nil {
		baseProvider = defaultProviderRegistry(model, getAPIKey)
	}

	fresh, err := assemble(ctx, session, runtimeConfig{
		model:                  model,
		modelID:                model.ID,
		provider:               baseProvider,
		repo:                   repo,
		thinkingLevel:          thinkingLevel,
		systemPrompt:           customSystemPrompt,
		getAPIKey:              getAPIKey,
		settings:               cloneSettings(settings),
		settingsManager:        settingsManager,
		extensions:             extensions,
		hooksPath:              hooksPath,
		tools:                  baseToolDefs,
		toolBuilder:            toolBuilder,
		promptTemplates:        base.promptTemplates,
		skills:                 base.skills,
		promptPaths:            promptPaths,
		skillPaths:             skillPaths,
		agentDir:               agentDir,
		contextFiles:           contextFiles,
		diagnostics:            append(base.diagnostics, plugins.Diagnostics...),
		executionEnv:           execEnv,
		recoverState:           false,
		resourceDiscoverReason: "reload",
	})
	if err != nil {
		return err
	}

	if err := o.emitSessionShutdown(ctx, "reload", ""); err != nil {
		_ = fresh.Close()
		return err
	}

	current := resourcesSnapshot(fresh.skills, fresh.promptTemplates)
	if oldRegistry != nil && oldRegistry.HasHandlers(hook.EventResourcesUpdate) {
		_, _ = oldRegistry.Emit(ctx, hook.ResourcesUpdateEvent{
			Type:              hook.EventResourcesUpdate,
			Resources:         current,
			PreviousResources: previous,
		})
	}

	o.mu.Lock()
	if o.phase != phaseIdle {
		currentPhase := o.phase
		o.mu.Unlock()
		_ = fresh.Close()
		return &BusyError{Phase: string(currentPhase)}
	}
	o.harness = fresh.harness
	o.hooks = fresh.hooks
	o.provider = fresh.provider
	o.baseProvider = fresh.baseProvider
	o.model = fresh.model
	o.thinkingLevel = fresh.thinkingLevel
	o.systemPrompt = fresh.systemPrompt
	o.systemPromptOptions = fresh.systemPromptOptions
	o.settings = cloneSettings(settings)
	o.settingsManager = settingsManager
	o.getAPIKey = getAPIKey
	o.tools = fresh.tools
	o.toolDefs = fresh.toolDefs
	o.baseToolDefs = fresh.baseToolDefs
	o.toolBuilder = fresh.toolBuilder
	o.extensions = fresh.extensions
	o.hooksPath = fresh.hooksPath
	o.commands = fresh.commands
	o.customSystemPrompt = fresh.customSystemPrompt
	o.promptTemplates = fresh.promptTemplates
	o.skills = fresh.skills
	o.promptPaths = fresh.promptPaths
	o.skillPaths = fresh.skillPaths
	o.agentDir = fresh.agentDir
	o.contextFiles = fresh.contextFiles
	o.diagnostics = fresh.diagnostics
	o.execEnv = fresh.execEnv
	o.autoRetryEnabledOverride = nil
	o.autoCompactionEnabled = fresh.autoCompactionEnabled
	o.steeringMode = fresh.steeringMode
	o.followUpMode = fresh.followUpMode
	o.mu.Unlock()

	o.emitResourcesUpdate(ctx, current, previous)
	return nil
}

func reloadPluginExtensions(existing []ExtensionSource, plugins []ExtensionSource) []ExtensionSource {
	next := make([]ExtensionSource, 0, len(existing)+len(plugins))
	for _, source := range existing {
		if !strings.HasPrefix(source.Path, "plugin:") {
			next = append(next, source)
		}
	}
	return append(next, plugins...)
}

func loadBaseResources(
	cwd string,
	agentDir string,
	promptPaths []string,
	skillPaths []string,
) (loadedResourceSet, error) {
	promptTemplates, diagnostics, err := resource.LoadPromptTemplates(resource.LoadPromptTemplatesOptions{
		CWD:             cwd,
		AgentDir:        agentDir,
		Paths:           promptPaths,
		IncludeDefaults: true,
	})
	out := loadedResourceSet{
		promptTemplates: promptTemplates,
		diagnostics:     append([]resource.ResourceDiagnostic{}, diagnostics...),
	}
	if err != nil {
		return out, fmt.Errorf("orchestrator: reload prompt templates: %w", err)
	}

	skills, diagnostics, err := resource.LoadSkills(resource.LoadSkillsOptions{
		CWD:             cwd,
		AgentDir:        agentDir,
		Paths:           skillPaths,
		IncludeDefaults: true,
	})
	out.skills = skills
	out.diagnostics = append(out.diagnostics, diagnostics...)
	if err != nil {
		return out, fmt.Errorf("orchestrator: reload skills: %w", err)
	}
	return out, nil
}

func (o *Orchestrator) persistGlobalSetting(path []string, value any) {
	if o.settingsManager == nil {
		return
	}
	_ = o.settingsManager.Set(config.ScopeGlobal, path, value)
}

func (o *Orchestrator) Abort(ctx context.Context) (AbortResult, error) {
	if err := ctx.Err(); err != nil {
		return AbortResult{}, err
	}

	o.mu.Lock()
	cancel := o.activeCancel
	queuesChanged := len(o.steerQueue) > 0 || len(o.followUpQueue) > 0
	result := AbortResult{
		Aborted:         cancel != nil,
		ClearedSteer:    cloneMessages(o.steerQueue),
		ClearedFollowUp: cloneMessages(o.followUpQueue),
		ClearedNextTurn: cloneMessages(o.nextTurnQueue),
	}
	o.steerQueue = nil
	o.followUpQueue = nil
	o.nextTurnQueue = nil
	ev := o.queueUpdateEventLocked()
	o.mu.Unlock()

	if queuesChanged {
		o.publish(ev)
	}

	if cancel != nil {
		cancel()
	}
	if o.hooks != nil {
		_, err := o.hooks.Emit(ctx, hook.AbortEvent{
			Type:            hook.EventAbort,
			ClearedSteer:    toAnyMessages(result.ClearedSteer),
			ClearedFollowUp: toAnyMessages(result.ClearedFollowUp),
		})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (o *Orchestrator) WaitForIdle(ctx context.Context) error {
	for {
		o.mu.Lock()
		ch := o.idleCh
		if o.phase == phaseIdle || ch == nil {
			o.mu.Unlock()
			return nil
		}
		o.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

func (o *Orchestrator) SetModel(ctx context.Context, m ai.Model) error {
	return o.setModel(ctx, m, modelSelectSourceSet)
}

func (o *Orchestrator) setModel(ctx context.Context, m ai.Model, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	o.mu.Lock()
	previousModel := o.model
	o.model = m
	if o.phase != phaseIdle {
		o.pendingWrites = append(o.pendingWrites, modelChange{model: m})
		o.mu.Unlock()
		o.emitModelSelect(ctx, m, previousModel, source)
		return nil
	}
	o.mu.Unlock()

	if _, err := o.session.AppendModelChange(ctx, m.Provider, m.ID); err != nil {
		return err
	}
	o.emitModelSelect(ctx, m, previousModel, source)
	return nil
}

func (o *Orchestrator) SetThinkingLevel(ctx context.Context, level agentloop.ThinkingLevel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if level == "" {
		level = agentloop.ThinkingOff
	}

	o.mu.Lock()
	previousLevel := o.thinkingLevel
	o.thinkingLevel = level
	if level == previousLevel {
		o.mu.Unlock()
		return nil
	}
	if o.phase != phaseIdle {
		o.pendingWrites = append(o.pendingWrites, thinkingLevelChange{level: level})
		o.mu.Unlock()
		// pi emits thinking_level_changed after the level changes:
		// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1525.
		o.publish(Event{Type: EventThinkingLevelChanged, Level: string(level)})
		o.emitThinkingLevelSelect(ctx, level, previousLevel)
		return nil
	}
	o.mu.Unlock()

	if _, err := o.session.AppendThinkingLevelChange(ctx, string(level)); err != nil {
		return err
	}
	// pi emits thinking_level_changed after persisting the changed level:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1525.
	o.publish(Event{Type: EventThinkingLevelChanged, Level: string(level)})
	o.emitThinkingLevelSelect(ctx, level, previousLevel)
	return nil
}

func (o *Orchestrator) Compact(ctx context.Context, opts CompactOptions) (CompactResult, error) {
	runCtx, cancel, state, err := o.beginRun(ctx, phaseCompaction, false)
	if err != nil {
		return CompactResult{}, err
	}
	defer cancel()

	// pi emits compaction_start for manual compaction before compacting:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1615.
	o.publishCompactionStart(manualCompactionReason)
	result, compactErr := o.harness.Compact(runCtx, state, opts.CustomInstructions)
	flushErr := o.flushPendingWrites(context.WithoutCancel(ctx))
	o.finishRun()

	endErr := errors.Join(compactErr, flushErr)
	o.publishCompactionEnd(
		manualCompactionReason,
		result,
		compactionAborted(endErr),
		false,
		endErr,
	)

	if compactErr != nil && flushErr != nil {
		return result, errors.Join(compactErr, flushErr)
	}
	if compactErr != nil {
		return result, compactErr
	}
	if flushErr != nil {
		return result, flushErr
	}
	return result, nil
}

func (o *Orchestrator) NavigateTree(
	ctx context.Context,
	target session.EntryID,
	opts NavOptions,
) (NavResult, error) {
	runCtx, cancel, state, err := o.beginRun(ctx, phaseBranchSummary, false)
	if err != nil {
		return NavResult{}, err
	}
	defer cancel()

	result, navErr := o.harness.NavigateTree(runCtx, target, state, opts)
	flushErr := o.flushPendingWrites(context.WithoutCancel(ctx))
	o.finishRun()

	if navErr != nil && flushErr != nil {
		return result, errors.Join(navErr, flushErr)
	}
	if navErr != nil {
		return result, navErr
	}
	if flushErr != nil {
		return result, flushErr
	}
	return result, nil
}

func (o *Orchestrator) DispatchCommand(
	ctx context.Context,
	name string,
	args []string,
) ([]CommandNotification, error) {
	o.mu.Lock()
	def, ok := o.commands[name]
	o.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("orchestrator: command %q not found", name)
	}
	if def.Handler == nil {
		return nil, fmt.Errorf("orchestrator: command %q has no handler", name)
	}
	extCtx := o.extensionContext(ctx)
	var notifications []CommandNotification
	extCtx.Notify = func(message, level string) {
		if message == "" {
			return
		}
		notifications = append(notifications, CommandNotification{
			Message: message,
			Level:   normalizeCommandNotificationLevel(level),
		})
	}
	if err := def.Handler(ctx, append([]string(nil), args...), extCtx); err != nil {
		return nil, err
	}
	return notifications, nil
}

func normalizeCommandNotificationLevel(level string) string {
	switch level {
	case "warning", "error":
		return level
	default:
		return "info"
	}
}

func (o *Orchestrator) beginRun(
	ctx context.Context,
	p phase,
	clearPending bool,
) (context.Context, context.CancelFunc, harness.TurnState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.phase != phaseIdle {
		return nil, nil, harness.TurnState{}, &BusyError{Phase: string(o.phase)}
	}

	state := o.turnStateLocked()
	runCtx, cancel := context.WithCancel(ctx)
	o.phase = p
	o.idleCh = make(chan struct{})
	o.activeCancel = cancel
	if clearPending {
		o.pendingWrites = nil
	}
	return runCtx, cancel, state, nil
}

func (o *Orchestrator) finishRun() {
	o.mu.Lock()
	ch := o.idleCh
	o.phase = phaseIdle
	o.activeCancel = nil
	o.idleCh = nil
	o.mu.Unlock()

	if ch != nil {
		close(ch)
	}
}

func (o *Orchestrator) emitInput(ctx context.Context, input PromptInput) (PromptInput, bool) {
	if o.hooks == nil || !o.hooks.HasHandlers(hook.EventInput) {
		return input, false
	}

	source := input.Source
	if source == "" {
		source = "interactive"
	}

	currentText := input.Text
	currentImages := hookImagesFromContent(input.Content)
	transformed := false

	for _, handler := range o.hooks.Handlers(hook.EventInput) {
		result, err := handler(ctx, hook.InputEvent{
			Type:   hook.EventInput,
			Text:   currentText,
			Images: cloneHookImages(currentImages),
			Source: source,
		})
		if err != nil {
			continue
		}
		if result == nil {
			continue
		}

		r, ok := result.(hook.InputResult)
		if !ok {
			continue
		}

		switch r.Action {
		case "handled":
			return input, true
		case "transform":
			currentText = r.Text
			if r.Images != nil {
				currentImages = cloneHookImages(r.Images)
			}
			transformed = true
		}
	}

	if !transformed {
		return input, false
	}

	input.Text = currentText
	input.Content = contentFromHookImages(currentImages)
	input.Message = nil
	input.Source = source
	return input, false
}

func (o *Orchestrator) promptMessages(input PromptInput) []message.AgentMessage {
	userMessage := promptInputMessage(input)
	messages := []message.AgentMessage{userMessage}

	o.mu.Lock()
	pendingNextTurn := cloneMessages(o.nextTurnQueue)
	o.nextTurnQueue = nil
	o.mu.Unlock()

	// pi appends the current user message first, then pending nextTurn messages:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1057-1071.
	messages = append(messages, pendingNextTurn...)
	return messages
}

func (o *Orchestrator) resourceExpansionSnapshot() (
	[]resource.Skill,
	[]resource.PromptTemplate,
) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]resource.Skill(nil), o.skills...),
		append([]resource.PromptTemplate(nil), o.promptTemplates...)
}

func (o *Orchestrator) turnState() harness.TurnState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.turnStateLocked()
}

func (o *Orchestrator) turnStateLocked() harness.TurnState {
	model := o.model
	thinkingLevel := o.thinkingLevel
	systemPrompt := o.systemPrompt
	systemPromptOptions := cloneSystemPromptOptions(o.systemPromptOptions)
	tools := append([]tool.Tool(nil), o.tools...)

	return harness.TurnState{
		Model:               model,
		ThinkingLevel:       thinkingLevel,
		SystemPrompt:        systemPrompt,
		SystemPromptOptions: systemPromptOptions,
		ActiveTools:         tools,
		GetSteeringMessages: o.drainSteeringMessages,
		GetFollowUpMessages: o.drainFollowUpMessages,
		SessionID:           o.session.GetMetadata().ID,
		Refresh: func() (ai.Model, agentloop.ThinkingLevel) {
			o.mu.Lock()
			defer o.mu.Unlock()
			return o.model, o.thinkingLevel
		},
	}
}

func cloneSystemPromptOptions(opts hook.SystemPromptOptions) hook.SystemPromptOptions {
	return hook.SystemPromptOptions{
		CustomPrompt:       opts.CustomPrompt,
		SelectedTools:      append([]string{}, opts.SelectedTools...),
		ToolSnippets:       cloneStringMap(opts.ToolSnippets),
		PromptGuidelines:   append([]string{}, opts.PromptGuidelines...),
		AppendSystemPrompt: opts.AppendSystemPrompt,
		CWD:                opts.CWD,
		ContextFiles:       append([]hook.ContextFileInfo{}, opts.ContextFiles...),
		Skills:             append([]hook.SkillInfo{}, opts.Skills...),
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func (o *Orchestrator) drainSteeringMessages(ctx context.Context) ([]message.AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	var drained []message.AgentMessage
	if o.steeringMode == QueueModeAll {
		drained = drainAll(&o.steerQueue)
	} else {
		drained = drainOne(&o.steerQueue)
	}
	ev := o.queueUpdateEventLocked()
	o.mu.Unlock()
	if len(drained) > 0 {
		o.publish(ev)
	}
	return drained, nil
}

func (o *Orchestrator) drainFollowUpMessages(ctx context.Context) ([]message.AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	var drained []message.AgentMessage
	if o.followUpMode == QueueModeAll {
		drained = drainAll(&o.followUpQueue)
	} else {
		drained = drainOne(&o.followUpQueue)
	}
	ev := o.queueUpdateEventLocked()
	o.mu.Unlock()
	if len(drained) > 0 {
		o.publish(ev)
	}
	return drained, nil
}

func drainOne(queue *[]message.AgentMessage) []message.AgentMessage {
	if len(*queue) == 0 {
		return nil
	}
	msg := (*queue)[0]
	*queue = (*queue)[1:]
	return []message.AgentMessage{message.Snapshot(msg)}
}

func drainAll(queue *[]message.AgentMessage) []message.AgentMessage {
	if len(*queue) == 0 {
		return nil
	}
	out := cloneMessages(*queue)
	*queue = nil
	return out
}

func (o *Orchestrator) flushPendingWrites(ctx context.Context) error {
	// Per-turn settle: a turn_end hook handler (events.go) calls this at every
	// turn_end so mid-run config-change writes persist per turn, matching pi's
	// flushPendingSessionWrites at each turn_end (agent-harness.ts:484-535).
	// Messages persist eagerly at MessageEnd (harness/prompt.go) and are
	// intentionally NOT routed through pendingWrites — only config changes
	// (model/thinking level) are queued here. Callers must not hold o.mu.
	o.mu.Lock()
	defer o.mu.Unlock()

	for _, write := range o.pendingWrites {
		if err := write.apply(ctx, o.session); err != nil {
			return err
		}
	}
	o.pendingWrites = nil
	return nil
}

func hookImagesFromContent(content []ai.ContentBlock) []hook.ImageContent {
	var images []hook.ImageContent
	for _, block := range content {
		if block.Type != ai.ContentImage {
			continue
		}
		images = append(images, hook.ImageContent{
			MimeType: block.MimeType,
			Data:     block.ImageData,
		})
	}
	return images
}

func contentFromHookImages(images []hook.ImageContent) []ai.ContentBlock {
	if images == nil {
		return nil
	}
	content := make([]ai.ContentBlock, 0, len(images))
	for _, image := range images {
		content = append(content, ai.ContentBlock{
			Type:      ai.ContentImage,
			MimeType:  image.MimeType,
			ImageData: image.Data,
		})
	}
	return content
}

func cloneHookImages(images []hook.ImageContent) []hook.ImageContent {
	if images == nil {
		return nil
	}
	return append([]hook.ImageContent(nil), images...)
}

func promptInputMessage(input PromptInput) message.AgentMessage {
	if input.Message != nil {
		return message.Snapshot(input.Message)
	}
	return ai.Message{
		Role:      ai.RoleUser,
		Content:   contentBlocks(input.Text, input.Content),
		Timestamp: time.Now().UnixMilli(),
	}
}

func userInputMessage(input UserInput) message.AgentMessage {
	if input.Message != nil {
		return message.Snapshot(input.Message)
	}
	return ai.Message{
		Role:      ai.RoleUser,
		Content:   contentBlocks(input.Text, input.Content),
		Timestamp: time.Now().UnixMilli(),
	}
}

func contentBlocks(text string, content []ai.ContentBlock) []ai.ContentBlock {
	out := make([]ai.ContentBlock, 0, len(content)+1)
	if text != "" {
		out = append(out, ai.ContentBlock{Type: ai.ContentText, Text: text})
	}
	out = append(out, content...)
	return out
}

func cloneMessages(in []message.AgentMessage) []message.AgentMessage {
	out := make([]message.AgentMessage, 0, len(in))
	for _, msg := range in {
		out = append(out, message.Snapshot(msg))
	}
	return out
}

func (o *Orchestrator) queueUpdateEventLocked() Event {
	// pi _emitQueueUpdate sends the current string queues:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:457-459.
	return Event{
		Type:     EventQueueUpdate,
		Steering: queueMessageTexts(o.steerQueue),
		FollowUp: queueMessageTexts(o.followUpQueue),
	}
}

func queueMessageTexts(messages []message.AgentMessage) []string {
	texts := make([]string, 0, len(messages))
	for _, msg := range messages {
		aiMsg, ok := message.AsAIMessage(msg)
		if !ok {
			texts = append(texts, "")
			continue
		}
		texts = append(texts, extractUserMessageText(aiMsg.Content))
	}
	return texts
}

func (o *Orchestrator) publishCompactionStart(reason string) {
	o.publish(Event{
		Type:   EventCompactionStart,
		Reason: reason,
	})
}

func (o *Orchestrator) publishCompactionEnd(
	reason string,
	result any,
	aborted bool,
	willRetry bool,
	err error,
) {
	ev := Event{
		Type:      EventCompactionEnd,
		Reason:    reason,
		Aborted:   aborted,
		WillRetry: willRetry,
	}
	if err == nil {
		ev.Result = result
	} else if !aborted {
		ev.ErrorMessage = compactionErrorMessage(reason, err)
	}
	// pi emits compaction_end with reason/result/aborted/willRetry/errorMessage:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1716-1734,1996-2021.
	o.publish(ev)
}

func compactionAborted(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return strings.EqualFold(err.Error(), "compaction cancelled") ||
		strings.EqualFold(err.Error(), "compaction canceled")
}

func compactionErrorMessage(reason string, err error) string {
	switch reason {
	case manualCompactionReason:
		return "Compaction failed: " + err.Error()
	case overflowCompactionReason:
		return "Context overflow recovery failed: " + err.Error()
	default:
		return "Auto-compaction failed: " + err.Error()
	}
}

func toAnyMessages(in []message.AgentMessage) []any {
	out := make([]any, len(in))
	for i, msg := range in {
		out[i] = msg
	}
	return out
}
