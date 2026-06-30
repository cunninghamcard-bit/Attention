package main

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// orchestratorCommonOptions holds the orchestrator option fields that are
// identical across the New and Open constructor paths. Factoring them here
// avoids restating the ~15 shared fields once per constructor; applyTo copies
// them into the concrete NewOptions/OpenOptions just before the call.
type orchestratorCommonOptions struct {
	ModelID            string
	ModelProvider      string
	Provider           *provider.Registry
	Settings           config.Settings
	SettingsManager    *config.Manager
	HooksPath          string
	SystemPrompt       string
	AppendSystemPrompt string
	ThinkingLevel      agentloop.ThinkingLevel
	PromptTemplates    []resource.PromptTemplate
	Skills             []resource.Skill
	PromptPaths        []string
	SkillPaths         []string
	AgentDir           string
	ContextFiles       []resource.ContextFile
	Diagnostics        []resource.ResourceDiagnostic
	ExecutionEnv       execenv.ExecutionEnv
	Tools              []extension.ToolDefinition
	Extensions         []orchestrator.ExtensionSource
}

func (c orchestratorCommonOptions) newOptions() orchestrator.NewOptions {
	return orchestrator.NewOptions{
		ModelID:            c.ModelID,
		ModelProvider:      c.ModelProvider,
		Provider:           c.Provider,
		Settings:           c.Settings,
		SettingsManager:    c.SettingsManager,
		HooksPath:          c.HooksPath,
		SystemPrompt:       c.SystemPrompt,
		AppendSystemPrompt: c.AppendSystemPrompt,
		ThinkingLevel:      c.ThinkingLevel,
		PromptTemplates:    c.PromptTemplates,
		Skills:             c.Skills,
		PromptPaths:        c.PromptPaths,
		SkillPaths:         c.SkillPaths,
		AgentDir:           c.AgentDir,
		ContextFiles:       c.ContextFiles,
		Diagnostics:        c.Diagnostics,
		ExecutionEnv:       c.ExecutionEnv,
		Tools:              c.Tools,
		Extensions:         c.Extensions,
	}
}

func (c orchestratorCommonOptions) openOptions() orchestrator.OpenOptions {
	return orchestrator.OpenOptions{
		ModelID:            c.ModelID,
		ModelProvider:      c.ModelProvider,
		Provider:           c.Provider,
		Settings:           c.Settings,
		SettingsManager:    c.SettingsManager,
		HooksPath:          c.HooksPath,
		SystemPrompt:       c.SystemPrompt,
		AppendSystemPrompt: c.AppendSystemPrompt,
		ThinkingLevel:      c.ThinkingLevel,
		PromptTemplates:    c.PromptTemplates,
		Skills:             c.Skills,
		PromptPaths:        c.PromptPaths,
		SkillPaths:         c.SkillPaths,
		AgentDir:           c.AgentDir,
		ContextFiles:       c.ContextFiles,
		Diagnostics:        c.Diagnostics,
		ExecutionEnv:       c.ExecutionEnv,
		Tools:              c.Tools,
		Extensions:         c.Extensions,
	}
}

// buildOrchestrator dispatches to the orchestrator constructor selected by the
// resolved plan:
//   - planNew      -> orchestrator.New with repo + CreateOptions (fresh session)
//   - planOpen     -> orchestrator.Open with repo + Metadata (resume)
//   - planEphemeral-> orchestrator.New with a pre-built in-memory Session, which
//     causes New to skip repo.Create entirely (nothing is persisted).
func buildOrchestrator(
	ctx context.Context,
	repo *session.JsonlSessionRepo,
	plan sessionPlan,
	common orchestratorCommonOptions,
) (*orchestrator.Orchestrator, error) {
	switch plan.kind {
	case planOpen:
		opts := common.openOptions()
		opts.Repo = repo
		opts.Metadata = plan.metadata
		return orchestrator.Open(ctx, opts)
	case planEphemeral:
		opts := common.newOptions()
		opts.Session = plan.ephemeral
		return orchestrator.New(ctx, opts)
	default: // planNew, planFork — both create a fresh session; planFork's
		// CreateOptions carries ParentSessionPath so the header records the fork.
		opts := common.newOptions()
		opts.Repo = repo
		opts.CreateOptions = plan.createOptions
		return orchestrator.New(ctx, opts)
	}
}
