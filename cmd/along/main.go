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

	printmode "github.com/cunninghamcard-bit/Attention/internal/mode/print"
	rpcmode "github.com/cunninghamcard-bit/Attention/internal/mode/rpc"
	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/auth"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/obs"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
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

	fs := flag.NewFlagSet("along", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	modelID := fs.String("model", "claude-sonnet-4-5", "model ID")
	mode := fs.String("mode", "print", "output mode: print, json, or rpc")
	apiKey := fs.String("api-key", "", "API key override for the selected model's provider (highest priority, not persisted)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := validateMode(*mode); err != nil {
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
	if err := resolveModel(prov, *modelID); err != nil {
		return err
	}
	// pi applies --api-key as a runtime override for the selected model's
	// provider: .agents/references/pi/packages/coding-agent/src/main.ts:586.
	if *apiKey != "" {
		if m, ok := prov.Resolve(*modelID); ok {
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

	root, err := sessionsRoot()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
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

	promptPaths := settingsStringSlice(settings, "prompts")
	skillPaths := settingsStringSlice(settings, "skills")
	resourceDiagnostics := []resource.ResourceDiagnostic{}
	templates, templateDiagnostics, err := resource.LoadPromptTemplates(resource.LoadPromptTemplatesOptions{
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
	skills, skillDiagnostics, err := resource.LoadSkills(resource.LoadSkillsOptions{
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
	projectContext, contextDiagnostics := resource.LoadContextFiles(cwd, cfg.AgentDir)
	resourceDiagnostics = append(resourceDiagnostics, contextDiagnostics...)
	logResourceDiagnostics(os.Stderr, resourceDiagnostics)
	obs.Time("context/skills/templates load")

	// Declarative shell hooks live at <agentDir>/hooks.json; a missing file is
	// a no-op (LoadShellHooks returns nil,nil).
	hooksPath := filepath.Join(cfg.AgentDir, "hooks.json")

	orch, err := orchestrator.New(ctx, orchestrator.NewOptions{
		Repo:            session.NewJsonlSessionRepo(root),
		CreateOptions:   session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:         *modelID,
		Provider:        prov,
		Settings:        settings,
		SettingsManager: settingsManager,
		HooksPath:       hooksPath,
		PromptTemplates: templates,
		Skills:          skills,
		PromptPaths:     promptPaths,
		SkillPaths:      skillPaths,
		AgentDir:        cfg.AgentDir,
		ContextFiles:    projectContext,
		Diagnostics:     resourceDiagnostics,
		ExecutionEnv:    env,
		Tools:           builtin.NewCodingTools(env, shellCommandPrefix),
	})
	if err != nil {
		return fmt.Errorf("create orchestrator: %w", err)
	}
	obs.Time("orchestrator create")
	obs.Report(os.Stderr)

	if err := runPromptMode(ctx, *mode, orch, prompts); err != nil {
		if closeErr := orch.Close(); closeErr != nil {
			return errors.Join(err, fmt.Errorf("close orchestrator: %w", closeErr))
		}
		return err
	}
	if err := orch.Close(); err != nil {
		return fmt.Errorf("close orchestrator: %w", err)
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
