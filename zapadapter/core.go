// Package zapadapter is the zap adapter for dllog: a zapcore.Core that buffers
// below-level entries inside a dllog scope and replays them when the scope
// trips.
//
// It is driven by the same internal engine as the slog handler in the root
// dllog package, so a scope opened once is shared: an Error logged through zap
// replays what slog buffered on that context, and the reverse, in the order the
// entries were logged.
//
// # Binding a scope
//
// zapcore.Core has no context.Context in Check or Write, so unlike the slog
// handler this adapter cannot find the scope at log time. The context is bound
// once, up front, with [Core.For]:
//
//	base := zapadapter.NewJSON(os.Stderr, zap.NewProductionEncoderConfig())
//	logger := zap.New(base)          // process logger, no scope
//	ctx, done := zapadapter.Scope(r.Context())
//	defer done()
//	reqLog := zap.New(base.For(ctx)) // request logger, buffering
//
// A Core that was never bound behaves as a plain level filter in front of the
// downstream core. See the package README section in the root doc.go for the
// ergonomic difference this imposes versus slog's *Context methods.
package zapadapter

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap/zapcore"

	"github.com/arhuman/dllog/internal/core"
)

// Core is a zapcore.Core that buffers below-level entries inside a scope and
// replays them when the scope trips.
//
// A Core is bound to at most one context, by [Core.For]. Unbound, it is an
// ordinary level filter in front of the downstream core, and an entry below the
// level costs what a disabled zap call costs. Bound, entries from the buffer
// floor up to the effective level are appended to the scope's bounded ring,
// entries at or above the effective level are written immediately, and an entry
// at or above the trip level flushes the ring to the downstream core, oldest
// first, before the triggering entry is written.
//
// A Core is safe for concurrent use. Build one with [New].
type Core struct {
	cfg        config
	downstream zapcore.Core
	pool       *core.ScopePool[core.Entry]

	// carrier is the bound scope, nil on an unbound Core. It is set once by For
	// and never mutated, so the three Enabled branches are a nil check rather
	// than a context lookup on every entry.
	carrier *carrier
}

// resolve applies opts over the defaults. Shared by every constructor so the
// option set cannot drift between them.
//
// It panics when the resolved levels are out of order: buffer floor above the
// effective level silently drops in-scope entries the level promised, and an
// effective level above the trip level trips on entries that would never be
// written, so neither configuration can do what the package exists for.
func resolve(opts []Option) config {
	cfg := newConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.bufferFloor > cfg.level || cfg.level > cfg.tripLevel {
		panic(fmt.Sprintf(
			"dllog/zapadapter: levels out of order: buffer floor %v, level %v, trip level %v; "+
				"WithBufferFloor <= WithLevel <= WithTripLevel must hold or the core can never buffer and replay",
			cfg.bufferFloor, cfg.level, cfg.tripLevel))
	}
	return cfg
}

// NewJSON returns a Core writing JSON entries to ws.
//
// It builds the downstream core itself, opened at the buffer floor, so the
// caller never has to open one by hand: use this rather than [New] unless you
// already have a downstream core to wrap. Because this package holds the only
// reference to that core, nothing can gate it shut afterwards and the mis-wiring
// [New] panics on cannot happen.
//
// encCfg is the zap encoder configuration, usually
// zap.NewProductionEncoderConfig(). There is no default worth guessing here, so
// unlike the slog side it is a parameter.
func NewJSON(ws zapcore.WriteSyncer, encCfg zapcore.EncoderConfig, opts ...Option) *Core {
	return newEncoded(zapcore.NewJSONEncoder(encCfg), ws, opts)
}

// NewConsole is [NewJSON] with zap's console encoding.
func NewConsole(ws zapcore.WriteSyncer, encCfg zapcore.EncoderConfig, opts ...Option) *Core {
	return newEncoded(zapcore.NewConsoleEncoder(encCfg), ws, opts)
}

// newEncoded builds a Core over a downstream this package constructs.
//
// The options resolve first, so the downstream core is opened at the final
// buffer floor. No Enabled probe: it is opened at the floor by construction,
// which is what New's probe checks for at runtime.
func newEncoded(enc zapcore.Encoder, ws zapcore.WriteSyncer, opts []Option) *Core {
	cfg := resolve(opts)
	return &Core{
		cfg:        cfg,
		downstream: zapcore.NewCore(enc, ws, cfg.bufferFloor),
		pool:       core.NewScopePool[core.Entry](cfg.capacity, cfg.postTripLimit),
	}
}

// New returns a Core wrapping downstream.
//
// Prefer [NewJSON] or [NewConsole] unless you already have a downstream core:
// they build one correctly opened and cannot be mis-wired.
//
// downstream must be constructed wide open, at or below the buffer floor:
// dllog owns the effective level, and a downstream that filters would discard
// exactly the replayed entries the package exists to deliver. New panics if
// downstream.Enabled reports the buffer floor disabled, because that is a
// wiring mistake in program setup with no sensible runtime recovery, and
// failing at construction is far kinder than silently losing every replay in
// production.
//
// New panics if downstream is nil, for the same reason.
func New(downstream zapcore.Core, opts ...Option) *Core {
	if downstream == nil {
		panic("dllog/zapadapter: New called with a nil downstream core")
	}

	cfg := resolve(opts)

	if !downstream.Enabled(cfg.bufferFloor) {
		panic(fmt.Sprintf(
			"dllog/zapadapter: downstream core has %v disabled; construct it wide open "+
				"(zapcore.NewCore(enc, ws, %v)) so replayed entries survive, "+
				"or use zapadapter.NewJSON/NewConsole to have this package build it for you",
			cfg.bufferFloor, cfg.bufferFloor))
	}

	return &Core{
		cfg:        cfg,
		downstream: downstream,
		pool:       core.NewScopePool[core.Entry](cfg.capacity, cfg.postTripLimit),
	}
}

// For returns a Core bound to the scope on ctx, sharing this Core's
// configuration, downstream and scope pool.
//
// This is the whole of the ergonomic difference from the slog handler. slog
// threads a context through every call site (logger.DebugContext(ctx, ...)), so
// its handler can find the scope per record. zapcore.Core.Check and Write take
// no context, so the binding must happen once, here, and the caller must carry
// the derived logger rather than the context.
//
// For returns the receiver unchanged when ctx carries no scope, so wrapping is
// always safe. The returned Core buffers into the same ring every other adapter
// on that context shares.
func (c *Core) For(ctx context.Context) *Core {
	cr := fromContext(ctx)
	if cr == nil {
		return c
	}
	return c.derive(c.downstream, cr)
}

// Enabled reports whether an entry at level should be produced. It is the cost
// model of the package, in three branches:
//
//   - No scope bound: the downstream core decides, so an unbound Debug call
//     stays the near-free no-op it would be without dllog.
//   - Scope bound, not yet tripped: true from the buffer floor up, because those
//     entries are candidates for the buffer.
//   - Scope bound, tripped: true from the buffer floor up, because they now pass
//     straight through.
//
// The last two coincide today; they are kept distinct because they answer
// different questions and only the tripped branch is bounded by the post-trip
// limit, which Write applies.
func (c *Core) Enabled(level zapcore.Level) bool {
	if c.carrier == nil {
		return c.downstream.Enabled(level)
	}
	return level >= c.cfg.bufferFloor
}

// Check adds this core to ce when the entry is enabled, as zapcore requires.
//
// It cannot consult a context: zapcore.Core.Check receives none. The scope was
// bound by For, which is why the decision here is a field read rather than a
// context lookup.
func (c *Core) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

// Write buffers, replays, or forwards ent according to the bound scope.
//
// Without a scope, ent reaches the downstream core when it is at or above the
// configured level and is dropped otherwise. Within a scope, an entry below the
// effective level is cloned into the ring, an entry at or above it is written
// immediately, and an entry at or above the trip level trips the scope, flushing
// the buffer to the downstream cores the buffered entries were logged through
// before ent itself is written. Everything happens synchronously, on the calling
// goroutine.
func (c *Core) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	if c.carrier == nil {
		if ent.Level < c.cfg.level {
			return nil
		}
		return c.downstream.Write(ent, fields)
	}

	if ent.Level >= c.cfg.tripLevel {
		// MarkTripped both flushes an existing ring and records the trip when
		// there is no ring yet, so entries logged after this failure pass
		// through either way.
		if s := c.carrier.MarkTripped(); s != nil {
			core.Flush(s, c.cfg.replayKey)
		}
		return c.downstream.Write(ent, fields)
	}

	// At or above the effective level the entry is written on the spot and
	// never buffered: the buffer withholds only what the level would have
	// silenced, so a scope that ends cleanly must not swallow an entry the
	// caller was owed. After a trip it draws on the same post-trip budget as
	// everything else. A trip racing this check can let one entry through
	// unbudgeted; that entry was writable either way.
	if ent.Level >= c.cfg.level {
		if c.carrier.Tripped() && c.carrier.Bind(c.pool).PassThrough() == core.ActionSuppressed {
			return nil
		}
		return c.downstream.Write(ent, fields)
	}

	sl := slot{entry: ent, fields: clone(fields), downstream: c.downstream}
	switch c.carrier.Bind(c.pool).Append(sl.coreEntry()) {
	case core.ActionBuffered, core.ActionSuppressed:
		return nil
	default:
		return c.downstream.Write(ent, fields)
	}
}

// With returns a Core whose entries carry fields, sharing this Core's
// configuration, scope binding and pool.
//
// The fields are applied eagerly to the downstream core, and each buffered
// entry travels with the downstream it was logged through, so a replay renders
// exactly the fields that were in effect at log time. Sibling cores derived
// from the same parent stay independent.
func (c *Core) With(fields []zapcore.Field) zapcore.Core {
	if len(fields) == 0 {
		return c
	}
	return c.derive(c.downstream.With(fields), c.carrier)
}

// Sync flushes the downstream core. It does not trip or discard the scope: a
// scope's lifetime is owned by the done func that opened it, never by a Sync.
func (c *Core) Sync() error { return c.downstream.Sync() }

// Trip flushes the bound scope immediately, replaying every buffered entry to
// the downstream core it was logged through. Use it for failures that are
// returned rather than logged, which is the common Go case.
//
// Trip is idempotent, and a no-op on an unbound Core, on an already-tripped
// scope, and on a released one. After it returns, entries down to the buffer
// floor pass straight through until the scope ends, whether or not anything had
// been buffered yet.
//
// It marks replayed entries with this Core's configured replay key, which is
// why it exists as a method rather than as a package-level function: a
// package-level Trip has no Core and cannot see [WithReplayKey].
func (c *Core) Trip() {
	if c.carrier == nil {
		return
	}
	if s := c.carrier.MarkTripped(); s != nil {
		core.Flush(s, c.cfg.replayKey)
	}
}

// derive builds a sibling Core over an already-derived downstream. Config and
// pool are shared: the scope an entry lands in is the one bound to the Core,
// never one owned by a particular derived core.
func (c *Core) derive(downstream zapcore.Core, cr *carrier) *Core {
	return &Core{
		cfg:        c.cfg,
		downstream: downstream,
		pool:       c.pool,
		carrier:    cr,
	}
}

// carrier is this adapter's handle on the shared scope carrier.
//
// The state it wraps lives in internal/core, together with the context key, so
// every adapter built on the engine binds the same scope on a given context.
// The wrapper exists only so this adapter can recover its own value from a
// context with pointer identity intact; it holds no state of its own.
type carrier struct{ *core.Carrier }

// fromContext returns the shared carrier ctx holds, wrapped for this adapter,
// or nil when ctx has no scope.
//
// It resolves through core.FromContext rather than looking for its own wrapper
// type, because the scope may have been opened by any adapter and would then
// carry that adapter's wrapper. Reaching the shared *core.Carrier under
// whichever wrapper is present is exactly what makes the adapters share one
// ring.
func fromContext(ctx context.Context) *carrier {
	shared := core.FromContext(ctx)
	if shared == nil {
		return nil
	}
	return &carrier{Carrier: shared}
}

// Scope opens a buffering scope on ctx and returns the derived context together
// with the function that releases it. It joins rather than nests, exactly as
// dllog.Scope does.
//
// The returned done must be called, normally with defer, exactly once per
// successful call; calling it more than once is safe and does nothing. A scope
// that ends without tripping discards its buffer.
//
// It stores this package's own wrapper, not the slog adapter's, and the two are
// interchangeable: both resolve through core.FromContext, so a scope opened here
// buffers slog records logged on the same context, and the reverse. That is what
// keeps this package free of any import of the root dllog package, so neither
// adapter is built on the other.
func Scope(ctx context.Context) (context.Context, func()) {
	if cr := fromContext(ctx); cr != nil {
		return ctx, func() {}
	}

	cr := &carrier{Carrier: &core.Carrier{}}
	var once sync.Once
	done := func() { once.Do(cr.Close) }
	return core.NewContext(ctx, cr), done
}

// clone copies fields so a buffered entry is not aliased by a caller that
// reuses its slice. zap does not promise the slice outlives the Write call,
// which is the same contract slog.Record.Clone answers for the other adapter.
func clone(fields []zapcore.Field) []zapcore.Field {
	if len(fields) == 0 {
		return nil
	}
	out := make([]zapcore.Field, len(fields))
	copy(out, fields)
	return out
}
