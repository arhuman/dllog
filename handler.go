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
// Inside a scope, records from the buffer floor up to the effective level are
// cloned into a bounded ring, records at or above the effective level are
// emitted immediately, and a record at or above the trip level flushes the ring
// to the downstream handler, oldest first, before the triggering record is
// emitted.
//
// A Handler is safe for concurrent use. Build one with New.
type Handler struct {
	cfg        config
	downstream slog.Handler
	pool       *core.ScopePool[core.Entry]
}

// resolve applies opts over the defaults. Shared by every constructor so the
// option set cannot drift between them.
//
// It panics when the resolved levels are out of order: buffer floor above the
// effective level silently drops in-scope records the level promised, and an
// effective level above the trip level trips on records that would never be
// emitted, so neither configuration can do what the package exists for. The
// check is a construction-time snapshot, like New's downstream probe: a dynamic
// Leveler reordered afterwards is the caller's contract to keep.
func resolve(opts []Option) config {
	cfg := newConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	floor, level, trip := cfg.bufferFloor.Level(), cfg.level.Level(), cfg.tripLevel.Level()
	if floor > level || level > trip {
		panic(fmt.Sprintf(
			"dllog: levels out of order: buffer floor %v, level %v, trip level %v; "+
				"WithBufferFloor <= WithLevel <= WithTripLevel must hold or the handler can never buffer and replay",
			floor, level, trip))
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
// model of the package, in three ordered checks:
//
//   - At or above the effective level: true, in a scope or out of one. Handle
//     may still drop the record when a tripped scope's post-trip budget is
//     spent; Enabled overreporting there is the slog contract's cheap side.
//   - Below the buffer floor: false, everywhere. Nothing keeps such a record.
//   - In between, the buffer band: true exactly when ctx carries a live scope,
//     because only a scope has somewhere to put it.
//
// The downstream is never consulted. It is deliberately constructed wide open
// (New requires it, so replayed records survive), which makes its answer
// useless as a gate: delegating to it made every out-of-scope call in the
// buffer band build a full record that Handle then threw away. Answering from
// the handler's own levels lets slog refuse those calls before the record
// exists, which is what keeps an out-of-scope Debug near the cost of a
// disabled slog call.
//
// The level checks come first so the common out-of-band calls never pay the
// context lookup; only the buffer band needs to know whether a scope is there.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= h.cfg.level.Level() {
		return true
	}
	if level < h.cfg.bufferFloor.Level() {
		return false
	}
	return fromContext(ctx) != nil
}

// Handle buffers, replays, or forwards r according to the scope on ctx.
//
// Without a scope, r reaches the downstream handler when it is at or above the
// configured level and is dropped otherwise. Within a scope, a record below the
// effective level is cloned into the ring, a record at or above it is emitted
// immediately, and a record at or above the trip level trips the scope, flushing
// the buffer to the downstream handlers the buffered records were logged through
// before r itself is emitted. Everything happens synchronously, on the calling
// goroutine.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	c := fromContext(ctx)
	if c == nil {
		if r.Level < h.cfg.level.Level() {
			return nil
		}
		return h.downstream.Handle(ctx, r)
	}

	if r.Level >= h.cfg.tripLevel.Level() {
		if h.tripAndSuppress(c) {
			return nil
		}
		return h.downstream.Handle(ctx, r)
	}

	// At or above the effective level the record is emitted on the spot and
	// never buffered: the buffer withholds only what the level would have
	// silenced, so a scope that ends cleanly must not swallow a record the
	// caller was owed. After a trip it draws on the same post-trip budget as
	// everything else. A trip racing this check can let one record through
	// unbudgeted; that record was emittable either way.
	if r.Level >= h.cfg.level.Level() {
		if c.Tripped() && h.suppressed(c) {
			return nil
		}
		return h.downstream.Handle(ctx, r)
	}

	s := c.bind(h.pool)
	if s == nil {
		// The scope was released between fromContext and here, so this record
		// belongs to no live scope: a below-level record without a scope is
		// dropped by the level.
		return nil
	}
	switch s.Append(&slot{ctx: ctx, record: r.Clone(), downstream: h.downstream, replayKey: h.cfg.replayKey}) {
	case core.ActionBuffered:
		return nil
	case core.ActionSuppressed:
		return nil
	default:
		return h.downstream.Handle(ctx, r)
	}
}

// tripAndSuppress trips the scope for a record at or above the trip level and
// reports whether that record must be dropped rather than emitted.
//
// MarkTripped both flushes an existing ring and records the trip when there is
// no ring yet, so records logged after this failure pass through either way.
// Whichever branch applies, exactly one call is the one that moved the scope
// into the tripped state.
//
// That record is the trigger and is never suppressed: it is the error line the
// replayed batch hangs from, and dropping it would leave a pile of Debug records
// with nothing explaining them. Every trip-level record after it draws on the
// post-trip budget like anything else, because an error storm following the
// failure is exactly what that budget exists to bound.
func (h *Handler) tripAndSuppress(c *carrier) bool {
	s, trigger := c.MarkTripped()
	if s != nil {
		trigger = flush(s)
	}
	return !trigger && h.suppressed(c)
}

// suppressed reports whether a record the handler is about to emit must be
// dropped instead, because the scope has tripped and its post-trip budget is
// spent. It is the one place that budget is charged for a record the buffer
// never held.
//
// A released scope suppresses nothing: with no ring there is no budget to spend,
// and the record is governed by the level alone.
func (h *Handler) suppressed(c *carrier) bool {
	ring := c.bind(h.pool)
	return ring != nil && ring.PassThrough() == core.ActionSuppressed
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
