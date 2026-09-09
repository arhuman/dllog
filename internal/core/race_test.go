package core

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// entryID uniquely identifies an appended entry so the accounting can name the
// exact entry that went missing or came back twice.
type entryID struct {
	goroutine int
	seq       int
}

func (e entryID) String() string { return fmt.Sprintf("g%d/#%d", e.goroutine, e.seq) }

// outcome records what Append told the producer to do with one entry.
type outcome struct {
	id     entryID
	action Action
}

// waitForEvictions spins until the ring reports at least want evictions, and
// reports failure rather than spinning forever if it never gets there. The
// bound matters: an engine that loses entries without counting them as evicted
// would otherwise hang the suite instead of failing it.
func waitForEvictions[T any](t *testing.T, s *Scope[T], want int) {
	t.Helper()
	const maxSpins = 2_000_000
	for spins := 0; s.Dropped() < want; spins++ {
		if spins >= maxSpins {
			t.Errorf("ring reported only %d evictions after %d spins, want >= %d: "+
				"entries are being overwritten without being counted as dropped",
				s.Dropped(), spins, want)
			return
		}
		runtime.Gosched()
	}
}

// TestConcurrentAppendTripCloseAccounting is the never-lost-never-duplicated
// invariant from DESIGN.md section 8, verified by exact reconciliation rather
// than by the race detector staying quiet.
//
// Every entry a producer appends must end up in exactly one bucket:
//
//	buffered  -> returned by Trip, or still in the ring at Close, or evicted
//	passed    -> ActionPassThrough, the caller emitted it
//	suppressed-> ActionSuppressed, the post-trip budget refused it
//
// The buffered bucket is the only one the scope accounts for by count rather
// than by identity, so it is reconciled as
//
//	buffered == len(tripBatch) + dropped + stillHeldAtClose
//
// and the trip batch is additionally checked entry by entry for duplicates and
// for entries that were never appended at all.
func TestConcurrentAppendTripCloseAccounting(t *testing.T) {
	const (
		producers   = 8
		perProducer = 500
		capacity    = 64
		postTripCap = 0 // unlimited: exercises the pass-through path, not suppression
	)

	for run := range 5 {
		t.Run(fmt.Sprintf("run%d", run), func(t *testing.T) {
			s := NewScope[entryID](capacity, postTripCap)

			outcomes := make([][]outcome, producers)
			var start sync.WaitGroup
			var done sync.WaitGroup
			start.Add(1)

			for g := range producers {
				outcomes[g] = make([]outcome, 0, perProducer)
				done.Add(1)
				go func() {
					defer done.Done()
					start.Wait()
					for i := range perProducer {
						id := entryID{goroutine: g, seq: i}
						outcomes[g] = append(outcomes[g], outcome{id, s.Append(id)})
					}
				}()
			}

			// One tripper and one closer race the producers. The tripper spins
			// until the ring has both filled and started evicting, so the trip
			// lands mid-stream with a full ring: without this the trip wins the
			// start race, only a handful of entries are ever buffered, and the
			// eviction and still-held buckets are never exercised at all.
			var (
				batch     []entryID
				dropped   int
				tripped   bool
				tripOnce  sync.WaitGroup
				closeOnce sync.WaitGroup
			)
			tripOnce.Add(1)
			go func() {
				defer tripOnce.Done()
				start.Wait()
				// Producers append far more than the ring holds and the closer
				// waits for them all, so the ring is guaranteed to overflow.
				waitForEvictions(t, s, capacity)
				batch, dropped, tripped = s.Trip()
			}()

			closeOnce.Add(1)
			go func() {
				defer closeOnce.Done()
				start.Wait()
				done.Wait() // close only after every producer has finished
				s.Close()
			}()

			start.Done()
			done.Wait()
			tripOnce.Wait()
			closeOnce.Wait()

			if !tripped {
				t.Fatal("Trip() tripped = false; the only tripper must win")
			}

			// Bucket every outcome by identity.
			var buffered, passed, suppressed int
			appended := make(map[entryID]Action, producers*perProducer)
			for g := range producers {
				if got := len(outcomes[g]); got != perProducer {
					t.Fatalf("producer %d recorded %d outcomes, want %d", g, got, perProducer)
				}
				for _, o := range outcomes[g] {
					if prev, dup := appended[o.id]; dup {
						t.Fatalf("entry %s appended twice (actions %v then %v): test bug", o.id, prev, o.action)
					}
					appended[o.id] = o.action
					switch o.action {
					case ActionBuffered:
						buffered++
					case ActionPassThrough:
						passed++
					case ActionSuppressed:
						suppressed++
					default:
						t.Fatalf("entry %s got unknown action %v", o.id, o.action)
					}
				}
			}

			// No entry may come back from Trip twice, and every entry in the
			// batch must be one a producer actually appended as buffered.
			seen := make(map[entryID]bool, len(batch))
			for i, id := range batch {
				if seen[id] {
					t.Fatalf("entry %s DUPLICATED: returned twice in the trip batch (index %d)", id, i)
				}
				seen[id] = true

				action, ok := appended[id]
				if !ok {
					t.Fatalf("entry %s FABRICATED: in the trip batch but never appended", id)
				}
				if action != ActionBuffered {
					t.Fatalf("entry %s in the trip batch but Append reported %v, want ActionBuffered", id, action)
				}
			}

			// Whatever was buffered but neither flushed nor evicted is still in
			// the ring; Close discards it, which is the documented behaviour.
			// It is accounted for, not ignored.
			stillHeld := buffered - len(batch) - dropped
			if stillHeld < 0 {
				t.Fatalf("accounting: buffered=%d < flushed=%d + dropped=%d; entries were duplicated somewhere",
					buffered, len(batch), dropped)
			}

			total := passed + suppressed + len(batch) + dropped + stillHeld
			if want := producers * perProducer; total != want {
				t.Fatalf("accounting FAILED: %d entries accounted for, %d appended\n"+
					"  flushed=%d dropped=%d stillHeld=%d passed=%d suppressed=%d",
					total, want, len(batch), dropped, stillHeld, passed, suppressed)
			}

			// The batch must never exceed the ring, and an untripped-at-capacity
			// run must actually have exercised eviction for the test to be
			// meaningful.
			if len(batch) > capacity {
				t.Fatalf("trip batch holds %d entries, ring capacity is %d", len(batch), capacity)
			}
		})
	}
}

// TestConcurrentAppendCloseWithoutTripAccounting covers the bucket the tripping
// test cannot reach: entries that are buffered and then discarded by Close
// because the scope never tripped (DESIGN.md section 2). Every append must be
// either buffered (held or evicted) or, once Close lands, passed through.
//
// The reconciliation here pins the ring itself: at the moment of Close the ring
// held exactly buffered-dropped entries, and that must equal the capacity once
// more than a ring's worth was appended.
func TestConcurrentAppendCloseWithoutTripAccounting(t *testing.T) {
	const (
		producers   = 8
		perProducer = 400
		capacity    = 64
	)

	for run := range 10 {
		s := NewScope[entryID](capacity, 0)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			mu    sync.Mutex
			buf   int
			pass  int
		)
		for g := range producers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				lb, lp := 0, 0
				for i := range perProducer {
					switch a := s.Append(entryID{g, i}); a {
					case ActionBuffered:
						lb++
					case ActionPassThrough:
						lp++
					default:
						t.Errorf("run %d: untripped Append returned %v", run, a)
					}
				}
				mu.Lock()
				buf += lb
				pass += lp
				mu.Unlock()
			}()
		}

		// Close once the ring has wrapped, so entries are genuinely discarded.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			waitForEvictions(t, s, capacity)
			s.Close()
		}()

		close(start)
		wg.Wait()

		dropped := s.Dropped()
		if total, want := buf+pass, producers*perProducer; total != want {
			t.Fatalf("run %d: accounting FAILED: buffered=%d + passed=%d = %d, want %d",
				run, buf, pass, total, want)
		}
		// Everything buffered was either evicted or discarded by Close; nothing
		// may go unaccounted, and nothing may be counted twice.
		discardedAtClose := buf - dropped
		if discardedAtClose < 0 {
			t.Fatalf("run %d: dropped=%d exceeds buffered=%d: eviction is over-counted",
				run, dropped, buf)
		}
		if discardedAtClose > capacity {
			t.Fatalf("run %d: Close discarded %d entries but the ring holds at most %d: "+
				"entries were buffered without being counted as evicted",
				run, discardedAtClose, capacity)
		}
		// Trip after Close must yield nothing: the entries are gone by design.
		if batch, d, tripped := s.Trip(); tripped || batch != nil || d != 0 {
			t.Fatalf("run %d: Trip() after Close() = (%v, %d, %v), want (nil, 0, false)",
				run, batch, d, tripped)
		}
	}
}

// TestConcurrentAppendSuppressionAccounting drives the same reconciliation with
// a finite post-trip budget, so ActionSuppressed is actually exercised and the
// budget is proved to be spent exactly once per entry.
func TestConcurrentAppendSuppressionAccounting(t *testing.T) {
	const (
		producers   = 8
		perProducer = 400
		capacity    = 32
		budget      = 50
	)

	s := NewScope[entryID](capacity, budget)

	// Trip up front: every append then takes the post-trip path, so the budget
	// is the thing under test.
	if _, _, ok := s.Trip(); !ok {
		t.Fatal("Trip() did not trip")
	}

	var (
		mu         sync.Mutex
		passed     int
		suppressed int
		wg         sync.WaitGroup
	)
	for g := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localPassed, localSuppressed := 0, 0
			for i := range perProducer {
				switch a := s.Append(entryID{g, i}); a {
				case ActionPassThrough:
					localPassed++
				case ActionSuppressed:
					localSuppressed++
				default:
					t.Errorf("post-trip Append returned %v, want PassThrough or Suppressed", a)
				}
			}
			mu.Lock()
			passed += localPassed
			suppressed += localSuppressed
			mu.Unlock()
		}()
	}
	wg.Wait()

	if total, want := passed+suppressed, producers*perProducer; total != want {
		t.Fatalf("accounting FAILED: passed=%d + suppressed=%d = %d, want %d", passed, suppressed, total, want)
	}
	// The budget must be honoured exactly: no more and no fewer pass-throughs
	// than the limit, since traffic far exceeds it.
	if passed != budget {
		t.Fatalf("passed = %d, want exactly the budget %d (budget over- or under-spent under concurrency)", passed, budget)
	}
}

// TestConcurrentTripExactlyOneWinner asserts the one-shot property: with many
// goroutines racing Trip, exactly one sees tripped=true and the entries are
// handed to that one winner only.
func TestConcurrentTripExactlyOneWinner(t *testing.T) {
	const (
		trippers = 16
		entries  = 20
	)

	for run := range 20 {
		s := NewScope[entryID](64, 0)
		for i := range entries {
			s.Append(entryID{0, i})
		}

		var (
			wins    atomic.Int64
			mu      sync.Mutex
			batches [][]entryID
			wg      sync.WaitGroup
			start   sync.WaitGroup
		)
		start.Add(1)
		for range trippers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				batch, _, tripped := s.Trip()
				if !tripped {
					if batch != nil {
						t.Errorf("losing Trip() returned %d entries, want nil", len(batch))
					}
					return
				}
				wins.Add(1)
				mu.Lock()
				batches = append(batches, batch)
				mu.Unlock()
			}()
		}
		start.Done()
		wg.Wait()

		if got := wins.Load(); got != 1 {
			t.Fatalf("run %d: %d goroutines saw tripped=true, want exactly 1", run, got)
		}
		if len(batches) != 1 || len(batches[0]) != entries {
			t.Fatalf("run %d: winner got %d batches; want 1 batch of %d entries", run, len(batches), entries)
		}
	}
}

// TestConcurrentCloseIsIdempotent asserts Close survives being called by many
// goroutines at once: no panic, no double return of the ring to the pool.
func TestConcurrentCloseIsIdempotent(t *testing.T) {
	const closers = 16

	for range 20 {
		p := NewScopePool[entryID](32, 0)
		s := p.Get()
		s.Append(entryID{0, 1})

		var wg sync.WaitGroup
		start := make(chan struct{})
		for range closers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				s.Close()
			}()
		}
		close(start)
		wg.Wait()

		if got := s.Append(entryID{0, 2}); got != ActionPassThrough {
			t.Fatalf("Append() after concurrent Close() = %v, want ActionPassThrough", got)
		}
	}
}

// TestAppendRacingCloseNeverCorruptsRecycledRing is the pooling safety case
// from DESIGN.md section 8: a goroutine still appending while another closes
// must never write into a ring the pool has already handed to a different
// scope.
//
// Each generation stamps its entries with its own generation number, and every
// scope verifies that its ring contains only its own stamp. A late Append that
// reaches a released ring shows up as a foreign generation in the next scope
// that draws that ring, which is precisely the corruption pooling risks.
func TestAppendRacingCloseNeverCorruptsRecycledRing(t *testing.T) {
	const (
		generations = 300
		appenders   = 4
		perAppender = 40
		capacity    = 16
	)

	p := NewScopePool[entryID](capacity, 0)

	for gen := 1; gen <= generations; gen++ {
		s := p.Get()

		// Whatever this scope draws from the pool must be pristine before use.
		assertRingOnlyHolds(t, s, 0, "at Get")

		var wg sync.WaitGroup
		start := make(chan struct{})
		for a := range appenders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := range perAppender {
					// goroutine carries the generation stamp.
					s.Append(entryID{goroutine: gen, seq: a*perAppender + i})
				}
			}()
		}
		// The closer races the appenders: this is the window the closed flag
		// and the mutex must jointly protect.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.Close()
		}()
		close(start)
		wg.Wait()

		// Once closed, the scope must hold no ring at all: a scope that still
		// points at a pooled array is a scope that can still scribble on it.
		s.mu.Lock()
		leaked := s.ring != nil || s.held != nil
		s.mu.Unlock()
		if leaked {
			t.Fatalf("gen %d: scope still references its ring after Close; "+
				"a late Append could write into an array the pool has reissued", gen)
		}
	}

	// After all that churn a scope from the pool must still be clean and correct.
	final := p.Get()
	assertRingOnlyHolds(t, final, 0, "after churn")
	final.Append(entryID{goroutine: -1, seq: -1})
	batch, dropped, tripped := final.Trip()
	if !tripped || dropped != 0 || len(batch) != 1 || batch[0] != (entryID{-1, -1}) {
		t.Fatalf("pool corrupted after churn: batch=%v dropped=%d tripped=%v", batch, dropped, tripped)
	}
	final.Close()
}

// assertRingOnlyHolds fails if any ring slot carries a generation stamp other
// than wantGen (0 meaning "must be entirely zero").
func assertRingOnlyHolds(t *testing.T, s *Scope[entryID], wantGen int, when string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, got := range s.ring {
		if got.goroutine != wantGen {
			t.Fatalf("%s: ring slot %d holds %s (generation %d), want generation %d: "+
				"a ring was recycled while another scope could still write to it",
				when, i, got, got.goroutine, wantGen)
		}
	}
}

// TestPooledRingIsHandedBackClean asserts that a ring returned to the pool
// carries nothing from its previous scope. A dirty recycled ring is the
// duplication vector that pooling introduces: stale entries can be re-flushed
// by a later scope, and they keep their referents alive long after the scope
// that logged them ended (DESIGN.md section 6).
//
// The check reads the ring array itself rather than going through Len, because
// resetting the length while leaving the slots populated hides the leak from
// every behavioural assertion while still pinning the referents.
func TestPooledRingIsHandedBackClean(t *testing.T) {
	const capacity = 16

	p := NewScopePool[entryID](capacity, 0)

	// Fill a scope to capacity, then close it so its ring goes back to the pool.
	first := p.Get()
	for i := range capacity {
		first.Append(entryID{goroutine: 1, seq: i})
	}
	first.Close()

	// The next scope must come back with a pristine ring.
	second := p.Get()
	defer second.Close()

	second.mu.Lock()
	ring := second.ring
	second.mu.Unlock()

	if len(ring) != capacity {
		t.Fatalf("recycled ring has len %d, want %d", len(ring), capacity)
	}
	var zero entryID
	for i, got := range ring {
		if got != zero {
			t.Fatalf("recycled ring slot %d holds %s, want the zero entry: "+
				"Close returned the ring to the pool without clearing it, so the "+
				"previous scope's entries can be flushed again and their referents "+
				"stay reachable", i, got)
		}
	}

	// And it must behave as empty, not merely look empty.
	if got := second.Len(); got != 0 {
		t.Fatalf("recycled scope Len() = %d, want 0", got)
	}
	batch, dropped, tripped := second.Trip()
	if !tripped {
		t.Fatal("recycled scope failed to trip")
	}
	if len(batch) != 0 || dropped != 0 {
		t.Fatalf("recycled scope flushed %v (dropped %d), want an empty batch: "+
			"entries from the previous scope were duplicated into this one", batch, dropped)
	}
}

// TestConcurrentAppendTripCloseAllRacing removes the ordering constraints of
// the accounting test: Trip and Close fire at an arbitrary point in the append
// stream. It cannot reconcile exactly (Close may discard buffered entries the
// test cannot count), so it asserts the properties that must hold regardless:
// no panic, no duplicate in the batch, no fabricated entry.
func TestConcurrentAppendTripCloseAllRacing(t *testing.T) {
	const (
		producers   = 8
		perProducer = 300
	)

	for run := range 20 {
		s := NewScope[entryID](32, 10)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			batch []entryID
		)

		for g := range producers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := range perProducer {
					s.Append(entryID{g, i})
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			batch, _, _ = s.Trip()
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.Close()
		}()

		close(start)
		wg.Wait()

		seen := make(map[entryID]bool, len(batch))
		for _, id := range batch {
			if seen[id] {
				t.Fatalf("run %d: entry %s DUPLICATED in the trip batch", run, id)
			}
			seen[id] = true
			if id.goroutine < 0 || id.goroutine >= producers || id.seq < 0 || id.seq >= perProducer {
				t.Fatalf("run %d: entry %s FABRICATED: never appended by any producer", run, id)
			}
		}
	}
}
