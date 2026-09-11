package dllog

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/arhuman/dllog/internal/core"
)

// Handler is a slog.Handler that buffers below-level records inside a scope and
// replays them when the scope trips.
//
// Outside a scope it is an ordinary level filter in front of the downstream
// handler, and a call below the level costs what a disabled slog call costs.
// Inside a scope, records from the buffer floor up are cloned into a bounded
// ring; a record at or above the trip level flushes that ring to the downstream
// handler, oldest first, before the triggering record is emitted.
//
// A Handler is safe for concurrent use. Build one with New.
type Handler struct {
	cfg        config
	downstream slog.Handler
	pool       *core.ScopePool[core.Entry]
}

// resolve applies opts over the defaults. Shared by every constructor so the
// option set cannot drift between them.
func resolve(opts []Option) config {
	cfg := newConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// NewJSON returns a Handler writing JSON records to w.
//
// It builds the downstream handler itself, opened at the buffer floor, so the
// caller never has to open one by hand: use this rather than [New] unless you
// already have a downstream handler to wrap. Because dllog holds the only
// reference to that handler, nothing can gate it shut afterwards and the
// mis-wiring [New] panics on cannot happen.
//
// The effective level is still [WithLevel], defaulting to Info: NewJSON(w)
// emits Info and above, and replays buffered Debug records when an operation
// fails.
func NewJSON(w io.Writer, opts ...Option) *Handler {
	return newEncoded(slog.NewJSONHandler, w, slog.HandlerOptions{}, opts)
}

// NewText returns a Handler writing text records to w. It is [NewJSON] with
// slog's text encoding.
func NewText(w io.Writer, opts ...Option) *Handler {
	return newEncoded(slog.NewTextHandler, w, slog.HandlerOptions{}, opts)
}

// NewJSONWith is [NewJSON] with control over the rest of the slog.HandlerOptions,
// for AddSource or a ReplaceAttr hook.
//
// ho.Level must be nil: dllog owns the downstream level, and a caller-set level
// is the wiring mistake this constructor exists to make impossible. NewJSONWith
// panics rather than overriding it silently, so the conflict surfaces at startup
// instead of becoming a question about which level won. ho is copied, so the
// caller's struct is not modified.
func NewJSONWith(w io.Writer, ho slog.HandlerOptions, opts ...Option) *Handler {
	mustOwnLevel(ho, "NewJSONWith")
	return newEncoded(slog.NewJSONHandler, w, ho, opts)
}

// NewTextWith is [NewJSONWith] with slog's text encoding.
func NewTextWith(w io.Writer, ho slog.HandlerOptions, opts ...Option) *Handler {
	mustOwnLevel(ho, "NewTextWith")
	return newEncoded(slog.NewTextHandler, w, ho, opts)
}

// mustOwnLevel rejects a caller-supplied level on the *With constructors.
func mustOwnLevel(ho slog.HandlerOptions, fn string) {
	if ho.Level != nil {
		panic(fmt.Sprintf(
			"dllog: %s called with HandlerOptions.Level set (%v); dllog owns the "+
				"downstream level, so leave it nil and pass WithLevel/WithBufferFloor instead",
			fn, ho.Level.Level()))
	}
}

// newEncoded builds a Handler over a downstream this package constructs with
// encode, which is slog.NewJSONHandler or slog.NewTextHandler.
//
// The options resolve first, so the downstream is opened at the final buffer
// floor, and it receives the configured Leveler rather than a resolved Level so
// a dynamic floor keeps tracking. No Enabled probe: the downstream is opened at
// the floor by construction, which is what New's probe checks for at runtime.
func newEncoded[H slog.Handler](
	encode func(io.Writer, *slog.HandlerOptions) H,
	w io.Writer,
	ho slog.HandlerOptions,
	opts []Option,
) *Handler {
	cfg := resolve(opts)
	ho.Level = cfg.bufferFloor
	return &Handler{
		cfg:        cfg,
		downstream: encode(w, &ho),
		pool:       core.NewScopePool[core.Entry](cfg.capacity, cfg.postTripLimit),
	}
}

// New returns a Handler wrapping downstream.
//
// Prefer [NewJSON] or [NewText] unless you already have a downstream handler:
// they build one correctly opened and cannot be mis-wired.
//
// downstream must be constructed wide open, at or below the buffer floor:
// dllog owns the effective level, and a downstream that filters would discard
// exactly the replayed records the package exists to deliver. New panics if
// downstream.Enabled reports the buffer floor disabled, because that is a wiring
// mistake in program setup with no sensible runtime recovery, and failing at
// construction is far kinder than silently losing every replay in production.
//
// New panics if downstream is nil, for the same reason.
func New(downstream slog.Handler, opts ...Option) *Handler {
	if downstream == nil {
		panic("dllog: New called with a nil downstream handler")
	}

	cfg := resolve(opts)

	floor := cfg.bufferFloor.Level()
	if !downstream.Enabled(context.Background(), floor) {
		panic(fmt.Sprintf(
			"dllog: downstream handler has %v disabled; construct it wide open "+
				"(slog.HandlerOptions{Level: %v}) so replayed records survive, "+
				"or use dllog.NewJSON/NewText to have dllog build it for you",
			floor, floor))
	}

	return &Handler{
		cfg:        cfg,
		downstream: downstream,
		pool:       core.NewScopePool[core.Entry](cfg.capacity, cfg.postTripLimit),
	}
}

// Enabled reports whether a record at level should be produced. It is the cost
// model of the package, in three branches:
//
//   - No scope on ctx: the downstream handler decides, so an out-of-scope Debug
//     call stays the near-free no-op it would be without dllog.
//   - Scope present, not yet tripped: true from the buffer floor up, because
//     those records are candidates for the buffer.
//   - Scope present, tripped: true from the buffer floor up, because they now
//     pass straight through.
//
// The last two coincide today; they are kept distinct because they answer
// different questions and only the tripped branch is bounded by the post-trip
// limit, which Handle applies.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if fromContext(ctx) == nil {
		return h.downstream.Enabled(ctx, level)
	}
	return level >= h.cfg.bufferFloor.Level()
}

// Handle buffers, replays, or forwards r according to the scope on ctx.
//
// Without a scope, r reaches the downstream handler when it is at or above the
// configured level and is dropped otherwise. Within a scope, a record below the
// trip level is cloned into the ring; a record at or above it trips the scope,
// flushing the buffer to the downstream handlers the buffered records were
// logged through before r itself is emitted. Everything happens synchronously,
// on the calling goroutine.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	c := fromContext(ctx)
	if c == nil {
		if r.Level < h.cfg.level.Level() {
			return nil
		}
		return h.downstream.Handle(ctx, r)
	}

	if r.Level >= h.cfg.tripLevel.Level() {
		// markTripped both flushes an existing ring and records the trip when
		// there is no ring yet, so records logged after this failure pass
		// through either way.
		if s := c.MarkTripped(); s != nil {
			flush(s, h.cfg.replayKey)
		}
		return h.downstream.Handle(ctx, r)
	}

	switch s := c.bind(h.pool); s.Append(slot{record: r.Clone(), downstream: h.downstream}) {
	case core.ActionBuffered:
		return nil
	case core.ActionSuppressed:
		return nil
	default:
		return h.downstream.Handle(ctx, r)
	}
}

// WithAttrs returns a Handler whose records carry attrs, sharing this Handler's
// configuration and scope pool.
//
// The attrs are applied eagerly to the downstream handler, and each buffered
// record travels with the downstream it was logged through, so a replay renders
// exactly the attrs that were in effect at log time. Sibling handlers derived
// from the same parent stay independent.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.derive(h.downstream.WithAttrs(attrs))
}

// WithGroup returns a Handler that nests subsequent attrs under name, following
// the slog.Handler contract that an empty name returns the receiver unchanged.
//
// The group is opened on the downstream handler eagerly, exactly as WithAttrs
// applies attrs. One consequence is documented rather than engineered around:
// the replay marker is added at flush time, so for a record buffered through a
// grouped handler the marker nests inside that group instead of sitting at the
// record root.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.derive(h.downstream.WithGroup(name))
}

// derive builds a sibling Handler over an already-derived downstream. Config
// and pool are shared: the scope a record lands in is the one on its context,
// never one owned by a particular derived handler.
func (h *Handler) derive(downstream slog.Handler) *Handler {
	return &Handler{
		cfg:        h.cfg,
		downstream: downstream,
		pool:       h.pool,
	}
}
