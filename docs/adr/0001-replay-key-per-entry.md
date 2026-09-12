# 1. The replay key belongs to the record, not to the trip

Date: 2026-09-12

## Status

Accepted. Supersedes the replay-marking decision recorded in the P8 design note
(`.claude/doc/DESIGN.md` section 10).

## Context

A replayed record carries a marker attr so a query can tell a replayed line from
a live one. The key defaults to `replay` and is configurable per handler with
`WithReplayKey`.

Until now the key was chosen when the scope tripped. `core.Entry.Emit` took the
key as a parameter and `core.Flush(scope, key)` stamped one key on every entry
in the ring. Whoever raised the trip therefore decided how the whole batch was
marked. Three trip paths exist, and they disagreed:

- `(*Handler).Trip(ctx)` passed the handler's configured key.
- The package-level `Trip(ctx)` holds no handler and passed the default key.
- The zap adapter passed its own core's key.

Two failures follow, and both are reachable from documented usage.

**The middleware cannot honour `WithReplayKey`.** `Middleware` builds no handler
and calls the package-level `Trip`, so a service configured with
`WithReplayKey("from_buffer")` gets middleware-tripped replays marked `replay`
and handler-tripped replays marked `from_buffer`. A dashboard filtering on the
configured key silently misses every replay the middleware produced, which is
most of them in an HTTP service.

**A trip through one adapter mismarks the other adapter's records.** Both
adapters share one ring by design, so a trip raised through zap stamped zap's
key on records slog had buffered, and the reverse. Nothing warned about it.

The P8 note treated the first failure as inherent and added `(*Handler).Trip` as
the workaround, documenting the split as deliberate. It rejected "store the key
on the scope" as ambiguous when two handlers with different keys share a scope.
That rejection was correct about the scope, and it is what made the trip-time
model look forced: a comment in `internal/core/carrier.go` stated the key
"cannot be baked into the closure" because it is chosen at trip time. That is
circular. The key is a property of the handler that logged the record, known at
append time; only the implementation deferred it.

## Decision

Capture the replay key per buffered entry, when the record is appended, instead
of per flush.

- `slot` (both adapters) carries the origin handler's `replayKey`, alongside the
  record and the downstream it was logged through.
- `core.Entry.Emit` takes no parameter; each closure marks with its own key.
- `core.Flush(scope)` loses its key parameter, and so do the internal `trip` and
  `flush` helpers.

Trip entry points keep their signatures and no public API is added. Package
`Trip`, `(*Handler).Trip` and the zap trip paths now all mean the same thing:
flush the ring, each record marked as its origin handler configured it.

## Consequences

### Positive

- The middleware honours `WithReplayKey` with no new option and no wiring: the
  key reaches the replay through the record, not through the tripper.
- A cross-adapter trip marks every record with its own adapter's key.
- The P8 ambiguity dissolves rather than being arbitrated. "Which key when two
  handlers share a scope" has no answer to pick: each record carries its own.
- Free at runtime, measured rather than assumed. The ring stores `core.Entry`
  (16 B of function pointers), never the slot; the slot is already captured by
  the `Emit` closure and heap-allocated per buffered record. The added string
  header rides in an allocation that already happens, so allocations per record
  are unchanged (3) and timings move within run-to-run noise:

  | benchmark | before | after | allocs |
  |---|---|---|---|
  | ScopedDebugAppend | 258.6 ns | 260.6 ns | 3 -> 3 |
  | TripReplay/buffered-16 | 9304 ns | 9335 ns | 56 -> 56 |
  | TripReplay/buffered-256 | 134316 ns | 135813 ns | 776 -> 776 |

### Negative

- `(*Handler).Trip` loses its reason to exist. It stays, because removing it
  would break callers and it is still the clearer spelling when a handler is in
  hand, but it is now identical in behaviour to the package-level `Trip`. Its
  doc comment says so.
- A behaviour change for anyone who relied on the old split: a caller using
  `WithReplayKey` and the package-level `Trip` previously got `replay` and now
  gets the configured key. This is the bug being fixed, but it is visible in
  output, so it belongs in the changelog rather than passing silently.
- `TestPackageTripKeepsTheDefaultKey` asserted the old contract and is replaced
  by `TestPackageTripUsesOriginKey`, which asserts the new one over the same
  call path.

### Neutral

- The `WithGroup` marker-nesting caveat is unchanged: the marker is still added
  when the record is emitted, so a record buffered through a grouped handler
  still nests its marker inside that group.
