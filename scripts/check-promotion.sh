#!/usr/bin/env bash
# Fails if our own code imports a module that go.mod marks indirect.
#
# An indirect module is one nobody here chose: it is in the graph only because a
# dependency pulled it. Importing it directly promotes it to a functional
# dependency without anyone arbitrating that, and it then starts pulling its own
# structural dependencies into our perimeter. Catching it here keeps the chain
# at depth 1 rather than discovering it at depth 4.
#
# The comparison is against packages OUR files import, not against the whole
# build graph: go list -deps reports every transitive module, so comparing that
# to the direct requires flags every ordinary indirect dependency and can never
# signal a real promotion.
set -euo pipefail

cd "$(dirname "$0")/.."

self=$(go list -m)

# Modules backing the packages our own files import, tests included: a promotion
# through a _test.go file is still a promotion.
imported=$(mktemp)
resolved=$(mktemp)
direct=$(mktemp)
trap 'rm -f "$imported" "$resolved" "$direct"' EXIT

go list -f '{{range .Imports}}{{.}}
{{end}}{{range .TestImports}}{{.}}
{{end}}{{range .XTestImports}}{{.}}
{{end}}' ./... | sort -u | grep -v '^$' >"$imported"

# shellcheck disable=SC2046 # word splitting is the point: one arg per package.
go list -f '{{if .Module}}{{.Module.Path}}{{end}}' $(cat "$imported") 2>/dev/null |
	sort -u | grep -v '^$' | grep -vx "$self" >"$resolved"

go mod edit -json |
	python3 -c 'import json,sys
d = json.load(sys.stdin)
for r in d.get("Require") or []:
    if not r.get("Indirect"):
        print(r["Path"])' | sort >"$direct"

promoted=$(comm -23 "$resolved" "$direct")

if [ -n "$promoted" ]; then
	echo "check-promotion: our code imports a module go.mod marks indirect:" >&2
	echo "$promoted" >&2
	echo "run 'go mod tidy' to make it a direct require, or stop importing it" >&2
	exit 1
fi

echo "check-promotion: ok (no indirect module is imported directly)"
