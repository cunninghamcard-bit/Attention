package local

import (
	"context"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
)

func TestBusCoalesces(t *testing.T) {
	b := NewNotifyBus()
	ch, cancel := b.Subscribe("ses_a")
	defer cancel()
	b.Publish("ses_a")
	b.Publish("ses_a") // 合并，不阻塞
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no signal")
	}
}

func TestBusCancelIdempotent(t *testing.T) {
	b := NewNotifyBus()
	_, cancel := b.Subscribe("ses_a")
	cancel()
	cancel() // 第二次不得 panic 或误删他人
	ch2, cancel2 := b.Subscribe("ses_a")
	defer cancel2()
	b.Publish("ses_a")
	select {
	case <-ch2:
	case <-time.After(time.Second):
		t.Fatal("survivor lost signal")
	}
}

func TestJobQueueFIFO(t *testing.T) {
	q := NewJobQueue(16)
	ctx := context.Background()
	_ = q.Enqueue(ctx, backend.Job{SessionID: "a", Kind: backend.JobPrompt})
	_ = q.Enqueue(ctx, backend.Job{SessionID: "a", Kind: backend.JobCancel})
	m1, _ := q.Lease(ctx)
	m2, _ := q.Lease(ctx)
	if m1.Job.Kind != backend.JobPrompt || m2.Job.Kind != backend.JobCancel {
		t.Fatalf("order wrong: %v %v", m1.Job.Kind, m2.Job.Kind)
	}
	if m1.LeaseToken != "" || m2.LeaseToken != "" || m1.Attempts != 0 || m2.Attempts != 0 {
		t.Fatalf("local lease metadata = %+v %+v, want zero values", m1, m2)
	}
	if err := q.Ack(ctx, m1.LeaseToken); err != nil {
		t.Fatalf("local Ack: %v", err)
	}
	if err := q.Nack(ctx, m2.LeaseToken, time.Millisecond); err != nil {
		t.Fatalf("local Nack: %v", err)
	}
	if err := q.Heartbeat(ctx, ""); err != nil {
		t.Fatalf("local Heartbeat: %v", err)
	}
	if leased, ok, err := q.LeaseSession(ctx, []string{"a"}, "worker"); err != nil || ok {
		t.Fatalf("local LeaseSession = %+v ok=%v err=%v, want no-op", leased, ok, err)
	}
}

func TestJobQueueLeaseBlocksUntilCtxDone(t *testing.T) {
	q := NewJobQueue(1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := q.Lease(ctx); err == nil {
		t.Fatal("want ctx error on empty queue")
	}
}
