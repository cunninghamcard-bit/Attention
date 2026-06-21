package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// NavigateTree executes session branch switching. It looks up the target
// entry, optionally generates a branch summary, determines the new leaf,
// and moves the session.
func (h *Harness) NavigateTree(ctx context.Context, targetID session.EntryID, state TurnState, opts NavigationOptions) (NavigationResult, error) {
	// 1. Get current leaf.
	oldLeaf, err := h.cfg.Session.GetLeafID()
	if err != nil {
		return NavigationResult{}, err
	}
	if oldLeaf != nil && *oldLeaf == targetID {
		// No-op: already at target.
		return NavigationResult{}, nil
	}

	// 2. Look up target entry.
	targetEntry, ok := h.cfg.Session.GetEntry(targetID)
	if !ok {
		return NavigationResult{}, fmt.Errorf("entry %q not found", targetID)
	}

	// 3. Collect entries for branch summary.
	entries, commonAncestorID, err := collectEntriesForBranchSummary(h.cfg.Session, oldLeaf, targetID)
	if err != nil {
		return NavigationResult{}, err
	}

	// 4. session_before_tree hook — check cancel/override.
	var hookSummary *hook.BranchSummaryResult
	customInstructions := opts.CustomInstructions
	replaceInstructions := opts.ReplaceInstructions

	if h.cfg.Hooks.HasHandlers(hook.EventSessionBeforeTree) {
		entriesToSummarize := make([]any, len(entries))
		for i, entry := range entries {
			entriesToSummarize[i] = entry
		}
		var prepCustomInstructions *string
		if opts.CustomInstructions != "" {
			prepCustomInstructions = &opts.CustomInstructions
		}
		var prepReplaceInstructions *bool
		if opts.ReplaceInstructions {
			prepReplaceInstructions = &opts.ReplaceInstructions
		}
		var label *string
		if opts.Label != "" {
			label = &opts.Label
		}
		result, err := h.cfg.Hooks.Emit(ctx, hook.SessionBeforeTreeEvent{
			Type: hook.EventSessionBeforeTree,
			Preparation: hook.TreePreparation{
				TargetID:            string(targetID),
				OldLeafID:           entryIDStringPtr(oldLeaf),
				CommonAncestorID:    entryIDStringPtr(commonAncestorID),
				EntriesToSummarize:  entriesToSummarize,
				UserWantsSummary:    opts.Summarize,
				CustomInstructions:  prepCustomInstructions,
				ReplaceInstructions: prepReplaceInstructions,
				Label:               label,
			},
			Signal: ctx,
		})
		if err != nil {
			return NavigationResult{}, err
		}
		if result != nil {
			r, ok := result.(hook.SessionBeforeTreeResult)
			if ok {
				if r.Cancel {
					return NavigationResult{Cancelled: true}, nil
				}
				hookSummary = r.Summary
				if r.CustomInstructions != nil {
					customInstructions = *r.CustomInstructions
				}
				if r.ReplaceInstructions != nil {
					replaceInstructions = *r.ReplaceInstructions
				}
			}
		}
	}

	// 5. Generate branch summary if requested and hook didn't provide one.
	var summary string
	var summaryDetails any
	if hookSummary != nil {
		summary = hookSummary.Summary
		summaryDetails = hookSummary.Details
	}
	if summary == "" && opts.Summarize && len(entries) > 0 {
		generated, err := h.generateBranchSummary(ctx, entries, state, customInstructions, replaceInstructions)
		if err != nil {
			if errors.Is(err, errSummaryAborted) {
				return NavigationResult{Cancelled: true}, nil
			}
			return NavigationResult{}, err
		}
		summary = generated.Summary
		summaryDetails = generated.Details
	}

	// 6. Determine new leaf and extract editor text.
	var newLeaf *session.EntryID
	var editorText string
	switch targetEntry.Type {
	case "message":
		msg, ok := message.AsAIMessage(targetEntry.Message)
		if ok && msg.Role == ai.RoleUser {
			editorText = textFromContent(msg.Content)
			newLeaf = targetEntry.ParentID
		} else {
			id := targetEntry.ID
			newLeaf = &id
		}
	case "custom_message":
		newLeaf = targetEntry.ParentID
		switch content := targetEntry.Content.(type) {
		case string:
			editorText = content
		case []ai.ContentBlock:
			editorText = textFromContent(content)
		case []any:
			blocks, err := message.CustomContentBlocks(content)
			if err == nil {
				editorText = textFromContent(blocks)
			}
		}
	default:
		id := targetEntry.ID
		newLeaf = &id
	}

	// 7. Build branch summary for MoveTo.
	var branchSummary *session.BranchSummary
	if summary != "" {
		branchSummary = &session.BranchSummary{
			Summary:  summary,
			Details:  summaryDetails,
			FromHook: hookSummary != nil,
		}
	}

	// 8. Move session to new leaf.
	summaryEntryID, err := h.cfg.Session.MoveTo(ctx, newLeaf, branchSummary)
	if err != nil {
		return NavigationResult{}, err
	}

	// 10. Get summary entry if created.
	var summaryEntry *session.SessionEntry
	if summaryEntryID != nil {
		entry, ok := h.cfg.Session.GetEntry(*summaryEntryID)
		if ok {
			summaryEntry = &entry
		}
	}

	// 11. session_tree notification hook.
	if h.cfg.Hooks.HasHandlers(hook.EventSessionTree) {
		actualLeaf, err := h.cfg.Session.GetLeafID()
		if err != nil {
			return NavigationResult{}, err
		}
		_, err = h.cfg.Hooks.Emit(ctx, hook.SessionTreeEvent{
			Type:         hook.EventSessionTree,
			NewLeafId:    entryIDStringPtr(actualLeaf),
			OldLeafId:    entryIDStringPtr(oldLeaf),
			SummaryEntry: summaryEntry,
			FromHook:     hookSummary != nil,
		})
		if err != nil {
			return NavigationResult{}, err
		}
	}

	return NavigationResult{
		Cancelled:    false,
		EditorText:   editorText,
		SummaryEntry: summaryEntry,
	}, nil
}

// collectEntriesForBranchSummary collects entries that will be summarized
// when navigating from oldLeaf to targetID. These are the entries on the
// old branch that diverge from the path to targetID.
func collectEntriesForBranchSummary(
	s Session,
	oldLeaf *session.EntryID,
	targetID session.EntryID,
) ([]session.SessionEntry, *session.EntryID, error) {
	if oldLeaf == nil {
		return nil, nil, nil
	}

	oldBranch, err := s.GetBranch(oldLeaf)
	if err != nil {
		return nil, nil, err
	}
	oldPath := make(map[session.EntryID]struct{}, len(oldBranch))
	for _, entry := range oldBranch {
		oldPath[entry.ID] = struct{}{}
	}

	targetBranch, err := s.GetBranch(&targetID)
	if err != nil {
		return nil, nil, err
	}
	var commonAncestorID *session.EntryID
	for i := len(targetBranch) - 1; i >= 0; i-- {
		id := targetBranch[i].ID
		if _, ok := oldPath[id]; ok {
			commonAncestorID = &id
			break
		}
	}

	start := 0
	if commonAncestorID != nil {
		for i, entry := range oldBranch {
			if entry.ID == *commonAncestorID {
				start = i + 1
				break
			}
		}
	}
	entries := append([]session.SessionEntry(nil), oldBranch[start:]...)

	return entries, commonAncestorID, nil
}

const branchSummaryPreamble = `The user explored a different conversation branch before returning here.
Summary of that exploration:

`

const branchSummaryPrompt = `Create a structured summary of this conversation branch for context when returning later.

Use this EXACT format:

## Goal
[What was the user trying to accomplish in this branch?]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Work that was started but not finished]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [What should happen next to continue this work]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

type generatedBranchSummary struct {
	Summary string
	Details any
}

type branchPreparation struct {
	messages    []message.AgentMessage
	fileOps     fileOperations
	totalTokens int
}

func (h *Harness) generateBranchSummary(
	ctx context.Context,
	entries []session.SessionEntry,
	state TurnState,
	customInstructions string,
	replaceInstructions bool,
) (generatedBranchSummary, error) {
	auth, err := h.providerAuth(ctx, state.Model)
	if err != nil {
		return generatedBranchSummary{}, err
	}
	contextWindow := state.Model.ContextWindow
	if contextWindow == 0 {
		contextWindow = 128000
	}
	tokenBudget := contextWindow - 16384
	prep := prepareBranchEntries(entries, tokenBudget)
	if len(prep.messages) == 0 {
		return generatedBranchSummary{
			Summary: "No content to summarize",
			Details: map[string]any{
				"readFiles":     []string{},
				"modifiedFiles": []string{},
			},
		}, nil
	}

	conversationText, err := serializeAgentConversation(prep.messages)
	if err != nil {
		return generatedBranchSummary{}, err
	}
	instructions := branchSummaryPrompt
	if replaceInstructions && customInstructions != "" {
		instructions = customInstructions
	} else if customInstructions != "" {
		instructions += "\n\nAdditional focus: " + customInstructions
	}
	promptText := "<conversation>\n" + conversationText + "\n</conversation>\n\n" + instructions

	// pi's branch summarization never requests reasoning
	// (branch-summarization.ts has no thinkingLevel plumbing).
	summary, err := h.completeSummary(ctx, state.Model, auth, promptText, 2048, "")
	if err != nil {
		return generatedBranchSummary{}, err
	}
	readFiles, modifiedFiles := computeFileLists(prep.fileOps)
	summary = branchSummaryPreamble + summary + formatFileOperations(readFiles, modifiedFiles)
	if summary == "" {
		summary = "No summary generated"
	}
	return generatedBranchSummary{
		Summary: summary,
		Details: map[string]any{
			"readFiles":     readFiles,
			"modifiedFiles": modifiedFiles,
		},
	}, nil
}

func prepareBranchEntries(entries []session.SessionEntry, tokenBudget int) branchPreparation {
	prep := branchPreparation{
		fileOps: newFileOperations(),
	}
	for _, entry := range entries {
		if entry.Type == "branch_summary" && !entry.FromHook && entry.Details != nil {
			prep.fileOps.addDetails(entry.Details)
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		msg, ok := messageFromEntryForBranchSummary(entry)
		if !ok {
			continue
		}
		extractFileOpsFromMessage(msg, &prep.fileOps)

		tokens := estimateAgentMessageTokens(msg)
		if tokenBudget > 0 && prep.totalTokens+tokens > tokenBudget {
			if entry.Type == "compaction" || entry.Type == "branch_summary" {
				if prep.totalTokens < int(float64(tokenBudget)*0.9) {
					prep.messages = append([]message.AgentMessage{msg}, prep.messages...)
					prep.totalTokens += tokens
				}
			}
			break
		}
		prep.messages = append([]message.AgentMessage{msg}, prep.messages...)
		prep.totalTokens += tokens
	}
	return prep
}

func messageFromEntryForBranchSummary(entry session.SessionEntry) (message.AgentMessage, bool) {
	switch entry.Type {
	case "message":
		if entry.Message == nil {
			return nil, false
		}
		if msg, ok := message.AsAIMessage(entry.Message); ok && msg.Role == ai.RoleToolResult {
			return nil, false
		}
		return message.Snapshot(entry.Message), true
	case "compaction":
		if entry.Summary == "" {
			return nil, false
		}
		return message.CreateCompactionSummaryMessage(
			entry.Summary,
			entry.TokensBefore,
			entry.Timestamp,
		), true
	default:
		return messageFromEntryForCompaction(entry)
	}
}

func entryIDStringPtr(id *session.EntryID) *string {
	if id == nil {
		return nil
	}
	value := string(*id)
	return &value
}
