# 2. A replayed record travels with the context it was logged with

Date: 2026-09-12

## Status

Accepted.

## Context

The slog handler buffers a record now and emits it later, if the operation
fails. `slog.Handler.Handle` takes a `context.Context`, and downstream
handlers routinely enrich records from it: OpenTelemetry trace and span IDs,
tenant and request metadata, correlation ids, `ReplaceAttr` closures reading
request state. Those are exactly the values an operator needs on the records a
replay produces during an incident.

Until now the buffered slot kept the record and its downstream but not the
context, and replayed with `context.Background()`. Live and replayed records
therefore looked different to any context-enriching downstream: the live error
carried its trace ID, the replayed Debug lines around it did not. The
divergence undermined the claim that dllog composes with an existing
observability stack, on the failure path where that claim matters most.

The original justification for dropping the context was that retaining it
would extend its lifetime past the operation. That reasoning does not hold:
the ring clears its slots when the scope trips and when it closes, and both
ends coincide with the end of the operation the context belongs to. Keeping
the context in the slot retains it for no longer than the operation retains it
anyway.

## Decision

Each buffered slot stores the `context.Context` the record was logged with,
and both `Emit` (the replay) and `Notify` (the eviction notice) hand that
context to the downstream handler.

The context is stored as passed, including its cancellation state. A replay
that runs after cancellation still hands the original context over: the slog
contract directs handlers to read values from a context and never to observe
its cancellation, so a conforming downstream sees the same enrichment either
way.

The zap adapter is untouched. `zapcore.Core.Check` and `Write` take no
context, so zap call sites never had one to lose; there is no asymmetry to
compensate, only an API difference inherent to zap.

## Consequences

- Live and replayed records are indistinguishable to a context-enriching
  downstream, apart from the replay marker itself.
- A slot costs one context reference more (two words, no extra allocation);
  the buffer's retention of context values is bounded by the operation, and is
  documented in the caveats beside the existing value-retention note.
- A downstream that (against the slog contract) branches on `ctx.Err()` may
  treat late replays differently; that is the downstream's contract breach,
  and the caveats say so.
- The `docs/caveats.md` entry that documented the old behaviour is replaced by
  one documenting this one.
