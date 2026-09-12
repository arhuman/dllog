package zapadapter

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap/zapcore"
)

// failingCore accepts every entry and fails to write it, standing in for a
// sink whose disk is full or whose connection dropped.
type failingCore struct{ err error }

func (f *failingCore) Enabled(zapcore.Level) bool        { return true }
func (f *failingCore) With([]zapcore.Field) zapcore.Core { return f }
func (f *failingCore) Sync() error                       { return nil }

func (f *failingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(ent, f)
}

func (f *failingCore) Write(zapcore.Entry, []zapcore.Field) error { return f.err }

// lockedBuffer is a WriteSyncer over a buffer, so the test can read back what
// the internal error output received.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Sync() error { return nil }

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A downstream write failure must be reported, not swallowed: a CheckedEntry
// built outside a zap.Logger has no ErrorOutput, so writeChecked supplies one.
// Without that assignment this failure would vanish and a dead sink would look
// like a healthy quiet one.
func TestWriteCheckedReportsDownstreamWriteFailure(t *testing.T) {
	buf := &lockedBuffer{}
	saved := internalErrorOutput
	internalErrorOutput = buf
	defer func() { internalErrorOutput = saved }()

	down := &failingCore{err: errors.New("sink is gone")}
	if err := writeChecked(down, zapcore.Entry{Level: zapcore.InfoLevel, Message: "m"}, nil); err != nil {
		t.Fatalf("writeChecked returned %v, want nil (errors are reported, not returned)", err)
	}

	if got := buf.String(); !strings.Contains(got, "sink is gone") {
		t.Fatalf("internal error output = %q, want the downstream failure reported in it", got)
	}
}
