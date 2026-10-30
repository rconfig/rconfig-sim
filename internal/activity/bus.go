// Package activity produces the live activity data the admin API serves: a
// sampled, rate-capped event stream (Bus) and a per-device state table
// (DeviceTable). Both are built for the SSH session path: with nobody
// watching, publishing an event is one atomic load, and a device state store
// is a couple of atomic operations. Neither allocates.
package activity

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Event kinds.
const (
	KindSessionStart = "session_start"
	KindSessionEnd   = "session_end"
	KindFault        = "fault"
	KindDropped      = "dropped"
)

// DefaultRateLimit is the default cap, in events per second, on what one
// subscriber is sent, so a burst of 50k sessions cannot flood a client.
const DefaultRateLimit = 200

// DefaultRingSize is the default number of recent events a Bus keeps.
const DefaultRingSize = 1024

// Event is one session event, in the JSON shape the admin API streams.
type Event struct {
	T       int64  `json:"t"` // unix milliseconds
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Kind    string `json:"kind"`
	Outcome string `json:"outcome,omitempty"` // session_end: ok, error, auth_fail, disconnect
	Fault   string `json:"fault,omitempty"`   // fault: the type; session_end: the last fault, if any
	Bytes   int64  `json:"bytes"`             // bytes sent in the session so far
	MS      int64  `json:"ms"`                // milliseconds since the session started
}

// Dropped reports how many events a subscriber lost to the rate cap or a
// full buffer since the last report.
type Dropped struct {
	Kind string `json:"kind"` // always KindDropped
	N    uint64 `json:"n"`
}

// Bus fans events out to subscribers. Each subscriber picks a sample
// probability and gets at most the Bus rate limit per second; events past
// that, or that find its buffer full, are counted as dropped, never waited
// for. The Bus also keeps the most recent events in a fixed-size ring.
//
// With no subscribers Publish returns at once and nothing is recorded, not
// even in the ring.
type Bus struct {
	subs atomic.Int32 // len(subscribers), read without the lock

	mu          sync.Mutex
	subscribers map[*Subscriber]struct{}
	ring        []Event
	next        int  // ring write index
	wrapped     bool // ring has filled at least once
	rate        float64
}

// NewBus returns a Bus that sends each subscriber at most rate events per
// second (DefaultRateLimit if rate <= 0) and keeps ringSize recent events
// (DefaultRingSize if ringSize <= 0).
func NewBus(rate float64, ringSize int) *Bus {
	if rate <= 0 {
		rate = DefaultRateLimit
	}
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	return &Bus{
		subscribers: map[*Subscriber]struct{}{},
		ring:        make([]Event, ringSize),
		rate:        rate,
	}
}

// Active reports whether anyone is subscribed. Callers can check it before
// building an Event to skip that work too. Nil-safe.
func (b *Bus) Active() bool {
	return b != nil && b.subs.Load() > 0
}

// Publish records ev in the ring and offers it to every subscriber. Never
// blocks on a subscriber. Nil-safe; free when nobody is subscribed.
func (b *Bus) Publish(ev Event) {
	if !b.Active() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ring[b.next] = ev
	b.next++
	if b.next == len(b.ring) {
		b.next = 0
		b.wrapped = true
	}
	var now time.Time
	for s := range b.subscribers {
		if s.sample < 1 && rand.Float64() >= s.sample {
			continue
		}
		if now.IsZero() {
			now = time.Now()
		}
		if !s.take(now) {
			s.dropped.Add(1)
			continue
		}
		select {
		case s.ch <- ev:
		default:
			s.dropped.Add(1)
		}
	}
}

// Recent returns up to n of the most recent events, oldest first.
func (b *Bus) Recent(n int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	have := b.next
	if b.wrapped {
		have = len(b.ring)
	}
	if n > have {
		n = have
	}
	out := make([]Event, 0, n)
	for i := n; i > 0; i-- {
		idx := (b.next - i + len(b.ring)) % len(b.ring)
		out = append(out, b.ring[idx])
	}
	return out
}

// Subscriber receives a sampled share of the Bus's events on C.
type Subscriber struct {
	// C delivers events. It is never closed; stop reading after Unsubscribe.
	C <-chan Event

	ch      chan Event
	sample  float64
	dropped atomic.Uint64

	// Token bucket, guarded by the Bus lock.
	rate   float64
	tokens float64
	last   time.Time
}

// Subscribe adds a subscriber that keeps each event with probability sample
// (clamped to (0,1]; values <= 0 mean 1) and buffers up to buffer events.
// Call Unsubscribe when done.
func (b *Bus) Subscribe(sample float64, buffer int) *Subscriber {
	if sample <= 0 || sample > 1 {
		sample = 1
	}
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan Event, buffer)
	s := &Subscriber{C: ch, ch: ch, sample: sample, rate: b.rate, tokens: b.rate, last: time.Now()}
	b.mu.Lock()
	b.subscribers[s] = struct{}{}
	b.subs.Store(int32(len(b.subscribers)))
	b.mu.Unlock()
	return s
}

// Unsubscribe removes s. Safe to call more than once.
func (b *Bus) Unsubscribe(s *Subscriber) {
	b.mu.Lock()
	delete(b.subscribers, s)
	b.subs.Store(int32(len(b.subscribers)))
	b.mu.Unlock()
}

// TakeDropped returns how many events s has lost since the last call, and
// resets the count.
func (s *Subscriber) TakeDropped() uint64 {
	return s.dropped.Swap(0)
}

// take spends one token, refilling at s.rate per second up to a one-second
// burst. Called with the Bus lock held.
func (s *Subscriber) take(now time.Time) bool {
	s.tokens += now.Sub(s.last).Seconds() * s.rate
	if s.tokens > s.rate {
		s.tokens = s.rate
	}
	s.last = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}
