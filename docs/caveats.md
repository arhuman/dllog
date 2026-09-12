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

**A replayed record does not carry the original context.** The downstream
handler receives `context.Background()` on replay, while a live record carries
the request context it was logged with. A downstream that reads values from
the context, a trace or tenant ID extracted in `Handle`, a `ReplaceAttr`
closure over it, will therefore see them on live records and not on replayed
ones. Retaining the context per buffered record would extend its lifetime, and
that of everything hanging off it, past the operation the scope bounds, which
is the trade dllog refuses by design. Put values you need on every record into
the record itself (`slog.With`, zap fields) rather than fishing them out of
the context downstream.

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
