package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/app"
	"github.com/cunninghamcard-bit/Attention/internal/auth"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	printmode "github.com/cunninghamcard-bit/Attention/internal/mode/print"
	rpcmode "github.com/cunninghamcard-bit/Attention/internal/mode/rpc"
	"github.com/cunninghamcard-bit/Attention/internal/obs"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type printPromptRunner interface {
	Prompt(context.Context, compat.PromptInput) (compat.PromptResult, error)
}

type jsonPromptRunner interface {
	printPromptRunner
	Subscribe(func(compat.Event)) func()
	SessionMetadata() session.Metadata
}

type printModeRunner func(context.Context, printPromptRunner, []string) error
type jsonModeRunner func(context.Context, jsonPromptRunner, []string) error
type rpcModeRunner func(context.Context, compat.Target) error
type cliModeRunner interface {
	jsonPromptRunner
	compat.Target
}

var runPrintMode printModeRunner = func(ctx context.Context, runner printPromptRunner, prompts []string) error {
	return printmode.Run(ctx, runner, prompts)
}

var runJSONMode jsonModeRunner = func(ctx context.Context, runner jsonPromptRunner, prompts []string) error {
	return rpcmode.Run(ctx, runner, prompts)
}

var runRPCMode rpcModeRunner = func(ctx context.Context, runner compat.Target) error {
	return rpcmode.Serve(ctx, runner)
}

func main() {
	// SIGHUP included: bash children run with Setpgid and never receive the
	// terminal's HUP, so along must cancel them itself — pi kills tracked
	// detached children on shutdown signals (utils/shell.ts:167-185).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// `along serve` 是子命令（新协议头）；其余一律走旧 CLI 形态（userspace 不变）。
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := runServe(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

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
	model, ok := prov.Resolve(*modelID)
	if !ok {
		return unknownModelError(*modelID, prov.All())
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
	shellPath := settingsString(settings, "shellPath")
	shellCommandPrefix := settingsString(settings, "shellCommandPrefix")

	comp, err := app.Compose(ctx, app.ComposeOptions{
		DataDir:            cfg.AgentDir,
		SessionsDir:        root,
		CWD:                cwd,
		Model:              model,
		ThinkingLevel:      defaultThinkingLevel(settings),
		Provider:           prov,
		ShellPath:          shellPath,
		ShellCommandPrefix: shellCommandPrefix,
	})
	if err != nil {
		return fmt.Errorf("compose engine: %w", err)
	}
	defer comp.Stop()

	sess, err := comp.Repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: cwd})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	facade := comp.NewSessionFacade(sess.GetMetadata().ID)
	obs.Time("engine compose")
	obs.Report(os.Stderr)

	if err := runPromptMode(ctx, *mode, facade, prompts); err != nil {
		return err
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

func discoverJSExtensions(agentDir, cwd string, configuredPaths []string, stderr io.Writer) []string {
	return resource.DiscoverJSExtensions(agentDir, cwd, configuredPaths, stderr)
}

func jsExtensionCandidates(agentDir, cwd string, configuredPaths []string) []string {
	return resource.JSExtensionCandidates(agentDir, cwd, configuredPaths)
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
	runner cliModeRunner,
	prompts []string,
) error {
	switch mode {
	case "print":
		return runPrintMode(ctx, runner, prompts)
	case "json":
		return runJSONMode(ctx, runner, prompts)
	case "rpc":
		return runRPCMode(ctx, runner)
	default:
		return validateMode(mode)
	}
}

func defaultThinkingLevel(settings config.Settings) agentloop.ThinkingLevel {
	if value, ok := settings["defaultThinkingLevel"].(string); ok && value != "" {
		return agentloop.ThinkingLevel(value)
	}
	return agentloop.ThinkingMedium
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
