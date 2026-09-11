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

`NewJSON` builds the handler and its output for you. There is nothing else to
wire: the service logs at `Info`, and a failed operation also gets the `Debug`
records that led to it.

```go
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/arhuman/dllog"
)

func main() {
	logger := slog.New(dllog.NewJSON(os.Stderr))
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

### Choosing the output

`NewText` is `NewJSON` with slog's text encoding. For `AddSource` or a
`ReplaceAttr` hook, `NewJSONWith` and `NewTextWith` take the rest of the
`slog.HandlerOptions`:

```go
logger := slog.New(dllog.NewJSONWith(os.Stderr, slog.HandlerOptions{
	AddSource:   true,
	ReplaceAttr: redact,
	// Level stays nil: dllog owns it.
}, dllog.WithLevel(slog.LevelWarn)))
```

`Level` must be left nil. dllog sets it from the buffer floor, and a
caller-supplied level is refused at construction rather than silently
overridden.

### Bringing your own handler

`New` wraps a handler you already have. It costs one rule: that handler must be
constructed wide open at `Debug`, and you set the level you actually want on
dllog instead.

```go
downstream := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
	Level: slog.LevelDebug, // wide open: dllog decides what is emitted
})
logger := slog.New(dllog.New(downstream, dllog.WithLevel(slog.LevelInfo)))
```

Both lines are load-bearing. dllog owns the effective level, so a downstream
that filters below the buffer floor would discard exactly the records dllog
exists to deliver. `New` panics when it detects that rather than letting it
reach production, but the check can only run at construction: a downstream
holding a `slog.Leveler` that is lowered later escapes it. The constructors
above have no such gap, since dllog builds the handler and keeps the only
reference to it.

## Measured cost

One run of `go test -bench=. -benchmem` on an Apple M3 Pro, `go1.26.6`,
`darwin/arm64`. Treat these as magnitudes, not exact figures: the sub-microsecond
rows move by a few percent between runs.

| Path | Time | Bytes | Allocations |
|---|---|---|---|
| Plain slog, Debug-open downstream | 150.7 ns/op | 0 | 0 |
| dllog, no scope in context | 151.6 ns/op | 0 | 0 |
| dllog, buffering inside a scope | 239.1 ns/op | 352 | 3 |
| `Enabled`, no scope | 6.0 ns/op | 0 | 0 |
| Full request through the middleware, 200 | 2507 ns/op | 3635 | 37 |
| Full request through the middleware, 500 | 5425 ns/op | 3909 | 41 |

Three things are worth reading carefully.

**Out of scope, dllog is free.** 151.6 ns against a 150.7 ns baseline is within
run-to-run noise: repeat the benchmark and the order of those two flips. The
~150 ns floor is the cost of building a `slog.Record` inside `slog.Logger`,
which every handler pays and none can avoid.

**Inside a scope, you pay to keep the record.** Buffering costs about 90 ns and
three allocations more than passing the record straight through, because the
record and its attributes have to be copied and held rather than written and
forgotten. That is the price of having the Debug context available if the
operation later fails.

**The honest baseline is a Debug-open downstream.** A plain slog logger over an
`Info`-gated handler costs 4.1 ns, because `slog.Logger` sees the level is
disabled and drops the call before building a record at all. dllog cannot be
compared against that: it has to receive `Debug` records in order to buffer
them. Quoting the 4 ns figure as the baseline would make dllog look 36x slower
while comparing two different amounts of work, so the table uses the
like-for-like number instead.

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

All four follow from the same thing: a buffered record is written now and
formatted later. Worth reading before you rely on it.

**A mutated value replays as it is now, not as it was.** dllog keeps your log
arguments as you passed them and only formats them if it replays them. So if you
log a pointer, a slice or a map and then change what it points at, the replayed
record shows the changed value, not the value at the moment you logged it. Log
ids and strings, or values you will not touch again.

**Buffering keeps things alive.** A record sitting in a scope's buffer still
holds references to whatever you logged, so the garbage collector cannot free
it, and that lasts until the scope replays or ends. Logging a large object at
`Debug` keeps that object in memory for the whole operation. The number of
records is capped by `WithCapacity`, but their size is not: a bounded count of
large objects is still large.

**Under `WithGroup`, the replay marker moves.** A handler built with
`WithGroup("http")` nests everything it logs under that group name. The marker
dllog adds on replay is written at replay time, so it lands inside the group
alongside your attributes rather than at the top level of the record. Only the
marker's position changes, not whether it is there. Matching slog's grouping
rules well enough to hoist it out would cost more than the tidier output is
worth.

**The package-level `Trip(ctx)` always marks with `"replay"`.** It is a plain
function with no handler to consult, so it cannot see a key you set with
`WithReplayKey`. Trip through the handler instead, which knows its own
configuration:

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
// Builds its own downstream core, as NewJSON does on the slog side.
base := zapadapter.NewJSON(os.Stderr, zap.NewProductionEncoderConfig())
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
