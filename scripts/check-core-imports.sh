#!/usr/bin/env bash
# Fails if internal/core imports log/slog, directly or transitively.
# The engine must stay logging-library agnostic so other adapters can reuse it.
set -euo pipefail

cd "$(dirname "$0")/.."

deps=$(go list -deps ./internal/... 2>/dev/null || true)

if grep -qx 'log/slog' <<<"$deps"; then
	echo "check-core-imports: internal/core depends on log/slog" >&2
	go list -deps -f '{{.ImportPath}}' ./internal/... | grep -x 'log/slog' >&2
	exit 1
fi

echo "check-core-imports: ok (internal/ is free of log/slog)"
