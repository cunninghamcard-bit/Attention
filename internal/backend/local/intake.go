package local

import (
	"context"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
)

// JobQueue 是控制面→执行面收件箱的进程内实现；server 形态换 jobs 表+lease（P5）。
// local 取出即消费，没有租约状态，Ack/Nack/Heartbeat 都是 no-op。
type JobQueue struct{ ch chan backend.Job }

func NewJobQueue(buf int) *JobQueue { return &JobQueue{ch: make(chan backend.Job, buf)} }

func (q *JobQueue) Enqueue(ctx context.Context, m backend.Job) error {
	select {
	case q.ch <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *JobQueue) Lease(ctx context.Context) (backend.LeasedJob, error) {
	select {
	case m := <-q.ch:
		return backend.LeasedJob{Job: m}, nil
	case <-ctx.Done():
		return backend.LeasedJob{}, ctx.Err()
	}
}

// LeaseSession is a desktop no-op. Local single-worker mode continues to
// consume all jobs through Lease; server assembly injects PgJobQueue.LeaseSession.
func (q *JobQueue) LeaseSession(
	ctx context.Context,
	sessionIDs []string,
	workerID string,
) (backend.LeasedJob, bool, error) {
	return backend.LeasedJob{}, false, nil
}

func (q *JobQueue) Ack(ctx context.Context, leaseToken string) error {
	return nil
}

func (q *JobQueue) Nack(ctx context.Context, leaseToken string, retryAfter time.Duration) error {
	return nil
}

func (q *JobQueue) Heartbeat(ctx context.Context, leaseToken string) error {
	return nil
}
