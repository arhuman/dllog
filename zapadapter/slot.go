package zapadapter

import (
	"go.uber.org/zap/zapcore"
)

// slot is what this adapter puts in a scope's ring: a zap entry with its copied
// fields, the downstream core that must write it, and the replay key to mark it
// with.
//
// Carrying the downstream is what makes With work without reimplementing field
// merging. Each derived Core eagerly derives its own downstream, and an entry
// buffered through that Core carries it into the ring, so the flush writes every
// entry through the core that was in scope when it was logged, marked the way
// that core was configured.
//
// It implements core.Entry with pointer methods, so buffering an entry costs
// the slot allocation and the field copy; no closures are built on the append
// path.
type slot struct {
	entry      zapcore.Entry
	fields     []zapcore.Field
	downstream zapcore.Core
	replayKey  string
}

// Emit writes the buffered entry through the downstream it was logged with,
// marked with its Core's replay key. It is how the core hands the entry back at
// flush time without naming zap.
func (s *slot) Emit() {
	fields := make([]zapcore.Field, 0, len(s.fields)+1)
	fields = append(fields, s.fields...)
	fields = append(fields, zapcore.Field{
		Key:       s.replayKey,
		Type:      zapcore.BoolType,
		Integer:   1,
		Interface: nil,
	})
	// Through Check, not Write: a replayed entry is offered to the downstream
	// on the same terms a live one is, so a sampler or tee below this adapter
	// still governs it. The offer happens now, at replay time, because a
	// CheckedEntry is single-use and cannot be held across the buffer window.
	//nolint:errcheck // a replayed entry has no caller left to return to.
	_ = writeChecked(s.downstream, s.entry, fields)
}

// Notify reports entries the ring evicted, as one entry carrying the count
// under DroppedKey. It borrows this slot's time and downstream so the notice
// lands just before the batch it belongs to, in the same stream.
//
// The core calls it on the oldest surviving entry, so an eviction announced on
// a batch whose oldest survivor belongs to another adapter is rendered by that
// adapter, in its own stream, rather than being forced into zap.
func (s *slot) Notify(n int) {
	ent := zapcore.Entry{
		Level:   zapcore.WarnLevel,
		Time:    s.entry.Time,
		Message: DroppedMessage,
	}
	field := zapcore.Field{Key: DroppedKey, Type: zapcore.Int64Type, Integer: int64(n)}
	//nolint:errcheck // same as Emit: nobody left to report to.
	_ = writeChecked(s.downstream, ent, []zapcore.Field{field})
}
