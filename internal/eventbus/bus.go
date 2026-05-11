package eventbus

import (
	"log"
	"sync"
	"sync/atomic"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Handler is a function that processes an event. Handlers are called synchronously
// in a dedicated goroutine per subscriber, so they can block without affecting others.
type Handler func(domain.Event)

// Subscriber is a named event handler with an optional filter.
type Subscriber struct {
	Name   string   // for logging: "ws-broadcast", "scorer", etc.
	Filter []string // if non-empty, only deliver events matching these type prefixes
	Handle Handler
}

// Bus is an in-process event bus. Pollers and system components publish events;
// subscribers (WS broadcaster, scorer, audit logger) consume them.
type Bus struct {
	mu          sync.RWMutex
	subscribers []subscriberEntry
	closed      bool
}

type subscriberEntry struct {
	sub     Subscriber
	ch      chan domain.Event
	closed  bool          // true once the channel has been closed
	dropped *atomic.Int64 // number of events dropped due to full buffer
}

// New creates a new event bus.
func New() *Bus {
	return &Bus{}
}

// Subscribe registers a handler. Starts a goroutine that drains events to the handler.
// Returns an unsubscribe function.
func (b *Bus) Subscribe(sub Subscriber) func() {
	ch := make(chan domain.Event, 256)
	entry := subscriberEntry{sub: sub, ch: ch, dropped: &atomic.Int64{}}

	b.mu.Lock()
	b.subscribers = append(b.subscribers, entry)
	idx := len(b.subscribers) - 1
	b.mu.Unlock()

	// Drain goroutine
	go func() {
		for evt := range ch {
			sub.Handle(evt)
		}
	}()

	// Return unsubscribe function
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, e := range b.subscribers {
			if e.ch == ch {
				if !b.subscribers[i].closed {
					b.subscribers[i].closed = true
					close(ch)
				}
				b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
				return
			}
		}
		_ = idx // suppress unused warning
	}
}

// Publish sends an event to all matching subscribers. Non-blocking — if a subscriber's
// buffer is full, the event is dropped with a warning and the drop counter is incremented.
func (b *Bus) Publish(evt domain.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for i := range b.subscribers {
		entry := &b.subscribers[i]
		if entry.closed {
			continue
		}
		if !matchesFilter(evt.EventType, entry.sub.Filter) {
			continue
		}
		select {
		case entry.ch <- evt:
		default:
			entry.dropped.Add(1)
			log.Printf("[eventbus] dropping event %s for slow subscriber %s (total dropped: %d)",
				evt.EventType, entry.sub.Name, entry.dropped.Load())
		}
	}
}

// Close shuts down all subscriber channels. Safe to call after individual unsubscribes.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for i := range b.subscribers {
		if !b.subscribers[i].closed {
			b.subscribers[i].closed = true
			close(b.subscribers[i].ch)
		}
	}
	b.subscribers = nil
}

// matchesFilter returns true if the event type matches any of the filter prefixes,
// or if the filter is empty (match all).
func matchesFilter(eventType string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, prefix := range filter {
		if len(eventType) >= len(prefix) && eventType[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
