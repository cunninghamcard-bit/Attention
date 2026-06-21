package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

type JsonlSessionRepo struct {
	sessionsRoot string
}

var _ JsonlSessionRepoAPI = (*JsonlSessionRepo)(nil)

func NewJsonlSessionRepo(sessionsRoot string) *JsonlSessionRepo {
	return &JsonlSessionRepo{sessionsRoot: sessionsRoot}
}

func (r *JsonlSessionRepo) Create(ctx context.Context, opts JsonlSessionCreateOptions) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.CWD == "" {
		return nil, sessionError(ErrorInvalidSession, "cwd is required")
	}

	id := opts.ID
	if id == "" {
		id = newRandomID()
	}
	createdAt := newTimestamp()
	sessionDir, err := r.sessionDir(opts.CWD)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, wrapSessionError(ErrorStorage, "Failed to create session directory "+sessionDir, err)
	}
	path := filepath.Join(sessionDir, sessionFileName(createdAt, id))
	storage, err := CreateJSONL(path, CreateOptions{
		ID:                id,
		CWD:               opts.CWD,
		ParentSessionPath: opts.ParentSessionPath,
	})
	if err != nil {
		return nil, err
	}
	return NewSession(storage), nil
}

func (r *JsonlSessionRepo) Open(ctx context.Context, metadata Metadata) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if metadata.Path == "" {
		return nil, sessionError(ErrorNotFound, "Session path is empty")
	}
	if _, err := os.Stat(metadata.Path); err != nil {
		if os.IsNotExist(err) {
			return nil, sessionError(ErrorNotFound, "Session not found: "+metadata.Path)
		}
		return nil, wrapSessionError(ErrorStorage, "Failed to check session "+metadata.Path, err)
	}
	storage, err := OpenJSONL(metadata.Path)
	if err != nil {
		return nil, err
	}
	return NewSession(storage), nil
}

func (r *JsonlSessionRepo) List(ctx context.Context, listOpts ...JsonlSessionListOptions) ([]Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(listOpts) > 1 {
		return nil, sessionError(ErrorInvalidSession, "expected at most one list options value")
	}
	opts := JsonlSessionListOptions{}
	if len(listOpts) == 1 {
		opts = listOpts[0]
	}

	dirs := []string{}
	if opts.CWD != "" {
		dir, err := r.sessionDir(opts.CWD)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	} else {
		root, err := r.root()
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			return []Metadata{}, nil
		}
		if err != nil {
			return nil, wrapSessionError(ErrorStorage, "Failed to list sessions root "+root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(root, entry.Name()))
			}
		}
	}

	sessions := []Metadata{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, wrapSessionError(ErrorStorage, "Failed to list sessions in "+dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			metadata, err := LoadJSONLMetadata(path)
			if err != nil {
				if isInvalidSessionError(err) {
					continue
				}
				return nil, err
			}
			sessions = append(sessions, metadata)
		}
	}
	// pi sorts by last-activity time, newest first (session-manager.ts:1410-1414).
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Modified.After(sessions[j].Modified)
	})
	return sessions, nil
}

func (r *JsonlSessionRepo) Delete(ctx context.Context, metadata Metadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if metadata.Path == "" {
		return nil
	}
	if err := os.Remove(metadata.Path); err != nil && !os.IsNotExist(err) {
		return wrapSessionError(ErrorStorage, "Failed to delete session "+metadata.Path, err)
	}
	return nil
}

func (r *JsonlSessionRepo) Fork(ctx context.Context, source Metadata, opts JsonlSessionForkOptions) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sourceSession, err := r.Open(ctx, source)
	if err != nil {
		return nil, err
	}
	entries, err := getEntriesToFork(sourceSession.GetStorage(), opts.EntryID, opts.Position)
	if err != nil {
		return nil, err
	}

	if opts.CWD == "" {
		return nil, sessionError(ErrorInvalidSession, "cwd is required")
	}
	parentSessionPath := opts.ParentSessionPath
	if parentSessionPath == "" {
		parentSessionPath = source.Path
	}
	forked, err := r.Create(ctx, JsonlSessionCreateOptions{
		ID:                opts.ID,
		CWD:               opts.CWD,
		ParentSessionPath: parentSessionPath,
	})
	if err != nil {
		return nil, err
	}
	if opts.SkipConversationRestore {
		// pi CHANGELOG #286 and extensions/types.ts:1021 define this as
		// creating the fork file without restoring conversation messages.
		return forked, nil
	}
	storage := forked.GetStorage()
	for _, entry := range entries {
		if err := storage.AppendEntry(entry); err != nil {
			return nil, err
		}
	}
	return forked, nil
}

func (r *JsonlSessionRepo) root() (string, error) {
	if r.sessionsRoot == "" {
		return "", sessionError(ErrorInvalidSession, "sessions root is empty")
	}
	return filepath.Abs(r.sessionsRoot)
}

func (r *JsonlSessionRepo) sessionDir(cwd string) (string, error) {
	root, err := r.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, encodeCWD(cwd)), nil
}

func getEntriesToFork(
	storage SessionStorage,
	entryID *EntryID,
	position ForkPosition,
) ([]SessionEntry, error) {
	if entryID == nil {
		return storage.GetEntries(), nil
	}

	target, ok := storage.GetEntry(*entryID)
	if !ok {
		return nil, sessionError(ErrorInvalidForkTarget, fmt.Sprintf("Entry %s not found", *entryID))
	}
	if position == ForkAt {
		return storage.GetPathToRoot(&target.ID)
	}

	msg, ok := message.AsAIMessage(target.Message)
	if target.Type != "message" || !ok || msg.Role != ai.RoleUser {
		return nil, sessionError(
			ErrorInvalidForkTarget,
			fmt.Sprintf("Entry %s is not a user message", *entryID),
		)
	}
	return storage.GetPathToRoot(target.ParentID)
}

func encodeCWD(cwd string) string {
	// pi strips exactly ONE leading separator (replace(/^[/\\]/, "") is
	// non-global, session-manager.ts:431), so "//host/share" and
	// "/host/share" encode to distinct directories.
	trimmed := cwd
	if len(trimmed) > 0 && (trimmed[0] == '/' || trimmed[0] == '\\') {
		trimmed = trimmed[1:]
	}
	replacer := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return "--" + replacer.Replace(trimmed) + "--"
}

func sessionFileName(timestamp string, id string) string {
	safeTimestamp := strings.NewReplacer(":", "-", ".", "-").Replace(timestamp)
	return safeTimestamp + "_" + id + ".jsonl"
}

func isInvalidSessionError(err error) bool {
	sessionErr, ok := err.(*Error)
	return ok && sessionErr.Code == ErrorInvalidSession
}
