// Package local 是三接口的桌面实现：JSONL 事件存储 + 进程内总线/队列。
package local

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

type EventStore struct {
	root string
	mu   sync.Mutex
	last map[string]uint64 // sessionID → 最近 seq（懒加载）
}

func NewEventStore(root string) *EventStore {
	return &EventStore{root: root, last: map[string]uint64{}}
}

func (s *EventStore) path(sessionID string) string {
	return filepath.Join(s.root, sessionID+".events.jsonl")
}

func (s *EventStore) Append(ctx context.Context, e *protocol.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.last[e.SessionID]
	if !ok {
		var err error
		if last, err = s.scanLastSeq(e.SessionID); err != nil {
			return err
		}
	}
	e.Seq = last + 1
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("eventstore: marshal: %w", err)
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(e.SessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	s.last[e.SessionID] = e.Seq
	return nil
}

func (s *EventStore) ReadAfter(
	ctx context.Context,
	sessionID string,
	afterSeq uint64,
	limit int,
) ([]protocol.Envelope, error) {
	f, err := os.Open(s.path(sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []protocol.Envelope
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e protocol.Envelope
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("eventstore: corrupt line in %s: %w", sessionID, err)
		}
		if e.Seq > afterSeq {
			out = append(out, e)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, sc.Err()
}

func (s *EventStore) scanLastSeq(sessionID string) (uint64, error) {
	evs, err := s.ReadAfter(context.Background(), sessionID, 0, 0)
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return 0, nil
	}
	return evs[len(evs)-1].Seq, nil
}
