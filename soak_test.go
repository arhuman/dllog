package dllog_test

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"testing"

	"github.com/arhuman/dllog"
)

// soakScopes is the number of scopes each worker drives through the full
// lifecycle. The product of this and soakWorkers is what makes a per-scope leak
// visible: a single retained ring is invisible, sixty thousand are not.
const (
	soakWorkers = 60
	soakScopes  = 1000
	soakRecords = 40
)

// heapGrowthLimit bounds the heap growth the churn may leave behind, in bytes.
//
// Chosen from measurement, not from taste. Observed growth over five runs of
// 60000 scopes on an M3 Pro: 99 KB, 42 KB, 43 KB, 25 KB, 35 KB. That is the
// noise of a pool reaching steady state, and it does not scale with the number
// of scopes processed.
//
// A real per-scope leak is orders of magnitude larger: retaining one 64-slot
// ring per scope holds tens of megabytes by the end of the run. 2 MiB therefore
// sits about 20x above the observed noise, so it will not flake, and far enough
// below a genuine leak that it always fires. The mutation check in
// TestSoakBoundCatchesALeak proves the second half.
//
// Tighten this if the noise band narrows. Never loosen it to green a red run:
// the whole value of the assertion is that it can fail.
const heapGrowthLimit = 2 << 20

// TestSoakScopeChurnKeepsHeapFlat drives thousands of concurrent scopes through
// create, append, trip or discard, and release, then asserts the heap did not
// grow with the number of scopes processed.
//
// This is the memory-safety claim (R5) as an executable check. The bounded ring
// and the explicit lifecycle are what make it hold: every scope releases its
// buffer at done, whether it tripped or not, so steady-state memory depends on
// the number of LIVE scopes, never on the number of scopes ever created.
func TestSoakScopeChurnKeepsHeapFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test: skipped under -short, runs in make fulltest")
	}

	// A downstream that discards keeps the test measuring dllog's retention
	// rather than an encoder's buffers.
	logger := slog.New(dllog.New(discardHandler{}, dllog.WithCapacity(64)))

	before := heapAlloc()

	var wg sync.WaitGroup
	for w := range soakWorkers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range soakScopes {
				churnOneScope(logger, worker, i)
			}
		}(w)
	}
	wg.Wait()

	after := heapAlloc()

	total := soakWorkers * soakScopes
	if growth := int64(after) - int64(before); growth > heapGrowthLimit {
		t.Fatalf("heap grew by %d bytes over %d scopes (limit %d): a scope is retaining its buffer past done()",
			growth, total, heapGrowthLimit)
	}
}

// churnOneScope runs one scope through its whole life. Every fourth scope trips,
// so both exit paths (replay, then discard) are exercised in proportion.
func churnOneScope(logger *slog.Logger, worker, i int) {
	ctx, done := dllog.Scope(context.Background())
	defer done()

	for r := range soakRecords {
		// A fresh string per record so nothing is shared between scopes: if the
		// ring retained an entry, its referent would be retained with it.
		logger.DebugContext(ctx, fmt.Sprintf("w%d s%d r%d", worker, i, r), "worker", worker)
	}

	if i%4 == 0 {
		logger.ErrorContext(ctx, "failed")
	}
}

// heapAlloc returns live heap bytes after a collection, so the sample reflects
// what is still reachable rather than what has not been swept yet.
func heapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// discardHandler is a downstream that accepts everything and keeps nothing.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

// TestSoakBoundCatchesALeak is the mutation check for heapGrowthLimit, kept as
// a test rather than run once by hand.
//
// A memory bound that cannot fail is worse than no bound: it reads as proof and
// provides none. So this drives the same churn while deliberately retaining
// every scope's records in the harness, and asserts the growth DOES exceed the
// limit. If this test ever passes its own assertion, heapGrowthLimit has drifted
// too loose to catch a leak and must be tightened.
//
// The leak lives here, in the test, never in the library.
func TestSoakBoundCatchesALeak(t *testing.T) {
	if testing.Short() {
		t.Skip("soak mutation check: skipped under -short, runs in make fulltest")
	}

	// A tenth of the full churn is enough to blow past the bound, and keeps the
	// test fast. If a tenth already exceeds it, the full run certainly would.
	const workers, scopes = 20, 300

	var leaked [][]string
	var mu sync.Mutex

	logger := slog.New(dllog.New(discardHandler{}, dllog.WithCapacity(64)))

	before := heapAlloc()

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range scopes {
				ctx, done := dllog.Scope(context.Background())
				held := make([]string, 0, soakRecords)
				for r := range soakRecords {
					msg := fmt.Sprintf("w%d s%d r%d padding-to-make-the-leak-visible", worker, i, r)
					logger.DebugContext(ctx, msg, "worker", worker)
					held = append(held, msg)
				}
				done()

				mu.Lock()
				leaked = append(leaked, held)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	growth := int64(heapAlloc()) - int64(before)

	// Keep the leak reachable across the sample, or the GC collects it and the
	// check proves nothing.
	if len(leaked) != workers*scopes {
		t.Fatalf("harness error: retained %d scopes, want %d", len(leaked), workers*scopes)
	}

	if growth <= heapGrowthLimit {
		t.Fatalf("a deliberate %d-scope leak grew the heap by only %d bytes, which is within heapGrowthLimit (%d): the soak bound is too loose to catch a real leak",
			len(leaked), growth, heapGrowthLimit)
	}
}
