package core

import (
	"context"
	"sync"
	"testing"
)

// testEntry implements Entry for tests: emit and notify are optional thunks, so
// one type covers entries that record their emission, announce evictions, or do
// nothing at all.
type testEntry struct {
	emit   func()
	notify func(n int)
}

func (e *testEntry) Emit() {
	if e.emit != nil {
		e.emit()
	}
}

func (e *testEntry) Notify(n int) {
	if e.notify != nil {
		e.notify(n)
	}
}

// emitter builds an entry that appends its name to out on emission, so a test
// can assert the exact replay order the ring produced.
func emitter(mu *sync.Mutex, out *[]string, name string) *testEntry {
	return &testEntry{emit: func() {
		mu.Lock()
		defer mu.Unlock()
		*out = append(*out, name)
	}}
}

// foreignKey stands in for another package's context key. It is a named type
// rather than a bare string because a string key collides with every other
// package using the same value, which is the collision this test rules out.
type foreignKey struct{}

func TestFromContextOnBareAndForeignContexts(t *testing.T) {
	if FromContext(context.Background()) != nil {
		t.Fatal("a bare context reports a carrier")
	}
	foreign := context.WithValue(context.Background(), foreignKey{}, "v")
	if FromContext(foreign) != nil {
		t.Fatal("an unrelated context value was read as a carrier")
	}
	if HolderFromContext(context.Background()) != nil {
		t.Fatal("a bare context reports a holder")
	}
}

// A bare *Carrier satisfies Holder, so an adapter needing no wrapper of its own
// can store one directly and get it back.
func TestCarrierRoundTripsThroughContext(t *testing.T) {
	c := &Carrier{}
	ctx := NewContext(context.Background(), c)

	if got := FromContext(ctx); got != c {
		t.Fatalf("FromContext returned %p, want %p", got, c)
	}
	if got := c.Shared(); got != c {
		t.Fatalf("Shared() returned %p, want the receiver %p", got, c)
	}
}

// wrapper is a stand-in for an adapter's own context value, embedding the
// shared carrier exactly as the slog adapter's does.
type wrapper struct {
	*Carrier
	tag string
}

// An adapter may store its own wrapper and still be found by every other
// adapter, which is what makes one scope reachable from two adapters.
func TestHolderResolvesThroughAnAdapterWrapper(t *testing.T) {
	shared := &Carrier{}
	w := &wrapper{Carrier: shared, tag: "slog"}
	ctx := NewContext(context.Background(), w)

	if got := FromContext(ctx); got != shared {
		t.Fatalf("FromContext resolved to %p, want the shared carrier %p", got, shared)
	}
	got, ok := HolderFromContext(ctx).(*wrapper)
	if !ok || got != w {
		t.Fatalf("HolderFromContext = %#v, want the original wrapper %p", got, w)
	}
}

func TestBindIsIdempotentAndPooled(t *testing.T) {
	c := &Carrier{}
	pool := NewScopePool[Entry](8, 0)

	first := c.Bind(pool)
	if first == nil {
		t.Fatal("Bind returned nil")
	}
	if second := c.Bind(pool); second != first {
		t.Fatal("Bind created a second scope; a carrier must hold exactly one")
	}
	if got := c.Bound(); got != first {
		t.Fatalf("Bound() = %p, want the bound scope %p", got, first)
	}
	if got := first.Capacity(); got != 8 {
		t.Fatalf("capacity = %d, want 8: the binding pool must govern the ring", got)
	}
}

func TestBoundReportsNilBeforeAnyBind(t *testing.T) {
	c := &Carrier{}
	if c.Bound() != nil {
		t.Fatal("Bound() reported a scope before anything was buffered")
	}
	if c.Tripped() {
		t.Fatal("a fresh carrier reports tripped")
	}
}

// A trip arriving before any entry was buffered must still put the scope in the
// pass-through state once a ring is created.
func TestPreTrippedCarriesIntoTheRing(t *testing.T) {
	c := &Carrier{}
	s0, first := c.MarkTripped()
	if s0 != nil {
		t.Fatalf("MarkTripped on an unbound carrier returned %p, want nil", s0)
	}
	if !first {
		t.Fatal("the first trip on an unbound carrier did not report itself as the transition")
	}
	if _, again := c.MarkTripped(); again {
		t.Fatal("a second trip reported itself as the transition: only one call may claim it")
	}
	if !c.Tripped() {
		t.Fatal("a carrier that recorded an early trip does not report tripped")
	}

	s := c.Bind(NewScopePool[Entry](8, 0))
	if !s.Tripped() {
		t.Fatal("the ring created after an early trip is not tripped: the trip vanished")
	}
	if got := s.Append(&testEntry{}); got != ActionPassThrough {
		t.Fatalf("append after an early trip = %v, want %v", got, ActionPassThrough)
	}
}

// Once a ring exists, MarkTripped hands it back so the caller flushes it rather
// than recording a second trip.
func TestMarkTrippedReturnsTheBoundScope(t *testing.T) {
	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))

	got, first := c.MarkTripped()
	if got != s {
		t.Fatalf("MarkTripped = %p, want the bound scope %p", got, s)
	}
	if first {
		// With a scope in hand the transition is Flush's to report, so this
		// return must not also claim it: both claiming would exempt two records
		// from the post-trip budget.
		t.Fatal("MarkTripped claimed the transition while returning a scope")
	}
}

func TestCarrierCloseIsSafeAndIdempotent(t *testing.T) {
	unbound := &Carrier{}
	unbound.Close() // must not panic
	unbound.Close()

	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))
	c.Close()
	c.Close()

	if got := s.Append(&testEntry{}); got != ActionSuppressed {
		t.Fatalf("append after Close = %v, want %v", got, ActionSuppressed)
	}
	if _, _, tripped := s.Trip(); tripped {
		t.Fatal("Trip succeeded on a closed scope, want a no-op")
	}
}

// Flush replays in append order and reports whether the trip took.
func TestFlushEmitsInAppendOrder(t *testing.T) {
	var mu sync.Mutex
	var out []string

	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))
	for _, name := range []string{"a", "b", "c"} {
		s.Append(emitter(&mu, &out, name))
	}

	if !Flush(s) {
		t.Fatal("Flush reported no trip on a fresh scope")
	}
	if want := []string{"a", "b", "c"}; !equalStrings(out, want) {
		t.Fatalf("emitted %v, want %v", out, want)
	}
	if Flush(s) {
		t.Fatal("a second Flush tripped again; the trip is one-shot")
	}
}

// Each entry marks itself: the core calls Emit and interprets nothing, so two
// entries appended with different keys keep them through one flush.
func TestFlushLetsEachEntryMarkItself(t *testing.T) {
	var got []string
	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))
	for _, key := range []string{"slog_key", "zap_key"} {
		s.Append(&testEntry{emit: func() { got = append(got, key) }})
	}

	Flush(s)
	if want := []string{"slog_key", "zap_key"}; !equalStrings(got, want) {
		t.Fatalf("emitted %v, want %v: the flush overrode per-entry keys", got, want)
	}
}

// A nil Entry (the zero value of the interface, what a cleared ring slot
// holds) must not panic the flush.
func TestFlushSkipsNilEntries(t *testing.T) {
	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))
	s.Append(nil)

	if !Flush(s) {
		t.Fatal("Flush reported no trip")
	}
}

// The eviction notice is announced on the oldest survivor, before the batch.
func TestFlushAnnouncesEvictionOnTheOldestSurvivor(t *testing.T) {
	var mu sync.Mutex
	var out []string

	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](2, 0))

	s.Append(emitter(&mu, &out, "evicted"))
	oldest := emitter(&mu, &out, "oldest")
	oldest.notify = func(n int) {
		mu.Lock()
		defer mu.Unlock()
		out = append(out, "dropped:"+itoa(n))
	}
	s.Append(oldest)
	s.Append(emitter(&mu, &out, "newest"))

	Flush(s)

	want := []string{"dropped:1", "oldest", "newest"}
	if !equalStrings(out, want) {
		t.Fatalf("emitted %v, want %v: the notice must precede the batch it belongs to", out, want)
	}
}

// An adapter with no notion of an eviction notice leaves Notify nil; the flush
// must still deliver the batch.
func TestFlushWithoutNotifyStillReplays(t *testing.T) {
	var mu sync.Mutex
	var out []string

	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](2, 0))
	for _, name := range []string{"a", "b", "c"} {
		s.Append(emitter(&mu, &out, name))
	}

	Flush(s)
	if want := []string{"b", "c"}; !equalStrings(out, want) {
		t.Fatalf("emitted %v, want %v", out, want)
	}
}

func TestConcurrentBindAgreesOnOneScope(t *testing.T) {
	c := &Carrier{}
	pool := NewScopePool[Entry](16, 0)

	const n = 32
	var wg sync.WaitGroup
	seen := make([]*Scope[Entry], n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen[i] = c.Bind(pool)
		}()
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if seen[i] != seen[0] {
			t.Fatalf("goroutine %d bound a different scope; a carrier must hold exactly one", i)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itoa keeps the test free of an strconv import for a single small number.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// Close is permanent: a carrier released while it still had no ring must not
// build one for a late entry. Without this the late Bind would allocate a ring
// from the pool that nothing will ever Close, so it is never recycled.
func TestCloseIsPermanentOnUnboundCarrier(t *testing.T) {
	var c Carrier
	pool := NewScopePool[Entry](4, 0)

	c.Close()

	if s := c.Bind(pool); s != nil {
		t.Fatal("Bind() on a closed carrier returned a scope, want nil: a late entry must not resurrect a released scope")
	}
	if c.Bound() != nil {
		t.Fatal("Bound() reports a scope after Close(), want nil")
	}
}

// The same permanence must hold once a ring existed: Bind must stop handing the
// closed scope back, so an adapter sees "no scope" rather than a scope whose
// Append passes everything through.
func TestCloseIsPermanentOnBoundCarrier(t *testing.T) {
	var c Carrier
	pool := NewScopePool[Entry](4, 0)

	if c.Bind(pool) == nil {
		t.Fatal("Bind() before Close returned nil")
	}
	c.Close()

	if s := c.Bind(pool); s != nil {
		t.Fatalf("Bind() after Close() = %v, want nil", s)
	}
}

// A trip arriving after the scope was released must not be recorded: there is
// nothing left to flush, and remembering it would make a later Bind hand out a
// pre-tripped scope.
func TestMarkTrippedAfterCloseIsInert(t *testing.T) {
	var c Carrier
	pool := NewScopePool[Entry](4, 0)

	c.Close()

	s, first := c.MarkTripped()
	if s != nil {
		t.Fatalf("MarkTripped() after Close() = %v, want nil", s)
	}
	if first {
		t.Fatal("MarkTripped() claimed the transition on a closed carrier")
	}
	if c.Tripped() {
		t.Fatal("Tripped() reports true after a post-Close trip, want false: a released scope has no state to trip")
	}
	if c.Bind(pool) != nil {
		t.Fatal("Bind() after a post-Close trip returned a scope, want nil")
	}
}

// Closed is the predicate both adapters branch on, so it must report the two
// states distinctly regardless of whether a ring was ever bound.
func TestClosedReportsReleaseState(t *testing.T) {
	var c Carrier
	if c.Closed() {
		t.Fatal("a fresh carrier reports closed")
	}
	c.Close()
	if !c.Closed() {
		t.Fatal("Closed() is false after Close()")
	}
}
