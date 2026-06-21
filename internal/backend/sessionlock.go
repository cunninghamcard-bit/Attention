package backend

import "context"

// SessionAffinity is the controlled coupling point for server-form session
// ownership. Implementations use a non-blocking lock so exactly one worker owns
// a session at a time.
type SessionAffinity interface {
	Acquire(ctx context.Context, sessionID string) (release func(), acquired bool, err error)
}

// SessionSignal wakes the worker that owns a session to drain session-targeted
// jobs. It is intentionally separate from NotifyBus so job wakeups do not mix
// with client event notifications.
type SessionSignal interface {
	Publish(sessionID string)
	Subscribe(sessionID string) (<-chan struct{}, func())
}
