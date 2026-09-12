package core

import (
	"context"
	"sync"
)

// Entry is what a shared ring holds: an adapter-supplied emit thunk.
//
// The core never inspects an entry. It stores it, evicts it, and hands it back
// at flush time by calling Emit. Everything logging-specific (the record, the
// downstream handler, the replay key, how a replay marker is rendered) is
// captured in the closure by the adapter that appended it, which is what lets
// this package hold entries from several adapters without naming any of their
// types.
//
// The replay key is part of that capture rather than a flush-time parameter:
// it belongs to the handler that logged the record, which is known at append
// time, so an entry marks itself the way its origin configured it however the
// trip was raised. See docs/adr/0001-replay-key-per-entry.md.
type Entry struct {
	// Emit renders the entry. It is nil only in the zero Entry, which a ring
	// slot holds before its first write and after a clear; Flush never calls a
	// nil Emit.
	Emit func()

	// Notify renders an eviction notice for n lost entries into the same
	// destination this entry would emit to. Flush calls it on the oldest
	// surviving entry when the ring evicted anything, so the notice lands in
	// the stream the batch is about to land in, whichever adapter owns it.
	//
	// It is optional: an adapter that has no notion of such a notice leaves it
	// nil and the eviction is simply not announced in its stream.
	Notify func(n int)
}

// Carrier is the value an adapter stores in a context to mark a buffering
// scope. It holds the one ring every adapter on that context shares, so a trip
// raised through any of them replays what all of them buffered, in the order
// the entries were appended.
//
// The ring is created lazily, by the first adapter that buffers, from that
// adapter's pool and with its capacity. A Carrier is safe for concurrent use;
// once bound, its scope never changes.
type Carrier struct {
	mu    sync.Mutex
	scope *Scope[Entry]

	// preTripped records a Trip that arrived before any entry was buffered.
	// Without it such a trip would vanish, because there is no ring yet to hold
	// the state, and the entries that followed the failure would be buffered
	// and then discarded instead of passing through.
	preTripped bool
}

// Bind returns the carrier's scope, creating it from pool on first use.
//
// Concurrent callers agree on one scope: whoever takes the mutex first wins and
// every later caller receives that same scope, so a context chain always has
// exactly one ring no matter how many goroutines log into it at once. That
// holds across adapters too: the second adapter to buffer joins the ring the
// first one created rather than starting its own.
//
// A scope created after an early Trip is tripped immediately, so it starts in
// the pass-through state its caller already asked for.
func (c *Carrier) Bind(pool *ScopePool[Entry]) *Scope[Entry] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scope == nil {
		c.scope = pool.Get()
		if c.preTripped {
			c.scope.Trip()
		}
	}
	return c.scope
}

// MarkTripped records a trip on a carrier with no ring yet and reports whether
// it was the first such trip. It returns the scope when one already exists, so
// the caller can flush it instead.
func (c *Carrier) MarkTripped() *Scope[Entry] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scope != nil {
		return c.scope
	}
	c.preTripped = true
	return nil
}

// Bound returns the carrier's scope without creating one. It reports nil for a
// scope no adapter has buffered into yet, which is the state a scope that only
// ever saw above-level entries stays in.
func (c *Carrier) Bound() *Scope[Entry] {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scope
}

// Tripped reports whether the scope on this carrier has already tripped,
// whether through a buffered entry that reached the trip level or through a
// bare trip that arrived before anything was buffered.
func (c *Carrier) Tripped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.preTripped {
		return true
	}
	return c.scope != nil && c.scope.Tripped()
}

// Close releases the carrier's ring, if one was ever bound. It is safe to call
// on an unbound carrier and safe to call more than once.
func (c *Carrier) Close() {
	if s := c.Bound(); s != nil {
		s.Close()
	}
}

// ctxKey types the context value so nothing else can collide with it.
//
// It lives here rather than in an adapter because it is what makes a scope
// shared: two adapters that each defined their own key would each see their own
// carrier on the same context, and a trip raised through one could never replay
// what the other buffered.
type ctxKey struct{}

// NewContext returns ctx carrying c, and is how an adapter opens a scope.
//
// c is stored as an any so an adapter can put its own wrapper around the shared
// carrier on the context and still get that exact value back, pointer identity
// intact. Holder is what lets every adapter reach the one Carrier underneath,
// whichever wrapper happens to be stored.
func NewContext(ctx context.Context, c Holder) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// Holder is anything that can produce the shared carrier. An adapter's own
// context wrapper implements it by returning the embedded *Carrier, which is
// how a second adapter finds the ring the first one bound.
type Holder interface {
	Shared() *Carrier
}

// Shared satisfies Holder, so a bare *Carrier can be stored on a context
// directly by an adapter that needs no wrapper of its own. An adapter that
// embeds *Carrier in its own wrapper inherits this method, and with it the
// ability to be found by every other adapter.
func (c *Carrier) Shared() *Carrier { return c }

// FromContext returns the shared carrier ctx holds, or nil when ctx has no
// scope. It resolves through Holder, so it finds the carrier whatever adapter
// put it there.
func FromContext(ctx context.Context) *Carrier {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(ctxKey{}).(Holder)
	if h == nil {
		return nil
	}
	return h.Shared()
}

// HolderFromContext returns the exact value stored on ctx, before Holder
// resolution, so an adapter can recover its own wrapper with identity intact.
func HolderFromContext(ctx context.Context) Holder {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(ctxKey{}).(Holder)
	return h
}

// Flush drains a tripped scope, calling each buffered entry's Emit with key. It
// returns false when the scope did not trip, which means another goroutine got
// there first or the scope is closed.
//
// When the ring evicted entries, the oldest survivor's Notify announces the
// count before that entry is emitted, so the replayed sequence reads in order:
// what was lost, then what was kept. Nothing is announced when no entry
// survived, since there is then no entry to anchor the notice to.
//
// Entries are emitted in append order across every adapter that shares the
// ring, because they share one ring: that ordering is the reason the carrier
// holds a single sequence rather than one ring per adapter.
func Flush(s *Scope[Entry]) bool {
	entries, dropped, tripped := s.Trip()
	if !tripped {
		return false
	}

	if dropped > 0 && len(entries) > 0 && entries[0].Notify != nil {
		entries[0].Notify(dropped)
	}
	for _, e := range entries {
		if e.Emit != nil {
			e.Emit()
		}
	}
	return true
}
