package dllog

import (
	"context"
	"log/slog"
	"sync"

	"github.com/arhuman/dllog/internal/core"
)

// slot is what a scope's ring holds: a cloned record together with the
// downstream handler that must emit it.
//
// The pair is what makes WithAttrs and WithGroup work without reimplementing
// attr flattening. Each derived Handler eagerly derives its own downstream, and
// a record buffered through that Handler carries it into the ring, so the flush
// emits every record through the handler that was in scope when it was logged.
type slot struct {
	record     slog.Record
	downstream slog.Handler
}

// carrier is the value dllog stores in a context. It exists so Scope can be a
// package-level function while WithCapacity and WithPostTripLimit stay per
// Handler: the carrier is created empty, and the first Handler that needs to
// buffer builds the ring from its own pool, with its own capacity.
//
// A carrier is safe for concurrent use. Once bound, its scope never changes.
type carrier struct {
	mu    sync.Mutex
	scope *core.Scope[slot]

	// preTripped records a Trip that arrived before any record was buffered.
	// Without it such a trip would vanish, because there is no ring yet to hold
	// the state, and the records that followed the failure would be buffered
	// and then discarded instead of passing through.
	preTripped bool
}

// bind returns the carrier's scope, creating it from pool on first use.
//
// Concurrent callers agree on one scope: whoever takes the mutex first wins and
// every later caller receives that same scope, so a context chain always has
// exactly one ring no matter how many goroutines log into it at once.
//
// A scope created after an early Trip is tripped immediately, so it starts in
// the pass-through state its caller already asked for.
func (c *carrier) bind(pool *core.ScopePool[slot]) *core.Scope[slot] {
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

// markTripped records a trip on a carrier with no ring yet and reports whether
// it was the first such trip. It returns the scope when one already exists, so
// the caller can flush it instead.
func (c *carrier) markTripped() *core.Scope[slot] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scope != nil {
		return c.scope
	}
	c.preTripped = true
	return nil
}

// bound returns the carrier's scope without creating one. It reports nil for a
// scope that no Handler has buffered into yet, which is the state a scope that
// only ever saw above-level records stays in.
func (c *carrier) bound() *core.Scope[slot] {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scope
}

// ctxKey types the context value so nothing else can collide with it.
type ctxKey struct{}

// fromContext returns the carrier ctx holds, or nil when ctx has no scope.
func fromContext(ctx context.Context) *carrier {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(ctxKey{}).(*carrier)
	return c
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
// WithPostTripLimit govern it.
func Scope(ctx context.Context) (context.Context, func()) {
	if c := fromContext(ctx); c != nil {
		return ctx, func() {}
	}

	c := &carrier{}
	var once sync.Once
	done := func() {
		once.Do(func() {
			if s := c.bound(); s != nil {
				s.Close()
			}
		})
	}
	return context.WithValue(ctx, ctxKey{}, c), done
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
// It marks replayed records with the default key. A handler built with
// WithReplayKey should be tripped through its own [Handler.Trip], which knows
// the configured key; this function has no handler and cannot.
func Trip(ctx context.Context) {
	trip(ctx, defaultReplayKey)
}

// Trip flushes the scope on ctx exactly as the package-level [Trip] does, but
// marks the replayed records with this handler's configured replay key.
//
// Prefer it over [Trip] whenever the handler was built with WithReplayKey:
// mixing the two would put two different markers in one stream, and a query
// filtering on the configured key would silently miss the records that the
// package-level function replayed.
func (h *Handler) Trip(ctx context.Context) {
	trip(ctx, h.cfg.replayKey)
}

// trip carries the shared behaviour of both entry points. They differ only in
// which marker key they hand to the flush.
func trip(ctx context.Context, replayKey string) {
	c := fromContext(ctx)
	if c == nil {
		return
	}
	s := c.markTripped()
	if s == nil {
		// Nothing buffered yet: the trip is remembered and applied to the ring
		// when one is created. There is nothing to replay.
		return
	}
	flush(s, replayKey)
}

// flush drains a tripped scope to the downstream handlers captured with each
// record. It returns false when the scope did not trip, which means another
// goroutine got there first or the scope is closed.
//
// The dropped-count marker is emitted before the batch so the replayed sequence
// reads in order: what was lost, then what was kept.
func flush(s *core.Scope[slot], replayKey string) bool {
	entries, dropped, tripped := s.Trip()
	if !tripped {
		return false
	}

	if dropped > 0 && len(entries) > 0 {
		emitDropped(entries[0], dropped)
	}
	for _, e := range entries {
		r := e.record
		r.AddAttrs(slog.Bool(replayKey, true))
		//nolint:errcheck // a replayed record has no caller left to return to.
		_ = e.downstream.Handle(context.Background(), r)
	}
	return true
}

// emitDropped reports entries the ring evicted, as one record carrying the
// count under DroppedKey. It borrows the oldest surviving entry's time and
// downstream so the notice lands just before the batch it belongs to, in the
// same stream.
func emitDropped(oldest slot, dropped int) {
	r := slog.NewRecord(oldest.record.Time, slog.LevelWarn, DroppedMessage, 0)
	r.AddAttrs(slog.Int(DroppedKey, dropped))
	//nolint:errcheck // same as flush: nobody left to report to.
	_ = oldest.downstream.Handle(context.Background(), r)
}
