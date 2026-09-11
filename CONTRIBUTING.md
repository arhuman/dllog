# Contributing

How to build, test, and submit changes.

## Setup

No services to bring up: the only dependency is zap, and it is confined to
`zapadapter`.

```bash
git clone https://github.com/arhuman/dllog
cd dllog
make ci
```

## Make targets

| Target | Purpose |
| ------ | ------- |
| `make build` | Compile every package. |
| `make test` | Short unit tests. |
| `make fulltest` | All tests with the race detector and coverage. |
| `make bench` | Benchmarks with allocation counts. |
| `make cover` | Tests with coverage; fails below `COVER_MIN`. |
| `make audit` | Coverage gate, `checkcore`, `checkpromotion`, `go mod verify`, `golangci-lint`, `govulncheck`. Same command locally and in CI. |
| `make checkcore` | Fail if `internal/` depends on `log/slog`. |
| `make checkpromotion` | Fail if our code imports a module `go.mod` marks indirect. |
| `make tidy` | `go fmt` + `go mod tidy`. |
| `make ci` | Full local pipeline (`tidy` + `audit` + `fulltest`). |

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/): `type(scope): subject`, type one of
`feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert`. Enforced locally (commit-msg hook)
and in CI (commitlint on PRs).

## Before opening a PR

1. `make ci` passes.
2. Update `CHANGELOG.md` under `[Unreleased]`.
3. Keep the invariants: `internal/` imports no logging library, the two adapters
   stay peers (neither imports the other), and memory stays hard-bounded. The
   first two are enforced by `make checkcore` and `make checkpromotion`.
4. Changing a benchmark figure in `README.md` means re-running `make bench` and
   quoting the new run, not adjusting the old number.
