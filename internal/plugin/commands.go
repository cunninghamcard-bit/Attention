package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	internalextension "github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
)

const defaultCommandHandlerTimeout = 10 * time.Second

type commandRegistryFile struct {
	Commands []declarativeCommand `json:"commands"`
}

type declarativeCommand struct {
	Name         string                    `json:"name"`
	Description  string                    `json:"description"`
	ArgumentHint string                    `json:"argumentHint"`
	Handler      declarativeCommandHandler `json:"handler"`
}

type declarativeCommandHandler struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type commandHandlerInput struct {
	CommandName string   `json:"command_name"`
	Args        []string `json:"args"`
	SessionID   string   `json:"session_id"`
	CWD         string   `json:"cwd"`
}

type commandHandlerOutput struct {
	Notifications []commandHandlerNotification `json:"notifications"`
}

type commandHandlerNotification struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

func loadPluginCommands(
	root string,
	env map[string]string,
) (map[string]internalextension.CommandDefinition, []resource.ResourceDiagnostic) {
	path := filepath.Join(root, commandsDirName, commandsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []resource.ResourceDiagnostic{pluginCommandDiagnostic(path, fmt.Errorf("plugin command: read commands file: %w", err))}
	}

	var registry commandRegistryFile
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, []resource.ResourceDiagnostic{pluginCommandDiagnostic(path, fmt.Errorf("plugin command: parse commands file: %w", err))}
	}

	commands := map[string]internalextension.CommandDefinition{}
	var diagnostics []resource.ResourceDiagnostic
	for i, entry := range registry.Commands {
		entry := entry
		if strings.TrimSpace(entry.Name) == "" {
			diagnostics = append(diagnostics, pluginCommandDiagnostic(path, fmt.Errorf("plugin command: commands[%d] missing name", i)))
			continue
		}
		if _, exists := commands[entry.Name]; exists {
			diagnostics = append(diagnostics, pluginCommandDiagnostic(path, fmt.Errorf("plugin command: duplicate command %q", entry.Name)))
			continue
		}
		if entry.Handler.Type != "command" {
			diagnostics = append(diagnostics, pluginCommandDiagnostic(path, fmt.Errorf("plugin command %q: unsupported handler type %q", entry.Name, entry.Handler.Type)))
			continue
		}
		if strings.TrimSpace(entry.Handler.Command) == "" {
			diagnostics = append(diagnostics, pluginCommandDiagnostic(path, fmt.Errorf("plugin command %q: missing handler command", entry.Name)))
			continue
		}
		commands[entry.Name] = internalextension.CommandDefinition{
			Description:  entry.Description,
			ArgumentHint: entry.ArgumentHint,
			Source:       resource.NewSourceInfo(resource.SourcePath, path, root),
			Handler: func(ctx context.Context, args []string, extCtx internalextension.ExtensionContext) error {
				return runPluginCommandHandler(ctx, entry.Name, entry.Handler, env, args, extCtx)
			},
		}
	}
	return commands, diagnostics
}

func runPluginCommandHandler(
	ctx context.Context,
	name string,
	handler declarativeCommandHandler,
	baseEnv map[string]string,
	args []string,
	extCtx internalextension.ExtensionContext,
) error {
	timeout := defaultCommandHandlerTimeout
	if handler.Timeout > 0 {
		timeout = time.Duration(handler.Timeout) * time.Second
	}

	inputJSON, err := json.Marshal(commandHandlerInput{
		CommandName: name,
		Args:        append([]string{}, args...),
		SessionID:   extCtx.SessionID,
		CWD:         extCtx.Cwd,
	})
	if err != nil {
		return err
	}

	env := maps.Clone(baseEnv)
	if env == nil {
		env = map[string]string{}
	}
	if extCtx.Cwd != "" {
		env[projectDirEnv] = extCtx.Cwd
	}
	command := replaceCommandEnv(handler.Command, env)
	commandArgs := make([]string, 0, len(handler.Args))
	for _, arg := range handler.Args {
		commandArgs = append(commandArgs, replaceCommandEnv(arg, env))
	}
	mergedEnv := mergeCommandEnv(env)

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, commandPath(command, mergedEnv), commandArgs...)
	if extCtx.Cwd != "" {
		cmd.Dir = extCtx.Cwd
	} else if root := env[pluginRootEnv]; root != "" {
		cmd.Dir = root
	}
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = mergedEnv
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.Output()
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("plugin command %q timed out after %s", name, timeout)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("plugin command %q failed: %w: %s", name, err, detail)
		}
		return fmt.Errorf("plugin command %q failed: %w", name, err)
	}

	var output commandHandlerOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &output); err != nil {
		return fmt.Errorf("plugin command %q returned non-JSON stdout: %w", name, err)
	}
	if extCtx.Notify != nil {
		for _, notification := range output.Notifications {
			extCtx.Notify(notification.Message, notification.Level)
		}
	}
	return nil
}

func pluginCommandDiagnostic(path string, err error) resource.ResourceDiagnostic {
	return resource.ResourceDiagnostic{
		Type:    resource.DiagnosticWarning,
		Message: err.Error(),
		Path:    path,
	}
}

func replaceCommandEnv(text string, env map[string]string) string {
	for key, value := range env {
		text = strings.ReplaceAll(text, "${"+key+"}", value)
		text = strings.ReplaceAll(text, "$"+key, value)
	}
	return text
}

func mergeCommandEnv(extra map[string]string) []string {
	env := os.Environ()
	seen := map[string]bool{}
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			seen[key] = true
		}
	}
	for key, value := range extra {
		entry := key + "=" + value
		if !seen[key] {
			env = append(env, entry)
			seen[key] = true
			continue
		}
		for i, existing := range env {
			if strings.HasPrefix(existing, key+"=") {
				env[i] = entry
				break
			}
		}
	}
	return env
}

func commandPath(command string, env []string) string {
	if strings.ContainsAny(command, `/\`) {
		return command
	}
	path := pathValue(env)
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, command)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return command
}

func pathValue(env []string) string {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == pathEnv {
			return value
		}
	}
	return os.Getenv(pathEnv)
}
