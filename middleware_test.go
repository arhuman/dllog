package dllog

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"

	"github.com/arhuman/dllog/internal/core"
	"strings"
	"sync"
	"testing"
)

// mwSink is an ordered, concurrency-safe record store used as the downstream of
// the dllog Handler under test. Tests assert on what reached it, never on
// middleware internals.
type mwSink struct {
	mu      sync.Mutex
	records []slog.Record
}

func (s *mwSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *mwSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r.Clone())
	return nil
}

func (s *mwSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *mwSink) WithGroup(string) slog.Handler      { return s }

// messages returns the message of every record seen, in order.
func (s *mwSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r.Message)
	}
	return out
}

// snapshot returns a copy of the records seen, in order.
func (s *mwSink) snapshot() []slog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slog.Record(nil), s.records...)
}

// mwAttrs flattens a record's attrs into a map for assertions.
func mwAttrs(r slog.Record) map[string]any {
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

// newMWLogger returns a logger backed by a dllog Handler over a fresh sink.
func newMWLogger() (*slog.Logger, *mwSink) {
	s := &mwSink{}
	return slog.New(New(s)), s
}

// countMessage counts records carrying msg.
func countMessage(msgs []string, msg string) int {
	n := 0
	for _, m := range msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// serve runs one request through mw wrapping h and returns the recorder.
func serve(mw func(http.Handler) http.Handler, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mw(h).ServeHTTP(rec, req)
	return rec
}

func TestMiddlewareCleanRequestDiscardsBuffer(t *testing.T) {
	logger, sink := newMWLogger()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "step one")
		logger.DebugContext(r.Context(), "step two")
		w.WriteHeader(http.StatusOK)
	})

	rec := serve(Middleware(WithLogger(logger)), h, httptest.NewRequest(http.MethodGet, "/ok", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := sink.messages(); len(got) != 0 {
		t.Fatalf("clean request emitted %v, want nothing", got)
	}
}

func TestMiddlewareTripsOn5xxAndReplaysInOrder(t *testing.T) {
	logger, sink := newMWLogger()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "step one")
		logger.DebugContext(r.Context(), "step two")
		w.WriteHeader(http.StatusInternalServerError)
	})

	serve(Middleware(WithLogger(logger)), h, httptest.NewRequest(http.MethodPost, "/boom", nil))

	got := sink.messages()
	want := []string{"step one", "step two", AnchorMessage}
	if len(got) != len(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	records := sink.snapshot()
	for i, r := range records[:2] {
		if a := mwAttrs(r); a[DefaultReplayKey] != true {
			t.Errorf("replayed record %d missing %q marker: %v", i, DefaultReplayKey, a)
		}
	}

	anchor := records[2]
	if anchor.Level != slog.LevelError {
		t.Errorf("anchor level = %v, want Error", anchor.Level)
	}
	a := mwAttrs(anchor)
	if a["method"] != http.MethodPost {
		t.Errorf("anchor method = %v, want POST", a["method"])
	}
	if a["path"] != "/boom" {
		t.Errorf("anchor path = %v, want /boom", a["path"])
	}
	if a["status"] != int64(http.StatusInternalServerError) {
		t.Errorf("anchor status = %v, want 500", a["status"])
	}
}

// A raw URL path carries whatever the route interpolated into it: reset tokens,
// api keys, user ids. The anchor is emitted by this package, on the failure path,
// where logs are most likely to be shipped and retained, so it must not be the
// thing that puts those values in the log. When the request was routed, the
// pattern says the same thing with the secrets left out.
func TestMiddlewareAnchorPrefersRoutePatternOverRawPath(t *testing.T) {
	logger, sink := newMWLogger()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}/reset/{token}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	serve(Middleware(WithLogger(logger)), mux,
		httptest.NewRequest(http.MethodGet, "/users/12345/reset/SECRET-RESET-TOKEN", nil))

	records := sink.snapshot()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1 anchor", len(records))
	}
	a := mwAttrs(records[0])

	got, _ := a["path"].(string)
	if strings.Contains(got, "SECRET-RESET-TOKEN") || strings.Contains(got, "12345") {
		t.Fatalf("anchor path = %q; it leaks the interpolated path segments", got)
	}
	if got != "GET /users/{id}/reset/{token}" {
		t.Fatalf("anchor path = %q, want the route pattern", got)
	}
}

// Without a ServeMux there is no pattern, so the raw path is all there is. It is
// still emitted: a handler mounted directly has no templated form to fall back
// to, and dropping the field entirely would cost the anchor its usefulness.
func TestMiddlewareAnchorFallsBackToPathWhenUnrouted(t *testing.T) {
	logger, sink := newMWLogger()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	serve(Middleware(WithLogger(logger)), h, httptest.NewRequest(http.MethodGet, "/unrouted", nil))

	a := mwAttrs(sink.snapshot()[0])
	if a["path"] != "/unrouted" {
		t.Fatalf("anchor path = %v, want /unrouted", a["path"])
	}
}

// The caller owns the final say: a program that routes with something other than
// net/http's mux, or that wants the path redacted its own way, supplies its own.
func TestMiddlewareWithAnchorPathOverrides(t *testing.T) {
	logger, sink := newMWLogger()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	mw := Middleware(WithLogger(logger), WithAnchorPath(func(*http.Request) string {
		return "redacted"
	}))
	serve(mw, h, httptest.NewRequest(http.MethodGet, "/users/12345/reset/SECRET", nil))

	a := mwAttrs(sink.snapshot()[0])
	if a["path"] != "redacted" {
		t.Fatalf("anchor path = %v, want the value WithAnchorPath returned", a["path"])
	}
}

func TestMiddlewareDoesNotTripOn4xx(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusTooManyRequests} {
		logger, sink := newMWLogger()

		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.DebugContext(r.Context(), "step one")
			w.WriteHeader(status)
		})

		serve(Middleware(WithLogger(logger)), h, httptest.NewRequest(http.MethodGet, "/missing", nil))

		if got := sink.messages(); len(got) != 0 {
			t.Errorf("status %d emitted %v, want nothing", status, got)
		}
	}
}

func TestMiddlewareDoesNotTripOnContextCancellation(t *testing.T) {
	logger, sink := newMWLogger()

	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "step one")
		cancel()
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	})

	serve(Middleware(WithLogger(logger)), h, req)

	if got := sink.messages(); len(got) != 0 {
		t.Fatalf("cancelled request emitted %v, want nothing", got)
	}
}

func TestMiddlewarePanicRepanicsAndAnchors(t *testing.T) {
	logger, sink := newMWLogger()

	boom := errors.New("handler exploded")
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "step one")
		panic(boom)
	})

	var got any
	func() {
		defer func() { got = recover() }()
		serve(Middleware(WithLogger(logger)), h, httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()

	if got == nil {
		t.Fatal("panic was swallowed by the middleware; it must re-panic")
	}
	if !errors.Is(got.(error), boom) {
		t.Fatalf("re-panicked with %v, want the original value %v", got, boom)
	}

	msgs := sink.messages()
	want := []string{"step one", AnchorMessage}
	if len(msgs) != len(want) || msgs[0] != want[0] || msgs[1] != want[1] {
		t.Fatalf("records = %v, want %v", msgs, want)
	}
}

func TestMiddlewareSuppressesDuplicateAnchor(t *testing.T) {
	logger, sink := newMWLogger()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "step one")
		logger.ErrorContext(r.Context(), "handler own error")
		w.WriteHeader(http.StatusInternalServerError)
	})

	serve(Middleware(WithLogger(logger)), h, httptest.NewRequest(http.MethodGet, "/dup", nil))

	msgs := sink.messages()
	if n := countMessage(msgs, AnchorMessage); n != 0 {
		t.Fatalf("anchor emitted %d times on an already-tripped scope, want 0 (records: %v)", n, msgs)
	}
	if n := countMessage(msgs, "handler own error"); n != 1 {
		t.Fatalf("handler error seen %d times, want 1 (records: %v)", n, msgs)
	}
	if msgs[0] != "step one" {
		t.Fatalf("buffer not replayed before the handler error: %v", msgs)
	}
}

func TestMiddlewareTripOnPredicate(t *testing.T) {
	tripOn404 := WithTripOn(func(status int) bool { return status == http.StatusNotFound })

	t.Run("custom status trips", func(t *testing.T) {
		logger, sink := newMWLogger()
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.DebugContext(r.Context(), "step one")
			w.WriteHeader(http.StatusNotFound)
		})
		serve(Middleware(WithLogger(logger), tripOn404), h, httptest.NewRequest(http.MethodGet, "/nope", nil))

		msgs := sink.messages()
		if len(msgs) != 2 || msgs[0] != "step one" || msgs[1] != AnchorMessage {
			t.Fatalf("records = %v, want [step one %s]", msgs, AnchorMessage)
		}
	})

	t.Run("default 500 no longer trips", func(t *testing.T) {
		logger, sink := newMWLogger()
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.DebugContext(r.Context(), "step one")
			w.WriteHeader(http.StatusInternalServerError)
		})
		serve(Middleware(WithLogger(logger), tripOn404), h, httptest.NewRequest(http.MethodGet, "/boom", nil))

		if got := sink.messages(); len(got) != 0 {
			t.Fatalf("500 tripped under a 404-only predicate: %v", got)
		}
	})
}

func TestMiddlewareWithLoggerRoutesAnchor(t *testing.T) {
	chosen, chosenSink := newMWLogger()
	other, otherSink := newMWLogger()
	previous := slog.Default()
	slog.SetDefault(other)
	t.Cleanup(func() { slog.SetDefault(previous) })

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	serve(Middleware(WithLogger(chosen)), h, httptest.NewRequest(http.MethodGet, "/x", nil))

	if n := countMessage(chosenSink.messages(), AnchorMessage); n != 1 {
		t.Fatalf("supplied logger saw %d anchors, want 1", n)
	}
	if n := countMessage(otherSink.messages(), AnchorMessage); n != 0 {
		t.Fatalf("default logger saw %d anchors, want 0", n)
	}
}

func TestMiddlewareImplicitStatus(t *testing.T) {
	t.Run("body without WriteHeader", func(t *testing.T) {
		logger, sink := newMWLogger()
		var seen int
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.DebugContext(r.Context(), "step one")
			//nolint:errcheck // httptest.ResponseRecorder never fails a write.
			_, _ = w.Write([]byte("hello"))
		})
		mw := Middleware(WithLogger(logger), WithTripOn(func(s int) bool { seen = s; return s >= 500 }))
		rec := serve(mw, h, httptest.NewRequest(http.MethodGet, "/implicit", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if seen != http.StatusOK {
			t.Errorf("predicate saw status %d, want 200", seen)
		}
		if got := sink.messages(); len(got) != 0 {
			t.Fatalf("implicit 200 tripped: %v", got)
		}
	})

	t.Run("empty response", func(t *testing.T) {
		logger, sink := newMWLogger()
		var seen int
		h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			logger.DebugContext(r.Context(), "step one")
		})
		mw := Middleware(WithLogger(logger), WithTripOn(func(s int) bool { seen = s; return s >= 500 }))
		serve(mw, h, httptest.NewRequest(http.MethodGet, "/empty", nil))

		if seen != http.StatusOK {
			t.Errorf("predicate saw status %d, want 200", seen)
		}
		if got := sink.messages(); len(got) != 0 {
			t.Fatalf("empty response tripped: %v", got)
		}
	})
}

func TestMiddlewareWriteHeaderIsIdempotent(t *testing.T) {
	logger, sink := newMWLogger()
	var seen int

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "step one")
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusInternalServerError)
	})
	mw := Middleware(WithLogger(logger), WithTripOn(func(s int) bool { seen = s; return s >= 500 }))
	serve(mw, h, httptest.NewRequest(http.MethodGet, "/twice", nil))

	if seen != http.StatusOK {
		t.Fatalf("predicate saw status %d, want the first status 200", seen)
	}
	if got := sink.messages(); len(got) != 0 {
		t.Fatalf("superfluous WriteHeader tripped the scope: %v", got)
	}
}

func TestMiddlewareFlusherReachableThroughWrapper(t *testing.T) {
	var flushed bool

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(*statusWriter); !ok {
			t.Errorf("handler did not receive the wrapper, got %T", w)
		}
		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush through the wrapper: %v", err)
			return
		}
		flushed = true
	})

	rec := httptest.NewRecorder()
	Middleware()(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/flush", nil))

	if !flushed {
		t.Fatal("Flush was not reachable through the wrapper")
	}
	if !rec.Flushed {
		t.Fatal("recorder was never flushed")
	}
}

// hijackRecorder is an httptest.ResponseRecorder that also hijacks, so the test
// can prove http.NewResponseController finds Hijack through the wrapper.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestMiddlewareHijackerReachableThroughWrapper(t *testing.T) {
	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		if _, _, err := rc.Hijack(); err != nil {
			t.Errorf("Hijack through the wrapper: %v", err)
		}
	})

	Middleware()(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hijack", nil))

	if !rec.hijacked {
		t.Fatal("Hijack was not reachable through the wrapper")
	}
}

func TestMiddlewareConcurrentRequestsDoNotCrossContaminate(t *testing.T) {
	const requests = 32

	logger, sink := newMWLogger()
	mw := Middleware(WithLogger(logger))

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path
		logger.DebugContext(r.Context(), "debug "+id)
		logger.DebugContext(r.Context(), "debug again "+id)
		if strings.HasSuffix(id, "-fail") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	wrapped := mw(h)

	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := "/req" + string(rune('a'+i%26)) + string(rune('0'+i/26))
			if i%2 == 0 {
				path += "-fail"
			}
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		}()
	}
	wg.Wait()

	// Every record present must belong to a failing request: a clean request's
	// debug lines leaking into a failing request's flush is the cross
	// contamination this test exists to catch.
	for _, r := range sink.snapshot() {
		if r.Message == AnchorMessage {
			a := mwAttrs(r)
			path, _ := a["path"].(string)
			if !strings.HasSuffix(path, "-fail") {
				t.Errorf("anchor for a clean request: %v", path)
			}
			continue
		}
		if !strings.HasSuffix(r.Message, "-fail") {
			t.Errorf("record from a clean request leaked into a flush: %q", r.Message)
		}
	}

	// Each failing request must contribute exactly its own two debug lines plus
	// one anchor, which also proves nothing was lost.
	want := requests / 2 * 3
	if got := len(sink.snapshot()); got != want {
		t.Fatalf("emitted %d records, want %d (2 debug + 1 anchor per failing request)", got, want)
	}
}

func TestMiddlewareComposesWithAnotherMiddleware(t *testing.T) {
	logger, sink := newMWLogger()

	var order []string
	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "outer-in")
			next.ServeHTTP(w, r)
			order = append(order, "outer-out")
		})
	}
	inner := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "inner-in")
			// An inner middleware logging on the request context must land in
			// the same scope the dllog middleware opened.
			logger.DebugContext(r.Context(), "inner debug")
			next.ServeHTTP(w, r)
		})
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
		logger.DebugContext(r.Context(), "handler debug")
		w.WriteHeader(http.StatusBadGateway)
	})

	rec := httptest.NewRecorder()
	outer(Middleware(WithLogger(logger))(inner(h))).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chain", nil))

	wantOrder := []string{"outer-in", "inner-in", "handler", "outer-out"}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}

	msgs := sink.messages()
	want := []string{"inner debug", "handler debug", AnchorMessage}
	if strings.Join(msgs, ",") != strings.Join(want, ",") {
		t.Fatalf("records = %v, want %v", msgs, want)
	}
}

// TestMiddlewareReleasesTheScope pins the release itself, not its consequences.
// Every other test here asserts on emitted records, and a middleware that never
// calls done() still emits correctly: the buffer is simply retained forever.
// That is the leak the explicit lifecycle exists to prevent, so it needs an
// assertion of its own.
func TestMiddlewareReleasesTheScope(t *testing.T) {
	sink := &mwSink{}
	logger := slog.New(New(sink))

	var captured *carrier
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = fromContext(r.Context())
		logger.DebugContext(r.Context(), "buffered")
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	Middleware(WithLogger(logger))(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/release", nil))

	if captured == nil {
		t.Fatal("handler saw no scope on the request context")
	}
	s := captured.bound()
	if s == nil {
		t.Fatal("no ring was bound despite a buffered record")
	}
	// A released scope buffers nothing: a late record passes through instead.
	if got := s.Append(slot{}); got != core.ActionPassThrough {
		t.Fatalf("append after the request returned = %v, want %v: the middleware did not release the scope",
			got, core.ActionPassThrough)
	}
}
