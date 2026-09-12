package zapadapter_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/arhuman/dllog"
	"github.com/arhuman/dllog/zapadapter"
)

// sink is an ordered, concurrency-safe record of messages, so a test can put
// zap output and slog output into one stream and assert the true interleaving.
type sink struct {
	mu  sync.Mutex
	out []string
}

func (s *sink) add(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append(s.out, msg)
}

func (s *sink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.out...)
}

// zapSink is a wide-open zapcore.Core reporting each entry's message, plus any
// replay or dropped field, to a shared sink.
type zapSink struct {
	sink   *sink
	fields []zapcore.Field
}

func newZapSink(s *sink) *zapSink { return &zapSink{sink: s} }

func (z *zapSink) Enabled(zapcore.Level) bool { return true }

func (z *zapSink) With(fields []zapcore.Field) zapcore.Core {
	return &zapSink{sink: z.sink, fields: append(append([]zapcore.Field{}, z.fields...), fields...)}
}

func (z *zapSink) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(ent, z)
}

func (z *zapSink) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	z.sink.add(render(ent.Message, append(append([]zapcore.Field{}, z.fields...), fields...)))
	return nil
}

func (z *zapSink) Sync() error { return nil }

// render suffixes the message with the marker fields a test asserts on, so the
// sink stays a plain []string and comparisons read cleanly.
func render(msg string, fields []zapcore.Field) string {
	for _, f := range fields {
		switch f.Key {
		case zapadapter.DefaultReplayKey, "rk":
			msg += "[replay]"
		case zapadapter.DroppedKey:
			msg += "[dropped=" + itoa(f.Integer) + "]"
		}
	}
	return msg
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// slogSink is a wide-open slog.Handler reporting each record to the same sink,
// so the cross-adapter tests can compare one ordered stream.
type slogSink struct{ sink *sink }

func (h *slogSink) Enabled(context.Context, slog.Level) bool { return true }

func (h *slogSink) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == dllog.DefaultReplayKey {
			msg += "[replay]"
		}
		return true
	})
	h.sink.add(msg)
	return nil
}

func (h *slogSink) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *slogSink) WithGroup(string) slog.Handler      { return h }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newLogger returns a zap logger over base bound to nothing, and the base Core
// so a test can bind it to a context with For.
func newLogger(c *zapadapter.Core) *zap.Logger {
	return zap.New(c)
}

// ---------------------------------------------------------------------------
// The tests that decide this phase: one scope, two adapters, one ordered sink.
// ---------------------------------------------------------------------------

// The requirement this phase exists for, in the zap direction: an Error logged
// through the zap adapter replays what the SLOG handler buffered on the same
// context, interleaved in log order.
func TestZapErrorReplaysSlogBufferInOrder(t *testing.T) {
	s := &sink{}
	slogLog := slog.New(dllog.New(&slogSink{sink: s}))
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	zapLog := newLogger(base.For(ctx))

	slogLog.DebugContext(ctx, "1-slog")
	zapLog.Debug("2-zap")
	slogLog.DebugContext(ctx, "3-slog")
	zapLog.Debug("4-zap")

	zapLog.Error("5-zap-boom")

	want := []string{
		"1-slog[replay]", "2-zap[replay]", "3-slog[replay]", "4-zap[replay]",
		"5-zap-boom",
	}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("cross-adapter replay order = %v, want %v: a zap trip must replay "+
			"BOTH adapters' entries in the order they were logged", got, want)
	}
}

// And the reverse: an Error logged through the SLOG handler replays what the
// zap adapter buffered.
func TestSlogErrorReplaysZapBufferInOrder(t *testing.T) {
	s := &sink{}
	slogLog := slog.New(dllog.New(&slogSink{sink: s}))
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	zapLog := newLogger(base.For(ctx))

	zapLog.Debug("1-zap")
	slogLog.DebugContext(ctx, "2-slog")
	zapLog.Debug("3-zap")
	slogLog.DebugContext(ctx, "4-slog")

	slogLog.ErrorContext(ctx, "5-slog-boom")

	want := []string{
		"1-zap[replay]", "2-slog[replay]", "3-zap[replay]", "4-slog[replay]",
		"5-slog-boom",
	}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("cross-adapter replay order = %v, want %v: a slog trip must replay "+
			"BOTH adapters' entries in the order they were logged", got, want)
	}
}

// A scope opened by the zap adapter must be equally visible to the slog
// handler: the sharing is a property of the core's context key, not of which
// package opened the scope.
func TestScopeOpenedByZapIsSharedWithSlog(t *testing.T) {
	s := &sink{}
	slogLog := slog.New(dllog.New(&slogSink{sink: s}))
	base := zapadapter.New(newZapSink(s))

	ctx, done := zapadapter.Scope(context.Background())
	defer done()

	zapLog := newLogger(base.For(ctx))
	zapLog.Debug("1-zap")
	slogLog.DebugContext(ctx, "2-slog")

	dllog.Trip(ctx)

	want := []string{"1-zap[replay]", "2-slog[replay]"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: a scope opened through zapadapter.Scope "+
			"must be the same scope dllog sees", got, want)
	}
}

// The package-level dllog.Trip must reach entries buffered by zap.
func TestPackageTripFlushesZapBuffer(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	zapLog := newLogger(base.For(ctx))
	zapLog.Debug("buffered")

	dllog.Trip(ctx)

	if got, want := s.messages(), []string{"buffered[replay]"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Adapter semantics
// ---------------------------------------------------------------------------

func TestCleanScopeDiscardsBuffer(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	zapLog := newLogger(base.For(ctx))
	zapLog.Debug("never seen")
	zapLog.Info("seen immediately")
	done()

	// The Debug entry is buffered and discarded; the Info entry is at the
	// effective level, so the scope must not have withheld it.
	want := []string{"seen immediately"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: a clean scope discards its buffer "+
			"but never an at-level entry", got, want)
	}
}

func TestUnboundCoreIsAPlainLevelFilter(t *testing.T) {
	s := &sink{}
	log := newLogger(zapadapter.New(newZapSink(s), zapadapter.WithLevel(zapcore.InfoLevel)))

	log.Debug("dropped")
	log.Info("kept")
	log.Error("kept too")

	if got, want := s.messages(), []string{"kept", "kept too"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

// For on a context with no scope must return a usable Core rather than nil or a
// panic, so wrapping is unconditionally safe at a call site.
func TestForWithoutScopeIsSafe(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	log := newLogger(base.For(context.Background()))
	log.Debug("dropped")
	log.Info("kept")

	if got, want := s.messages(), []string{"kept"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func TestEvictionEmitsDroppedCount(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s), zapadapter.WithCapacity(2))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	log := newLogger(base.For(ctx))
	log.Debug("a")
	log.Debug("b")
	log.Debug("c")
	log.Debug("d")
	log.Error("boom")

	want := []string{
		"dllog: buffered records dropped[dropped=2]",
		"c[replay]", "d[replay]", "boom",
	}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: eviction must announce the dropped count "+
			"before the surviving batch", got, want)
	}
}

func TestTripIsIdempotent(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	bound := base.For(ctx)
	log := newLogger(bound)
	log.Debug("buffered")

	bound.Trip()
	bound.Trip()
	bound.Trip()
	log.Error("boom")

	want := []string{"buffered[replay]", "boom"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: a second trip must not re-flush", got, want)
	}
}

func TestTripOnUnboundCoreIsNoOp(t *testing.T) {
	s := &sink{}
	zapadapter.New(newZapSink(s)).Trip()

	if got := s.messages(); len(got) != 0 {
		t.Fatalf("messages = %v, want none", got)
	}
}

// A trip that lands before anything was buffered must still put the scope in
// the pass-through state, or the entries that follow the failure would be
// buffered and then silently discarded at done().
func TestTripBeforeAnyEntryPassesThrough(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	bound := base.For(ctx)
	bound.Trip()
	newLogger(bound).Debug("after the trip")

	if got, want := s.messages(), []string{"after the trip"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func TestPostTripLimitBoundsPassThrough(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s), zapadapter.WithPostTripLimit(2))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	bound := base.For(ctx)
	log := newLogger(bound)
	log.Debug("buffered")
	bound.Trip()

	log.Debug("post-1")
	log.Debug("post-2")
	log.Debug("post-3")
	log.Debug("post-4")

	want := []string{"buffered[replay]", "post-1", "post-2"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: the post-trip budget must suppress the rest", got, want)
	}
}

// The three Enabled branches of the cost model.
func TestEnabledBranches(t *testing.T) {
	base := zapadapter.New(newZapSink(&sink{}), zapadapter.WithBufferFloor(zapcore.DebugLevel))

	t.Run("no scope gates at the effective level", func(t *testing.T) {
		// The sink is wide open, so a delegating Enabled would answer true for
		// Debug and zap would build a CheckedEntry that Write throws away. The
		// Core answers from its own level instead: below it refused, at or
		// above it produced.
		if base.Enabled(zapcore.DebugLevel) {
			t.Fatal("unbound Enabled(Debug) = true, want false: the effective level decides, not the wide-open downstream")
		}
		if !base.Enabled(zapcore.InfoLevel) {
			t.Fatal("unbound Enabled(Info) = false, want true")
		}
	})

	ctx, done := dllog.Scope(context.Background())
	defer done()
	bound := base.For(ctx)

	t.Run("scope untripped is enabled to the floor", func(t *testing.T) {
		if !bound.Enabled(zapcore.DebugLevel) {
			t.Fatal("bound Enabled(Debug) = false, want true at the buffer floor")
		}
	})

	t.Run("scope tripped is enabled to the floor", func(t *testing.T) {
		bound.Trip()
		if !bound.Enabled(zapcore.DebugLevel) {
			t.Fatal("tripped Enabled(Debug) = false, want true at the buffer floor")
		}
	})
}

// A buffer floor above Debug must disable below-floor entries inside a scope.
func TestBufferFloorDisablesBelowFloorInScope(t *testing.T) {
	base := zapadapter.New(newZapSink(&sink{}), zapadapter.WithBufferFloor(zapcore.InfoLevel))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	bound := base.For(ctx)
	if bound.Enabled(zapcore.DebugLevel) {
		t.Fatal("Enabled(Debug) = true below a WithBufferFloor(Info); want false")
	}
	if !bound.Enabled(zapcore.InfoLevel) {
		t.Fatal("Enabled(Info) = false at the buffer floor; want true")
	}
}

// Check must gate on the same three branches Enabled does, since that is the
// path zap actually takes for a logger call.
func TestCheckGatesEntries(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s), zapadapter.WithBufferFloor(zapcore.InfoLevel))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	log := newLogger(base.For(ctx))
	log.Debug("below the floor")
	log.Info("at the floor")
	log.Error("boom")

	// The Info entry sits at the effective level, so it is written on the spot
	// rather than buffered, and the trip finds nothing to replay.
	want := []string{"at the floor", "boom"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: Check must drop below-floor entries", got, want)
	}
}

// With must apply fields eagerly to the downstream, and a buffered entry must
// replay through the derived downstream it was logged with.
func TestWithFieldsSurviveReplay(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	log := newLogger(base.For(ctx))
	// The sink renders only marker keys, so this asserts the derived core is
	// used at replay time by checking the replay marker still lands.
	log.With(zap.String("req", "abc")).Debug("buffered")
	log.Error("boom")

	if got, want := s.messages(), []string{"buffered[replay]", "boom"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func TestWithNoFieldsReturnsReceiver(t *testing.T) {
	base := zapadapter.New(newZapSink(&sink{}))
	if base.With(nil) != zapcore.Core(base) {
		t.Fatal("With(nil) returned a new core, want the receiver")
	}
}

func TestWithReplayKeyIsHonored(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s), zapadapter.WithReplayKey("rk"))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	bound := base.For(ctx)
	newLogger(bound).Debug("buffered")
	bound.Trip()

	// The sink renders "rk" as [replay] too, so a miss here means no marker at
	// all was added under the configured key.
	if got, want := s.messages(), []string{"buffered[replay]"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v: WithReplayKey must reach the flush", got, want)
	}
}

func TestScopeJoinsRatherThanNests(t *testing.T) {
	outer, outerDone := zapadapter.Scope(context.Background())
	defer outerDone()

	inner, innerDone := zapadapter.Scope(outer)
	innerDone() // must be a no-op

	s := &sink{}
	base := zapadapter.New(newZapSink(s))
	newLogger(base.For(inner)).Debug("buffered")
	dllog.Trip(outer)

	if got, want := s.messages(), []string{"buffered[replay]"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v: the inner done must not close the outer scope", got, want)
	}
}

func TestSyncReachesDownstream(t *testing.T) {
	base := zapadapter.New(newZapSink(&sink{}))
	if err := base.Sync(); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestNewPanicsOnNilDownstream(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(nil) did not panic")
		}
	}()
	zapadapter.New(nil)
}

// A downstream that filters would swallow exactly the replays this package
// exists to deliver, so the wiring mistake must fail at construction.
func TestNewPanicsOnFilteringDownstream(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with an Info-gated downstream did not panic")
		}
	}()
	zapadapter.New(&levelCore{min: zapcore.InfoLevel})
}

// levelCore is a downstream that refuses entries below min, the wiring mistake
// New must reject.
type levelCore struct{ min zapcore.Level }

func (c *levelCore) Enabled(l zapcore.Level) bool { return l >= c.min }
func (c *levelCore) With([]zapcore.Field) zapcore.Core {
	return c
}

func (c *levelCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce
}
func (c *levelCore) Write(zapcore.Entry, []zapcore.Field) error { return nil }
func (c *levelCore) Sync() error                                { return nil }

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// Both adapters logging into one scope from many goroutines, with a concurrent
// trip, must not race and must not lose or duplicate an entry.
func TestConcurrentCrossAdapterLogging(t *testing.T) {
	s := &sink{}
	slogLog := slog.New(dllog.New(&slogSink{sink: s}))
	base := zapadapter.New(newZapSink(s), zapadapter.WithCapacity(64))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	bound := base.For(ctx)
	zapLog := newLogger(bound)

	const n = 32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		go func() { defer wg.Done(); zapLog.Debug("zap") }()
		go func() { defer wg.Done(); slogLog.DebugContext(ctx, "slog") }()
		if i == n/2 {
			wg.Add(1)
			go func() { defer wg.Done(); bound.Trip() }()
		}
	}
	wg.Wait()

	// The exact count is nondeterministic (entries buffered before the trip are
	// replayed, entries after pass through), but nothing may be lost beyond the
	// ring's own eviction, and the run must be race-free.
	if got := len(s.messages()); got == 0 {
		t.Fatal("no entries reached the sink under concurrency")
	}
}

func TestConcurrentForOnOneContext(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	const n = 16
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			newLogger(base.For(ctx)).Debug("buffered")
		}()
	}
	wg.Wait()

	dllog.Trip(ctx)

	if got := len(s.messages()); got != n {
		t.Fatalf("replayed %d entries, want %d: concurrent For calls must all bind "+
			"the same ring", got, n)
	}
}

// countingCore records how often Check and Write were called, so a test can
// prove the adapter routes emission through the downstream's Check rather than
// calling its Write directly. Everything a real core does in Check (sampling,
// tee fan-out, conditional routing) is skipped by a direct Write.
type countingCore struct {
	mu     sync.Mutex
	checks int
	writes int
	sink   *sink
}

func (c *countingCore) Enabled(zapcore.Level) bool { return true }

func (c *countingCore) With([]zapcore.Field) zapcore.Core { return c }

func (c *countingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	c.mu.Lock()
	c.checks++
	c.mu.Unlock()
	return ce.AddCore(ent, c)
}

func (c *countingCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	c.sink.add(render(ent.Message, fields))
	return nil
}

func (c *countingCore) Sync() error { return nil }

func (c *countingCore) counts() (checks, writes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checks, c.writes
}

// Every entry the adapter emits must reach the downstream through its Check.
// New accepts an arbitrary zapcore.Core, so a downstream whose behaviour lives
// in Check is a legal wiring, not an exotic one.
func TestEmissionRoutesThroughDownstreamCheck(t *testing.T) {
	tests := []struct {
		name string
		log  func(ctx context.Context, logger *zap.Logger)
	}{
		{
			name: "unbound above level",
			log: func(_ context.Context, logger *zap.Logger) {
				logger.Info("plain")
			},
		},
		{
			name: "replayed from the buffer",
			log: func(_ context.Context, logger *zap.Logger) {
				logger.Debug("buffered")
				logger.Error("boom")
			},
		},
		{
			name: "at level inside a scope",
			log: func(_ context.Context, logger *zap.Logger) {
				logger.Info("in scope")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			down := &countingCore{sink: &sink{}}
			base := zapadapter.New(down, zapadapter.WithLevel(zapcore.InfoLevel))

			ctx, done := zapadapter.Scope(context.Background())
			defer done()

			tt.log(ctx, zap.New(base.For(ctx)))

			checks, writes := down.counts()
			if writes == 0 {
				t.Fatal("nothing reached the downstream")
			}
			if checks != writes {
				t.Fatalf("downstream checks = %d, writes = %d: every write must be preceded by its own Check", checks, writes)
			}
		})
	}
}

// The eviction notice is emitted by the adapter itself rather than by a caller,
// so it is the path most likely to keep bypassing Check after the others are
// fixed.
func TestDroppedNoticeRoutesThroughDownstreamCheck(t *testing.T) {
	down := &countingCore{sink: &sink{}}
	base := zapadapter.New(down,
		zapadapter.WithLevel(zapcore.InfoLevel),
		zapadapter.WithCapacity(1),
	)

	ctx, done := zapadapter.Scope(context.Background())
	defer done()

	logger := zap.New(base.For(ctx))
	logger.Debug("first")
	logger.Debug("second") // evicts "first"
	logger.Error("boom")

	checks, writes := down.counts()
	if checks != writes {
		t.Fatalf("downstream checks = %d, writes = %d: the dropped notice must go through Check too", checks, writes)
	}
}

// A sampler implements its whole contract in Check and inherits an unsampled
// Write, so an adapter that calls Write directly silently disables sampling.
func TestDownstreamSamplerIsHonoured(t *testing.T) {
	s := &sink{}
	sampled := zapcore.NewSamplerWithOptions(newZapSink(s), time.Minute, 1, 0)
	base := zapadapter.New(sampled, zapadapter.WithLevel(zapcore.InfoLevel))

	logger := zap.New(base)
	for range 5 {
		logger.Info("repeated")
	}

	if got := len(s.messages()); got != 1 {
		t.Fatalf("downstream received %d entries, want 1: the sampler's first-1-thereafter-0 policy must apply", got)
	}
}

// The zap adapter must bound a post-trip error storm exactly as the slog
// handler does; the two adapters are peers and cannot differ on what an option
// means.
func TestPostTripLimitBoundsTripLevelEntries(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s),
		zapadapter.WithLevel(zapcore.InfoLevel),
		zapadapter.WithPostTripLimit(1),
	)

	ctx, done := zapadapter.Scope(context.Background())
	defer done()

	logger := zap.New(base.For(ctx))
	logger.Debug("seed")
	logger.Error("trigger") // trips, exempt from the budget
	logger.Error("second")  // spends the budget
	logger.Error("third")   // suppressed

	want := []string{"seed[replay]", "trigger", "second"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: errors after the trip must draw on the post-trip budget", got, want)
	}
}

// A released scope makes a bound Core behave as an unbound one: the entry is
// governed by the configured level, not buffered into a scope that is gone.
func TestReleasedScopeFiltersLateEntries(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s), zapadapter.WithLevel(zapcore.InfoLevel))

	ctx, done := zapadapter.Scope(context.Background())
	logger := zap.New(base.For(ctx))
	logger.Debug("buffered")
	done()

	logger.Debug("late")

	if got := s.messages(); len(got) != 0 {
		t.Fatalf("messages = %v, want none: an entry logged after done() must be filtered, not written", got)
	}
}

// Releasing a scope while other goroutines still log through a bound Core is a
// real shutdown race. A below-level entry that races done() must be dropped,
// exactly as the slog handler drops it: with no live scope, the level decides,
// and Debug is below it.
//
// This is the zap mirror of the root package's released-scope race test; the
// two adapters shipped divergent answers to this exact window once, which is
// why both sides now pin it.
func TestReleasedScopeRaceWithLateLogger(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s), zapadapter.WithLevel(zapcore.InfoLevel))

	ctx, done := zapadapter.Scope(context.Background())
	logger := zap.New(base.For(ctx))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				logger.Debug("concurrent")
			}
		}()
	}
	done()
	wg.Wait()

	// The scope never tripped, so every buffered entry was discarded with it
	// and nothing below the level may have reached the downstream.
	if got := s.messages(); len(got) != 0 {
		t.Fatalf("%d below-level entries reached the downstream during shutdown: %v", len(got), got[:min(3, len(got))])
	}
}
