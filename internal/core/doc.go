// Package core implements the buffering engine behind dllog: scope lifecycle,
// the trip state machine, and the bounded ring that holds pending entries.
//
// The package is generic over the entry type it stores and must not import
// log/slog or any other logging library. That constraint is what lets a single
// engine back several adapters; scripts/check-core-imports.sh enforces it.
package core
