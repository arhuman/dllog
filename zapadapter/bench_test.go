package zapadapter_test

import (
	"context"
	"io"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/arhuman/dllog/zapadapter"
)

// discardCore returns a real JSON core writing to io.Discard, opened at level.
// A real encoder rather than a counting stub, so the replay benchmark includes
// the work a deployment pays on flush; the level-gated benchmarks never reach
// it, so the stub would buy them nothing.
func discardCore(level zapcore.Level) zapcore.Core {
	return zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(io.Discard),
		level,
	)
}

// BenchmarkUnboundDebug settles the zap side of the headline claim: an unbound
// Debug call costs what a disabled zap call costs.
//
// The honest baseline is zap-only-disabled: a logger over an Info-gated core,
// where Debug is refused in Check before an entry is built. The adapter's
// Enabled answers from its own effective level, so an unbound Debug is refused
// the same way; no context lookup is involved on this side, since the scope is
// bound by For rather than carried per call.
func BenchmarkUnboundDebug(b *testing.B) {
	b.Run("zap-only-disabled", func(b *testing.B) {
		logger := zap.New(discardCore(zapcore.InfoLevel))
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("cache lookup", zap.String("key", "u-1"))
		}
	})

	b.Run("dllog-unbound", func(b *testing.B) {
		// The downstream must be wide open for New to accept it; the level
		// gate lives on the adapter, whose Enabled refuses this Debug before
		// zap builds a CheckedEntry.
		logger := zap.New(zapadapter.New(discardCore(zapcore.DebugLevel),
			zapadapter.WithLevel(zapcore.InfoLevel)))
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("cache lookup", zap.String("key", "u-1"))
		}
	})
}

// BenchmarkBoundDebugAppend is the buffering hot path: a bound Core cloning a
// below-level entry into the shared ring. The scope is opened once, outside
// the timed region, because the claim is about the per-entry cost of an open
// scope, not about scope setup.
func BenchmarkBoundDebugAppend(b *testing.B) {
	base := zapadapter.New(discardCore(zapcore.DebugLevel),
		zapadapter.WithLevel(zapcore.InfoLevel))

	ctx, done := zapadapter.Scope(context.Background())
	defer done()
	logger := zap.New(base.For(ctx))

	b.ReportAllocs()
	for b.Loop() {
		logger.Debug("cache lookup", zap.String("key", "u-1"))
	}
}

// BenchmarkTripReplay measures the flush path: fill a scope with buffered
// entries, then log an Error that trips it and replays the batch through the
// real JSON encoder, downstream Check included. The cost of a failure, paid
// once per failing operation.
func BenchmarkTripReplay(b *testing.B) {
	base := zapadapter.New(discardCore(zapcore.DebugLevel),
		zapadapter.WithLevel(zapcore.InfoLevel))

	b.ReportAllocs()
	for b.Loop() {
		ctx, done := zapadapter.Scope(context.Background())
		logger := zap.New(base.For(ctx))
		for i := range 16 {
			logger.Debug("step", zap.Int("i", i))
		}
		logger.Error("operation failed", zap.String("err", "boom"))
		done()
	}
}
