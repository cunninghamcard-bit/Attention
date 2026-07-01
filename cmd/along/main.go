package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/auth"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	printmode "github.com/cunninghamcard-bit/Attention/internal/mode/print"
	rpcmode "github.com/cunninghamcard-bit/Attention/internal/mode/rpc"
	"github.com/cunninghamcard-bit/Attention/internal/obs"
	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type modeRunner func(context.Context, *orchestrator.Orchestrator, []string) error

var runPrintMode modeRunner = func(ctx context.Context, orch *orchestrator.Orchestrator, prompts []string) error {
	return printmode.Run(ctx, orch, prompts)
}

var runJSONMode modeRunner = rpcmode.Run

func main() {
	// SIGHUP included: bash children run with Setpgid and never receive the
	// terminal's HUP, so along must cancel them itself — pi kills tracked
	// detached children on shutdown signals (utils/shell.ts:167-185).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	obs.Reset()

	if handled, err := runPluginCommand(ctx, os.Args[1:]); handled {
		return err
	}

	fs := flag.NewFlagSet("along", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	modelID := fs.String("model", "claude-sonnet-4-5", "model ID")
	providerFlag := fs.String("provider", "", "provider name used to resolve the model id")
	mode := fs.String("mode", "print", "output mode: print, json, or rpc")
	apiKey := fs.String("api-key", "", "API key override for the selected model's provider (highest priority, not persisted)")
	// System prompt.
	systemPromptFlag := fs.String("system-prompt", "", "system prompt (replaces the default coding-assistant prompt)")
	var appendSystemPrompt repeatableFlag
	fs.Var(&appendSystemPrompt, "append-system-prompt", "append text to the system prompt (can be used multiple times)")
	// Thinking level.
	thinkingFlag := fs.String("thinking", "", "thinking level: low, medium, or high")
	// Tools.
	toolsFlag := new(string)
	stringFlag(fs, toolsFlag, "tools", "t", "", "comma-separated allowlist of tool names to enable")
	excludeToolsFlag := new(string)
	stringFlag(fs, excludeToolsFlag, "exclude-tools", "xt", "", "comma-separated denylist of tool names to disable")
	noToolsFlag := new(bool)
	boolFlag(fs, noToolsFlag, "no-tools", "nt", "disable all tools")
	noBuiltinToolsFlag := new(bool)
	boolFlag(fs, noBuiltinToolsFlag, "no-builtin-tools", "nbt", "disable built-in tools")
	// Session directory.
	sessionDirFlag := fs.String("session-dir", "", "directory for session storage and lookup")
	// Resources.
	var skillFlag repeatableFlag
	fs.Var(&skillFlag, "skill", "load a skill file or directory (can be used multiple times)")
	noSkillsFlag := new(bool)
	boolFlag(fs, noSkillsFlag, "no-skills", "ns", "disable skills discovery and loading")
	var promptTemplateFlag repeatableFlag
	fs.Var(&promptTemplateFlag, "prompt-template", "load a prompt template file or directory (can be used multiple times)")
	noPromptTemplatesFlag := new(bool)
	boolFlag(fs, noPromptTemplatesFlag, "no-prompt-templates", "np", "disable prompt template discovery and loading")
	noContextFilesFlag := new(bool)
	boolFlag(fs, noContextFilesFlag, "no-context-files", "nc", "disable AGENTS.md and CLAUDE.md discovery and loading")
	// Session name / fork.
	nameFlag := new(string)
	stringFlag(fs, nameFlag, "name", "n", "", "set session display name")
	forkFlag := fs.String("fork", "", "fork a session file or partial id into a new session")
	// Early-exit flags.
	listModelsFlag := fs.Bool("list-models", false, "list available models and exit")
	versionFlag := new(bool)
	boolFlag(fs, versionFlag, "version", "v", "show version number and exit")
	// Session-selection flags, ported from pi (main.ts createSessionManager) and
	// adapted for a headless kernel. See session_plan.go for headless semantics.
	sessionFlag := fs.String("session", "", "resume a specific session by path or id")
	continueFlag := fs.Bool("continue", false, "resume the most recent session for the current directory")
	resumeFlag := fs.Bool("resume", false, "resume the most recent session (headless has no interactive picker)")
	sessionIDFlag := fs.String("session-id", "", "resume the session with this id, or create it if absent")
	noSessionFlag := fs.Bool("no-session", false, "use an ephemeral, non-persisted session")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// --version short-circuits before any setup, like pi.
	if *versionFlag {
		fmt.Println(versionString())
		return nil
	}
	if err := validateMode(*mode); err != nil {
		return err
	}
	thinkingLevel, err := resolveThinkingLevel(*thinkingFlag)
	if err != nil {
		return err
	}
	if err := validateName(*nameFlag, fs); err != nil {
		return err
	}
	sessFlags := sessionFlags{
		session:   *sessionFlag,
		cont:      *continueFlag,
		resume:    *resumeFlag,
		sessionID: *sessionIDFlag,
		noSession: *noSessionFlag,
		fork:      *forkFlag,
	}
	if err := validateSessionFlags(sessFlags); err != nil {
		return err
	}

	cfg, err := config.Load(ctx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// pi degrades corrupt settings files to defaults and surfaces the parse
	// error as a warning (settings-manager.ts:326-335).
	for _, settingsErr := range cfg.SettingsErrors {
		fmt.Fprintf(os.Stderr, "Warning: %s settings: %v\n", settingsErr.Scope, settingsErr.Err)
	}
	obs.Time("config load")

	prov, err := buildProvider(ctx, cfg)
	if err != nil {
		return err
	}
	// --list-models prints the available models and exits without building the
	// orchestrator or running a prompt (pi: --list-models).
	if *listModelsFlag {
		printModels(os.Stdout, prov.All())
		return nil
	}
	// pi reads a default model from settings (settings key `defaultModel`,
	// settings-manager.ts:83 + main.ts:386 getDefaultModel()): when --model was
	// not explicitly passed, settings.defaultModel (if non-empty) overrides the
	// hardcoded fallback. An explicit --model always wins.
	modelExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "model" {
			modelExplicit = true
		}
	})
	if !modelExplicit {
		if def := settingsString(cfg.Settings, "defaultModel"); def != "" {
			*modelID = def
		}
	}
	if err := resolveModelWithProvider(prov, *modelID, *providerFlag); err != nil {
		return err
	}
	// pi applies --api-key as a runtime override for the selected model's
	// provider: .agents/references/pi/packages/coding-agent/src/main.ts:586.
	if *apiKey != "" {
		if m, ok := resolveModelHint(prov, *modelID, *providerFlag); ok {
			prov.SetRuntimeAPIKey(m.Provider, *apiKey)
		}
	}
	obs.Time("provider build")

	// rpc is bidirectional: it reads commands from stdin, not a one-shot prompt.
	var prompts []string
	if *mode != "rpc" {
		prompts, err = buildPrompts(ctx, fs.Args())
		if err != nil {
			return err
		}
		if len(prompts) == 0 {
			return errors.New("no prompt provided (pass a message or pipe stdin)")
		}
	}

	defaultRoot, err := sessionsRoot()
	if err != nil {
		return err
	}
	// --session-dir > ALONG_CODING_AGENT_SESSION_DIR env (honored inside
	// sessionsRoot) > default. An explicit flag value expands a leading ~.
	root, err := resolveSessionDir(*sessionDirFlag, defaultRoot)
	if err != nil {
		return err
	}
	procCWD, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
	}

	repo := session.NewJsonlSessionRepo(root)
	// Resolve the session target before building the cwd-dependent runtime: a
	// resumed session may carry a different recorded CWD than the process, and
	// for headless we load context files / system prompt against that CWD.
	plan, err := resolveSessionPlan(ctx, repo, procCWD, sessFlags)
	if err != nil {
		return err
	}
	cwd := plan.cwd
	if cwd == "" {
		cwd = procCWD
	}

	settings := cfg.Settings
	settingsManager, err := config.NewManager(cfg.AgentDir, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load settings manager: %v\n", err)
		settingsManager = nil
	} else {
		settings = settingsManager.Settings()
	}

	shellPath := settingsString(settings, "shellPath")
	shellCommandPrefix := settingsString(settings, "shellCommandPrefix")
	env := local.New(cwd, local.WithShell(shellPath))

	// --prompt-template / --skill append explicit paths to the settings-derived
	// ones; --no-* flags skip discovery entirely (empty result, no diagnostics).
	promptPaths := append(settingsStringSlice(settings, "prompts"), promptTemplateFlag...)
	skillPaths := append(settingsStringSlice(settings, "skills"), skillFlag...)
	resourceDiagnostics := []resource.ResourceDiagnostic{}

	var templates []resource.PromptTemplate
	if *noPromptTemplatesFlag {
		promptPaths = []string{}
	} else {
		var templateDiagnostics []resource.ResourceDiagnostic
		templates, templateDiagnostics, err = resource.LoadPromptTemplates(resource.LoadPromptTemplatesOptions{
			CWD:             cwd,
			AgentDir:        cfg.AgentDir,
			Paths:           promptPaths,
			IncludeDefaults: true,
		})
		resourceDiagnostics = append(resourceDiagnostics, templateDiagnostics...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load prompt templates: %v\n", err)
			templates = nil
		}
	}

	var skills []resource.Skill
	if *noSkillsFlag {
		skillPaths = []string{}
	} else {
		var skillDiagnostics []resource.ResourceDiagnostic
		skills, skillDiagnostics, err = resource.LoadSkills(resource.LoadSkillsOptions{
			CWD:             cwd,
			AgentDir:        cfg.AgentDir,
			Paths:           skillPaths,
			IncludeDefaults: true,
		})
		resourceDiagnostics = append(resourceDiagnostics, skillDiagnostics...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load skills: %v\n", err)
			skills = nil
		}
	}

	var projectContext []resource.ContextFile
	if !*noContextFilesFlag {
		var contextDiagnostics []resource.ResourceDiagnostic
		projectContext, contextDiagnostics = resource.LoadContextFiles(cwd, cfg.AgentDir)
		resourceDiagnostics = append(resourceDiagnostics, contextDiagnostics...)
	}
	plugins := plugin.Load(settings, cfg.AgentDir, cwd)
	resourceDiagnostics = append(resourceDiagnostics, plugins.Diagnostics...)
	logResourceDiagnostics(os.Stderr, resourceDiagnostics)
	obs.Time("context/skills/templates load")

	// Declarative shell hooks live at <agentDir>/hooks.json; a missing file is
	// a no-op (LoadShellHooks returns nil,nil).
	hooksPath := filepath.Join(cfg.AgentDir, "hooks.json")

	// Tool selection: --no-tools/--no-builtin-tools/--tools/--exclude-tools.
	// Precedence and base sets are handled in selectTools (flags.go). The full
	// tool set is passed as a thunk so it is only built when --tools is given.
	tools, err := selectTools(
		toolSelection{
			tools:         splitCommaList(*toolsFlag),
			excludeTools:  splitCommaList(*excludeToolsFlag),
			noTools:       *noToolsFlag,
			noBuiltinTool: *noBuiltinToolsFlag,
		},
		baseToolSet(env, shellCommandPrefix, plugins.BinDirs...),
		func() []extension.ToolDefinition { return allToolSet(env, shellCommandPrefix, plugins.BinDirs...) },
	)
	if err != nil {
		return err
	}

	// Append-system-prompt entries join with blank lines, mirroring pi's
	// concatenation of repeated --append-system-prompt values.
	appendSystemPromptText := strings.Join(appendSystemPrompt, "\n\n")

	// Shared option fields, identical between the New and Open constructor
	// paths. Only Repo/CreateOptions/Metadata/Session differ per plan, so they
	// are applied below rather than duplicated here.
	common := orchestratorCommonOptions{
		ModelID:            *modelID,
		ModelProvider:      *providerFlag,
		Provider:           prov,
		Settings:           settings,
		SettingsManager:    settingsManager,
		HooksPath:          hooksPath,
		SystemPrompt:       *systemPromptFlag,
		AppendSystemPrompt: appendSystemPromptText,
		ThinkingLevel:      thinkingLevel,
		PromptTemplates:    templates,
		Skills:             skills,
		PromptPaths:        promptPaths,
		SkillPaths:         skillPaths,
		AgentDir:           cfg.AgentDir,
		ContextFiles:       projectContext,
		Diagnostics:        resourceDiagnostics,
		ExecutionEnv:       env,
		Tools:              tools,
		Extensions:         plugins.Sources,
	}

	orch, err := buildOrchestrator(ctx, repo, plan, common)
	if err != nil {
		return fmt.Errorf("create orchestrator: %w", err)
	}
	// --name sets the display name on a freshly created session (new or forked);
	// pi sets the name right after the session is created.
	if name := strings.TrimSpace(*nameFlag); name != "" && (plan.kind == planNew || plan.kind == planFork) {
		if err := orch.SetSessionName(ctx, name); err != nil {
			if closeErr := orch.Close(); closeErr != nil {
				return errors.Join(fmt.Errorf("set session name: %w", err), fmt.Errorf("close orchestrator: %w", closeErr))
			}
			return fmt.Errorf("set session name: %w", err)
		}
	}
	obs.Time("orchestrator create")
	obs.Report(os.Stderr)

	runErr := runPromptMode(ctx, *mode, orch, prompts)
	// pi disposes on EVERY exit path, emitting session_shutdown so extension and
	// hook shutdown handlers run (agent-harness.ts / print-mode finally). rpc mode
	// already emits session_shutdown in serve()'s defer, so only the print/json
	// (non-rpc) one-shot path needs it here — guard on the mode to avoid a double
	// emit. ctx may already be cancelled by a signal (NotifyContext above), so use
	// context.WithoutCancel to let shutdown handlers run anyway.
	if shutdownErr := shutdownOrchestrator(ctx, *mode, orch); shutdownErr != nil {
		if runErr != nil {
			return errors.Join(runErr, shutdownErr)
		}
		return shutdownErr
	}
	if runErr != nil {
		return runErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func validateMode(mode string) error {
	switch mode {
	case "print", "json", "rpc":
		return nil
	default:
		return fmt.Errorf("unknown mode %q (want print, json, or rpc)", mode)
	}
}

func logResourceDiagnostics(w io.Writer, diagnostics []resource.ResourceDiagnostic) {
	if w == nil {
		return
	}
	for _, diagnostic := range diagnostics {
		kind := diagnostic.Type
		if kind == "" {
			kind = resource.DiagnosticWarning
		}
		if diagnostic.Path != "" {
			fmt.Fprintf(w, "%s: %s (%s)\n", kind, diagnostic.Message, diagnostic.Path)
			continue
		}
		fmt.Fprintf(w, "%s: %s\n", kind, diagnostic.Message)
	}
}

func settingsString(settings config.Settings, key string) string {
	value, ok := settings[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

// pi: .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:840-876.
func settingsStringSlice(settings config.Settings, key string) []string {
	value, ok := settings[key]
	if !ok {
		return []string{}
	}
	switch items := value.(type) {
	case []any:
		result := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if ok {
				result = append(result, text)
			}
		}
		return result
	case []string:
		return append([]string(nil), items...)
	default:
		return []string{}
	}
}

func runPromptMode(
	ctx context.Context,
	mode string,
	orch *orchestrator.Orchestrator,
	prompts []string,
) error {
	switch mode {
	case "print":
		return runPrintMode(ctx, orch, prompts)
	case "json":
		return runJSONMode(ctx, orch, prompts)
	case "rpc":
		return rpcmode.Serve(ctx, orch)
	default:
		return validateMode(mode)
	}
}

// shutdownTarget is the orchestrator surface used on the one-shot exit path.
type shutdownTarget interface {
	NotifySessionShutdown(context.Context, string) error
	Close() error
}

// shutdownOrchestrator runs the print/json (non-rpc) exit path: it emits
// session_shutdown before Close so extension/hook shutdown handlers run for
// one-shot runs, mirroring pi's dispose-on-every-exit-path behaviour. rpc mode
// already emits session_shutdown in serve()'s defer, so it is skipped here to
// avoid a double emit. The shutdown ctx is detached because the run ctx may
// already be cancelled by a signal.
func shutdownOrchestrator(ctx context.Context, mode string, orch shutdownTarget) error {
	var shutdownErr error
	if mode != "rpc" {
		if err := orch.NotifySessionShutdown(context.WithoutCancel(ctx), "quit"); err != nil {
			shutdownErr = fmt.Errorf("shutdown orchestrator: %w", err)
		}
	}
	if err := orch.Close(); err != nil {
		return errors.Join(shutdownErr, fmt.Errorf("close orchestrator: %w", err))
	}
	return shutdownErr
}

func buildProvider(ctx context.Context, cfg config.Config) (*provider.Registry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store, err := auth.NewStore("")
	if err != nil {
		return nil, fmt.Errorf("create auth store: %w", err)
	}
	modelsConfig, err := provider.ParseModelsConfig(cfg.ModelsJSON)
	if err != nil {
		return nil, fmt.Errorf("parse models.json: %w", err)
	}

	prov := provider.New(ai.BuiltinModels(), auth.NewResolver(store))
	if err := prov.ApplyConfig(modelsConfig); err != nil {
		return nil, fmt.Errorf("apply models.json: %w", err)
	}
	return prov, nil
}

func resolveModel(prov *provider.Registry, modelID string) error {
	if _, ok := prov.Resolve(modelID); ok {
		return nil
	}
	return unknownModelError(modelID, prov.All())
}

// resolveModelHint resolves a model id, honoring an optional --provider hint to
// disambiguate ids that exist under multiple providers.
func resolveModelHint(prov *provider.Registry, modelID, providerHint string) (ai.Model, bool) {
	if providerHint != "" {
		return prov.ResolveByProvider(providerHint, modelID)
	}
	return prov.Resolve(modelID)
}

// resolveModelWithProvider validates that modelID resolves under the optional
// --provider hint, producing a friendly error otherwise.
func resolveModelWithProvider(prov *provider.Registry, modelID, providerHint string) error {
	if _, ok := resolveModelHint(prov, modelID, providerHint); ok {
		return nil
	}
	if providerHint != "" {
		return fmt.Errorf("unknown model %q for provider %q (available: %s)", modelID, providerHint, strings.Join(modelIDs(prov.All()), ", "))
	}
	return unknownModelError(modelID, prov.All())
}

// printModels writes the available models (id and provider) to w, one per line,
// sorted by id then provider. Used by --list-models.
func printModels(w io.Writer, models []ai.Model) {
	sorted := append([]ai.Model(nil), models...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Provider < sorted[j].Provider
	})
	for _, model := range sorted {
		if model.ID == "" {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\n", model.ID, model.Provider)
	}
}

func modelIDs(models []ai.Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	sort.Strings(ids)
	return ids
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
		return fmt.Errorf("unknown model %q", modelID)
	}
	return fmt.Errorf("unknown model %q (available: %s)", modelID, strings.Join(ids, ", "))
}

// buildPrompts mirrors pi: piped stdin content and the first positional
// message merge verbatim into the initial prompt
// (initial-message.ts:26-44); every remaining positional arg becomes its own
// sequential prompt (print-mode.ts:120-126).
func buildPrompts(ctx context.Context, args []string) ([]string, error) {
	stdinContent, hasStdin, err := readPipedStdin(ctx)
	if err != nil {
		return nil, err
	}

	prompts := []string{}
	if hasStdin || len(args) > 0 {
		first := stdinContent
		if len(args) > 0 {
			first += args[0]
			args = args[1:]
		}
		prompts = append(prompts, first)
	}
	return append(prompts, args...), nil
}

// readPipedStdin reads stdin only when it is piped; pi never reads a TTY
// (main.ts:630-645).
func readPipedStdin(ctx context.Context) (string, bool, error) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return "", false, nil
	}

	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(os.Stdin)
		ch <- readResult{data: data, err: err}
	}()

	select {
	case result := <-ch:
		if result.err != nil {
			return "", false, fmt.Errorf("read prompt from stdin: %w", result.err)
		}
		return string(result.data), true, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

func sessionsRoot() (string, error) {
	root, err := config.SessionDir()
	if err != nil {
		return "", fmt.Errorf("resolve sessions directory: %w", err)
	}
	return root, nil
}
