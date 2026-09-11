# Changelog

All notable changes to this project are documented here.
Format: [Keep a Changelog](https://keepachangelog.com).
This project adheres to [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- `dllog.New(downstream, opts...)`: a `log/slog` handler that buffers records
  below the configured level inside an operation scope and replays them if the
  operation fails. A service can run at Info and still get Debug context around
  errors. The downstream handler must be constructed wide open at Debug; `New`
  panics otherwise, rather than silently discarding the records it exists to
  deliver.
- `dllog.NewJSON(w, opts...)` and `dllog.NewText(w, opts...)` build the
  downstream handler as well, so setting up dllog is one call and the
  downstream cannot be gated shut. `NewJSONWith` and `NewTextWith` take the rest
  of the `slog.HandlerOptions` for `AddSource` or a `ReplaceAttr` hook; they
  panic if the caller also sets `Level`, which dllog owns. `zapadapter.NewJSON`
  and `zapadapter.NewConsole` are the zap equivalents. `New` is unchanged and
  remains the way to wrap a handler you already have.
- `dllog.Scope(ctx)` returns a derived context and a `done` func. Scopes join
  rather than nest: opening one on a context that already carries a scope
  returns that same scope and a no-op `done`. A scope that ends without
  tripping discards its buffer.
- `dllog.Trip(ctx)` and `(*Handler).Trip(ctx)` flush a scope explicitly, for
  failures that are returned rather than logged. Prefer the method when the
  handler was built with `WithReplayKey`, since it knows the configured key.
- `dllog.Middleware()`: an HTTP middleware opening a scope per request and
  tripping on a 5xx response or a panic. Panics are re-panicked, never
  swallowed. Does not trip on 4xx or on context cancellation.
- Six handler options: `WithLevel`, `WithBufferFloor`, `WithTripLevel`,
  `WithCapacity`, `WithPostTripLimit`, `WithReplayKey`. Two middleware options:
  `WithLogger`, `WithTripOn`.
- `zapadapter`: a `zapcore.Core` driven by the same engine. A scope is shared
  across adapters, so an Error logged through zap replays what slog buffered on
  that context, and the reverse, in log order. Because `zapcore.Core` receives
  no context, the scope is bound once via `Core.For(ctx)` and the caller carries
  the derived logger. `zap` is imported only by that package.

### Security

- The middleware's anchor record now logs the matched route pattern
  (`GET /users/{id}`) instead of the raw URL path. Path segments routinely carry
  user ids, reset tokens and api keys, and the anchor is emitted on the failure
  path where logs are most likely to be exported and retained. Requests with no
  pattern fall back to the raw path; `WithAnchorPath` overrides the derivation.

### Notes

- A scope cannot grow without limit. Each one holds a fixed number of records,
  set by `WithCapacity`. Once it is full, the oldest record is discarded to make
  room for the newest, and the replay says how many were lost rather than
  hiding the gap. Buffers are reused between scopes, which costs 112 B and one
  allocation per scope instead of 2160 B and two.
- When no scope is open, dllog adds nothing measurable. Logging a Debug record
  through dllog took 151.6 ns, against 150.7 ns for plain slog writing to the
  same Debug-enabled handler. Neither allocates, and the difference is
  run-to-run noise. Measured on an Apple M3 Pro.
- dllog keeps your log arguments as you passed them and only formats them if it
  replays them later. So if you log a pointer, a slice or a map and then change
  what it points at, the replayed line shows the changed value, not the value at
  the moment you logged it. Log ids and strings, or values you will not touch
  again.
- Passes `testing/slogtest.TestHandler`.
