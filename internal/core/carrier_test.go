package core

import (
	"context"
	"sync"
	"testing"
)

// emitter builds an Entry that appends its name to out on emission, so a test
// can assert the exact replay order the ring produced.
func emitter(mu *sync.Mutex, out *[]string, name string) Entry {
	return Entry{Emit: func(string) {
		mu.Lock()
		defer mu.Unlock()
		*out = append(*out, name)
	}}
}

func TestFromContextOnBareAndForeignContexts(t *testing.T) {
	if FromContext(context.Background()) != nil {
		t.Fatal("a bare context reports a carrier")
	}
	//nolint:staticcheck // a deliberately foreign key, which is the point.
	foreign := context.WithValue(context.Background(), "k", "v")
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
	if s := c.MarkTripped(); s != nil {
		t.Fatalf("MarkTripped on an unbound carrier returned %p, want nil", s)
	}
	if !c.Tripped() {
		t.Fatal("a carrier that recorded an early trip does not report tripped")
	}

	s := c.Bind(NewScopePool[Entry](8, 0))
	if !s.Tripped() {
		t.Fatal("the ring created after an early trip is not tripped: the trip vanished")
	}
	if got := s.Append(Entry{}); got != ActionPassThrough {
		t.Fatalf("append after an early trip = %v, want %v", got, ActionPassThrough)
	}
}

// Once a ring exists, MarkTripped hands it back so the caller flushes it rather
// than recording a second trip.
func TestMarkTrippedReturnsTheBoundScope(t *testing.T) {
	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))

	if got := c.MarkTripped(); got != s {
		t.Fatalf("MarkTripped = %p, want the bound scope %p", got, s)
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

	if got := s.Append(Entry{}); got != ActionPassThrough {
		t.Fatalf("append after Close = %v, want %v", got, ActionPassThrough)
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

	if !Flush(s, "replay") {
		t.Fatal("Flush reported no trip on a fresh scope")
	}
	if want := []string{"a", "b", "c"}; !equalStrings(out, want) {
		t.Fatalf("emitted %v, want %v", out, want)
	}
	if Flush(s, "replay") {
		t.Fatal("a second Flush tripped again; the trip is one-shot")
	}
}

// The key reaches Emit untouched: the core forwards it without interpreting it.
func TestFlushForwardsTheKey(t *testing.T) {
	var got string
	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))
	s.Append(Entry{Emit: func(k string) { got = k }})

	Flush(s, "custom-key")
	if got != "custom-key" {
		t.Fatalf("Emit saw key %q, want %q", got, "custom-key")
	}
}

// A nil Emit (the zero Entry) must not panic the flush.
func TestFlushSkipsEntriesWithNoEmit(t *testing.T) {
	c := &Carrier{}
	s := c.Bind(NewScopePool[Entry](8, 0))
	s.Append(Entry{})

	if !Flush(s, "replay") {
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
	oldest.Notify = func(n int) {
		mu.Lock()
		defer mu.Unlock()
		out = append(out, "dropped:"+itoa(n))
	}
	s.Append(oldest)
	s.Append(emitter(&mu, &out, "newest"))

	Flush(s, "replay")

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

	Flush(s, "replay")
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
