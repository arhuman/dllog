package dllog

import (
	"context"
	"log/slog"
	"sync"

	"github.com/arhuman/dllog/internal/core"
)

// slot is what this adapter puts in a scope's ring: a cloned record, the
// downstream handler that must emit it, and the replay key to mark it with.
//
// The triple is what makes WithAttrs and WithGroup work without reimplementing
// attr flattening. Each derived Handler eagerly derives its own downstream, and
// a record buffered through that Handler carries it into the ring, so the flush
// emits every record through the handler that was in scope when it was logged,
// marked the way that handler was configured.
//
// It implements core.Entry with pointer methods, so buffering a record costs
// the one allocation of the slot itself; the interface word carries the
// pointer without boxing, and no closures are built on the append path.
type slot struct {
	record     slog.Record
	downstream slog.Handler
	replayKey  string
}

// Emit renders the buffered record through the downstream it was logged with,
// marked with its handler's replay key. It is how the core hands the record
// back at flush time without naming slog.
func (s *slot) Emit() {
	r := s.record
	r.AddAttrs(slog.Bool(s.replayKey, true))
	//nolint:errcheck // a replayed record has no caller left to return to.
	_ = s.downstream.Handle(context.Background(), r)
}

// Notify reports entries the ring evicted, as one record carrying the count
// under DroppedKey. It borrows this slot's time and downstream so the notice
// lands just before the batch it belongs to, in the same stream.
//
// The core calls it on the oldest surviving entry, so an eviction announced on
// a batch whose oldest survivor belongs to another adapter is rendered by that
// adapter, in its own stream, rather than being forced into slog.
func (s *slot) Notify(n int) {
	r := slog.NewRecord(s.record.Time, slog.LevelWarn, DroppedMessage, 0)
	r.AddAttrs(slog.Int(DroppedKey, n))
	//nolint:errcheck // same as Emit: nobody left to report to.
	_ = s.downstream.Handle(context.Background(), r)
}

// carrier is this adapter's handle on the shared scope carrier.
//
// The state it wraps lives in internal/core, together with the context key, so
// every adapter built on the engine binds the same scope on a given context.
// The wrapper exists only to hang slog-typed helpers off that shared state; it
// holds no state of its own, and exactly one is created per scope, so comparing
// two of them still answers "is this the same scope?".
type carrier struct {
	*core.Carrier

	// view memoizes the typed ring view so repeated binds on one scope return
	// the identical *ring. Callers compare what bind returns to decide whether
	// two goroutines agreed on one scope, so handing out a fresh wrapper per
	// call would report a split that never happened.
	viewOnce sync.Once
	view     *ring
}

// typed returns the carrier's one ring view over scope, created on first use.
func (c *carrier) typed(scope *core.Scope[core.Entry]) *ring {
	c.viewOnce.Do(func() { c.view = &ring{scope: scope} })
	return c.view
}

// ring is a typed view of the shared ring, so this adapter appends its own slot
// type while the entries themselves land in the one ring every adapter shares.
//
// It exists because the core ring holds core.Entry, not slot: without the view,
// every append site would have to spell the wrapping out.
type ring struct{ scope *core.Scope[core.Entry] }

// Append offers a slot to the shared ring and reports what the caller must do
// with it, exactly as the underlying scope does. It takes the pointer the ring
// will hold, so the caller's single allocation is the only one.
func (r *ring) Append(s *slot) core.Action { return r.scope.Append(s) }

// Capacity returns the shared ring's size.
func (r *ring) Capacity() int { return r.scope.Capacity() }

// PassThrough reports what the caller must do with a record it never buffers,
// exactly as the underlying scope does.
func (r *ring) PassThrough() core.Action { return r.scope.PassThrough() }

// Trip flushes the shared ring, returning the raw core entries. The emitting
// paths go through flush, which knows the configured replay key; this is the
// form the tests reach for when they only need to know whether a trip took.
func (r *ring) Trip() (entries []core.Entry, dropped int, tripped bool) {
	return r.scope.Trip()
}

// bind returns a typed view of the carrier's shared ring, creating the ring
// from pool on first use, or nil once the scope has been released.
//
// The nil case must not reach typed: memoizing a view over a nil scope would
// make every later bind on this carrier hand back a ring that panics on use.
func (c *carrier) bind(pool *core.ScopePool[core.Entry]) *ring {
	s := c.Bind(pool)
	if s == nil {
		return nil
	}
	return c.typed(s)
}

// bound returns a typed view of the carrier's ring without creating one, or nil
// when no adapter has buffered into this scope yet.
func (c *carrier) bound() *ring {
	s := c.Bound()
	if s == nil {
		return nil
	}
	return c.typed(s)
}

// fromContext returns the carrier ctx holds, or nil when ctx has no live scope.
//
// A released scope reports nil, exactly as a context that never carried one.
// The context outlives the scope, so records keep arriving on it after done has
// run; treating the carrier as absent is what makes those records fall back to
// ordinary level filtering instead of being emitted by a dead scope or binding
// a ring the pool will never see again.
//
// For a scope this package opened it recovers our own wrapper, so two lookups
// return the identical *carrier and callers may compare them to decide whether
// they are looking at one scope.
//
// For a scope another adapter opened it resolves through the core and wraps the
// shared carrier afresh each time. The wrapper is stateless, so the scope,
// its ring and its trip state are still shared; only the wrapper pointer
// differs. Memoizing one here would mean retaining a wrapper per foreign scope
// for the life of the process, which is the unbounded growth the bounded-memory
// design rules out. Compare the embedded Carrier, not the wrapper.
func fromContext(ctx context.Context) *carrier {
	if c, ok := core.HolderFromContext(ctx).(*carrier); ok {
		if c.Closed() {
			return nil
		}
		return c
	}
	// The scope was opened by another adapter, so the context holds its wrapper
	// rather than ours. Resolving through the core reaches the shared carrier
	// anyway; without this the handler would treat the context as scope-less and
	// drop every record logged below the configured level.
	shared := core.FromContext(ctx)
	if shared == nil || shared.Closed() {
		return nil
	}
	return &carrier{Carrier: shared}
}

// Scope opens a buffering scope on ctx and returns the derived context together
// with the function that releases it. Records logged with the returned context
// are buffered instead of dropped, and replayed if the scope trips.
//
// The returned done must be called, normally with defer, exactly once per
// successful call; calling it more than once is safe and does nothing. A scope
// that ends without tripping discards its buffer: those records are gone, by
// design.
//
// Scope joins rather than nests. Called on a context that already carries a
// scope, it returns a context for that same scope and a done that does nothing,
// so only the creator's done releases the buffer and an inner scope can never
// cut an outer one short.
//
// The ring itself is not allocated here. It is created by the first Handler
// that buffers a record, which is what lets the Handler's WithCapacity and
// WithPostTripLimit govern it. Because the scope lives in internal/core, an
// adapter for another logging library binds the same ring on the same context.
func Scope(ctx context.Context) (context.Context, func()) {
	if c := fromContext(ctx); c != nil {
		return ctx, func() {}
	}

	c := &carrier{Carrier: &core.Carrier{}}
	var once sync.Once
	done := func() { once.Do(c.Close) }
	return core.NewContext(ctx, c), done
}

// Trip flushes the scope on ctx immediately, replaying every buffered record to
// the downstream handler it was logged through. Use it for failures that are
// returned rather than logged, which is the common Go case.
//
// Trip is idempotent, and a no-op when ctx carries no scope, when the scope has
// already tripped, or when it has been released. After it returns, records down
// to the buffer floor pass straight through to the downstream handler until the
// scope ends, whether or not anything had been buffered yet.
//
// Each replayed record is marked with the replay key of the handler that
// logged it, so this function honours WithReplayKey without holding a handler.
func Trip(ctx context.Context) {
	trip(ctx)
}

// Trip flushes the scope on ctx exactly as the package-level [Trip] does.
//
// It is kept because a handler is often in hand at the failure site and this
// spelling says so. Since a record now carries the replay key of the handler
// that logged it, the two are equivalent: neither imposes a key on records
// logged through a different handler. See
// docs/adr/0001-replay-key-per-entry.md.
func (h *Handler) Trip(ctx context.Context) {
	trip(ctx)
}

// trip carries the shared behaviour of both entry points.
func trip(ctx context.Context) {
	c := fromContext(ctx)
	if c == nil {
		return
	}
	s, _ := c.MarkTripped()
	if s == nil {
		// Nothing buffered yet: the trip is remembered and applied to the ring
		// when one is created. There is nothing to replay, and a bare Trip is
		// not a record, so whether it won the transition changes nothing here.
		return
	}
	flush(s)
}

// flush drains a tripped scope to the downstream handlers captured with each
// entry. It returns false when the scope did not trip, which means another
// goroutine got there first or the scope is closed.
//
// The dropped-count marker is emitted before the batch so the replayed sequence
// reads in order: what was lost, then what was kept.
func flush(s *core.Scope[core.Entry]) bool {
	return core.Flush(s)
}
