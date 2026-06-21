package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

type JsonlSessionStorage struct {
	mu       sync.Mutex
	path     string
	metadata Metadata
	header   sessionHeader
	// flushed reports whether the header and buffered entries have been
	// written to disk. pi defers all writes until the session contains an
	// assistant message, so abandoned launches leave no file behind
	// (session-manager.ts:772-794,843-861).
	flushed bool
	entries []SessionEntry
	byID    map[EntryID]int
	leafID  *EntryID
	labels  map[EntryID]string
}

type sessionHeader struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

func CreateJSONL(path string, opts CreateOptions) (*JsonlSessionStorage, error) {
	if opts.CWD == "" {
		return nil, sessionError(ErrorInvalidSession, "cwd is required")
	}

	id := opts.ID
	if id == "" {
		id = newRandomID()
	}
	timestamp := newTimestamp()
	header := sessionHeader{
		Type:          "session",
		Version:       3,
		ID:            id,
		Timestamp:     timestamp,
		CWD:           opts.CWD,
		ParentSession: opts.ParentSessionPath,
	}

	// pi's newSession writes nothing; the header and any buffered entries
	// reach disk only once the first assistant message lands
	// (session-manager.ts:772-794,843-861).
	return &JsonlSessionStorage{
		path:   path,
		header: header,
		metadata: Metadata{
			ID:                id,
			CWD:               opts.CWD,
			CreatedAt:         timestamp,
			Path:              path,
			ParentSessionPath: opts.ParentSessionPath,
		},
		entries: []SessionEntry{},
		byID:    map[EntryID]int{},
		labels:  map[EntryID]string{},
	}, nil
}

func OpenJSONL(path string) (*JsonlSessionStorage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, sessionError(ErrorNotFound, "Session not found: "+path)
		}
		return nil, wrapSessionError(ErrorStorage, "Failed to read session "+path, err)
	}

	// pi migrates pre-v3 session files on load and rewrites them at v3
	// (session-manager.ts:216-276,755-757); along previously rejected them.
	data, err = migrateSessionFile(path, data)
	if err != nil {
		return nil, err
	}

	var header *sessionHeader
	entries := []SessionEntry{}
	byID := map[EntryID]int{}
	labels := map[EntryID]string{}
	var leafID *EntryID

	lines := bytes.Split(data, []byte("\n"))
	for index, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		lineNumber := index + 1
		if header == nil {
			parsed, err := parseHeaderLine(line, path)
			if err != nil {
				if !json.Valid(line) {
					// pi skips malformed lines wholesale (session-manager.ts:448-455).
					continue
				}
				return nil, err
			}
			header = &parsed
			continue
		}

		entry, err := decodeEntry(line, path, lineNumber)
		if err != nil {
			// pi wraps each line's JSON.parse in try/catch and skips bad
			// lines, so a partially-written trailing line — the exact
			// artifact of a crash during append — costs only itself, never
			// the whole session (session-manager.ts:448-455).
			continue
		}
		entries = append(entries, entry)
		byID[entry.ID] = len(entries) - 1
		updateLabelCache(labels, entry)
		leafID = leafIDAfterEntry(entry)
	}

	if header == nil {
		return nil, invalidSession(path, "missing session header", nil)
	}

	return &JsonlSessionStorage{
		path:    path,
		header:  *header,
		flushed: true,
		metadata: Metadata{
			ID:                header.ID,
			CWD:               header.CWD,
			CreatedAt:         header.Timestamp,
			Path:              path,
			ParentSessionPath: header.ParentSession,
		},
		entries: entries,
		byID:    byID,
		leafID:  leafID,
		labels:  labels,
	}, nil
}

const currentSessionVersion = 3

// migrateSessionFile brings a pre-v3 session file to the current version,
// rewriting it on disk, and returns the (possibly rewritten) content. It
// operates on raw JSON maps because pre-v3 entries do not satisfy the strict
// v3 decoder (no id/parentId).
//
// pi: migrateV1ToV2 adds the id/parentId chain and converts compaction
// firstKeptEntryIndex (an index into the file entries INCLUDING the header)
// to firstKeptEntryId; migrateV2ToV3 renames the hookMessage message role to
// custom (session-manager.ts:215-276).
func migrateSessionFile(path string, data []byte) ([]byte, error) {
	fileEntries := []map[string]any{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			// pi parses with per-line try/catch before migrating; malformed
			// lines are dropped (session-manager.ts:448-455).
			continue
		}
		fileEntries = append(fileEntries, entry)
	}

	version := 0.0
	for _, entry := range fileEntries {
		if entry["type"] == "session" {
			if v, ok := entry["version"].(float64); ok {
				version = v
			} else {
				version = 1 // v1 headers carry no version field
			}
			break
		}
	}
	if version == 0 || version >= currentSessionVersion {
		return data, nil
	}

	if version < 2 {
		migrateSessionV1ToV2(fileEntries)
	}
	if version < 3 {
		migrateSessionV2ToV3(fileEntries)
	}

	var buf bytes.Buffer
	for _, entry := range fileEntries {
		line, err := json.Marshal(entry)
		if err != nil {
			return nil, wrapSessionError(ErrorStorage, "Failed to migrate session "+path, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return nil, wrapSessionError(ErrorStorage, "Failed to rewrite migrated session "+path, err)
	}
	return buf.Bytes(), nil
}

func migrateSessionV1ToV2(fileEntries []map[string]any) {
	ids := map[string]struct{}{}
	generateID := func() string {
		for range 100 {
			id := newRandomID()[:8]
			if _, ok := ids[id]; !ok {
				return id
			}
		}
		return newRandomID()
	}

	var prevID any
	for _, entry := range fileEntries {
		if entry["type"] == "session" {
			entry["version"] = 2
			continue
		}

		id := generateID()
		ids[id] = struct{}{}
		entry["id"] = id
		entry["parentId"] = prevID
		prevID = id

		if entry["type"] == "compaction" {
			if idx, ok := entry["firstKeptEntryIndex"].(float64); ok {
				if i := int(idx); i >= 0 && i < len(fileEntries) {
					target := fileEntries[i]
					if target["type"] != "session" {
						// Targets later in the file have no id yet; pi has the
						// same property (compaction targets precede it).
						if targetID, ok := target["id"].(string); ok {
							entry["firstKeptEntryId"] = targetID
						}
					}
				}
				delete(entry, "firstKeptEntryIndex")
			}
		}
	}
}

func migrateSessionV2ToV3(fileEntries []map[string]any) {
	for _, entry := range fileEntries {
		if entry["type"] == "session" {
			entry["version"] = 3
			continue
		}
		if entry["type"] != "message" {
			continue
		}
		if msg, ok := entry["message"].(map[string]any); ok && msg["role"] == "hookMessage" {
			msg["role"] = "custom"
		}
	}
}

func LoadJSONLMetadata(path string) (Metadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Metadata{}, sessionError(ErrorNotFound, "Session not found: "+path)
		}
		return Metadata{}, wrapSessionError(ErrorStorage, "Failed to read session header "+path, err)
	}

	lines := bytes.Split(data, []byte("\n"))
	var header *sessionHeader
	var lastActivity time.Time
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if header == nil {
			parsed, err := parseHeaderLine(line, path)
			if err != nil {
				if !json.Valid(line) {
					continue
				}
				return Metadata{}, err
			}
			header = &parsed
			continue
		}
		if activity, ok := entryActivityTime(line); ok && activity.After(lastActivity) {
			lastActivity = activity
		}
	}
	if header == nil {
		return Metadata{}, invalidSession(path, "missing session header", nil)
	}

	// pi's modified time is the latest user/assistant message timestamp,
	// falling back to the header timestamp, then the file mtime
	// (session-manager.ts:515-551).
	modified := lastActivity
	if modified.IsZero() {
		if headerTime, err := time.Parse(time.RFC3339Nano, header.Timestamp); err == nil {
			modified = headerTime
		} else if info, err := os.Stat(path); err == nil {
			modified = info.ModTime()
		}
	}

	return Metadata{
		ID:                header.ID,
		CWD:               header.CWD,
		CreatedAt:         header.Timestamp,
		Modified:          modified,
		Path:              path,
		ParentSessionPath: header.ParentSession,
	}, nil
}

// entryActivityTime extracts the activity timestamp of a user/assistant
// message entry, mirroring pi's getLastActivityTime
// (session-manager.ts:515-541).
func entryActivityTime(line []byte) (time.Time, bool) {
	var probe struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   *struct {
			Role      string          `json:"role"`
			Timestamp int64           `json:"timestamp"`
			Content   json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return time.Time{}, false
	}
	if probe.Type != "message" || probe.Message == nil {
		return time.Time{}, false
	}
	msg := probe.Message
	if msg.Role != "user" && msg.Role != "assistant" {
		return time.Time{}, false
	}
	if len(msg.Content) == 0 || string(msg.Content) == "null" {
		return time.Time{}, false
	}
	if msg.Timestamp > 0 {
		return time.UnixMilli(msg.Timestamp), true
	}
	if entryTime, err := time.Parse(time.RFC3339Nano, probe.Timestamp); err == nil {
		return entryTime, true
	}
	return time.Time{}, false
}

func (s *JsonlSessionStorage) GetMetadata() Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metadata
}

func (s *JsonlSessionStorage) GetLeafID() (*EntryID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leafID != nil {
		if _, ok := s.byID[*s.leafID]; !ok {
			return nil, sessionError(ErrorInvalidSession, fmt.Sprintf("Entry %s not found", *s.leafID))
		}
	}
	return copyEntryIDPtr(s.leafID), nil
}

func (s *JsonlSessionStorage) SetLeafID(leafID *EntryID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if leafID != nil {
		if _, ok := s.byID[*leafID]; !ok {
			return sessionError(ErrorNotFound, fmt.Sprintf("Entry %s not found", *leafID))
		}
	}

	entry := SessionEntry{
		Type:      "leaf",
		ID:        s.createEntryIDLocked(),
		ParentID:  copyEntryIDPtr(s.leafID),
		Timestamp: newTimestamp(),
		TargetID:  copyEntryIDPtr(leafID),
	}
	s.entries = append(s.entries, entry)
	s.byID[entry.ID] = len(s.entries) - 1
	s.leafID = copyEntryIDPtr(leafID)
	return s.persistLocked(entry)
}

func (s *JsonlSessionStorage) CreateEntryID() EntryID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createEntryIDLocked()
}

func (s *JsonlSessionStorage) AppendEntry(entry SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = append(s.entries, entry)
	s.byID[entry.ID] = len(s.entries) - 1
	updateLabelCache(s.labels, entry)
	s.leafID = leafIDAfterEntry(entry)
	return s.persistLocked(entry)
}

func (s *JsonlSessionStorage) GetEntry(id EntryID) (SessionEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, ok := s.byID[id]
	if !ok {
		return SessionEntry{}, false
	}
	return s.entries[index], true
}

func (s *JsonlSessionStorage) FindEntries(entryType string) []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := []SessionEntry{}
	for _, entry := range s.entries {
		if entry.Type == entryType {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (s *JsonlSessionStorage) GetLabel(id EntryID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	label, ok := s.labels[id]
	return label, ok
}

func (s *JsonlSessionStorage) GetPathToRoot(leafID *EntryID) ([]SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if leafID == nil {
		return []SessionEntry{}, nil
	}
	currentID := *leafID
	path := []SessionEntry{}
	for {
		index, ok := s.byID[currentID]
		if !ok {
			return nil, sessionError(ErrorNotFound, fmt.Sprintf("Entry %s not found", currentID))
		}
		current := s.entries[index]
		path = append(path, current)
		if current.ParentID == nil {
			break
		}
		parentID := *current.ParentID
		if _, ok := s.byID[parentID]; !ok {
			return nil, sessionError(ErrorInvalidSession, fmt.Sprintf("Entry %s not found", parentID))
		}
		currentID = parentID
	}
	reverseEntries(path)
	return path, nil
}

func (s *JsonlSessionStorage) GetEntries() []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SessionEntry(nil), s.entries...)
}

func (s *JsonlSessionStorage) createEntryIDLocked() EntryID {
	for range 100 {
		id := EntryID(newRandomID()[:8])
		if _, ok := s.byID[id]; !ok {
			return id
		}
	}
	return EntryID(newRandomID())
}

// persistLocked mirrors pi's _persist: nothing reaches disk until the session
// contains an assistant message; at that point the header and every buffered
// entry are flushed, and later entries append directly
// (session-manager.ts:843-861).
func (s *JsonlSessionStorage) persistLocked(entry SessionEntry) error {
	if s.flushed {
		return s.appendLineLocked(entry)
	}
	if !s.hasAssistantLocked() {
		return nil
	}
	return s.flushAllLocked()
}

func (s *JsonlSessionStorage) hasAssistantLocked() bool {
	for _, entry := range s.entries {
		if entry.Type != "message" {
			continue
		}
		if msg, ok := message.AsAIMessage(entry.Message); ok && msg.Role == ai.RoleAssistant {
			return true
		}
	}
	return false
}

func (s *JsonlSessionStorage) flushAllLocked() error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	if err := encoder.Encode(s.header); err != nil {
		return wrapSessionError(ErrorStorage, "Failed to flush session "+s.path, err)
	}
	for _, entry := range s.entries {
		if err := encoder.Encode(entry); err != nil {
			return wrapSessionError(ErrorStorage, "Failed to flush session "+s.path, err)
		}
	}
	if err := os.WriteFile(s.path, buf.Bytes(), 0o600); err != nil {
		return wrapSessionError(ErrorStorage, "Failed to flush session "+s.path, err)
	}
	s.flushed = true
	return nil
}

func (s *JsonlSessionStorage) appendLineLocked(entry SessionEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return wrapSessionError(
			ErrorInvalidEntry,
			"Failed to encode session entry "+string(entry.ID),
			err,
		)
	}

	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return wrapSessionError(ErrorStorage, "Failed to append session entry "+string(entry.ID), err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	if _, err := writer.Write(data); err != nil {
		return wrapSessionError(ErrorStorage, "Failed to append session entry "+string(entry.ID), err)
	}
	if err := writer.WriteByte('\n'); err != nil {
		return wrapSessionError(ErrorStorage, "Failed to append session entry "+string(entry.ID), err)
	}
	if err := writer.Flush(); err != nil {
		return wrapSessionError(ErrorStorage, "Failed to append session entry "+string(entry.ID), err)
	}
	return nil
}

func parseHeaderLine(line []byte, filePath string) (sessionHeader, error) {
	var parsed struct {
		Type          string          `json:"type"`
		Version       int             `json:"version"`
		ID            string          `json:"id"`
		Timestamp     string          `json:"timestamp"`
		CWD           string          `json:"cwd"`
		ParentSession json.RawMessage `json:"parentSession"`
	}
	if err := json.Unmarshal(line, &parsed); err != nil {
		return sessionHeader{}, invalidSession(filePath, "first line is not a valid session header", err)
	}
	if parsed.Type != "session" {
		return sessionHeader{}, invalidSession(filePath, "first line is not a valid session header", nil)
	}
	// pi treats a header without a version as v1 and migrates v1/v2 on open
	// (session-manager.ts:266-276); metadata reads tolerate them as-is. Only
	// versions newer than ours are unsupported.
	if parsed.Version > currentSessionVersion {
		return sessionHeader{}, invalidSession(filePath, "unsupported session version", nil)
	}
	if parsed.ID == "" {
		return sessionHeader{}, invalidSession(filePath, "session header is missing id", nil)
	}
	if parsed.Timestamp == "" {
		return sessionHeader{}, invalidSession(filePath, "session header is missing timestamp", nil)
	}
	if parsed.CWD == "" {
		return sessionHeader{}, invalidSession(filePath, "session header is missing cwd", nil)
	}
	header := sessionHeader{
		Type:      parsed.Type,
		Version:   parsed.Version,
		ID:        parsed.ID,
		Timestamp: parsed.Timestamp,
		CWD:       parsed.CWD,
	}
	if len(parsed.ParentSession) > 0 {
		if string(parsed.ParentSession) == "null" {
			return sessionHeader{}, invalidSession(filePath, "session header parentSession must be a string", nil)
		}
		var parentSession string
		if err := json.Unmarshal(parsed.ParentSession, &parentSession); err != nil {
			return sessionHeader{}, invalidSession(filePath, "session header parentSession must be a string", err)
		}
		header.ParentSession = parentSession
	}
	return header, nil
}

func invalidSession(filePath string, message string, err error) error {
	return wrapSessionError(
		ErrorInvalidSession,
		fmt.Sprintf("Invalid JSONL session file %s: %s", filePath, message),
		err,
	)
}

func invalidEntry(filePath string, lineNumber int, message string, err error) error {
	return wrapSessionError(
		ErrorInvalidEntry,
		fmt.Sprintf("Invalid JSONL session file %s: line %d %s", filePath, lineNumber, message),
		err,
	)
}

func updateLabelCache(labels map[EntryID]string, entry SessionEntry) {
	if entry.Type != "label" || entry.TargetID == nil {
		return
	}
	label := strings.TrimSpace(entry.Label)
	if label == "" {
		delete(labels, *entry.TargetID)
		return
	}
	labels[*entry.TargetID] = label
}

func leafIDAfterEntry(entry SessionEntry) *EntryID {
	if entry.Type == "leaf" {
		return copyEntryIDPtr(entry.TargetID)
	}
	return copyEntryIDPtr(&entry.ID)
}

func reverseEntries(entries []SessionEntry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}

func newRandomID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		fillFallbackRandom(random[:])
	}

	timestamp := time.Now().UTC().UnixMilli()
	uuidMu.Lock()
	if timestamp > uuidLastTimestamp {
		uuidSequence = uint32(random[6])<<24 |
			uint32(random[7])<<16 |
			uint32(random[8])<<8 |
			uint32(random[9])
		uuidLastTimestamp = timestamp
	} else {
		uuidSequence++
		if uuidSequence == 0 {
			uuidLastTimestamp++
		}
	}
	timestamp = uuidLastTimestamp
	sequence := uuidSequence
	uuidMu.Unlock()

	random[0] = byte(timestamp >> 40)
	random[1] = byte(timestamp >> 32)
	random[2] = byte(timestamp >> 24)
	random[3] = byte(timestamp >> 16)
	random[4] = byte(timestamp >> 8)
	random[5] = byte(timestamp)
	random[6] = 0x70 | byte((sequence>>28)&0x0f)
	random[7] = byte(sequence >> 20)
	random[8] = 0x80 | byte((sequence>>14)&0x3f)
	random[9] = byte(sequence >> 6)
	random[10] = byte((sequence&0x3f)<<2) | (random[10] & 0x03)

	return formatUUID(random)
}

func newTimestamp() string {
	return time.Now().UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

var (
	uuidMu            sync.Mutex
	uuidLastTimestamp int64 = -1
	uuidSequence      uint32
)

func fillFallbackRandom(dst []byte) {
	value := time.Now().UnixNano()
	for i := range dst {
		dst[i] = byte(value >> ((i % 8) * 8))
	}
}

func formatUUID(bytes [16]byte) string {
	var encoded [36]byte
	hex.Encode(encoded[0:8], bytes[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], bytes[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], bytes[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], bytes[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], bytes[10:16])
	return string(encoded[:])
}
