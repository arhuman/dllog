package zapadapter_test

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"

	"github.com/arhuman/dllog"
	"github.com/arhuman/dllog/zapadapter"
)

// TestAtLevelEntryWrittenInsideScope pins the same promise as the slog side: a
// scope withholds only below-level entries, so an Info entry at the default
// level is written on the spot and survives a clean scope.
func TestAtLevelEntryWrittenInsideScope(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s))

	ctx, done := dllog.Scope(context.Background())
	log := newLogger(base.For(ctx))
	log.Info("at level")
	done()

	want := []string{"at level"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: at-level entries must not wait for a trip", got, want)
	}
}

// TestWithTripLevelTripsBelowDefault pins the option the review found untested:
// a Warn trip level replays the buffer on a Warn entry, below the Error default.
func TestWithTripLevelTripsBelowDefault(t *testing.T) {
	s := &sink{}
	base := zapadapter.New(newZapSink(s), zapadapter.WithTripLevel(zapcore.WarnLevel))

	ctx, done := dllog.Scope(context.Background())
	defer done()

	log := newLogger(base.For(ctx))
	log.Debug("withheld")
	log.Warn("trips at warn")

	want := []string{"withheld[replay]", "trips at warn"}
	if got := s.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: WithTripLevel(Warn) must trip on Warn", got, want)
	}
}

// TestNewPanicsOnOutOfOrderLevels mirrors the slog-side validation: levels that
// cannot cooperate are refused at construction.
func TestNewPanicsOnOutOfOrderLevels(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic on out-of-order levels")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", r)
		}
		if !strings.Contains(msg, "levels out of order") {
			t.Errorf("panic message is not actionable: %v", msg)
		}
	}()
	s := &sink{}
	zapadapter.New(newZapSink(s),
		zapadapter.WithBufferFloor(zapcore.ErrorLevel),
		zapadapter.WithTripLevel(zapcore.DebugLevel))
}
