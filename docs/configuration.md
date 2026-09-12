# Configuration

## Choosing the output

`NewText` is `NewJSON` with slog's text encoding. For `AddSource` or a
`ReplaceAttr` hook, `NewJSONWith` and `NewTextWith` take the rest of the
`slog.HandlerOptions`:

```go
logger := slog.New(dllog.NewJSONWith(os.Stderr, slog.HandlerOptions{
	AddSource:   true,
	ReplaceAttr: redact,
	// Level stays nil: dllog owns it.
}, dllog.WithLevel(slog.LevelWarn)))
```

`Level` must be left nil. dllog sets it from the buffer floor, and a
caller-supplied level is refused at construction rather than silently
overridden.

## Bringing your own handler

`New` wraps a handler you already have. It costs one rule: that handler must be
constructed wide open at `Debug`, and you set the level you actually want on
dllog instead.

```go
downstream := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
	Level: slog.LevelDebug, // wide open: dllog decides what is emitted
})
logger := slog.New(dllog.New(downstream, dllog.WithLevel(slog.LevelInfo)))
```

Both lines are load-bearing. dllog owns the effective level, so a downstream
that filters below the buffer floor would discard exactly the records dllog
exists to deliver. `New` panics when it detects that rather than letting it
reach production, but the check can only run at construction: a downstream
holding a `slog.Leveler` that is lowered later escapes it. The constructors
above have no such gap, since dllog builds the handler and keeps the only
reference to it.

## Handler options

| Option | Default | Effect |
|---|---|---|
| `WithLevel` | `Info` | Effective level: records at or above it are emitted as usual, in a scope or out of one; below it they are buffered in a scope and dropped outside |
| `WithBufferFloor` | `Debug` | Lowest level captured inside a scope |
| `WithTripLevel` | `Error` | Level at which a record triggers replay |
| `WithCapacity` | 256 | Ring slots per scope |
| `WithPostTripLimit` | 0 (unlimited) | Records allowed through after a trip |
| `WithReplayKey` | `"replay"` | Attribute marking a replayed record |

The three levels must satisfy buffer floor <= level <= trip level. A
configuration that breaks that order could never buffer and replay, so every
constructor rejects it with a panic rather than shipping a handler that does
nothing.

## Middleware options

| Option | Default | Effect |
|---|---|---|
| `WithTripOn` | `status >= 500` | Predicate deciding which statuses trip |
| `WithLogger` | `slog.Default()` | Logger used for the anchor error record |
| `WithAnchorPath` | route pattern, else raw path | How the anchor's `path` attribute is derived |

The middleware does not trip on 4xx or on context cancellation: 404s and client
disconnects would bury the signal. On a panic it trips, then re-panics with the
original value. It is a logging tool, never a recovery layer.

The anchor record's `path` is the matched route pattern (`GET /users/{id}`),
not the raw URL. Path segments routinely carry user ids, reset tokens and api
keys, and the anchor is emitted on the failure path, which is exactly when logs
get exported and retained; the pattern says which endpoint failed without saying
it about whom. A request that carries no pattern, either mounted directly or
routed by something other than `http.ServeMux`, falls back to the raw path.
Use `WithAnchorPath` to supply your own router's route, or to redact it
differently.
