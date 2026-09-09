package zapadapter

import (
	"go.uber.org/zap/zapcore"

	"github.com/arhuman/dllog/internal/core"
)

// slot is what this adapter puts in a scope's ring: a zap entry with its copied
// fields, together with the downstream core that must write it.
//
// The triple is what makes With work without reimplementing field merging. Each
// derived Core eagerly derives its own downstream, and an entry buffered
// through that Core carries it into the ring, so the flush writes every entry
// through the core that was in scope when it was logged.
type slot struct {
	entry      zapcore.Entry
	fields     []zapcore.Field
	downstream zapcore.Core
}

// coreEntry wraps s as a core entry, closing over the write this adapter owes
// the entry so the core can hold it beside entries from other adapters without
// naming zap.
func (s slot) coreEntry() core.Entry {
	return core.Entry{
		Emit: func(replayKey string) {
			fields := make([]zapcore.Field, 0, len(s.fields)+1)
			fields = append(fields, s.fields...)
			fields = append(fields, zapcore.Field{
				Key:       replayKey,
				Type:      zapcore.BoolType,
				Integer:   1,
				Interface: nil,
			})
			//nolint:errcheck // a replayed entry has no caller left to return to.
			_ = s.downstream.Write(s.entry, fields)
		},
		Notify: func(n int) { emitDropped(s, n) },
	}
}

// emitDropped reports entries the ring evicted, as one entry carrying the count
// under DroppedKey. It borrows the oldest survivor's time and downstream so the
// notice lands just before the batch it belongs to, in the same stream.
//
// It reaches the core as that entry's Notify thunk, so an eviction announced on
// a batch whose oldest survivor belongs to another adapter is rendered by that
// adapter, in its own stream, rather than being forced into zap.
func emitDropped(oldest slot, dropped int) {
	ent := zapcore.Entry{
		Level:   zapcore.WarnLevel,
		Time:    oldest.entry.Time,
		Message: DroppedMessage,
	}
	field := zapcore.Field{Key: DroppedKey, Type: zapcore.Int64Type, Integer: int64(dropped)}
	//nolint:errcheck // same as the replay path: nobody left to report to.
	_ = oldest.downstream.Write(ent, []zapcore.Field{field})
}
