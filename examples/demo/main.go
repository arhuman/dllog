// Command demo runs one fixed workload under three logging configurations so
// the same sequence of events can be compared side by side.
//
// The workload is three checkouts: one that succeeds, one that fails, one that
// succeeds. The successes on either side are what show dllog going quiet again
// once the failing operation is behind it.
//
//	demo info    plain slog at Info: the error, none of the context
//	demo debug   plain slog at Debug: everything, all the time
//	demo dllog   dllog at Info: Info in the clear, Debug replayed on failure
//
// It exists to produce the README's comparison GIFs, so it prints at a human
// pace rather than as fast as it can.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/arhuman/dllog"
)

// step is the pace between records. Slow enough to read in a GIF.
const step = 190 * time.Millisecond

func main() {
	mode := "dllog"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	out := colourWriter{w: os.Stdout}

	var logger *slog.Logger
	switch mode {
	case "info":
		logger = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: render,
		}))
	case "debug":
		logger = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			ReplaceAttr: render,
		}))
	case "dllog":
		logger = slog.New(dllog.NewTextWith(out, slog.HandlerOptions{
			ReplaceAttr: render,
		}))
	default:
		fmt.Fprintf(os.Stderr, "usage: demo [info|debug|dllog]\n")
		os.Exit(2)
	}

	ctx := context.Background()
	scoped := func(c context.Context) (context.Context, func()) { return c, func() {} }
	if mode == "dllog" {
		scoped = dllog.Scope
	}

	// Three operations, each its own scope: one that succeeds, one that fails,
	// one that succeeds again. Only the middle one replays, which is what makes
	// the quiet on either side of it visible.
	run(ctx, logger, scoped, order4415)
	run(ctx, logger, scoped, order4417)
	run(ctx, logger, scoped, order4421)
}

// run executes one operation inside its own scope, so a trip in one does not
// leak into the next.
func run(ctx context.Context, log *slog.Logger, scoped scopeFunc, op func(context.Context, *slog.Logger)) {
	ctx, done := scoped(ctx)
	defer done()
	op(ctx, log)
}

// scopeFunc opens a buffering scope, or does nothing for the plain slog modes.
type scopeFunc func(context.Context) (context.Context, func())

// order4415 is the quiet success before the failure: plenty of Debug work, and
// nothing but the Info bookends survive at level.
func order4415(ctx context.Context, log *slog.Logger) {
	log.InfoContext(ctx, "checkout started", "order", "ORD-4415")
	pause()
	log.DebugContext(ctx, "cart loaded", "items", 1)
	pause()
	log.DebugContext(ctx, "address validated", "country", "DE")
	pause()
	log.DebugContext(ctx, "card authorized", "token", "tok_3a81")
	pause()
	log.DebugContext(ctx, "stock reserved", "warehouse", "EU-2")
	pause()
	log.InfoContext(ctx, "checkout completed", "order", "ORD-4415")
	pause()
}

// order4417 is the failure: the same kind of Debug work, two Warn records that
// hint at trouble, and exactly one Error that replays everything above it.
func order4417(ctx context.Context, log *slog.Logger) {
	log.InfoContext(ctx, "checkout started", "order", "ORD-4417")
	pause()

	log.DebugContext(ctx, "cart loaded", "items", 3)
	pause()
	log.DebugContext(ctx, "discount applied", "code", "SUMMER")
	pause()
	log.DebugContext(ctx, "address validated", "country", "FR")
	pause()

	log.WarnContext(ctx, "gateway slow", "latency_ms", 1840)
	pause()

	log.DebugContext(ctx, "retrying authorization", "attempt", 2)
	pause()
	log.DebugContext(ctx, "card token refreshed", "token", "tok_9f2c")
	pause()

	log.WarnContext(ctx, "gateway slow", "latency_ms", 3120)
	pause()

	log.DebugContext(ctx, "authorization declined", "code", "do_not_honor")
	pause()

	log.ErrorContext(ctx, "checkout failed", "err", errors.New("payment declined"))
	pause()
}

// order4421 is the quiet success after the failure. It needs its own scope: a
// tripped scope passes Debug straight through, so reusing 4417's would keep the
// noise on and hide the point.
func order4421(ctx context.Context, log *slog.Logger) {
	log.InfoContext(ctx, "checkout started", "order", "ORD-4421")
	pause()
	log.DebugContext(ctx, "cart loaded", "items", 2)
	pause()
	log.DebugContext(ctx, "address validated", "country", "ES")
	pause()
	log.DebugContext(ctx, "card authorized", "token", "tok_5d20")
	pause()
	log.InfoContext(ctx, "checkout completed", "order", "ORD-4421")
	pause()
}

func pause() { time.Sleep(step) }

// clockStart is the wall time the demo pretends to run at, so the three GIFs
// carry identical timestamps and differ only in which records survive.
var clockStart = time.Now()

// ANSI colours for the level names: blue Debug, green Info, orange Warn, red
// Error. Bright variants, because the GIFs are recorded on a dark theme.
const (
	ansiReset  = "\033[0m"
	ansiBlue   = "\033[94m"
	ansiGreen  = "\033[92m"
	ansiOrange = "\033[38;5;214m"
	ansiRed    = "\033[91m"
)

// render rewrites the time attr and leaves the rest alone.
//
// The time becomes an offset from clockStart on a fixed base. Keeping the
// offset real is the point: a replayed Debug record shows the second it was
// logged at, not the second it was replayed at, which is what lets the replayed
// batch be read against the Warn records around it.
func render(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		base := time.Date(2026, 1, 9, 10, 32, 7, 0, time.UTC)
		return slog.String(slog.TimeKey, base.Add(a.Value.Time().Sub(clockStart)).Format("05.000"))
	default:
		return a
	}
}

// colourWriter colours the level name of each log line on its way out.
//
// The colouring happens here rather than in a ReplaceAttr hook because slog's
// text handler runs every value through needsQuoting, which sees the ANSI
// escapes and quotes the whole thing, printing a literal \x1b. Rewriting the
// finished line sidesteps that entirely.
type colourWriter struct{ w io.Writer }

// levelNames are the tokens to colour, longest first so DEBUG is not shadowed
// by a prefix of another name.
var levelNames = []struct {
	token  string
	colour string
}{
	{"level=DEBUG", ansiBlue},
	{"level=INFO", ansiGreen},
	{"level=WARN", ansiOrange},
	{"level=ERROR", ansiRed},
}

// Write colours the first level token it finds in p. slog emits one record per
// Write, so a single substitution per call is enough.
func (c colourWriter) Write(p []byte) (int, error) {
	for _, l := range levelNames {
		i := bytes.Index(p, []byte(l.token))
		if i < 0 {
			continue
		}
		name := l.token[len("level="):]
		out := make([]byte, 0, len(p)+len(l.colour)+len(ansiReset))
		out = append(out, p[:i]...)
		out = append(out, "level="...)
		out = append(out, l.colour...)
		out = append(out, name...)
		out = append(out, ansiReset...)
		out = append(out, p[i+len(l.token):]...)
		// Report the caller's length: it wrote len(p) bytes of record, and the
		// escapes we added are not its concern.
		if _, err := c.w.Write(out); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.w.Write(p)
}
