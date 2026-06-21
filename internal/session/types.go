package session

import (
	"context"
	"encoding/json"

	"github.com/cunninghamcard-bit/Attention/internal/message"
	"time"
)

type EntryID string

type CreateOptions struct {
	ID                string
	CWD               string
	ParentSessionPath string
}

type Metadata struct {
	ID        string
	CWD       string
	CreatedAt string
	// Modified is the last user/assistant activity time, falling back to the
	// header timestamp then file mtime (pi session-manager.ts:515-551).
	Modified          time.Time
	Path              string
	ParentSessionPath string
}

type JsonlSessionCreateOptions struct {
	ID                string
	CWD               string
	ParentSessionPath string
}

type JsonlSessionListOptions struct {
	CWD string
}

type ForkPosition string

const (
	ForkBefore ForkPosition = "before"
	ForkAt     ForkPosition = "at"
)

type JsonlSessionForkOptions struct {
	EntryID                 *EntryID
	Position                ForkPosition
	ID                      string
	CWD                     string
	ParentSessionPath       string
	SkipConversationRestore bool
}

type SessionRepo[TCreateOptions, TListOptions, TForkOptions any] interface {
	Create(ctx context.Context, opts TCreateOptions) (*Session, error)
	Open(ctx context.Context, metadata Metadata) (*Session, error)
	List(ctx context.Context, opts ...TListOptions) ([]Metadata, error)
	Delete(ctx context.Context, metadata Metadata) error
	Fork(ctx context.Context, source Metadata, opts TForkOptions) (*Session, error)
}

type JsonlSessionRepoAPI interface {
	SessionRepo[JsonlSessionCreateOptions, JsonlSessionListOptions, JsonlSessionForkOptions]
}

type Context struct {
	Messages      []message.AgentMessage
	ThinkingLevel string
	Model         *ModelRef
}

type ModelRef struct {
	Provider string
	ModelID  string
}

type BranchSummary struct {
	Summary  string
	Details  any
	FromHook bool
}

type SessionStorage interface {
	GetMetadata() Metadata
	GetLeafID() (*EntryID, error)
	SetLeafID(leafID *EntryID) error
	CreateEntryID() EntryID
	AppendEntry(entry SessionEntry) error
	GetEntry(id EntryID) (SessionEntry, bool)
	FindEntries(entryType string) []SessionEntry
	GetLabel(id EntryID) (string, bool)
	GetPathToRoot(leafID *EntryID) ([]SessionEntry, error)
	GetEntries() []SessionEntry
}

type SessionEntry struct {
	Type      string   `json:"type"`
	ID        EntryID  `json:"id"`
	ParentID  *EntryID `json:"parentId"`
	Timestamp string   `json:"timestamp"`

	Raw json.RawMessage `json:"-"`

	Message message.AgentMessage `json:"message,omitempty"`

	Summary          string   `json:"summary,omitempty"`
	FirstKeptEntryID EntryID  `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int      `json:"tokensBefore,omitempty"`
	Details          any      `json:"details,omitempty"`
	FromHook         bool     `json:"fromHook,omitempty"`
	FromID           *EntryID `json:"fromId,omitempty"`

	Level string `json:"thinkingLevel,omitempty"`

	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`

	CustomType string `json:"customType,omitempty"`
	Data       any    `json:"data,omitempty"`
	Content    any    `json:"content,omitempty"`
	Display    bool   `json:"display,omitempty"`

	TargetID *EntryID `json:"targetId,omitempty"`
	Label    string   `json:"label,omitempty"`

	Name string `json:"name,omitempty"`
}
