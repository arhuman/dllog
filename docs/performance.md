# Performance and memory

## Measured cost

One run of `go test -bench=. -benchmem` on an Apple M3 Pro, `go1.26.6`,
`darwin/arm64`. Treat these as magnitudes, not exact figures: the sub-microsecond
rows move by a few percent between runs. This table is the single source of
truth; the prose below explains it and quotes no other numbers.

| Path | Time | Bytes | Allocations |
|---|---|---|---|
| Plain slog, Info-gated handler, Debug call | 4.2 ns/op | 0 | 0 |
| dllog, no scope in context, Debug call | 9.1 ns/op | 0 | 0 |
| Plain slog, Debug-open downstream, Debug call | 154.9 ns/op | 0 | 0 |
| dllog, buffering inside a scope | 234.8 ns/op | 352 | 1 |
| `Enabled`, no scope | 6.7 ns/op | 0 | 0 |
| Full request through the middleware, 200 | 2231 ns/op | 3635 | 21 |
| Full request through the middleware, 500 | 5137 ns/op | 3910 | 25 |

Three things are worth reading carefully.

**Out of scope, dllog costs a few nanoseconds over a disabled call.** The
honest baseline is the first row: a production service configured at Info,
where `slog.Logger` sees Debug disabled and drops the call before building a
record. dllog's `Enabled` answers from its own levels, so an out-of-scope Debug
is refused the same way; the difference between 9.1 ns and 4.2 ns is one
context lookup, paid only for levels between the buffer floor and the effective
level. The 155 ns third row is what rendering Debug everywhere costs; dllog
does not pay it, and neither does the caller.

**Inside a scope, you pay to keep the record.** Buffering costs about 235 ns
and one allocation: the record and its attributes are cloned into a ring slot
and held rather than written and forgotten. That is the price of having the
Debug context available if the operation later fails. The ring arrays
themselves are recycled through a pool, so scope churn does not grow the heap;
the per-record slot is the one allocation that remains.

**A failure costs microseconds, once.** The middleware rows bracket the range:
a clean request pays scope setup and teardown, a failing one additionally
replays its buffer through the real encoder. Both are request-scale numbers,
paid per request rather than per record.

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
