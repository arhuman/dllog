package dllog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
)

// openJSON returns a JSON handler wide open at Debug, plus the buffer it writes
// to. Wide open is what New requires of a downstream.
func openJSON(buf *bytes.Buffer) slog.Handler {
	return slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
}

// record is one decoded downstream line.
type record map[string]any

// sink is the shared, ordered storage every derived capture writes into, so a
// test sees one timeline regardless of which derived handler emitted a record.
type sink struct {
	mu      sync.Mutex
	records []slog.Record
}

// capture is a downstream that keeps every record it is handed, so tests can
// assert on order, level, time and attrs without parsing text. Derived captures
// share their root's sink but carry their own accumulated attrs and groups.
type capture struct {
	shared *sink
	attrs  []slog.Attr
	groups []string
}

// out returns the capture's sink, creating the root's on first use.
func (c *capture) out() *sink {
	if c.shared == nil {
		c.shared = &sink{}
	}
	return c.shared
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	s := c.out()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Record the emitting handler's attrs alongside the record so a test can
	// prove the replay travelled through the right derived downstream.
	stored := r.Clone()
	stored.AddAttrs(c.attrs...)
	s.records = append(s.records, stored)
	return nil
}

func (c *capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Sibling isolation depends on copying rather than appending in place.
	return &capture{
		shared: c.out(),
		attrs:  append(append([]slog.Attr{}, c.attrs...), attrs...),
		groups: append([]string{}, c.groups...),
	}
}

func (c *capture) WithGroup(name string) slog.Handler {
	return &capture{
		shared: c.out(),
		attrs:  append([]slog.Attr{}, c.attrs...),
		groups: append(append([]string{}, c.groups...), name),
	}
}

// snapshot returns a copy of the captured records, in order.
func (c *capture) snapshot() []slog.Record {
	s := c.out()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slog.Record{}, s.records...)
}

// messages returns the captured messages in order.
func (c *capture) messages() []string {
	recs := c.snapshot()
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Message
	}
	return out
}

// TestSlogtestConformance is the highest-value test in the package: the
// standard harness drives every corner of the slog.Handler contract, including
// the empty-record and WithGroup cases that handler wrappers routinely break.
//
// It runs out of scope, which is the path a conformant handler must satisfy:
// in-scope records are deliberately withheld until a trip, which is not what
// the harness expects of a handler.
func TestSlogtestConformance(t *testing.T) {
	var buf bytes.Buffer
	h := New(openJSON(&buf), WithLevel(slog.LevelDebug))

	results := func() []map[string]any {
		var out []map[string]any
		for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatalf("unmarshal %q: %v", line, err)
			}
			out = append(out, m)
		}
		return out
	}

	if err := slogtest.TestHandler(h, results); err != nil {
		t.Fatalf("slogtest.TestHandler: %v", err)
	}
}

func TestEnabledThreeBranches(t *testing.T) {
	// Downstream gated at Info, so the no-scope branch has something to
	// delegate to that differs from the in-scope answer.
	var buf bytes.Buffer
	gated := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := New(gated, WithLevel(slog.LevelInfo))

	t.Run("no scope delegates to downstream", func(t *testing.T) {
		// The downstream's answer is the handler's answer, verbatim. Asserting
		// equality (not a fixed truth) is what pins delegation: a branch that
		// returned a constant would pass one case and fail the other.
		var b bytes.Buffer
		down := openJSON(&b)
		open := New(down, WithLevel(slog.LevelInfo))
		for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelError} {
			want := down.Enabled(context.Background(), lvl)
			if got := open.Enabled(context.Background(), lvl); got != want {
				t.Errorf("Enabled(%v) with no scope = %v, want %v (the downstream's answer)", lvl, got, want)
			}
		}
	})

	t.Run("scope untripped enables down to the floor", func(t *testing.T) {
		ctx, done := Scope(context.Background())
		defer done()
		if !h.Enabled(ctx, slog.LevelDebug) {
			t.Fatal("Enabled(Debug) in an untripped scope = false, want true")
		}
	})

	t.Run("scope tripped still enables down to the floor", func(t *testing.T) {
		ctx, done := Scope(context.Background())
		defer done()
		slog.New(h).DebugContext(ctx, "seed")
		Trip(ctx)
		if !h.Enabled(ctx, slog.LevelDebug) {
			t.Fatal("Enabled(Debug) in a tripped scope = false, want true")
		}
	})

	t.Run("buffer floor is honored in scope", func(t *testing.T) {
		floored := New(openJSON(&buf), WithBufferFloor(slog.LevelInfo))
		ctx, done := Scope(context.Background())
		defer done()
		if floored.Enabled(ctx, slog.LevelDebug) {
			t.Fatal("Enabled(Debug) below the buffer floor = true, want false")
		}
		if !floored.Enabled(ctx, slog.LevelInfo) {
			t.Fatal("Enabled(Info) at the buffer floor = false, want true")
		}
	})
}

// TestNoScopeDropsBelowLevel proves the unscoped path is a plain level filter.
func TestNoScopeDropsBelowLevel(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	log.Debug("dropped")
	log.Info("kept")

	if got, want := c.messages(), []string{"kept"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

// TestNoScopeEnabledFollowsGatedDownstream asserts the delegation branch
// against a downstream that actually says no.
func TestNoScopeEnabledFollowsGatedDownstream(t *testing.T) {
	// Construct with a wide-open downstream, then assert delegation by using a
	// buffer floor equal to the gate so New's probe passes.
	var buf bytes.Buffer
	gated := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := New(gated, WithBufferFloor(slog.LevelInfo), WithLevel(slog.LevelInfo))

	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("Enabled(Debug) with an Info-gated downstream and no scope = true, want false")
	}
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled(Info) = false, want true")
	}
}

func TestBufferThenReplayOnError(t *testing.T) {
	c := &capture{}
	h := New(c, WithLevel(slog.LevelInfo))
	log := slog.New(h)

	ctx, done := Scope(context.Background())
	defer done()

	log.DebugContext(ctx, "first")
	log.DebugContext(ctx, "second")
	log.DebugContext(ctx, "third")

	if got := c.messages(); len(got) != 0 {
		t.Fatalf("records emitted before the trip: %v", got)
	}

	log.ErrorContext(ctx, "boom")

	want := []string{"first", "second", "third", "boom"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}

	recs := c.snapshot()
	for i, r := range recs[:3] {
		if !hasAttr(r, DefaultReplayKey, true) {
			t.Errorf("record %d (%q) missing %s=true", i, r.Message, DefaultReplayKey)
		}
		if r.Time.IsZero() {
			t.Errorf("record %d (%q) lost its original timestamp", i, r.Message)
		}
	}
	if hasAttr(recs[3], DefaultReplayKey, true) {
		t.Error("the triggering record must not be marked as a replay")
	}
	// Original order implies non-decreasing times; a newest-first flush breaks it.
	for i := 1; i < 3; i++ {
		if recs[i].Time.Before(recs[i-1].Time) {
			t.Fatalf("replay out of order: record %d (%q) is older than record %d (%q)",
				i, recs[i].Message, i-1, recs[i-1].Message)
		}
	}
}

func TestCleanScopeDiscardsBuffer(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	log.DebugContext(ctx, "never seen")
	log.DebugContext(ctx, "also never seen")
	done()

	if got := c.messages(); len(got) != 0 {
		t.Fatalf("a clean scope emitted %v, want nothing", got)
	}
}

func TestEvictionEmitsDroppedCount(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo), WithCapacity(2)))

	ctx, done := Scope(context.Background())
	defer done()

	for _, m := range []string{"a", "b", "c", "d", "e"} {
		log.DebugContext(ctx, m)
	}
	log.ErrorContext(ctx, "boom")

	// Capacity 2 keeps the newest two; three were evicted.
	want := []string{DroppedMessage, "d", "e", "boom"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}

	recs := c.snapshot()
	if !hasAttr(recs[0], DroppedKey, int64(3)) {
		t.Fatalf("dropped record does not carry %s=3: %v", DroppedKey, attrsOf(recs[0]))
	}
}

func TestTripFlushesOnce(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	defer done()

	log.DebugContext(ctx, "buffered")
	log.ErrorContext(ctx, "first error")
	log.ErrorContext(ctx, "second error")

	want := []string{"buffered", "first error", "second error"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v (a re-flush would repeat \"buffered\")", got, want)
	}
}

func TestExplicitTrip(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	ctx, done := Scope(context.Background())
	defer done()

	log.DebugContext(ctx, "context")
	Trip(ctx)

	if got, want := c.messages(), []string{"context"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	// A second Trip must not replay again.
	Trip(ctx)
	if got, want := c.messages(), []string{"context"}; !equal(got, want) {
		t.Fatalf("after a second Trip messages = %v, want %v", got, want)
	}
}

func TestTripToleratesMissingAndClosedScopes(t *testing.T) {
	t.Run("no scope", func(t *testing.T) {
		Trip(context.Background()) // must not panic
	})

	t.Run("scope never used", func(t *testing.T) {
		ctx, done := Scope(context.Background())
		defer done()
		Trip(ctx) // nothing bound yet
	})

	t.Run("closed scope flushes nothing", func(t *testing.T) {
		c := &capture{}
		log := slog.New(New(c, WithLevel(slog.LevelInfo)))
		ctx, done := Scope(context.Background())
		log.DebugContext(ctx, "buffered")
		done()

		Trip(ctx)
		if got := c.messages(); len(got) != 0 {
			t.Fatalf("Trip on a closed scope emitted %v, want nothing", got)
		}
	})

	t.Run("done twice", func(t *testing.T) {
		_, done := Scope(context.Background())
		done()
		done()
	})
}

func TestPostTripLimit(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo), WithPostTripLimit(2)))

	ctx, done := Scope(context.Background())
	defer done()

	Trip(ctx)
	for _, m := range []string{"one", "two", "three", "four"} {
		log.DebugContext(ctx, m)
	}

	if got, want := c.messages(), []string{"one", "two"}; !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func TestDerivedHandlersReplayTheirOwnAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := New(openJSON(&buf), WithLevel(slog.LevelInfo))

	ctx, done := Scope(context.Background())
	defer done()

	left := slog.New(h).With("side", "left")
	right := slog.New(h).With("side", "right")

	left.DebugContext(ctx, "L")
	right.DebugContext(ctx, "R")
	slog.New(h).ErrorContext(ctx, "boom")

	lines := decodeLines(t, &buf)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %v", len(lines), lines)
	}
	if lines[0]["msg"] != "L" || lines[0]["side"] != "left" {
		t.Errorf("first replay = %v, want msg=L side=left", lines[0])
	}
	if lines[1]["msg"] != "R" || lines[1]["side"] != "right" {
		t.Errorf("second replay = %v, want msg=R side=right (siblings must not cross-contaminate)", lines[1])
	}
	if _, ok := lines[2]["side"]; ok {
		t.Errorf("the triggering record inherited a sibling's attr: %v", lines[2])
	}
}

func TestDerivedGroupHandlerReplays(t *testing.T) {
	var buf bytes.Buffer
	h := New(openJSON(&buf), WithLevel(slog.LevelInfo))

	ctx, done := Scope(context.Background())
	defer done()

	grouped := slog.New(h).WithGroup("req").With("id", "42")
	grouped.DebugContext(ctx, "inside")
	slog.New(h).ErrorContext(ctx, "boom")

	lines := decodeLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	req, ok := lines[0]["req"].(map[string]any)
	if !ok {
		t.Fatalf("replayed record lost its group: %v", lines[0])
	}
	if req["id"] != "42" {
		t.Errorf("group attrs = %v, want id=42", req)
	}
	// Documented caveat: the replay marker nests inside the open group.
	if _, atRoot := lines[0][DefaultReplayKey]; !atRoot {
		if _, inGroup := req[DefaultReplayKey]; !inGroup {
			t.Errorf("replay marker missing entirely: %v", lines[0])
		}
	}
}

func TestReplayKeyIsConfigurable(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo), WithReplayKey("dllog_replay")))

	ctx, done := Scope(context.Background())
	defer done()
	log.DebugContext(ctx, "buffered")
	log.ErrorContext(ctx, "boom")

	recs := c.snapshot()
	if !hasAttr(recs[0], "dllog_replay", true) {
		t.Fatalf("custom replay key missing: %v", attrsOf(recs[0]))
	}
}

func TestNestedScopeIsIdempotent(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo)))

	outer, outerDone := Scope(context.Background())
	defer outerDone()

	log.DebugContext(outer, "outer record")

	inner, innerDone := Scope(outer)
	log.DebugContext(inner, "inner record")
	innerDone() // must not release the outer buffer

	log.ErrorContext(outer, "boom")

	want := []string{"outer record", "inner record", "boom"}
	if got := c.messages(); !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func TestNewPanicsOnGatedDownstream(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New did not panic on a downstream that filters the buffer floor")
		}
		if !strings.Contains(r.(string), "wide open") {
			t.Errorf("panic message is not actionable: %v", r)
		}
	}()

	var buf bytes.Buffer
	New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestNewPanicsOnNilDownstream(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New did not panic on a nil downstream")
		}
	}()
	New(nil)
}

func TestConcurrentAppendsInOneScope(t *testing.T) {
	c := &capture{}
	log := slog.New(New(c, WithLevel(slog.LevelInfo), WithCapacity(1024)))

	ctx, done := Scope(context.Background())
	defer done()

	const goroutines, each = 8, 25
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				log.DebugContext(ctx, "record", "g", g, "i", i)
			}
		}()
	}
	wg.Wait()

	log.ErrorContext(ctx, "boom")

	if got, want := len(c.messages()), goroutines*each+1; got != want {
		t.Fatalf("emitted %d records, want %d", got, want)
	}
}

// equal compares two string slices.
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

// hasAttr reports whether r carries key with the given value.
func hasAttr(r slog.Record, key string, want any) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key && a.Value.Any() == want {
			found = true
			return false
		}
		return true
	})
	return found
}

// attrsOf renders a record's attrs for failure messages.
func attrsOf(r slog.Record) string {
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(a.String())
		b.WriteByte(' ')
		return true
	})
	return b.String()
}

// decodeLines parses the JSON records written to buf, in order.
func decodeLines(t *testing.T, buf *bytes.Buffer) []record {
	t.Helper()
	var out []record
	for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m record
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}
