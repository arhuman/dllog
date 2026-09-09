// Package dllog provides a log/slog handler that records below-level entries
// per operation and replays them when that operation fails.
//
// The name stands for "dynamic level log". A service configured at Info keeps
// Debug records in a bounded per-operation buffer; when an error is logged or
// signalled, the buffer is replayed to the downstream handler, so the failure
// arrives with the context that preceded it. Operations that end without an
// error discard their buffer, so the verbosity costs nothing in the common case.
//
// Buffering is bound to a scope carried by a context.Context and released
// explicitly. Records logged outside any scope are passed to the downstream
// handler under the configured level, unbuffered, at the cost of an ordinary
// disabled slog call.
//
// The engine lives in internal/core and does not depend on log/slog, so the
// same machinery can back adapters for other logging libraries.
package dllog
