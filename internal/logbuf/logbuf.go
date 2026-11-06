// Package logbuf keeps the recent console output in memory and fans new lines
// out to live subscribers (the browser console uses SSE).
package logbuf

import (
	"fmt"
	"sync"
	"time"
)

// Line is one console line produced by the game server, steamcmd or the panel.
type Line struct {
	Time   time.Time `json:"time"`
	Source string    `json:"source"`
	Text   string    `json:"text"`
}

// Bus is a bounded ring buffer with fan-out.
type Bus struct {
	mu   sync.RWMutex
	buf  []Line
	size int
	subs map[int]chan Line
	next int
}

// New creates a bus that keeps at most size lines.
func New(size int) *Bus {
	if size <= 0 {
		size = 1000
	}
	return &Bus{
		buf:  make([]Line, 0, size),
		size: size,
		subs: map[int]chan Line{},
	}
}

// Append stores a line and delivers it to every subscriber.
//
// Slow subscribers are skipped instead of blocking the game server: they can
// reconnect and receive the ring buffer snapshot again.
func (b *Bus) Append(l Line) {
	if l.Time.IsZero() {
		l.Time = time.Now()
	}

	b.mu.Lock()
	if len(b.buf) == b.size {
		copy(b.buf, b.buf[1:])
		b.buf[len(b.buf)-1] = l
	} else {
		b.buf = append(b.buf, l)
	}
	subs := make([]chan Line, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- l:
		default:
			// Subscriber is too slow; drop this line rather than stall.
		}
	}
}

// Recent returns up to n of the most recent lines, oldest first.
func (b *Bus) Recent(n int) []Line {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if n <= 0 || n > len(b.buf) {
		n = len(b.buf)
	}
	out := make([]Line, n)
	copy(out, b.buf[len(b.buf)-n:])
	return out
}

// Subscribe returns a channel of live lines and a cancel function.
func (b *Bus) Subscribe() (<-chan Line, func()) {
	ch := make(chan Line, 256)

	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
}

// Printf appends a line attributed to source.
func (b *Bus) Printf(source, format string, args ...any) {
	b.Append(Line{Time: time.Now(), Source: source, Text: fmt.Sprintf(format, args...)})
}
