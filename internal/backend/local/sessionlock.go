package local

import "context"

// NoopSessionAffinity is the desktop implementation: every local worker may
// acquire because there is only one process-level owner.
type NoopSessionAffinity struct{}

func (NoopSessionAffinity) Acquire(context.Context, string) (func(), bool, error) {
	return func() {}, true, nil
}

// SessionSignal is the process-local wakeup channel for session-targeted jobs.
type SessionSignal struct {
	bus *NotifyBus
}

func NewSessionSignal() *SessionSignal {
	return &SessionSignal{bus: NewNotifyBus()}
}

func (s *SessionSignal) Publish(sessionID string) {
	s.bus.Publish(sessionID)
}

func (s *SessionSignal) Subscribe(sessionID string) (<-chan struct{}, func()) {
	return s.bus.Subscribe(sessionID)
}
