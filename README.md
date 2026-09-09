# dllog

Dynamic level log: a `log/slog` handler that keeps below-level records in a
bounded per-operation buffer and replays them when that operation fails.

## The problem

Every Go service runs into the same choice. Configure `Debug` and drown in
volume, or configure `Info` and lose the context that explains an error when one
happens. The information you need to diagnose a failure existed moments before
it, at a level nobody could afford to leave on.

dllog resolves that by deciding per operation instead of globally. A request
that succeeds emits what your level says. A request that fails emits its `Debug`
records too, retroactively, with their original timestamps.

## Usage

The downstream handler must be constructed at `Debug`. dllog owns the effective
level, and a downstream that filters below the buffer floor would silently
discard exactly the records dllog exists to deliver, so `New` panics rather than
letting that reach production.

```go
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/arhuman/dllog"
)

func main() {
	// Downstream wide open at Debug. dllog decides what is emitted.
	downstream := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	// Effective level Info: Debug is buffered, not emitted, until something fails.
	logger := slog.New(dllog.New(downstream, dllog.WithLevel(slog.LevelInfo)))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Buffered: invisible on a successful request.
		slog.DebugContext(ctx, "loading cart", "user", 42)
		slog.DebugContext(ctx, "applying discount", "code", "SUMMER")

		// Any Error record replays everything buffered above it first.
		slog.ErrorContext(ctx, "payment declined", "provider", "stripe")

		w.WriteHeader(http.StatusInternalServerError)
	})

	// The middleware opens a scope per request, trips on 5xx and on panic.
	http.ListenAndServe(":8080", dllog.Middleware()(mux))
}
```

Outside an HTTP handler, manage the scope yourself. `Trip` covers the common Go
case where the error is returned rather than logged:

```go
func process(ctx context.Context, id string) error {
	ctx, done := dllog.Scope(ctx)
	defer done()

	slog.DebugContext(ctx, "fetching record", "id", id)

	if err := doWork(ctx); err != nil {
		dllog.Trip(ctx) // replay the buffer, then return the error as usual
		return err
	}
	return nil // buffer discarded, nothing emitted
}
```

`Scope` joins rather than nests: calling it on a context that already carries a
scope returns that same scope and a `done` that does nothing, so only the
creator releases the buffer.

## Measured cost

Apple M3 Pro, `go1.26.6`, `darwin/arm64`, from `go test -bench=. -benchmem`:

| Path | Time | Allocations |
|---|---|---|
| Plain slog, Debug-open downstream | 149.2 ns/op | 0 |
| dllog, no scope in context | 150.0 ns/op | 0 |
| dllog, buffering inside a scope | 191.9 ns/op | 0 |
| `Enabled`, no scope | 2.7 ns/op | 0 |
| Full request through the middleware, 200 | 2547 ns/op | 11 |
| Full request through the middleware, 500 | 6194 ns/op | 15 |

Two things are worth reading carefully.

**Out of scope, dllog is free.** 150.0 ns against a 149.2 ns baseline is within
run-to-run noise. The ~150 ns floor is `slog.Record` construction inside
`slog.Logger`, which every handler pays and none can avoid.

**The honest baseline is a Debug-open downstream.** A plain slog logger over an
`Info`-gated handler costs 4.1 ns, because `slog.Logger` refuses the call before
building a record. dllog cannot be compared to that: it must see `Debug` records
to buffer them. Quoting the 4 ns number as the baseline would make dllog look
36x slower, and it would be a dishonest comparison, so the table above uses the
like-for-like one.

Inside a scope, buffering costs about 42 ns over the baseline and allocates
nothing in steady state, since ring buffers are recycled through a pool.

## Memory

Bounded by construction: `capacity x record size x live scopes`.

The ring is a fixed, preallocated array per scope, default 256 slots. When it
fills it evicts the oldest record, keeping the ones nearest the failure, which
are the diagnostic ones. On replay, a synthetic record reports how many were
dropped, so truncation is never silent.

Scopes are released explicitly by `done`, so the count of live scopes is bounded
by your concurrency, not by uptime. `TestSoakScopeChurnKeepsHeapFlat` drives
60000 scopes and 2.4 million records and asserts the heap does not grow with
them.

## Options

| Option | Default | Effect |
|---|---|---|
| `WithLevel` | `Info` | Effective level when no scope is active |
| `WithBufferFloor` | `Debug` | Lowest level captured inside a scope |
| `WithTripLevel` | `Error` | Level at which a record triggers replay |
| `WithCapacity` | 256 | Ring slots per scope |
| `WithPostTripLimit` | 0 (unlimited) | Records allowed through after a trip |
| `WithReplayKey` | `"replay"` | Attribute marking a replayed record |

Middleware options:

| Option | Default | Effect |
|---|---|---|
| `WithTripOn` | `status >= 500` | Predicate deciding which statuses trip |
| `WithLogger` | `slog.Default()` | Logger used for the anchor error record |
| `WithAnchorPath` | route pattern, else raw path | How the anchor's `path` attribute is derived |

The middleware does not trip on 4xx or on context cancellation: 404s and client
disconnects would bury the signal. On a panic it trips, then re-panics with the
original value. It is a logging tool, never a recovery layer.

The anchor record's `path` is the matched route pattern (`GET /users/{id}`),
not the raw URL. Path segments routinely carry user ids, reset tokens and api
keys, and the anchor is emitted on the failure path, which is exactly when logs
get exported and retained; the pattern says which endpoint failed without saying
it about whom. A request that carries no pattern, either mounted directly or
routed by something other than `http.ServeMux`, falls back to the raw path.
Use `WithAnchorPath` to supply your own router's route, or to redact it
differently.

## Caveats

These follow from deferred logging and are worth knowing before you rely on it.

Reference values render at flush time, not at log time. If you log a map or a
pointer and mutate it before the replay, the replayed record shows the mutated
state. Log identifiers, or values you do not mutate.

Buffered attributes stay reachable until the scope flushes or ends, bounded by
ring capacity. Logging a large object at `Debug` keeps it alive for the duration
of the operation.

For a handler created with `WithGroup`, the replay marker may nest inside that
group rather than sitting at the top level. Reimplementing group logic to avoid
this would cost more than it is worth.

The package-level `Trip(ctx)` marks replayed records with the default key, since
it holds no handler and cannot see `WithReplayKey`. If you rename the key, trip
through the handler instead, which knows its own configuration:

```go
h := dllog.New(downstream, dllog.WithReplayKey("from_buffer"))
logger := slog.New(h)
// ...
h.Trip(ctx) // marks with "from_buffer"
```

Both forms are otherwise identical. Mixing them puts two different markers in
one stream, so pick one per handler.

## zap

The engine lives in `internal/core` and imports no logging library, so the same
buffering drives zap through `zapadapter`. A scope is shared: an `Error` logged
through zap replays what slog buffered on that context, and the reverse, in the
order the entries were logged.

```go
base := zapadapter.New(downstream) // downstream wide open at Debug, as with slog
logger := zap.New(base)            // process logger, no scope, plain level filter

ctx, done := zapadapter.Scope(r.Context())
defer done()

reqLog := zap.New(base.For(ctx))   // request logger, buffering
reqLog.Debug("loading cart")       // buffered
reqLog.Error("payment declined")   // replays the buffer, then writes this
```

### The binding cost

This is the one place the two adapters differ, and it is imposed by zap rather
than chosen. `zapcore.Core.Check` and `Write` receive no `context.Context`, so
unlike the slog handler this adapter cannot find the scope at log time. The
context is bound once, up front, with `Core.For`, and **the caller carries the
derived logger rather than the context**.

With slog you thread a context through call sites you already thread it through:

```go
slog.DebugContext(ctx, "loading cart") // scope found per record
```

With zap you must carry `reqLog` to every function that logs inside the
operation, or re-derive it from `base.For(ctx)` where you have the context. A
`zap.Logger` from `zap.New(base)` with no binding is not buffering; it is a
plain level filter, and its `Debug` calls cost what a disabled zap call costs.
Passing the wrong one is silent: nothing errors, the records simply are not
buffered.

`Core.Trip()` is a method for the same reason, taking no context: the binding
already happened. It marks replayed entries with the core's configured replay
key, which a package-level function could not see.

Everything else matches the slog handler: same options (`WithLevel`,
`WithCapacity`, `WithTripLevel`, `WithBufferFloor`, `WithPostTripLimit`,
`WithReplayKey`), same drop-oldest ring, same post-trip pass-through. `zap` is
imported only by `zapadapter`, so the root package stays dependency-free.

## Status

The `log/slog` handler, the HTTP middleware, and the zap adapter are implemented
and tested. Neither adapter is built on the other: both drive `internal/core`
directly, and either one's `Scope` is visible to the other.
