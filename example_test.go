package dllog_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/arhuman/dllog"
)

// Example is the whole setup: one constructor, no downstream handler to wire.
// The service logs at Info, and a failed operation also gets the Debug records
// that preceded it.
func Example() {
	logger := slog.New(dllog.NewJSON(os.Stderr))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Buffered: invisible on a request that succeeds.
		slog.DebugContext(ctx, "loading cart", "user", 42)

		// Replays everything buffered above it, then emits.
		slog.ErrorContext(ctx, "payment declined", "provider", "stripe")
		w.WriteHeader(http.StatusInternalServerError)
	})

	// The middleware opens a scope per request and trips on 5xx or a panic.
	_ = http.ListenAndServe(":8080", dllog.Middleware()(mux))
}

// ExampleScope manages a scope outside HTTP. Trip covers the common Go case
// where the failure is returned rather than logged.
func ExampleScope() {
	logger := slog.New(dllog.NewJSON(os.Stderr))

	process := func(ctx context.Context, id string) error {
		ctx, done := dllog.Scope(ctx)
		defer done()

		logger.DebugContext(ctx, "fetching record", "id", id)

		if err := errors.New("not found"); err != nil {
			dllog.Trip(ctx) // replay the buffer, then return the error as usual
			return err
		}
		return nil // buffer discarded, nothing emitted
	}

	_ = process(context.Background(), "order-1")
}

// ExampleNewJSONWith keeps the caller's encoder settings while dllog keeps
// ownership of the level.
func ExampleNewJSONWith() {
	logger := slog.New(dllog.NewJSONWith(os.Stderr, slog.HandlerOptions{
		AddSource: true,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == "password" {
				return slog.String("password", "REDACTED")
			}
			return a
		},
		// Level stays nil: dllog sets it from WithBufferFloor.
	}, dllog.WithLevel(slog.LevelWarn)))

	logger.Warn("credentials rotated", "password", "hunter2")
}
