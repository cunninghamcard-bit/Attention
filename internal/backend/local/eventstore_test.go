package local

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func ev(sid string) *protocol.Envelope {
	return &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     sid,
		Kind:          protocol.KindMessageDelta,
		Actor:         protocol.ActorAgent,
		Payload:       json.RawMessage(`{}`),
		SchemaVersion: protocol.SchemaVersion,
	}
}

func TestAppendAssignsMonotonicSeq(t *testing.T) {
	s := NewEventStore(t.TempDir())
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		e := ev("ses_a")
		if err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		if e.Seq != uint64(i) {
			t.Fatalf("want seq %d got %d", i, e.Seq)
		}
	}
	got, err := s.ReadAfter(ctx, "ses_a", 1, 100)
	if err != nil || len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("ReadAfter wrong: %v %+v", err, got)
	}
}

func TestSeqSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s1 := NewEventStore(dir)
	_ = s1.Append(ctx, ev("ses_a"))
	s2 := NewEventStore(dir) // 重开模拟进程重启
	e := ev("ses_a")
	if err := s2.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	if e.Seq != 2 {
		t.Fatalf("want 2 got %d", e.Seq)
	}
}

func TestConcurrentAppendUniqueSeq(t *testing.T) {
	s := NewEventStore(t.TempDir())
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Append(ctx, ev("ses_a"))
		}()
	}
	wg.Wait()
	got, _ := s.ReadAfter(ctx, "ses_a", 0, 1000)
	if len(got) != 50 {
		t.Fatalf("want 50 got %d", len(got))
	}
	seen := map[uint64]bool{}
	for _, e := range got {
		if seen[e.Seq] {
			t.Fatalf("dup seq %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}
