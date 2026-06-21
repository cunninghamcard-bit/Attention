package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/exporthtml"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/render"
)

// ExportHTML renders the current session transcript to one HTML file and
// returns the resolved output path. Pi's session contract is exportToHtml with
// an optional outputPath returning a path:
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2973.
func (o *Orchestrator) ExportHTML(ctx context.Context, outputPath string) (string, error) {
	path, err := o.resolveExportHTMLPath(outputPath, time.Now())
	if err != nil {
		return "", err
	}

	sessionData, err := o.exportHTMLSessionData()
	if err != nil {
		return "", err
	}

	html := exporthtml.Render(sessionData, exporthtml.Options{})
	content := []byte(html)

	if o.execEnv != nil {
		if err := o.execEnv.CreateDir(ctx, filepath.Dir(path), true); err != nil {
			return "", fmt.Errorf("export html create parent: %w", err)
		}
		if err := o.execEnv.WriteFile(ctx, path, content); err != nil {
			return "", fmt.Errorf("export html write: %w", err)
		}
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("export html create parent: %w", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", fmt.Errorf("export html write: %w", err)
	}
	return path, nil
}

func (o *Orchestrator) exportHTMLSessionData() (exporthtml.SessionData, error) {
	if o.session == nil {
		return exporthtml.SessionData{}, fmt.Errorf("export html: session is required")
	}

	metadata := o.session.GetMetadata()
	leafID, err := o.session.GetLeafID()
	if err != nil {
		return exporthtml.SessionData{}, fmt.Errorf("export html leaf: %w", err)
	}

	o.mu.Lock()
	systemPrompt := o.systemPrompt
	tools := make([]exporthtml.ToolDefinition, 0, len(o.toolDefs))
	callRenderersByName := make(map[string]extension.ToolCallRenderer, len(o.toolDefs))
	resultRenderersByName := make(map[string]extension.ToolResultRenderer, len(o.toolDefs))
	for _, def := range o.toolDefs {
		tools = append(tools, exporthtml.ToolDefinition{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  def.Parameters,
		})
		if def.RenderCall != nil {
			callRenderersByName[def.Name] = def.RenderCall
		}
		if def.RenderResult != nil {
			resultRenderersByName[def.Name] = def.RenderResult
		}
	}
	o.mu.Unlock()

	entries := o.session.GetEntries()
	argsByID := map[string]map[string]any{}
	toolNameByID := map[string]string{}
	for _, entry := range entries {
		if entry.Type != "message" {
			continue
		}
		msg, ok := message.AsAIMessage(entry.Message)
		if !ok || msg.Role != ai.RoleAssistant {
			continue
		}
		for _, block := range msg.Content {
			if block.Type != ai.ContentToolCall || block.ToolCallID == "" {
				continue
			}
			argsByID[block.ToolCallID] = block.Arguments
			toolNameByID[block.ToolCallID] = block.ToolName
		}
	}

	rendered := map[string]exporthtml.RenderedTool{}
	for id, args := range argsByID {
		renderer := callRenderersByName[toolNameByID[id]]
		if renderer == nil {
			continue
		}
		call := renderer(extension.ToolCallRenderInput{
			Args:             args,
			ToolCallID:       id,
			CWD:              metadata.CWD,
			ExecutionStarted: true,
			ArgsComplete:     true,
		})
		callExpanded := renderer(extension.ToolCallRenderInput{
			Args:             args,
			ToolCallID:       id,
			CWD:              metadata.CWD,
			ExecutionStarted: true,
			ArgsComplete:     true,
			Expanded:         true,
		})
		if len(call) == 0 {
			continue
		}
		rt := rendered[id]
		rt.CallBlocks = call
		if len(callExpanded) > 0 && !blocksEqual(call, callExpanded) {
			rt.CallBlocksExpanded = callExpanded
		}
		rendered[id] = rt
	}

	for _, entry := range entries {
		if entry.Type != "message" {
			continue
		}
		msg, ok := message.AsAIMessage(entry.Message)
		if !ok || msg.Role != ai.RoleToolResult || msg.ToolCallID == "" {
			continue
		}
		renderer := resultRenderersByName[msg.ToolName]
		if renderer == nil {
			continue
		}
		input := extension.ToolResultRenderInput{
			Args:             argsByID[msg.ToolCallID],
			ToolCallID:       msg.ToolCallID,
			CWD:              metadata.CWD,
			ExecutionStarted: true,
			ArgsComplete:     true,
			IsError:          msg.IsError,
			Result: extension.RenderResult{
				Content: msg.Content,
				Details: msg.Details,
				IsError: msg.IsError,
			},
		}
		result := renderer(input)
		inputExpanded := input
		inputExpanded.Expanded = true
		resultExpanded := renderer(inputExpanded)
		if len(result) == 0 {
			continue
		}
		rt := rendered[msg.ToolCallID]
		rt.ResultBlocks = result
		if len(resultExpanded) > 0 && !blocksEqual(result, resultExpanded) {
			rt.ResultBlocksExpanded = resultExpanded
		}
		rendered[msg.ToolCallID] = rt
	}
	if len(rendered) == 0 {
		rendered = nil
	}

	return exporthtml.SessionData{
		Header: exporthtml.SessionHeader{
			Type:          "session",
			Version:       3,
			ID:            metadata.ID,
			Timestamp:     metadata.CreatedAt,
			CWD:           metadata.CWD,
			ParentSession: metadata.ParentSessionPath,
		},
		Entries:       entries,
		LeafID:        leafID,
		SystemPrompt:  systemPrompt,
		Tools:         tools,
		RenderedTools: rendered,
	}, nil
}

func blocksEqual(a, b []render.Block) bool {
	return reflect.DeepEqual(a, b)
}

func (o *Orchestrator) resolveExportHTMLPath(outputPath string, now time.Time) (string, error) {
	cwd := "."
	if o.session != nil {
		metadata := o.session.GetMetadata()
		if metadata.CWD != "" {
			cwd = metadata.CWD
		}
	}

	path := outputPath
	if path == "" {
		path = fmt.Sprintf("session-%s.html", now.Format("2006-01-02T15-04-05"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}

	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("export html resolve path: %w", err)
	}
	return filepath.Clean(resolved), nil
}
