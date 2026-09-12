package dllog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// messagesOf returns the "msg" field of each decoded line, in order.
func messagesOf(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, r := range decodeLines(t, buf) {
		msg, _ := r["msg"].(string)
		out = append(out, msg)
	}
	return out
}

// TestOwnedConstructorsBufferAndReplay is the headline claim: the one-line
// setup buffers Debug inside a scope and replays it when the scope trips, with
// no downstream wiring from the caller.
func TestOwnedConstructorsBufferAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*bytes.Buffer) *Handler
	}{
		{"NewJSON", func(b *bytes.Buffer) *Handler { return NewJSON(b) }},
		{"NewJSONWith", func(b *bytes.Buffer) *Handler {
			return NewJSONWith(b, slog.HandlerOptions{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(tc.make(&buf))

			ctx, done := Scope(context.Background())
			defer done()

			log.DebugContext(ctx, "buffered detail")
			if got := messagesOf(t, &buf); len(got) != 0 {
				t.Fatalf("Debug emitted before the scope tripped: %v", got)
			}

			log.ErrorContext(ctx, "boom")

			want := []string{"buffered detail", "boom"}
			if got := messagesOf(t, &buf); !equal(got, want) {
				t.Fatalf("messages = %v, want %v", got, want)
			}
		})
	}
}

// TestOwnedConstructorDiscardsOnSuccess pins the other half: a scope that ends
// without tripping emits nothing it buffered.
func TestOwnedConstructorDiscardsOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewJSON(&buf))

	ctx, done := Scope(context.Background())
	log.DebugContext(ctx, "buffered detail")
	done()

	if got := messagesOf(t, &buf); len(got) != 0 {
		t.Fatalf("a scope that did not trip emitted %v, want nothing", got)
	}
}

// TestOwnedConstructorOutOfScope asserts the effective level still applies
// outside a scope: Debug is dropped, Info is emitted.
func TestOwnedConstructorOutOfScope(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewJSON(&buf))
	ctx := context.Background()

	log.DebugContext(ctx, "dropped")
	log.InfoContext(ctx, "emitted")

	want := []string{"emitted"}
	if got := messagesOf(t, &buf); !equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

// TestOwnedConstructorTextEncoding covers the text variants, which share every
// path with the JSON ones except the encoder.
func TestOwnedConstructorTextEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*bytes.Buffer) *Handler
	}{
		{"NewText", func(b *bytes.Buffer) *Handler { return NewText(b) }},
		{"NewTextWith", func(b *bytes.Buffer) *Handler {
			return NewTextWith(b, slog.HandlerOptions{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(tc.make(&buf))

			ctx, done := Scope(context.Background())
			defer done()

			log.DebugContext(ctx, "buffered detail")
			if buf.Len() != 0 {
				t.Fatalf("Debug emitted before the scope tripped: %q", buf.String())
			}

			log.ErrorContext(ctx, "boom")

			out := buf.String()
			if !strings.Contains(out, "buffered detail") || !strings.Contains(out, "boom") {
				t.Fatalf("replay missing from output: %q", out)
			}
			if strings.Index(out, "buffered detail") > strings.Index(out, "boom") {
				t.Fatalf("replay came after the trigger, want before: %q", out)
			}
		})
	}
}

// TestOwnedConstructorHonoursBufferFloor proves the resolved floor reaches the
// downstream dllog builds, rather than a hardcoded Debug. Options must be
// applied before the downstream is constructed for this to hold.
func TestOwnedConstructorHonoursBufferFloor(t *testing.T) {
	var buf bytes.Buffer
	h := NewJSON(&buf, WithBufferFloor(slog.LevelWarn), WithLevel(slog.LevelError))
	ctx := context.Background()

	if h.downstream.Enabled(ctx, slog.LevelWarn) != true {
		t.Error("downstream Enabled(Warn) = false, want true: the floor did not reach it")
	}
	if h.downstream.Enabled(ctx, slog.LevelInfo) != false {
		t.Error("downstream Enabled(Info) = true, want false: the downstream is wider than the floor")
	}

	// Below the floor, a record is dropped even inside a scope.
	log := slog.New(h)
	sctx, done := Scope(ctx)
	defer done()
	log.InfoContext(sctx, "below the floor")
	log.ErrorContext(sctx, "boom")

	want := []string{"boom"}
	if got := messagesOf(t, &buf); !equal(got, want) {
		t.Fatalf("messages = %v, want %v: a sub-floor record was buffered", got, want)
	}
}

// TestOwnedConstructorTracksDynamicFloor covers what a caller cannot express
// with New: the downstream follows a LevelVar floor after construction.
func TestOwnedConstructorTracksDynamicFloor(t *testing.T) {
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)

	var buf bytes.Buffer
	// The level rides the same LevelVar so the floor <= level ordering holds
	// at construction and keeps holding when the floor moves.
	h := NewJSON(&buf, WithBufferFloor(lv), WithLevel(lv))
	ctx := context.Background()

	if h.downstream.Enabled(ctx, slog.LevelDebug) {
		t.Fatal("downstream Enabled(Debug) = true at a Warn floor")
	}

	lv.Set(slog.LevelDebug)
	if !h.downstream.Enabled(ctx, slog.LevelDebug) {
		t.Fatal("downstream Enabled(Debug) = false after the floor dropped; it did not track the Leveler")
	}
}

// TestNewWithPanicsOnCallerLevel pins the *With constructors' one rule: dllog
// owns the downstream level, so a caller-set one is refused rather than
// silently overridden.
func TestNewWithPanicsOnCallerLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*bytes.Buffer)
	}{
		{"NewJSONWith", func(b *bytes.Buffer) {
			NewJSONWith(b, slog.HandlerOptions{Level: slog.LevelInfo})
		}},
		{"NewTextWith", func(b *bytes.Buffer) {
			NewTextWith(b, slog.HandlerOptions{Level: slog.LevelInfo})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("no panic on a caller-supplied HandlerOptions.Level")
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("panic value is %T, want string", r)
				}
				if !strings.Contains(msg, "owns the") {
					t.Errorf("panic message is not actionable: %v", msg)
				}
			}()
			var buf bytes.Buffer
			tc.call(&buf)
		})
	}
}

// TestNewWithRunsReplaceAttr is the reason the *With constructors exist: the
// caller's encoder settings survive, only Level is taken over.
func TestNewWithRunsReplaceAttr(t *testing.T) {
	var buf bytes.Buffer
	ho := slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == "secret" {
				return slog.String("secret", "REDACTED")
			}
			return a
		},
	}
	log := slog.New(NewJSONWith(&buf, ho))
	log.Info("hello", "secret", "hunter2")

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if got := lines[0]["secret"]; got != "REDACTED" {
		t.Fatalf("secret = %v, want REDACTED: ReplaceAttr did not run", got)
	}
}

// TestNewWithDoesNotMutateCallerOptions asserts ho is taken by value: setting
// Level internally must not be visible to the caller's struct.
func TestNewWithDoesNotMutateCallerOptions(t *testing.T) {
	var buf bytes.Buffer
	ho := slog.HandlerOptions{AddSource: true}
	NewJSONWith(&buf, ho)

	if ho.Level != nil {
		t.Fatalf("caller's HandlerOptions.Level = %v, want nil: the struct was mutated", ho.Level)
	}
}
