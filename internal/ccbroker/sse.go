package ccbroker

import "sync"

// SSESubscriber receives events for a single session.
type SSESubscriber struct {
	Ch   chan *StreamClientEvent
	done chan struct{}
	once sync.Once
}

func newSSESubscriber() *SSESubscriber {
	return &SSESubscriber{
		Ch:   make(chan *StreamClientEvent, 256),
		done: make(chan struct{}),
	}
}

// Close marks the subscriber as done.
func (s *SSESubscriber) Close() {
	s.once.Do(func() { close(s.done) })
}

// Done returns a channel closed when the subscriber is removed.
func (s *SSESubscriber) Done() <-chan struct{} {
	return s.done
}

// SSEBroker fans out events to per-session subscribers.
type SSEBroker struct {
	mu          sync.RWMutex
	subscribers map[string]map[*SSESubscriber]struct{}
}

// NewSSEBroker creates a new SSE broker.
func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		subscribers: make(map[string]map[*SSESubscriber]struct{}),
	}
}

// Subscribe creates a new subscriber for a session.
func (b *SSEBroker) Subscribe(sessionID string) *SSESubscriber {
	sub := newSSESubscriber()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers[sessionID] == nil {
		b.subscribers[sessionID] = make(map[*SSESubscriber]struct{})
	}
	b.subscribers[sessionID][sub] = struct{}{}
	return sub
}

// Unsubscribe removes a subscriber.
func (b *SSEBroker) Unsubscribe(sessionID string, sub *SSESubscriber) {
	sub.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs, ok := b.subscribers[sessionID]; ok {
		delete(subs, sub)
		if len(subs) == 0 {
			delete(b.subscribers, sessionID)
		}
	}
}

// Publish sends an event to all subscribers of a session.
// If a subscriber's channel is full, it is closed (force reconnect).
func (b *SSEBroker) Publish(sessionID string, event *StreamClientEvent) {
	b.mu.RLock()
	subs, ok := b.subscribers[sessionID]
	if !ok {
		b.mu.RUnlock()
		return
	}
	// Snapshot subscriber list to avoid holding lock during send.
	snapshot := make([]*SSESubscriber, 0, len(subs))
	for sub := range subs {
		snapshot = append(snapshot, sub)
	}
	b.mu.RUnlock()

	for _, sub := range snapshot {
		select {
		case sub.Ch <- event:
		default:
			// Channel full — force reconnect.
			sub.Close()
		}
	}
}
