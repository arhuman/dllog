package zapadapter_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
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

// keySink records the replay key each record was actually marked with, rather
// than collapsing every key to one marker the way the shared sinks do. Asserting
// on the key is the whole point here, so the test carries its own sinks.
type keySink struct {
	mu  sync.Mutex
	out []string
}

func (k *keySink) add(msg string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.out = append(k.out, msg)
}

func (k *keySink) messages() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.out...)
}

// keySlogSink is a wide-open slog handler suffixing each message with the
// replay key it carries, if any.
type keySlogSink struct{ sink *keySink }

func (h *keySlogSink) Enabled(context.Context, slog.Level) bool { return true }

func (h *keySlogSink) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			msg += "[" + a.Key + "]"
		}
		return true
	})
	h.sink.add(msg)
	return nil
}

func (h *keySlogSink) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *keySlogSink) WithGroup(string) slog.Handler      { return h }

// keyZapSink is the zap equivalent.
type keyZapSink struct{ sink *keySink }

func (z *keyZapSink) Enabled(zapcore.Level) bool        { return true }
func (z *keyZapSink) With([]zapcore.Field) zapcore.Core { return z }
func (z *keyZapSink) Sync() error                       { return nil }

func (z *keyZapSink) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(ent, z)
}

func (z *keyZapSink) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	msg := ent.Message
	for _, f := range fields {
		if f.Type == zapcore.BoolType && f.Integer == 1 {
			msg += "[" + f.Key + "]"
		}
	}
	z.sink.add(msg)
	return nil
}

// TestCrossAdapterTripKeepsOriginKeys pins the second bug ADR 0001 fixes: the
// two adapters share one ring, and under the old flush-time key whichever
// adapter raised the trip stamped its key on the other's buffered records.
// Each record must carry the key of the adapter that logged it, whoever trips.
func TestCrossAdapterTripKeepsOriginKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tripZap bool
	}{
		{"tripped through zap", true},
		{"tripped through slog", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &keySink{}
			slogLog := slog.New(dllog.New(&keySlogSink{sink: s}, dllog.WithReplayKey("slog_key")))
			base := zapadapter.New(&keyZapSink{sink: s}, zapadapter.WithReplayKey("zap_key"))

			ctx, done := dllog.Scope(context.Background())
			defer done()

			zapLog := zap.New(base.For(ctx))
			slogLog.DebugContext(ctx, "from-slog")
			zapLog.Debug("from-zap")

			if tc.tripZap {
				zapLog.Error("boom")
			} else {
				slogLog.ErrorContext(ctx, "boom")
			}

			want := []string{"from-slog[slog_key]", "from-zap[zap_key]", "boom"}
			if got := s.messages(); !equal(got, want) {
				t.Fatalf("messages = %v, want %v: each record must keep its origin "+
					"adapter's replay key regardless of which adapter tripped", got, want)
			}
		})
	}
}
