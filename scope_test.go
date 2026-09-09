package dllog

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

func TestScopeAddsCarrierToContext(t *testing.T) {
	base := context.Background()
	if fromContext(base) != nil {
		t.Fatal("a bare context reports a scope")
	}

	ctx, done := Scope(base)
	defer done()

	if fromContext(ctx) == nil {
		t.Fatal("Scope did not put a carrier on the context")
	}
	if fromContext(base) != nil {
		t.Fatal("Scope mutated the parent context")
	}
}

// The ring is deliberately not allocated until a record needs it, which is what
// lets a package-level Scope honor a Handler's WithCapacity.
func TestScopeDefersRingAllocation(t *testing.T) {
	ctx, done := Scope(context.Background())
	defer done()

	c := fromContext(ctx)
	if c.bound() != nil {
		t.Fatal("Scope allocated a ring before any record was buffered")
	}

	log := slog.New(New(&capture{}, WithLevel(slog.LevelInfo)))
	log.DebugContext(ctx, "first")

	if c.bound() == nil {
		t.Fatal("buffering a record did not bind a ring")
	}
}

func TestScopeJoinsRatherThanNests(t *testing.T) {
	outer, outerDone := Scope(context.Background())
	defer outerDone()

	inner, innerDone := Scope(outer)

	if fromContext(inner) != fromContext(outer) {
		t.Fatal("the inner Scope created a second carrier, want the same one")
	}

	// The inner done is a no-op: only the creator releases.
	log := slog.New(New(&capture{}, WithLevel(slog.LevelInfo)))
	log.DebugContext(outer, "buffered")
	innerDone()

	s := fromContext(outer).bound()
	if s == nil {
		t.Fatal("no ring bound")
	}
	if _, _, tripped := s.Trip(); !tripped {
		t.Fatal("the inner done closed the outer scope; it must be a no-op")
	}
}

func TestScopeCapacityComesFromTheHandler(t *testing.T) {
	ctx, done := Scope(context.Background())
	defer done()

	log := slog.New(New(&capture{}, WithLevel(slog.LevelInfo), WithCapacity(4)))
	log.DebugContext(ctx, "bind me")

	if got := fromContext(ctx).bound().Capacity(); got != 4 {
		t.Fatalf("ring capacity = %d, want 4 (WithCapacity must reach the scope)", got)
	}
}

func TestDoneClosesTheScopeAndIsIdempotent(t *testing.T) {
	ctx, done := Scope(context.Background())

	log := slog.New(New(&capture{}, WithLevel(slog.LevelInfo)))
	log.DebugContext(ctx, "buffered")
	s := fromContext(ctx).bound()

	done()
	done() // must not panic or double-release

	if _, _, tripped := s.Trip(); tripped {
		t.Fatal("Trip succeeded on a closed scope, want no-op")
	}
}

// A scope that never buffered anything must still release cleanly.
func TestDoneOnUnboundScope(t *testing.T) {
	_, done := Scope(context.Background())
	done()
}

func TestConcurrentBindAgreesOnOneScope(t *testing.T) {
	ctx, done := Scope(context.Background())
	defer done()

	c := fromContext(ctx)
	h := New(&capture{}, WithLevel(slog.LevelInfo))

	const n = 16
	var wg sync.WaitGroup
	seen := make([]any, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen[i] = c.bind(h.pool)
		}()
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if seen[i] != seen[0] {
			t.Fatalf("goroutine %d bound a different scope; a context chain must have exactly one ring", i)
		}
	}
}

// A Trip that lands before any record was buffered must still put the scope in
// the pass-through state. Otherwise the records that follow a failure would be
// buffered and then silently discarded at done(), which is the opposite of what
// Trip was called for.
func TestTripBeforeAnyRecordStillPassesThrough(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	defer done()

	Trip(ctx)
	log.DebugContext(ctx, "after the trip")

	if got, want := c.messages(), []string{"after the trip"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

// The same gap via a level-triggered trip rather than an explicit one.
func TestErrorBeforeAnyBufferedRecordTripsScope(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	defer done()

	log.ErrorContext(ctx, "boom")
	log.DebugContext(ctx, "after")

	if got, want := c.messages(), []string{"boom", "after"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func TestFromContextIgnoresForeignValues(t *testing.T) {
	if fromContext(context.WithValue(context.Background(), struct{}{}, "x")) != nil {
		t.Fatal("an unrelated context value was read as a carrier")
	}
}
