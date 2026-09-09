package dllog_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arhuman/dllog"
)

// sink defeats dead-code elimination. Every benchmark body ends by feeding it
// something derived from the work it measured, so the compiler cannot decide
// the call had no observable effect and delete it.
//
// It is not enough on its own: slog.Logger.Debug takes a variadic ...any and
// calls through a handler interface, so the work is opaque to the optimizer
// anyway. The sink guards the cases where it would not be, namely Enabled,
// whose result is a plain bool that would otherwise be discarded.
var sink uint64

// countingHandler is the downstream under test. It counts records instead of
// formatting them, so a benchmark measures dllog and slog, not JSON encoding,
// and the count doubles as proof the records really reached the bottom.
//
// enabled is the level gate: the "downstream-disabled" baseline uses Info so a
// Debug call dies at the top of slog.Logger.Debug, exactly as it would in a
// production service configured at Info.
type countingHandler struct {
	enabled slog.Level
	n       uint64
}

func (h *countingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.enabled }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.n += uint64(r.NumAttrs()) + 1
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// jsonDiscard is the realistic downstream: a JSON handler writing to
// io.Discard. It is used where the cost of actually rendering matters (the
// replay and middleware benchmarks), so those numbers include the work a real
// deployment pays on flush.
func jsonDiscard(level slog.Level) slog.Handler {
	return slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: level})
}

// BenchmarkUnscopedDebug settles the first headline claim: an out-of-scope
// Debug call costs what a disabled slog call costs.
//
// Both sides must be present for the comparison to mean anything, so the two
// sub-benchmarks are identical except for the dllog Handler in the middle:
//
//   - slog-only: a logger over an Info-gated handler. Debug is refused by
//     slog.Logger before a Record is ever built. This is the baseline every Go
//     service already pays.
//   - dllog: the same Info-gated handler, wrapped by dllog, called with a
//     context carrying no scope. dllog's Enabled must delegate straight to the
//     downstream, so the extra cost is one nil map lookup on the context.
//
// The ratio between the two is the number the README quotes.
func BenchmarkUnscopedDebug(b *testing.B) {
	ctx := context.Background()

	b.Run("slog-only-disabled", func(b *testing.B) {
		down := &countingHandler{enabled: slog.LevelInfo}
		log := slog.New(down)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			log.DebugContext(ctx, "cache lookup", "key", "u-1")
		}
		b.StopTimer()
		sink += down.n
	})

	// slog-only-debug-open is the like-for-like baseline, and it is the one the
	// README quotes. dllog cannot run over an Info-gated downstream: it must see
	// Debug records to buffer them, so New rejects a downstream that would
	// swallow a replay. The honest question is therefore not "what does a
	// service that refuses Debug pay", it is "what does a service that accepts
	// Debug pay, with and without dllog in the way".
	b.Run("slog-only-debug-open", func(b *testing.B) {
		down := &countingHandler{enabled: slog.LevelDebug}
		log := slog.New(down)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			log.DebugContext(ctx, "cache lookup", "key", "u-1")
		}
		b.StopTimer()
		sink += down.n
	})

	b.Run("dllog-no-scope", func(b *testing.B) {
		// The downstream must be wide open for New to accept it, so the level
		// gate that makes this the same comparison lives on the wrapper.
		down := &countingHandler{enabled: slog.LevelDebug}
		log := slog.New(dllog.New(down, dllog.WithLevel(slog.LevelInfo)))
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			log.DebugContext(ctx, "cache lookup", "key", "u-1")
		}
		b.StopTimer()
		sink += down.n
	})
}

// BenchmarkScopedDebugAppend settles the second headline claim: an in-scope
// Debug append costs at most ~200ns and at most one allocation.
//
// This is the buffering hot path. The scope is opened once, outside the timed
// region, because the claim is about the per-record cost of an open scope, not
// about scope setup. The ring wraps around long before the loop ends, so the
// steady state measured is the drop-oldest one, which is what a real
// long-running request hits.
func BenchmarkScopedDebugAppend(b *testing.B) {
	down := &countingHandler{enabled: slog.LevelDebug}
	log := slog.New(dllog.New(down))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		log.DebugContext(ctx, "cache lookup", "key", "u-1")
	}
	b.StopTimer()
	sink += down.n
}

// BenchmarkScopeLifecycle measures one whole clean operation: open a scope,
// buffer a handful of records, discard them. This is the cost an endpoint that
// never fails pays for being instrumented, and the pooled ring should keep it
// off the allocator in steady state.
func BenchmarkScopeLifecycle(b *testing.B) {
	down := &countingHandler{enabled: slog.LevelDebug}
	log := slog.New(dllog.New(down))
	base := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, done := dllog.Scope(base)
		for i := range 8 {
			log.DebugContext(ctx, "step", "i", i)
		}
		done()
	}
	b.StopTimer()
	sink += down.n
}

// BenchmarkTripReplay measures the flush path: fill a scope with n records,
// then log an Error that trips it and replays the batch through a real JSON
// handler. Informational, not a headline claim: it is the cost of a failure,
// paid once per failing operation, and it scales with the buffer depth.
func BenchmarkTripReplay(b *testing.B) {
	for _, buffered := range []int{16, 256} {
		b.Run(bufferedName(buffered), func(b *testing.B) {
			log := slog.New(dllog.New(jsonDiscard(slog.LevelDebug)))
			base := context.Background()

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				ctx, done := dllog.Scope(base)
				for i := range buffered {
					log.DebugContext(ctx, "step", "i", i)
				}
				log.ErrorContext(ctx, "operation failed", "err", "boom")
				done()
			}
		})
	}
}

func bufferedName(n int) string {
	switch n {
	case 16:
		return "buffered-16"
	default:
		return "buffered-256"
	}
}

// BenchmarkMiddleware measures a full request through the HTTP middleware, on
// both branches: a 200 whose buffered records are discarded, and a 500 that
// trips and replays them. The gap between the two is what a service pays for an
// error under dllog.
func BenchmarkMiddleware(b *testing.B) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"status-200-clean", http.StatusOK},
		{"status-500-replay", http.StatusInternalServerError},
	} {
		b.Run(tc.name, func(b *testing.B) {
			log := slog.New(dllog.New(jsonDiscard(slog.LevelDebug)))
			h := dllog.Middleware(dllog.WithLogger(log))(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					for i := range 8 {
						log.DebugContext(r.Context(), "step", "i", i)
					}
					w.WriteHeader(tc.status)
				}))

			req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				sink += uint64(rec.Code)
			}
		})
	}
}

// BenchmarkEnabled isolates the three branches of the cost model documented on
// Handler.Enabled: no scope (delegate downstream), scope untripped (compare
// against the buffer floor), scope tripped (same comparison, different state).
//
// The result is assigned into the package sink, because a bare bool return from
// an inlinable method is exactly the case the optimizer would delete.
func BenchmarkEnabled(b *testing.B) {
	h := dllog.New(&countingHandler{enabled: slog.LevelDebug})

	noScope := context.Background()

	scoped, doneScoped := dllog.Scope(context.Background())
	defer doneScoped()

	tripped, doneTripped := dllog.Scope(context.Background())
	defer doneTripped()
	dllog.Trip(tripped)

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"no-scope", noScope},
		{"scoped-untripped", scoped},
		{"scoped-tripped", tripped},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var on uint64
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if h.Enabled(tc.ctx, slog.LevelDebug) {
					on++
				}
			}
			b.StopTimer()
			sink += on
		})
	}
}
