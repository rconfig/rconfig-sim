// Package runtime holds the simulator's live, swappable session config:
// faults, device profiles, response delays, the session cap and the active
// device count. Everything the operator may change while the server runs
// lives in one immutable Live value behind an atomic pointer.
//
// Sessions call Holder.Load once when they start and keep that snapshot for
// their whole life, so a swap affects new sessions only and the hot path pays
// one atomic load per session, with no locking.
package runtime

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
)

// Live is one generation of runtime config. It is immutable once stored:
// to change anything, build a new Live (Clone helps) and Store it. Do not
// modify a Live, its Faults or its Profiles map after Store.
type Live struct {
	// Faults is the per-type fault rates. Nil means no faults.
	Faults *fault.Set
	// Profiles maps port to that device's behaviour profile; absent means a
	// normal device.
	Profiles map[int]*profile.Profile
	// ProfileRules and ProfileSeed are what Profiles was built from, kept
	// for reporting: the rules exactly as given, one --device-profiles item
	// each (e.g. "hang:3600s@3%"). Manifest-pinned profiles are not listed.
	ProfileRules []string
	ProfileSeed  int64
	// DelayMin and DelayMax bound the uniform per-command response delay.
	DelayMin, DelayMax time.Duration
	// MaxSessions caps concurrent sessions on this instance.
	MaxSessions int64
	// ActiveDevices is how many devices, in port order from the first port,
	// accept sessions. Devices at or past it behave as the refuse profile.
	ActiveDevices int
	// Generation counts stores. Set by Holder.Store; ignored on input.
	Generation uint64
}

// Clone returns a shallow copy of l, for building the next generation. The
// Faults, Profiles and ProfileRules values are shared, which is safe because
// none is modified after Store; replace them rather than editing them.
// Clone of a nil Live is an empty Live.
func (l *Live) Clone() *Live {
	if l == nil {
		return &Live{}
	}
	c := *l
	return &c
}

// Holder publishes the current Live. The zero value holds nothing: Load
// returns nil until the first Store.
type Holder struct {
	mu  sync.Mutex // serialises Store and Update so no change is lost
	cur atomic.Pointer[Live]
}

// NewHolder returns a Holder whose first generation is initial.
func NewHolder(initial *Live) *Holder {
	h := &Holder{}
	h.Store(initial)
	return h
}

// Load returns the current Live. Callers must treat it as read-only.
func (h *Holder) Load() *Live {
	return h.cur.Load()
}

// Store publishes a copy of l as the next generation and returns that copy.
// Its Generation is one past the previous one (1 for the first Store); the
// Generation in l is ignored and l itself is not modified.
//
// Store replaces the whole Live. To change part of it while other writers may
// be active, use Update, or a concurrent change can be lost.
func (h *Holder) Store(l *Live) *Live {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.storeLocked(l.Clone())
}

// Update applies fn to a copy of the current Live and stores the result as
// the next generation, holding the store lock throughout, so concurrent
// Updates to different fields all survive. If fn returns an error nothing is
// stored and Update returns that error. fn must not call Store or Update.
func (h *Holder) Update(fn func(*Live) error) (*Live, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := h.cur.Load().Clone()
	if err := fn(next); err != nil {
		return nil, err
	}
	return h.storeLocked(next), nil
}

// storeLocked publishes next with the following generation. h.mu must be held.
func (h *Holder) storeLocked(next *Live) *Live {
	next.Generation = 1
	if prev := h.cur.Load(); prev != nil {
		next.Generation = prev.Generation + 1
	}
	h.cur.Store(next)
	return next
}
