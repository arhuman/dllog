# The zap adapter

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

## The binding cost

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
already happened. Each replayed entry carries the replay key of the adapter that
logged it, so a trip raised through zap marks slog-buffered records with the
slog handler's key and vice versa.

Everything else matches the slog handler: same options (`WithLevel`,
`WithCapacity`, `WithTripLevel`, `WithBufferFloor`, `WithPostTripLimit`,
`WithReplayKey`), same drop-oldest ring, same post-trip pass-through. `zap` is
imported only by `zapadapter`, so the root package stays dependency-free.
