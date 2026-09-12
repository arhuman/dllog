# Changelog

All notable changes to this project are documented here.
Format: [Keep a Changelog](https://keepachangelog.com).
This project adheres to [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- `dllog.New(downstream, opts...)`: a `log/slog` handler that buffers below-level
  records per operation and replays them when the operation fails, so a service
  runs at Info and still gets Debug context around errors.
- `dllog.NewJSON(w, opts...)` and `dllog.NewText(w, opts...)` build the
  downstream handler too, so setup is one call. `NewJSONWith` and `NewTextWith`
  add `slog.HandlerOptions` control; `zapadapter.NewJSON` and
  `zapadapter.NewConsole` are the zap equivalents.
- `dllog.Scope(ctx)`: a per-operation buffering scope, released by the returned
  `done`. Scopes join rather than nest, and a clean end discards the buffer.
- `dllog.Trip(ctx)` and `(*Handler).Trip(ctx)`: explicit flush for failures that
  are returned rather than logged.
- `dllog.Middleware()`: a scope per HTTP request, tripped by a 5xx response or a
  panic.
- Six handler options: `WithLevel`, `WithBufferFloor`, `WithTripLevel`,
  `WithCapacity`, `WithPostTripLimit`, `WithReplayKey`. Two middleware options:
  `WithLogger`, `WithTripOn`.
- `zapadapter`: a `zapcore.Core` on the same engine. A scope opened by either
  adapter is shared, so a trip through one replays what both buffered, in log
  order.

### Fixed

- Records at or above the effective level are now emitted immediately inside a
  scope, instead of being buffered and lost when the scope ended cleanly: a
  service logging at Info no longer loses Info records on successful requests.
- Constructors panic on out-of-order levels (buffer floor <= level <= trip
  level must hold) instead of accepting a configuration that can never replay.

### Security

- The middleware's anchor record logs the matched route pattern
  (`GET /users/{id}`) instead of the raw URL path, which routinely carries user
  ids and tokens. `WithAnchorPath` overrides the derivation.

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
