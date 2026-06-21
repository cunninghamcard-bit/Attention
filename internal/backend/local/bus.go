package local

import "sync"

// NotifyBus 是进程内通知总线：每订阅者一条容量 1 的合并信号通道
// （notify-then-fetch：信号只说"有新东西"，内容回 EventStore 取）。
type NotifyBus struct {
	mu   sync.Mutex
	subs map[string]map[int]chan struct{}
	next int
}

func NewNotifyBus() *NotifyBus { return &NotifyBus{subs: map[string]map[int]chan struct{}{}} }

func (b *NotifyBus) Publish(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[sessionID] {
		select {
		case ch <- struct{}{}:
		default: // 已有未消费信号，合并
		}
	}
}

func (b *NotifyBus) Subscribe(sessionID string) (<-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[int]chan struct{}{}
	}
	id := b.next
	b.next++
	ch := make(chan struct{}, 1)
	b.subs[sessionID][id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[sessionID], id)
			b.mu.Unlock()
		})
	}
}
