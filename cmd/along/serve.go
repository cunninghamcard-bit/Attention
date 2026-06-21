package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/cunninghamcard-bit/Attention/internal/app"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/server"
)

// runServe 起 REST+SSE 协议服务（along-api.md）：组装引擎 → 监听 →
// stdout 打一行 {"port":N,"token":"..."} → 阻塞至 ctx 取消或 stdin EOF
// （Electron 哑监工用 stdin EOF 绑生命周期，总纲 §5）。
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("along serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "127.0.0.1:0", "listen address")
	dataDir := fs.String("data-dir", "", "engine data dir (default: agent config dir)")
	modelID := fs.String("model", "claude-sonnet-4-5", "model ID")
	apiKey := fs.String("api-key", "", "API key override for the selected model's provider")
	bundledPlugins := fs.String("bundled-plugins", "", "bundled plugins dir (dev: plugins)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, err := config.Load(ctx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	for _, settingsErr := range cfg.SettingsErrors {
		fmt.Fprintf(os.Stderr, "Warning: %s settings: %v\n", settingsErr.Scope, settingsErr.Err)
	}

	prov, err := buildProvider(ctx, cfg)
	if err != nil {
		return err
	}
	if err := resolveModel(prov, *modelID); err != nil {
		return err
	}
	if *apiKey != "" {
		if m, ok := prov.Resolve(*modelID); ok {
			prov.SetRuntimeAPIKey(m.Provider, *apiKey)
		}
	}
	model, ok := prov.Resolve(*modelID)
	if !ok {
		return unknownModelError(*modelID, prov.All())
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
	}
	root, err := sessionsRoot()
	if err != nil {
		return err
	}
	engineDir := *dataDir
	if engineDir == "" {
		engineDir = cfg.AgentDir
	}

	// 插件注册表：用户目录 <agentDir>/plugins（捆绑目录随发版形态后定，
	// dev 下可用 --bundled-plugins 指仓内 plugins）。
	plugins, err := plugin.NewRegistry(*bundledPlugins, cfg.AgentDir)
	if err != nil {
		return fmt.Errorf("plugin registry: %w", err)
	}

	settings := cfg.Settings
	comp, err := app.Compose(ctx, app.ComposeOptions{
		DataDir:            engineDir,
		SessionsDir:        root,
		CWD:                cwd,
		Model:              model,
		ThinkingLevel:      defaultThinkingLevel(settings),
		Provider:           prov,
		Plugins:            plugins,
		ShellPath:          settingsString(settings, "shellPath"),
		ShellCommandPrefix: settingsString(settings, "shellCommandPrefix"),
	})
	if err != nil {
		return fmt.Errorf("compose engine: %w", err)
	}
	defer comp.Stop()

	token, err := newBootToken()
	if err != nil {
		return err
	}
	srvOpts := server.Options{
		Addr:       *addr,
		Token:      token,
		DefaultCWD: cwd,
		Store:      comp.Store,
		Bus:        comp.Bus,
		Queue:      comp.Queue,
		Repo:       comp.Repo,
		Plugins:    plugins,
	}
	if comp.ExtCommands != nil { // typed-nil 不入接口
		srvOpts.ExtCommands = comp.ExtCommands
	}
	srv, err := server.Start(ctx, srvOpts)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	defer srv.Close()

	_, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("parse listen port %q: %w", portStr, err)
	}
	boot, err := json.Marshal(struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}{Port: port, Token: token})
	if err != nil {
		return err
	}
	fmt.Println(string(boot))

	// stdin EOF = 监工进程死了，跟着退（信号经 main 的 NotifyContext 进 ctx）。
	stdinDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinDone)
	}()

	select {
	case <-ctx.Done():
	case <-stdinDone:
	}
	return nil
}

func newBootToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate boot token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
