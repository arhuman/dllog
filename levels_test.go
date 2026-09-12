package dllog

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestAtLevelRecordEmittedInsideScope pins the core promise of the README: a
// scope changes what happens to below-level records only. A record the level
// would have emitted anyway is written on the spot and survives a clean scope.
func TestAtLevelRecordEmittedInsideScope(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	log.InfoContext(ctx, "at level")
	log.WarnContext(ctx, "above level")

	want := []string{"at level", "above level"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: at-level records must not wait for a trip", got, want)
	}

	done()
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages after done = %v, want %v", got, want)
	}
}

// TestAtLevelRecordNotDuplicatedByReplay asserts the replay contains only the
// withheld records: one already emitted at its own time must not appear twice.
func TestAtLevelRecordNotDuplicatedByReplay(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	defer done()

	log.DebugContext(ctx, "withheld")
	log.InfoContext(ctx, "emitted once")
	log.ErrorContext(ctx, "boom")

	want := []string{"emitted once", "withheld", "boom"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	for _, r := range c.snapshot() {
		if r.Message == "emitted once" && hasAttr(r, DefaultReplayKey, true) {
			t.Fatal("an at-level record was replayed; it was never buffered")
		}
	}
}

// TestPostTripBudgetCoversAtLevelRecords asserts the post-trip limit bounds the
// scope's whole output: after the trip, at-level records draw on the same
// budget as the below-level ones the trip unlocked.
func TestPostTripBudgetCoversAtLevelRecords(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo), WithPostTripLimit(1)))

	ctx, done := Scope(context.Background())
	defer done()

	log.ErrorContext(ctx, "boom") // trips; nothing buffered yet
	log.InfoContext(ctx, "first post-trip")
	log.InfoContext(ctx, "second post-trip")

	want := []string{"boom", "first post-trip"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: the budget must count at-level records too", got, want)
	}
}

// TestWithTripLevelTripsBelowDefault pins the option the review found untested:
// a Warn trip level replays the buffer on a Warn record, below the Error
// default.
func TestWithTripLevelTripsBelowDefault(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo), WithTripLevel(slog.LevelWarn)))

	ctx, done := Scope(context.Background())
	defer done()

	log.DebugContext(ctx, "withheld")
	log.WarnContext(ctx, "trips at warn")

	want := []string{"withheld", "trips at warn"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: WithTripLevel(Warn) must trip on Warn", got, want)
	}

	recs := c.snapshot()
	if !hasAttr(recs[0], DefaultReplayKey, true) {
		t.Error("the withheld record is not marked as a replay")
	}
	if hasAttr(recs[1], DefaultReplayKey, true) {
		t.Error("the triggering record must not be marked as a replay")
	}
}

// TestNewPanicsOnOutOfOrderLevels pins the construction-time validation: a
// configuration whose levels cannot cooperate is refused rather than accepted
// as a handler that can never buffer.
func TestNewPanicsOnOutOfOrderLevels(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"floor above level", []Option{WithBufferFloor(slog.LevelWarn), WithLevel(slog.LevelInfo)}},
		{"level above trip", []Option{WithLevel(slog.LevelError), WithTripLevel(slog.LevelWarn)}},
		{"floor above trip", []Option{WithBufferFloor(slog.LevelError), WithTripLevel(slog.LevelDebug)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			c := &capture{}
			New(c, tc.opts...)
		})
	}
}

// TestEnabledMatchesWhatHandleDoes pins Enabled to the emission contract: it
// must not overreport, because slog.Logger builds the full record whenever
// Enabled says true, and an out-of-scope below-level record is built only to be
// thrown away in Handle. The downstream is deliberately wide open (New requires
// it), so a delegating Enabled would answer true for everything above the floor.
func TestEnabledMatchesWhatHandleDoes(t *testing.T) {
	h := New(&capture{}, WithLevel(slog.LevelInfo))

	noScope := context.Background()
	scoped, done := Scope(context.Background())
	defer done()

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		level slog.Level
		want  bool
	}{
		{"no scope, below level", noScope, slog.LevelDebug, false},
		{"no scope, at level", noScope, slog.LevelInfo, true},
		{"no scope, above level", noScope, slog.LevelError, true},
		{"scoped, buffer band", scoped, slog.LevelDebug, true},
		{"scoped, at level", scoped, slog.LevelInfo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.Enabled(tc.ctx, tc.level); got != tc.want {
				t.Fatalf("Enabled(%v) = %v, want %v", tc.level, got, tc.want)
			}
		})
	}
}

// A record below the buffer floor is refused everywhere: out of scope it is
// below the level, and in scope it is below what the buffer keeps.
func TestEnabledRefusesBelowTheFloor(t *testing.T) {
	h := New(&capture{}, WithBufferFloor(slog.LevelInfo), WithLevel(slog.LevelInfo))

	scoped, done := Scope(context.Background())
	defer done()

	if h.Enabled(scoped, slog.LevelDebug) {
		t.Fatal("Enabled(Debug) = true with an Info buffer floor: below-floor records are never kept")
	}
}
