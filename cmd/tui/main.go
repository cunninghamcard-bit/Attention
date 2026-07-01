// Adapted from github.com/dimetron/pi-go internal/tui
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
)

// appVersion is the viewer version reported in the status bar.
const appVersion = "0.0.0-phase2"

func main() {
	alongPath := flag.String("along-path", "along", "path to the along kernel binary (resolved via PATH if a bare name)")
	flag.Parse()

	if err := run(*alongPath); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}
}

func run(alongPath string) error {
	// Resolve a bare binary name through PATH; an explicit path is used as-is.
	resolved := alongPath
	if !containsPathSeparator(alongPath) {
		p, err := exec.LookPath(alongPath)
		if err != nil {
			return fmt.Errorf("locate along kernel %q: %w", alongPath, err)
		}
		resolved = p
	}

	agent, err := NewRPCAgent(resolved)
	if err != nil {
		return err
	}
	// Always tear the kernel down on exit.
	defer agent.Close()

	ctx := context.Background()

	sessionID, err := agent.CreateSession(ctx)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	cfg := Config{
		Agent:        agent,
		SessionID:    sessionID,
		AppVersion:   appVersion,
		ModelName:    agent.ResolvedModel,
		ProviderName: agent.ResolvedProvider,
		ThemeName:    "",
	}
	// Fetch the kernel's command list ONCE. This is the single source of truth:
	// cfg.Commands feeds slash-command completion + dispatch verbatim, and the
	// source=="skill" subset (with the "skill:" prefix stripped) feeds the
	// sidebar's Skills section. Non-fatal: empty/nil on error.
	cmds := agent.FetchCommands()
	cfg.Commands = cmds
	cfg.Skills = skillsFromCommands(cmds)

	return Run(ctx, cfg)
}

// containsPathSeparator reports whether s looks like a path (so it should be
// used verbatim) rather than a bare command name to resolve via PATH.
func containsPathSeparator(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == os.PathSeparator {
			return true
		}
	}
	return false
}
