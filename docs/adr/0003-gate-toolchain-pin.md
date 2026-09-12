# 3. Pin the gate toolchain in .go-version, decoupled from the go.mod floor

Date: 2026-09-12

## Status

Accepted

## Context

Two Go versions serve two different jobs in this repo, and conflating them has
now broken CI twice.

The `go 1.24` directive in `go.mod` is the support floor: the oldest toolchain
a consumer can build the module with. The CI test matrix exercises it directly
(`1.24` and `stable`).

The quality gate is a different animal. `make audit` installs pinned dev tools,
and golangci-lint v2.13.2 requires Go >= 1.26 to install. `actions/setup-go`
sets `GOTOOLCHAIN=local`, so a job handed the 1.24 floor cannot download the
newer toolchain and the install fails outright. The gate cannot track `stable`
either: the pinned linter's staticcheck must understand the toolchain's stdlib,
and a Go newer than it knows panics it. The gate toolchain must therefore be
pinned exactly, and it moves with the tool pins, not with the floor.

History of the breakage:

- The lint job originally followed `go.mod` and failed; commit 43ac698 fixed it
  by pinning `go-version: "1.26"` inline, with the rationale in a comment.
- `release.yml` still read `go-version-file: go.mod`. When commit 76021ad
  lowered the floor from 1.25 to 1.24, the v0.1.0 tag verification failed on
  the same tool install, in the workflow the first fix never covered.

The failure mode is two workflows encoding the same fact independently.

## Decision

The gate toolchain version lives in exactly one file: `.go-version` at the repo
root.

- Every workflow job that runs the pinned tools (the CI lint job, the release
  verification job) sets up Go with `go-version-file: .go-version`.
- Only the CI test matrix reads the floor, and it is the only place the floor
  needs CI coverage.
- `GOTOOLCHAIN=local` (the setup-go default) stays: a version mismatch must
  fail loudly at install time rather than silently download another toolchain.
- Raising the linter pin (`GOLANGCI_VERSION` in the Makefile) and raising
  `.go-version` are one change; a bump that needs a newer Go bumps both.
- The go.mod floor is never raised for tooling reasons. It moves only when the
  library itself needs a newer language or stdlib.

## Consequences

- The lint and release-verify jobs build and test with the gate toolchain, not
  the floor. Floor coverage comes exclusively from the test matrix, which runs
  on every push, including the one a tag points at.
- The v0.1.0 verification run stays red: a re-run reads the workflow file from
  the tag itself, so no fix can turn it green. The fix applies from the next
  tag onward, and the red run remains an accurate record.
- A future recurrence of the install error has a single file to update and this
  ADR to explain it.
