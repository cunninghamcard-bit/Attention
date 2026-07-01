package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
)

func runPluginCommand(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "plugin" {
		return false, nil
	}
	if len(args) < 2 {
		return true, errors.New("usage: along plugin install <git-url-or-path>")
	}
	switch args[1] {
	case "install":
		return true, runPluginInstallCommand(ctx, args[2:])
	default:
		return true, fmt.Errorf("unknown plugin command %q (want install)", args[1])
	}
}

func runPluginInstallCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("along plugin install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: along plugin install <git-url-or-path>")
	}

	agentDir, err := config.AgentDir()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	result, err := plugin.Install(ctx, plugin.InstallOptions{
		Source:   fs.Arg(0),
		AgentDir: agentDir,
		CWD:      cwd,
	})
	if err != nil {
		return err
	}

	if result.Installed {
		fmt.Fprintf(os.Stdout, "installed plugin %s at %s\n", result.Name, result.Dir)
		return nil
	}
	fmt.Fprintf(os.Stdout, "enabled existing plugin %s at %s\n", result.Name, result.Dir)
	return nil
}
