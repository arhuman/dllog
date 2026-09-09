package zapadapter

import "go.uber.org/zap/zapcore"

// Default configuration values, matching the slog handler's defaults so the two
// adapters buffer and trip alike on a shared scope.
const (
	// DefaultReplayKey is the field key marking a replayed entry.
	DefaultReplayKey = "replay"

	// DroppedKey carries the number of entries the ring evicted, on the
	// synthetic entry emitted before a replay batch.
	DroppedKey = "dllog.dropped"

	// DroppedMessage is the message of that synthetic entry.
	DroppedMessage = "dllog: buffered records dropped"
)

// Option configures a Core. Options are applied by New in the order given.
type Option func(*config)

// config is the resolved settings of a Core. Every field has a usable default
// applied by newConfig, so an option is only ever an override.
type config struct {
	level         zapcore.Level
	bufferFloor   zapcore.Level
	tripLevel     zapcore.Level
	capacity      int
	postTripLimit int
	replayKey     string
}

// newConfig returns the defaults from the design: Info effective level, Debug
// buffer floor, Error trip level, core-default ring slots, unlimited post-trip
// entries.
func newConfig() config {
	return config{
		level:       zapcore.InfoLevel,
		bufferFloor: zapcore.DebugLevel,
		tripLevel:   zapcore.ErrorLevel,
		capacity:    0, // core normalizes to its DefaultCapacity
		replayKey:   DefaultReplayKey,
	}
}

// WithLevel sets the effective level: outside a scope, entries below it are
// dropped. Defaults to zapcore.InfoLevel.
//
// This is the level the Core owns. The downstream core must stay wide open at
// the buffer floor so it cannot swallow replays; New checks that.
func WithLevel(l zapcore.Level) Option {
	return func(c *config) { c.level = l }
}

// WithBufferFloor sets the lowest level a scope buffers. Entries below it are
// dropped even inside a scope. Defaults to zapcore.DebugLevel.
func WithBufferFloor(l zapcore.Level) Option {
	return func(c *config) { c.bufferFloor = l }
}

// WithTripLevel sets the level at or above which an entry trips its scope and
// replays the buffer. Defaults to zapcore.ErrorLevel.
func WithTripLevel(l zapcore.Level) Option {
	return func(c *config) { c.tripLevel = l }
}

// WithCapacity sets how many entries a scope buffers before evicting the
// oldest. A value <= 0 selects the core default of 256.
func WithCapacity(n int) Option {
	return func(c *config) { c.capacity = n }
}

// WithPostTripLimit caps how many entries pass through after a scope trips,
// bounding the output of a long-lived scope that keeps logging after its
// failure. Zero, the default, means unlimited.
func WithPostTripLimit(n int) Option {
	return func(c *config) { c.postTripLimit = n }
}

// WithReplayKey sets the field key marking replayed entries. Defaults to
// "replay".
func WithReplayKey(k string) Option {
	return func(c *config) {
		if k != "" {
			c.replayKey = k
		}
	}
}
