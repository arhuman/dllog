package zapadapter_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/arhuman/dllog/zapadapter"
)

// nopSync adapts a buffer to zapcore.WriteSyncer.
type nopSync struct{ *bytes.Buffer }

func (nopSync) Sync() error { return nil }

func encCfg() zapcore.EncoderConfig {
	c := zap.NewProductionEncoderConfig()
	c.TimeKey = "" // keep assertions about content stable
	return c
}

// TestOwnedCoresBufferAndReplay is the headline claim on the zap side: the
// constructor builds its own downstream and the buffer still replays on trip.
func TestOwnedCoresBufferAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(zapcore.WriteSyncer) *zapadapter.Core
	}{
		{"NewJSON", func(ws zapcore.WriteSyncer) *zapadapter.Core {
			return zapadapter.NewJSON(ws, encCfg())
		}},
		{"NewConsole", func(ws zapcore.WriteSyncer) *zapadapter.Core {
			return zapadapter.NewConsole(ws, encCfg())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			base := tc.make(nopSync{&buf})

			ctx, done := zapadapter.Scope(context.Background())
			defer done()

			log := zap.New(base.For(ctx))
			log.Debug("buffered detail")
			if buf.Len() != 0 {
				t.Fatalf("Debug emitted before the trip: %q", buf.String())
			}

			log.Error("boom")

			out := buf.String()
			if !strings.Contains(out, "buffered detail") {
				t.Fatalf("replay missing: %q", out)
			}
			if strings.Index(out, "buffered detail") > strings.Index(out, "boom") {
				t.Fatalf("replay came after the trigger: %q", out)
			}
		})
	}
}

// TestOwnedCoreDiscardsOnSuccess pins the other half: no trip, no output.
func TestOwnedCoreDiscardsOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	base := zapadapter.NewJSON(nopSync{&buf}, encCfg())

	ctx, done := zapadapter.Scope(context.Background())
	zap.New(base.For(ctx)).Debug("buffered detail")
	done()

	if buf.Len() != 0 {
		t.Fatalf("a scope that did not trip emitted %q, want nothing", buf.String())
	}
}

// TestOwnedCoreUnboundIsAPlainFilter asserts the effective level still applies
// on an unbound core: Debug dropped, Info emitted.
func TestOwnedCoreUnboundIsAPlainFilter(t *testing.T) {
	var buf bytes.Buffer
	log := zap.New(zapadapter.NewJSON(nopSync{&buf}, encCfg()))

	log.Debug("dropped")
	log.Info("emitted")

	out := buf.String()
	if strings.Contains(out, "dropped") {
		t.Errorf("Debug emitted outside a scope: %q", out)
	}
	if !strings.Contains(out, "emitted") {
		t.Errorf("Info not emitted: %q", out)
	}
}

// TestOwnedCoreHonoursBufferFloor proves the resolved floor reaches the
// downstream this package builds, rather than a hardcoded Debug.
func TestOwnedCoreHonoursBufferFloor(t *testing.T) {
	var buf bytes.Buffer
	base := zapadapter.NewJSON(nopSync{&buf}, encCfg(),
		zapadapter.WithBufferFloor(zapcore.WarnLevel),
		zapadapter.WithLevel(zapcore.ErrorLevel))

	ctx, done := zapadapter.Scope(context.Background())
	defer done()

	log := zap.New(base.For(ctx))
	log.Info("below the floor")
	log.Error("boom")

	out := buf.String()
	if strings.Contains(out, "below the floor") {
		t.Errorf("a sub-floor entry was buffered and replayed: %q", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("trigger missing: %q", out)
	}
}
