package dllog

import "log/slog"

// Default configuration values. They are the ones a Handler built with no
// options uses.
const (
	// DefaultReplayKey is the attr key marking a replayed record.
	DefaultReplayKey = "replay"

	// DroppedKey carries the number of records the ring evicted, on the
	// synthetic record emitted before a replay batch.
	DroppedKey = "dllog.dropped"

	// DroppedMessage is the message of that synthetic record.
	DroppedMessage = "dllog: buffered records dropped"
)

// defaultReplayKey is the constant Trip falls back to, since a package-level
// function cannot reach a Handler's configured key.
const defaultReplayKey = DefaultReplayKey

// Option configures a Handler. Options are applied by New in the order given.
type Option func(*config)

// config is the resolved settings of a Handler. Every field has a usable zero
// replacement applied by newConfig, so an option is only ever an override.
type config struct {
	level         slog.Leveler
	bufferFloor   slog.Leveler
	tripLevel     slog.Leveler
	capacity      int
	postTripLimit int
	replayKey     string
}

// newConfig returns the defaults from the design: Info effective level, Debug
// buffer floor, Error trip level, 256 ring slots, unlimited post-trip records.
func newConfig() config {
	return config{
		level:       slog.LevelInfo,
		bufferFloor: slog.LevelDebug,
		tripLevel:   slog.LevelError,
		capacity:    0, // core normalizes to its DefaultCapacity
		replayKey:   DefaultReplayKey,
	}
}

// WithLevel sets the effective level: records at or above it are emitted as
// usual, inside a scope or out of one. Below it, records are buffered inside a
// scope and dropped outside. Defaults to slog.LevelInfo.
//
// This is the level the Handler owns. The downstream handler must stay wide
// open at the buffer floor so it cannot swallow replays; New checks that.
func WithLevel(l slog.Leveler) Option {
	return func(c *config) {
		if l != nil {
			c.level = l
		}
	}
}

// WithBufferFloor sets the lowest level a scope buffers. Records below it are
// dropped even inside a scope. Defaults to slog.LevelDebug.
func WithBufferFloor(l slog.Leveler) Option {
	return func(c *config) {
		if l != nil {
			c.bufferFloor = l
		}
	}
}

// WithTripLevel sets the level at or above which a record trips its scope and
// replays the buffer. Defaults to slog.LevelError.
func WithTripLevel(l slog.Leveler) Option {
	return func(c *config) {
		if l != nil {
			c.tripLevel = l
		}
	}
}

// WithCapacity sets how many records a scope buffers before evicting the
// oldest. A value <= 0 selects the default of 256.
func WithCapacity(n int) Option {
	return func(c *config) { c.capacity = n }
}

// WithPostTripLimit caps how many records pass through after a scope trips,
// bounding the output of a long-lived scope that keeps logging after its
// failure. Zero, the default, means unlimited.
func WithPostTripLimit(n int) Option {
	return func(c *config) { c.postTripLimit = n }
}

// WithReplayKey sets the attr key marking replayed records. Defaults to
// "replay".
//
// It applies to replays triggered by a record at the trip level. The
// package-level Trip has no Handler to read it from and always uses the
// default key.
func WithReplayKey(k string) Option {
	return func(c *config) {
		if k != "" {
			c.replayKey = k
		}
	}
}
