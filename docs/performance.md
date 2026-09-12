# Performance and memory

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

## Reproducing

```bash
make bench
```

Changing a figure in this file means re-running that target and pasting the new
numbers, rather than editing them by hand.
