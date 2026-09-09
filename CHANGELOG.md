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

### Notes

- Memory is hard-bounded: a fixed count-based ring per scope, drop-oldest with a
  truncation marker recording how many records were dropped. Ring buffers are
  pooled, so scope lifecycle allocation is 112 B.
- Unscoped Debug costs what plain slog costs over the same Debug-open downstream
  (150.0 ns vs 149.2 ns, 0 allocations, measured on an M3 Pro).
- Buffered values render at flush time, not at log time. Logging a mutable
  reference and then mutating it shows the mutated state on replay. Log
  identifiers, or values you do not mutate.
- Passes `testing/slogtest.TestHandler`.
