package core

import (
	"context"
	"sync"
	"sync/atomic"
)

// Entry is what a shared ring holds: an adapter-supplied value that knows how
// to render itself.
//
// The core never inspects an entry. It stores it, evicts it, and hands it back
// at flush time by calling Emit. Everything logging-specific (the record, the
// downstream handler, the replay key, how a replay marker is rendered) lives in
// the adapter's implementing type, which is what lets this package hold entries
// from several adapters without naming any of their types.
//
// It is an interface rather than a struct of funcs deliberately: an adapter's
// slot implements it with pointer methods, so buffering costs one heap
// allocation (the slot itself) where two escaping closures per entry used to
// come on top of it, on the hottest path the package has.
//
// The replay key lives in the implementing slot rather than arriving as a
// flush-time parameter: it belongs to the handler that logged the record, which
// is known at append time, so an entry marks itself the way its origin
// configured it however the trip was raised. See
// docs/adr/0001-replay-key-per-entry.md.
type Entry interface {
	// Emit renders the entry to the destination its adapter captured at append
	// time. Flush never calls Emit on a nil Entry, which is what an untouched
	// or cleared ring slot holds.
	Emit()

	// Notify renders an eviction notice for n lost entries into the same
	// destination this entry would emit to. Flush calls it on the oldest
	// surviving entry when the ring evicted anything, so the notice lands in
	// the stream the batch is about to land in, whichever adapter owns it.
	Notify(n int)
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
	// mu serializes the transitions: creating the scope, recording an early
	// trip, and closing. The state itself lives in the atomics below, written
	// only under mu and read lock-free, because every logged record reads this
	// state (is there a scope, is it closed, has it tripped) and a mutex per
	// read would put three lock acquisitions on the per-record hot path.
	mu sync.Mutex

	// scope is the carrier's one ring, published by Bind's slow path.
	scope atomic.Pointer[Scope[Entry]]

	// preTripped records a Trip that arrived before any entry was buffered.
	// Without it such a trip would vanish, because there is no ring yet to hold
	// the state, and the entries that followed the failure would be buffered
	// and then discarded instead of passing through.
	preTripped atomic.Bool

	// closed records that the scope's done func has run. A context outlives the
	// scope it carried, so entries keep arriving on it afterwards; without this
	// flag Bind would build a fresh ring for them that nothing will ever return
	// to the pool, and a ring that survived would keep intercepting records
	// long after the operation ended.
	closed atomic.Bool
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
//
// It returns nil once the carrier is closed, and callers must treat that as
// "this context has no scope" rather than creating one of their own. Close is
// permanent precisely so a late entry cannot resurrect a released scope.
//
// The common call, every buffered entry after the first, is two atomic loads.
// Only the call that creates the scope takes the mutex. A Close racing the
// lock-free path can let this return a scope that is being released; the scope
// itself answers Appends after its Close with ActionSuppressed, so the entry is
// dropped exactly as a scope-less below-level entry would be.
func (c *Carrier) Bind(pool *ScopePool[Entry]) *Scope[Entry] {
	if c.closed.Load() {
		return nil
	}
	if s := c.scope.Load(); s != nil {
		return s
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return nil
	}
	if s := c.scope.Load(); s != nil {
		return s
	}
	s := pool.Get()
	if c.preTripped.Load() {
		s.Trip()
	}
	c.scope.Store(s)
	return s
}

// MarkTripped records a trip on a carrier with no ring yet. It returns the scope
// when one already exists, so the caller can flush it instead, and reports
// whether this call is the one that moved the carrier into the tripped state.
//
// first is meaningful only alongside a nil scope: when a scope exists, the
// transition is the scope's to report, and Flush tells the caller whether it won
// the trip. A caller that treats "no scope" as "I am the trigger" would exempt
// every entry of a scope that never buffered anything, since each such call
// takes this same branch.
//
// It is inert on a closed carrier: there is nothing left to flush, and
// remembering the trip would hand a later Bind a pre-tripped scope.
//
// The scope-in-hand answer is lock-free; only recording the early trip, a
// transition, takes the mutex, where scope and preTripped are re-read so the
// one-shot claim cannot be granted twice.
func (c *Carrier) MarkTripped() (s *Scope[Entry], first bool) {
	if c.closed.Load() {
		return nil, false
	}
	if s := c.scope.Load(); s != nil {
		return s, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return nil, false
	}
	if s := c.scope.Load(); s != nil {
		return s, false
	}
	if c.preTripped.Load() {
		return nil, false
	}
	c.preTripped.Store(true)
	return nil, true
}

// Bound returns the carrier's scope without creating one. It reports nil for a
// scope no adapter has buffered into yet, which is the state a scope that only
// ever saw above-level entries stays in.
// It reports nil on a closed carrier for the same reason Bind does: a released
// scope is not one an adapter may still buffer into.
func (c *Carrier) Bound() *Scope[Entry] {
	if c.closed.Load() {
		return nil
	}
	return c.scope.Load()
}

// Tripped reports whether the scope on this carrier has already tripped,
// whether through a buffered entry that reached the trip level or through a
// bare trip that arrived before anything was buffered.
//
// A closed carrier is never tripped: it no longer has state to trip, and an
// adapter that saw true here would take its post-trip pass-through branch for an
// entry that belongs to no live scope.
func (c *Carrier) Tripped() bool {
	if c.closed.Load() {
		return false
	}
	if c.preTripped.Load() {
		return true
	}
	s := c.scope.Load()
	return s != nil && s.Tripped()
}

// Closed reports whether the scope's done func has run. Adapters branch on it to
// treat a released context exactly as they treat one that never carried a scope.
// Every logged record on a scoped context reads this, which is why it is an
// atomic load rather than a lock.
func (c *Carrier) Closed() bool {
	return c.closed.Load()
}

// Close releases the carrier's ring, if one was ever bound, and marks the
// carrier closed for good. It is safe to call on an unbound carrier and safe to
// call more than once.
//
// The closed flag is what makes release permanent. The context holding this
// carrier outlives the scope, so entries keep arriving on it; after Close they
// must be handled as if the context never carried a scope at all.
func (c *Carrier) Close() {
	c.mu.Lock()
	s := c.scope.Load()
	c.closed.Store(true)
	c.mu.Unlock()

	// Outside the lock: Scope.Close takes its own, and this ordering keeps the
	// two from being held together anywhere in the package.
	if s != nil {
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

// Flush drains a tripped scope, calling each buffered entry's Emit. It returns
// false when the scope did not trip, which means another goroutine got there
// first or the scope is closed.
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

	if dropped > 0 && len(entries) > 0 && entries[0] != nil {
		entries[0].Notify(dropped)
	}
	for _, e := range entries {
		if e != nil {
			e.Emit()
		}
	}
	return true
}
