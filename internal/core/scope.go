package core

import (
	"sync"
	"sync/atomic"
)

// DefaultCapacity is the ring size used when a non-positive capacity is requested.
const DefaultCapacity = 256

// Action tells the caller what to do with an entry it just passed to Append.
type Action uint8

const (
	// ActionBuffered means the entry was stored in the ring and must not be
	// emitted now; it will come back from Trip if the scope ever trips.
	ActionBuffered Action = iota
	// ActionPassThrough means the caller must emit the entry immediately, as
	// if no scope existed. Returned after a trip, and after Close.
	ActionPassThrough
	// ActionSuppressed means the caller must drop the entry: the post-trip
	// budget is exhausted.
	ActionSuppressed
)

// String returns the constant name, for test failures and debugging.
func (a Action) String() string {
	switch a {
	case ActionBuffered:
		return "ActionBuffered"
	case ActionPassThrough:
		return "ActionPassThrough"
	case ActionSuppressed:
		return "ActionSuppressed"
	default:
		return "Action(unknown)"
	}
}

// Scope is the per-operation buffering engine: a bounded drop-oldest ring of
// entries, a one-shot trip that hands the buffered entries back to the caller,
// and an explicit Close.
//
// A Scope is safe for concurrent use. It never emits anything itself: the
// caller owns emission and decides what an entry is.
//
// The zero value is not usable; call NewScope.
type Scope[T any] struct {
	// tripped mirrors the mutex-guarded state so the post-trip hot path can
	// skip the ring bookkeeping; it is only ever set under mu.
	tripped atomic.Bool

	mu     sync.Mutex
	ring   []T
	start  int // index of the oldest entry
	length int // entries currently held, always <= len(ring)

	dropped   int // entries evicted by the ring
	postTrip  int // pass-throughs already granted after the trip
	limit     int // post-trip budget, 0 = unlimited
	closed    bool
	tripState bool
}

// NewScope returns a scope holding at most capacity entries before evicting the
// oldest. A capacity <= 0 is normalized to DefaultCapacity. postTripLimit caps
// the number of entries allowed through after the trip; 0 means unlimited and
// negative values are treated as 0.
func NewScope[T any](capacity, postTripLimit int) *Scope[T] {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if postTripLimit < 0 {
		postTripLimit = 0
	}
	return &Scope[T]{
		ring:  make([]T, capacity),
		limit: postTripLimit,
	}
}

// Capacity returns the ring size, after normalization.
func (s *Scope[T]) Capacity() int { return len(s.ring) }

// Tripped reports whether the scope has tripped. It does not take the mutex.
func (s *Scope[T]) Tripped() bool { return s.tripped.Load() }

// Len returns the number of entries currently buffered.
func (s *Scope[T]) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.length
}

// Dropped returns how many entries the ring evicted to make room.
func (s *Scope[T]) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Append offers an entry to the scope and reports what the caller must do with
// it. Before the trip the entry is buffered (evicting the oldest when full).
// After the trip it passes through until the post-trip budget is spent, then is
// suppressed. After Close it always passes through.
func (s *Scope[T]) Append(entry T) Action {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ActionPassThrough
	}
	if s.tripState {
		if s.limit > 0 && s.postTrip >= s.limit {
			return ActionSuppressed
		}
		s.postTrip++
		return ActionPassThrough
	}

	idx := (s.start + s.length) % len(s.ring)
	s.ring[idx] = entry
	if s.length == len(s.ring) {
		s.start = (s.start + 1) % len(s.ring)
		s.dropped++
		return ActionBuffered
	}
	s.length++
	return ActionBuffered
}

// Trip flushes the scope exactly once. On the first call it returns the
// buffered entries in insertion order (oldest first), the number of entries the
// ring evicted, and tripped=true. Any later call, or a call on a closed scope,
// returns nil, 0, false and changes nothing.
//
// The returned slice is owned by the caller: the scope keeps no reference to
// it, so recycling the ring later cannot alias it.
func (s *Scope[T]) Trip() (entries []T, dropped int, tripped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.tripState {
		return nil, 0, false
	}
	s.tripState = true
	s.tripped.Store(true)

	entries = make([]T, s.length)
	for i := range entries {
		entries[i] = s.ring[(s.start+i)%len(s.ring)]
	}
	dropped = s.dropped

	s.clearRing()
	return entries, dropped, true
}

// Close releases the scope. Subsequent Appends pass through as if no scope
// existed, and Trip becomes a no-op. Close is idempotent and safe after a trip.
func (s *Scope[T]) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true
	s.clearRing()
}

// clearRing drops the scope's references to buffered entries so their referents
// can be collected without waiting for the scope itself. Callers hold mu.
func (s *Scope[T]) clearRing() {
	var zero T
	for i := range s.ring {
		s.ring[i] = zero
	}
	s.start = 0
	s.length = 0
}
