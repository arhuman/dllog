# Caveats

All of these follow from the same thing: a buffered record is written now and
emitted later. Worth reading before you rely on it.

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

**A replayed record carries the context it was logged with, cancelled or
not.** Each buffered record keeps its original `context.Context` and is
replayed under it, so a downstream that enriches from the context, trace and
span IDs, tenant, correlation ids, sees the same values on replayed records as
on live ones. Two consequences are worth knowing. The context may already be
cancelled when the replay runs; the slog contract tells handlers to read
values from a context and never its cancellation, so a conforming downstream
is unaffected. And the buffer holds a reference to the context until the scope
trips or ends, which is bounded by the operation itself but joins the
retention list above: whatever hangs off that context lives as long as the
operation does anyway. The zap adapter has no equivalent because zap itself
carries no per-call context; nothing is lost there that zap ever had. See
[ADR 0002](adr/0002-replay-context.md).

**Under `WithGroup`, the replay marker moves.** A handler built with
`WithGroup("http")` nests everything it logs under that group name. The marker
dllog adds on replay is written at replay time, so it lands inside the group
alongside your attributes rather than at the top level of the record. Only the
marker's position changes, not whether it is there. Matching slog's grouping
rules well enough to hoist it out would cost more than the tidier output is
worth.

Both trip forms mark a replayed record with the replay key of the handler that
logged it, so `WithReplayKey` is honoured whichever one you call, including the
one the middleware uses internally. `dllog.Trip(ctx)` and `h.Trip(ctx)` are
equivalent; prefer the method when a handler is already in hand. See
[ADR 0001](adr/0001-replay-key-per-entry.md).
