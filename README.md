# dllog

[![CI](https://github.com/arhuman/dllog/actions/workflows/ci.yml/badge.svg)](https://github.com/arhuman/dllog/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/arhuman/dllog.svg)](https://pkg.go.dev/github.com/arhuman/dllog)
[![Go Report Card](https://goreportcard.com/badge/github.com/arhuman/dllog)](https://goreportcard.com/report/github.com/arhuman/dllog)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Every Go service faces the same choice: `Debug` floods production, `Info` hides
the context that explains an error. dllog breaks that trade-off per operation.
It buffers below-level records in a bounded per-operation ring, so a successful
operation stays as quiet as your logger at Info while a failed one replays the
`Debug` records that led to it, with their original timestamps.

dllog is not a logging library and does not replace yours: it plugs into the
one you already use. It works with `log/slog` and `zap` today, even mixed in
the same service, and other libraries can be supported the same way.

## The same failure, three ways

Three checkouts, logged three times by the same program: one succeeds, one ends
in a declined payment, one succeeds again. Reproduce any column with
`go run ./examples/demo [info|debug|dllog]`.

<table>
<tr>
<th align="center"><code>slog</code> at Info</th>
<th align="center"><code>dllog</code> at Info</th>
<th align="center"><code>slog</code> at Debug</th>
</tr>
<tr>
<td><img src="docs/img/demo-info.gif" alt="slog at Info: the error with no context"></td>
<td><img src="docs/img/demo-dllog.gif" alt="dllog at Info: the error, with the Debug records that led to it replayed"></td>
<td><img src="docs/img/demo-debug.gif" alt="slog at Debug: every record, on every request"></td>
</tr>
<tr>
<td>You know the middle one failed. You do not know why: the card token, the
retry, the decline code were all below the level.</td>
<td>As quiet as the left column on the two that succeed. On the one that fails,
the buffered <code>Debug</code> records replay with their original timestamps,
marked <code>replay=true</code>.</td>
<td>The full story, but you pay for it on the successful checkouts too, which is
why nobody leaves this on.</td>
</tr>
</table>

## Install

```bash
go get github.com/arhuman/dllog
```

Requires Go 1.24 or later. The root package has no dependencies: `zap` is
optional and imported only by `zapadapter`.

## Usage

Pick the entry point that matches the code you are instrumenting:

- HTTP server: wrap your handler in `Middleware()`.
- Anything else: open a scope with `Scope`, and call `Trip` when you fail.
- Using zap instead of slog: see [the zap adapter](docs/zap.md).

### HTTP

`NewJSON` builds the handler and its output for you. There is nothing else to
wire: the service logs at `Info`, and a failed request also gets the `Debug`
records that led to it.

```go
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/arhuman/dllog"
)

func main() {
	logger := slog.New(dllog.NewJSON(os.Stderr))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Buffered: invisible on a successful request.
		slog.DebugContext(ctx, "loading cart", "user", 42)
		slog.DebugContext(ctx, "applying discount", "code", "SUMMER")

		// Any Error record replays everything buffered above it first.
		slog.ErrorContext(ctx, "payment declined", "provider", "stripe")

		w.WriteHeader(http.StatusInternalServerError)
	})

	// The middleware opens a scope per request, trips on 5xx and on panic.
	http.ListenAndServe(":8080", dllog.Middleware()(mux))
}
```

A request that fails emits the two buffered `Debug` records, marked and carrying
the time they were logged at, ahead of the error that released them:

```json
{"time":"2026-09-12T10:23:06.226402+02:00","level":"DEBUG","msg":"loading cart","user":42,"replay":true}
{"time":"2026-09-12T10:23:06.226433+02:00","level":"DEBUG","msg":"applying discount","code":"SUMMER","replay":true}
{"time":"2026-09-12T10:23:06.226434+02:00","level":"ERROR","msg":"payment declined","provider":"stripe"}
```

A request that succeeds emits neither: the buffer is discarded when the scope
ends.

### Outside HTTP

Manage the scope yourself. `Trip` covers the common Go case where the error is
returned rather than logged:

```go
func process(ctx context.Context, id string) error {
	ctx, done := dllog.Scope(ctx)
	defer done()

	slog.DebugContext(ctx, "fetching record", "id", id)

	if err := doWork(ctx); err != nil {
		dllog.Trip(ctx) // replay the buffer, then return the error as usual
		return err
	}
	return nil // buffer discarded, nothing emitted
}
```

`Scope` joins rather than nests: calling it on a context that already carries a
scope returns that same scope and a `done` that does nothing, so only the
creator releases the buffer.

## Cost

Outside a scope, a Debug call costs 9.1 ns/op and zero allocations against the
4.2 ns/op of a plain slog logger configured at Info: the record is refused
before it is built, and the difference is one context lookup. Inside a scope,
buffering a record costs about 235 ns and one allocation, the price of having
it available if the operation later fails.

The buffer is strictly count-bounded: a fixed preallocated ring per scope, 256
records by default, evicting oldest-first with a synthetic record reporting
anything dropped. What each buffered record retains is up to you, since a
record keeps references to what you logged until the scope ends: see
[the caveats](docs/caveats.md). Nothing grows with uptime.

Full tables and methodology: [docs/performance.md](docs/performance.md).

## Documentation

| Document | Contents |
|---|---|
| [Configuration](docs/configuration.md) | Every option, choosing the output encoding, wrapping a handler you already have |
| [Performance](docs/performance.md) | Benchmarks, the memory model, soak results |
| [Caveats](docs/caveats.md) | Mutated values, buffer retention, `WithGroup` |
| [zap adapter](docs/zap.md) | Driving the same engine from zap, and its binding cost |
| [ADRs](docs/adr/) | Architecture decision records |

A buffered record is written now and formatted later, which has consequences
worth knowing before you rely on it: read [the caveats](docs/caveats.md).

## Status

The `log/slog` handler, the HTTP middleware, and the zap adapter are implemented
and tested. Neither adapter is built on the other: both drive `internal/core`
directly, and either one's `Scope` is visible to the other.

Pre-v1: released as v0.1.x, and the API may still change before v1. Releases
are tagged and listed in the [CHANGELOG](CHANGELOG.md).

## Security

Report a vulnerability by email rather than a public issue: see
[SECURITY.md](SECURITY.md).

## License

MIT, see [LICENSE](LICENSE).
