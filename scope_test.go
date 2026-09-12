package dllog

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/arhuman/dllog/internal/core"
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

// foreignKey stands in for another package's context key. It is a named type
// rather than struct{}{} because an empty anonymous struct collides with every
// other package using the same trick, which is the collision this test would
// otherwise be demonstrating instead of ruling out.
type foreignKey struct{}

func TestFromContextIgnoresForeignValues(t *testing.T) {
	if fromContext(context.WithValue(context.Background(), foreignKey{}, "x")) != nil {
		t.Fatal("an unrelated context value was read as a carrier")
	}
}

// TestForeignScopeIsNotDropped pins the failure the zap adapter exposed: when
// another adapter opens the scope, this package must still see it.
//
// The old fromContext type-asserted to *carrier, so a scope carried by anyone
// else's wrapper resolved to nil. The handler then treated the context as
// scope-less and, at a level below the configured one, DROPPED the record: not
// buffered, not passed through, gone. Trip was a no-op on the same context.
// Silent log loss is the one outcome this library exists to prevent.
func TestForeignScopeIsNotDropped(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	// A scope opened by some other adapter: a bare *core.Carrier, not our wrapper.
	ctx := core.NewContext(context.Background(), new(core.Carrier))

	if core.FromContext(ctx) == nil {
		t.Fatal("test setup is wrong: the core does not see the carrier")
	}
	if fromContext(ctx) == nil {
		t.Fatal("a scope opened by another adapter resolved to nil: records logged " +
			"on this context are silently dropped")
	}

	log.DebugContext(ctx, "buffered by the foreign scope")
	Trip(ctx)

	if got := c.messages(); !equal(got, []string{"buffered by the foreign scope"}) {
		t.Fatalf("messages = %v, want the buffered record replayed: a Debug logged "+
			"in a foreign scope must be buffered and replayed, never dropped", got)
	}
}

// TestForeignScopeResolvesToOneScope pins what identity is actually for. A
// scope this package opened resolves to one memoized *carrier, so the join test
// can compare pointers. A foreign scope gets a fresh wrapper per lookup, since
// there is nowhere in this package to memoize one that dies with the scope, and
// memoizing outside it would retain a wrapper per scope forever, which is the
// unbounded growth R5 forbids.
//
// The wrapper is stateless, so what must agree is the shared carrier underneath.
// Two lookups that resolved to different scopes would split one operation's
// buffer in two.
func TestForeignScopeResolvesToOneScope(t *testing.T) {
	ctx := core.NewContext(context.Background(), new(core.Carrier))

	a, b := fromContext(ctx), fromContext(ctx)
	if a == nil || b == nil {
		t.Fatal("a foreign scope resolved to nil")
	}
	if a.Carrier != b.Carrier {
		t.Fatal("two lookups of one foreign scope reached different carriers: " +
			"records logged through each would land in different buffers")
	}
}

// TestOwnScopeKeepsPointerIdentity guards the fast path, which real callers do
// compare: TestScopeJoinsRatherThanNests asserts fromContext(inner) equals
// fromContext(outer), and that only holds while our own wrapper is memoized.
func TestOwnScopeKeepsPointerIdentity(t *testing.T) {
	ctx, done := Scope(context.Background())
	defer done()

	if a, b := fromContext(ctx), fromContext(ctx); a != b {
		t.Fatalf("fromContext returned different pointers for our own scope: %p vs %p", a, b)
	}
}

// fakeAdapter stands in for a second logging library's adapter, in the shape a
// real one (zap) would take: its own entry type, its own pool, and no knowledge
// of this package's slot, Handler or slog at all. It reaches the scope only
// through internal/core, which is exactly the path a real second adapter has.
type fakeAdapter struct {
	pool *core.ScopePool[core.Entry]
	mu   sync.Mutex
	out  []string
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{pool: core.NewScopePool[core.Entry](64, 0)}
}

// log buffers msg into whatever scope ctx carries, or emits it immediately when
// there is none. It mirrors what this package's Handle does, in miniature.
func (f *fakeAdapter) log(ctx context.Context, msg string) {
	c := core.FromContext(ctx)
	if c == nil {
		f.emit(msg)
		return
	}
	entry := core.Entry{Emit: func() { f.emit("replayed:" + msg) }}
	if c.Bind(f.pool).Append(entry) == core.ActionPassThrough {
		f.emit(msg)
	}
}

func (f *fakeAdapter) emit(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, s)
}

func (f *fakeAdapter) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.out...)
}

// The point of moving the carrier and the context key into internal/core: a
// second, independent adapter must land in the SAME scope as the slog Handler,
// reaching it purely through the core rather than through anything private to
// package dllog.
func TestSecondAdapterSharesTheScope(t *testing.T) {
	fake := newFakeAdapter()

	ctx, done := Scope(context.Background())
	defer done()

	// The scope was created by the public API; the fake finds it through core.
	if core.FromContext(ctx) == nil {
		t.Fatal("a scope opened by Scope() is invisible through internal/core; the key is not shared")
	}

	log := slog.New(New(&capture{}, WithLevel(slog.LevelInfo)))
	log.DebugContext(ctx, "from slog")
	fake.log(ctx, "from fake")

	// One ring, holding both adapters' entries.
	if got := fromContext(ctx).bound().scope.Len(); got != 2 {
		t.Fatalf("shared ring holds %d entries, want 2: the two adapters did not share one ring", got)
	}
}

// The requirement this phase exists for: a trip raised through EITHER adapter
// replays what BOTH buffered, in the order the entries were logged.
func TestTripReplaysBothAdaptersInOrder(t *testing.T) {
	c := &capture{}
	fake := newFakeAdapter()
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	defer done()

	// Interleave the two adapters so a per-adapter ring would betray itself: it
	// could only replay "a,c" then "b,d", never the true interleaving.
	log.DebugContext(ctx, "a")
	fake.log(ctx, "b")
	log.DebugContext(ctx, "c")
	fake.log(ctx, "d")

	Trip(ctx)

	// Each adapter emits into its own sink here, so this pins that a trip
	// raised through one adapter reaches the other's buffer at all. The global
	// interleaving is pinned by TestCrossAdapterReplayIsGloballyOrdered.
	if got, want := c.messages(), []string{"a", "c"}; !equal(got, want) {
		t.Fatalf("slog replay = %v, want %v", got, want)
	}
	if got, want := fake.messages(), []string{"replayed:b", "replayed:d"}; !equal(got, want) {
		t.Fatalf("fake replay = %v, want %v", got, want)
	}
}

// The ordering assertion above is per-sink, which cannot by itself prove the
// two adapters interleaved rather than being replayed adapter-by-adapter. This
// pins the true global order by giving both adapters ONE sink.
func TestCrossAdapterReplayIsGloballyOrdered(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); defer mu.Unlock(); order = append(order, s) }

	fake := &fakeAdapter{pool: core.NewScopePool[core.Entry](64, 0)}
	// Route the fake's emissions into the shared sink.
	fakeLog := func(ctx context.Context, msg string) {
		c := core.FromContext(ctx)
		entry := core.Entry{Emit: func() { record(msg) }}
		if c.Bind(fake.pool).Append(entry) == core.ActionPassThrough {
			record(msg)
		}
	}
	// And the slog handler's too.
	log := slog.New(New(&fnHandler{fn: func(r slog.Record) { record(r.Message) }}))

	ctx, done := Scope(context.Background())
	defer done()

	log.DebugContext(ctx, "1-slog")
	fakeLog(ctx, "2-fake")
	log.DebugContext(ctx, "3-slog")
	fakeLog(ctx, "4-fake")

	Trip(ctx)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"1-slog", "2-fake", "3-slog", "4-fake"}
	if !equal(order, want) {
		t.Fatalf("cross-adapter replay order = %v, want %v: entries from two adapters "+
			"must replay in the order they were logged, not grouped by adapter", order, want)
	}
}

// fnHandler is a minimal always-enabled slog.Handler that reports each record
// to fn, so a test can put slog output and another adapter's output into one
// ordered sink.
type fnHandler struct{ fn func(slog.Record) }

func (h *fnHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *fnHandler) Handle(_ context.Context, r slog.Record) error {
	h.fn(r)
	return nil
}
func (h *fnHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *fnHandler) WithGroup(string) slog.Handler      { return h }
