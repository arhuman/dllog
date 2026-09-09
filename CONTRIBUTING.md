# Contributing

How to build, test, and submit changes. Keep it short and true; delete sections that do not apply.

## Setup

```bash
# clone, then bring up dependencies (DB, etc.)
TODO
```

## Make targets

| Target | Purpose |
| ------ | ------- |
| `make build` | Compile the binary (version-stamped via `-ldflags`). |
| `make test` | Unit + integration tests. |
| `make fulltest` | All tests including DB-backed paths. Run before committing storage/handler changes. |
| `make cover` | Tests with coverage; fails below `COVER_MIN`. |
| `make audit` | `go vet` + `golangci-lint` + `govulncheck` + coverage gate. Same command locally and in CI. |
| `make tidy` | `go fmt` + `go mod tidy`. |
| `make ci` | Full local pipeline (`tidy` + `audit` + `fulltest`). |
| `make release` | Derive the next semver from Conventional Commits, gate via `make ci`, tag and push. |

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/): `type(scope): subject`, type one of
`feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert`. Enforced locally (commit-msg hook)
and in CI (commitlint on PRs).

## Before opening a PR

1. `make ci` passes.
2. Update `CHANGELOG.md` under `[Unreleased]`.
3. TODO: project-specific checks (e.g. verify UI flows in a browser).
