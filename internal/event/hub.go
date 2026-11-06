// Package event is a tiny fan-out hub used to push panel events (status
// changes, player list changes) to the browser over SSE.
package event

import "sync"

// Event is one server-sent event.
type Event struct {
	Kind string
	Data any
}

// Hub broadcasts events to all current subscribers.
type Hub struct {
	mu   sync.RWMutex
	subs map[int]chan Event
	next int
}

// New creates an empty hub.
func New() *Hub {
	return &Hub{subs: map[int]chan Event{}}
}

// Publish delivers an event to every subscriber without blocking.
func (h *Hub) Publish(kind string, data any) {
	h.mu.RLock()
	subs := make([]chan Event, 0, len(h.subs))
	for _, ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- Event{Kind: kind, Data: data}:
		default:
			// Dropped; the SSE stream also sends a periodic snapshot.
		}
	}
}

// Subscribe returns a channel of events and a cancel function.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 128)

	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if existing, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(existing)
		}
		h.mu.Unlock()
	}
}
