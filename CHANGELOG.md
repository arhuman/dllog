# Changelog

All notable changes to this project are documented here.
Format: [Keep a Changelog](https://keepachangelog.com).
This project adheres to [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- `dllog.New(downstream, opts...)`: a `log/slog` handler that buffers below-level
  records per operation and replays them when the operation fails.
- `dllog.NewJSON`, `dllog.NewText`, `dllog.NewJSONWith`, `dllog.NewTextWith`,
  `zapadapter.NewJSON`, `zapadapter.NewConsole`: constructors that build the
  downstream handler too.
- `dllog.Scope(ctx)`: opens the per-operation buffering scope, released by the
  returned `done`.
- `dllog.Trip(ctx)` and `(*Handler).Trip(ctx)`: explicit flush for failures
  returned rather than logged.
- `dllog.Middleware()`: a scope per HTTP request, tripped by 5xx or panic.
- Handler options `WithLevel`, `WithBufferFloor`, `WithTripLevel`,
  `WithCapacity`, `WithPostTripLimit`, `WithReplayKey`; middleware options
  `WithLogger`, `WithTripOn`.
- `zapadapter`: a `zapcore.Core` on the same engine, sharing scopes with the
  slog handler.

### Fixed

- Replayed records carry the replay key of the handler that logged them, so
  `WithReplayKey` is honoured by the middleware and across adapters (see
  [ADR 0001](docs/adr/0001-replay-key-per-entry.md)).
- In-scope records at or above the effective level are emitted immediately
  instead of lost when the scope ends cleanly.
- Constructors panic on out-of-order levels (buffer floor <= level <= trip
  level).
- A released scope no longer emits or buffers: a context used after its `done`
  behaves as one that never carried a scope.
- `WithPostTripLimit` bounds records at the trip level too, so a post-failure
  error storm is capped; the record that trips the scope stays exempt.
- The zap adapter writes through the downstream's `Check`, so a downstream
  sampler, tee or routing core governs replayed and live entries alike.
- The zap adapter drops a below-level entry that races the scope's release
  instead of writing it, matching the slog handler.
- A downstream write failure in the zap adapter is reported to stderr instead
  of vanishing silently.
- Replayed records carry the context they were logged with, so downstream
  handlers enriching from it (trace ids, tenants) see live and replayed
  records alike (see [ADR 0002](docs/adr/0002-replay-context.md)).

### Changed

- `Enabled` answers from the handler's own levels instead of delegating to the
  wide-open downstream, so an out-of-scope below-level call is refused before
  slog builds the record; `logger.Enabled(ctx, LevelDebug)` outside a scope now
  reports false. Same on the zap adapter.
- Buffering a record costs one allocation (the ring slot) instead of three:
  ring entries implement an interface rather than carrying closures.
- Per-record scope-state reads (bound, tripped, closed) are atomic loads; the
  carrier's mutex now guards only its transitions.
- A below-level record that races the scope's release is dropped rather than
  emitted: a closed ring suppresses what it is offered, as no scope would have
  kept it.

### Security

- The middleware's anchor record logs the matched route pattern instead of the
  raw URL path, keeping user ids and tokens out of error logs.

### Notes

- Scope memory is hard-bounded: a fixed pooled ring per scope, oldest dropped
  first with the loss counted in the replay.
- With no scope open, a Debug call costs 8.9 ns and zero allocations against
  the 4.0 ns of a plain slog logger configured at Info (M3 Pro).
- Buffered values are formatted at replay, not at log time; see the README
  caveats before logging mutable references.
- Requires Go 1.24 or later.
- Passes `testing/slogtest.TestHandler`.
