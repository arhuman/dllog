package dllog

import (
	"context"
	"log/slog"
	"net/http"
)

// AnchorMessage is the message of the record the middleware emits when a
// request trips, so a flush always has an anchoring error line.
const AnchorMessage = "dllog: request failed"

// MWOption configures Middleware. Options are applied in the order given.
type MWOption func(*mwConfig)

// mwConfig is the resolved middleware settings. Every field has a usable
// default applied by newMWConfig, so an option is only ever an override.
type mwConfig struct {
	tripOn     func(status int) bool
	logger     *slog.Logger
	anchorPath func(*http.Request) string
}

// newMWConfig returns the defaults: trip on 5xx, anchor through the default
// logger. The logger is resolved lazily at request time so a program that
// installs its logger after wiring the middleware still gets the right one.
func newMWConfig() mwConfig {
	return mwConfig{
		tripOn:     func(status int) bool { return status >= 500 },
		anchorPath: routePath,
	}
}

// routePath is the default anchor path: the matched route pattern when there is
// one, the raw path otherwise.
//
// The pattern is the safe form. A raw path carries whatever the route
// interpolated into it, and identifiers, reset tokens and api keys all routinely
// live in path segments; the anchor is emitted on the failure path, which is
// exactly when logs are most likely to be exported and retained. The pattern
// says which endpoint failed without saying it about whom.
//
// It is empty for a handler that was not routed through a [http.ServeMux], and
// for one routed by a third-party router that does not populate it. The raw path
// is the fallback there, because an anchor with no path at all cannot be
// correlated with anything. Callers who route with something else, or who want
// their own redaction, supply it with [WithAnchorPath].
//
// Pattern is only populated once the mux has matched, so this must not be called
// before the handler has run. The middleware calls it from the deferred anchor,
// after ServeHTTP returns, where it is set.
func routePath(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.URL.Path
}

// WithTripOn sets the predicate deciding whether a response status trips the
// scope. It defaults to status >= 500.
//
// Replacing it replaces the default entirely: a predicate that ignores 5xx
// makes the middleware ignore 5xx. A nil predicate is ignored.
func WithTripOn(pred func(status int) bool) MWOption {
	return func(c *mwConfig) {
		if pred != nil {
			c.tripOn = pred
		}
	}
}

// WithLogger sets the logger the anchor record is emitted through. It defaults
// to slog.Default() as of the request. A nil logger is ignored.
//
// The logger should be backed by a dllog Handler; that is what turns the anchor
// into a replay of the request's buffered records.
func WithLogger(l *slog.Logger) MWOption {
	return func(c *mwConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithAnchorPath sets how the anchor record's path attribute is derived from the
// request. It defaults to the matched route pattern, falling back to the raw URL
// path when the request carries no pattern.
//
// Use it when routing with something other than [http.ServeMux], which leaves
// [http.Request.Pattern] empty and so falls back to the raw path, or to redact
// the path some other way. Returning "" emits an empty path attribute rather
// than omitting it.
//
// The function is called on the failure path only, after the handler has
// returned, and must not panic: it runs inside the deferred anchor, so a panic
// there would replace the handler's own. A nil argument is ignored.
func WithAnchorPath(fn func(*http.Request) string) MWOption {
	return func(c *mwConfig) {
		if fn != nil {
			c.anchorPath = fn
		}
	}
}

// Middleware returns net/http middleware that opens a dllog scope per request.
//
// The scope is placed on the request context, the derived request is passed
// down, and the scope is released when the handler returns. A request that
// succeeds discards its buffered records; a request that fails replays them.
//
// A request trips when its response status satisfies the trip predicate
// (default: 5xx) or when the handler panics. It does not trip on 4xx or on
// context cancellation: 404s and client disconnects would drown the signal.
//
// A panic is recovered only long enough to trip the scope, then re-panicked
// with the original value. This is a logging tool, not a recovery layer, so an
// outer recovery middleware still sees exactly the panic it would have seen.
//
// When a request trips, one anchor Error record carrying method, path and
// status is emitted, so a flush is never a headless pile of Debug lines. It is
// suppressed when the handler already tripped the scope itself, which keeps a
// handler that logged its own Error from being anchored twice.
//
// The path is the matched route pattern ("GET /users/{id}"), not the raw URL, so
// the anchor does not put the ids and tokens interpolated into a path into the
// error log. A request with no pattern falls back to the raw path; see
// [WithAnchorPath] to control this.
func Middleware(opts ...MWOption) func(http.Handler) http.Handler {
	cfg := newMWConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, done := Scope(r.Context())
			defer done()

			req := r.WithContext(ctx)
			sw := &statusWriter{ResponseWriter: w}

			// The deferred func runs on both the normal and the panicking path,
			// so the panic case trips before the value continues outward.
			defer func() {
				if p := recover(); p != nil {
					cfg.anchor(ctx, req, sw.statusCode())
					panic(p)
				}
				if status := sw.statusCode(); cfg.tripOn(status) {
					cfg.anchor(ctx, req, status)
				}
			}()

			next.ServeHTTP(sw, req)
		})
	}
}

// anchor trips the scope and emits the anchor record, unless the scope has
// already tripped, in which case the request already has its error line and a
// second one would only duplicate it.
func (c mwConfig) anchor(ctx context.Context, r *http.Request, status int) {
	if alreadyTripped(ctx) {
		return
	}
	// Trip first so the buffered records land before the anchor, which then
	// passes straight through. Doing it in the other order would work only for
	// a logger that happens to be backed by a dllog Handler.
	Trip(ctx)

	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.ErrorContext(ctx, AnchorMessage,
		slog.String("method", r.Method),
		slog.String("path", c.anchorPath(r)),
		slog.Int("status", status),
	)
}

// alreadyTripped reports whether the scope on ctx has already tripped, whether
// through a record at the trip level or through a bare Trip that arrived before
// anything was buffered.
func alreadyTripped(ctx context.Context) bool {
	c := fromContext(ctx)
	if c == nil {
		return false
	}
	return c.Tripped()
}

// statusWriter observes the status a handler writes while leaving the
// ResponseWriter's optional interfaces reachable.
//
// It deliberately implements neither Flusher nor Hijacker: since Go 1.20 the
// correct way through a wrapper is http.NewResponseController, which follows
// Unwrap. Re-declaring those methods here would be the classic bug, promising
// capabilities the underlying writer may not have.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the first status only, matching the net/http server,
// which honours the first call and warns about the rest.
func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write fills in the implicit 200 a handler that writes a body without calling
// WriteHeader produces.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// statusCode reports the status the client will see, resolving the implicit 200
// of a handler that never called WriteHeader (including one that wrote nothing
// at all) so a trip predicate never has to reason about a zero.
func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Unwrap exposes the wrapped writer to http.ResponseController, which is what
// keeps Flush, Hijack, ReadFrom and Push working through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
