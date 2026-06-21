// Package harness provides the stateless execution layer between Orchestrator
// and agentloop. It owns event dispatch, session writes, and hook calls.
package harness

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// Session is the consumer-side contract that Harness requires from the session
// layer. The concrete *session.Session satisfies this interface implicitly.
type Session interface {
	// Read operations.
	BuildContext(ctx context.Context) (session.Context, error)
	GetMetadata() session.Metadata
	GetLeafID() (*session.EntryID, error)
	GetEntry(id session.EntryID) (session.SessionEntry, bool)
	GetEntries() []session.SessionEntry
	GetBranch(fromID *session.EntryID) ([]session.SessionEntry, error)
	GetLabel(id session.EntryID) (string, bool)
	GetSessionName() (string, bool)

	// Write operations.
	AppendMessage(ctx context.Context, msg message.AgentMessage) (session.EntryID, error)
	AppendModelChange(ctx context.Context, provider, modelID string) (session.EntryID, error)
	AppendThinkingLevelChange(ctx context.Context, level string) (session.EntryID, error)
	AppendCompaction(ctx context.Context, summary string, firstKeptEntryID session.EntryID, tokensBefore int, details any, fromHook bool) (session.EntryID, error)
	AppendCustomEntry(ctx context.Context, customType string, data any) (session.EntryID, error)
	AppendCustomMessageEntry(ctx context.Context, customType string, content any, display bool, details any) (session.EntryID, error)
	AppendLabel(ctx context.Context, targetID session.EntryID, label string) (session.EntryID, error)
	AppendSessionName(ctx context.Context, name string) (session.EntryID, error)
	MoveTo(ctx context.Context, entryID *session.EntryID, summary *session.BranchSummary) (*session.EntryID, error)
}

var _ Session = (*session.Session)(nil)
