.DEFAULT_GOAL := help

# ==================================================================================== #
# VARIABLES
# ==================================================================================== #

# Quality gates (ratchets: raise over time, never lower to green a build)
COVER_MIN  ?= 80
LINE_LIMIT ?= 500

# Pinned tool versions: must match 10x _shared/references/versions.md
GOLANGCI_VERSION    ?= v2.13.2
GOVULNCHECK_VERSION ?= v1.1.4

# ==================================================================================== #
# PHONY DECLARATIONS (in alphabetical order)
# ==================================================================================== #
.PHONY: audit bench build checkcore checklen checkpromotion ci clean cover fulltest help release test tidy tools

# ==================================================================================== #
# STANDARD TARGETS (in alphabetical order)
# ==================================================================================== #

## audit: run quality control checks (mod verify, lint, vuln scan, core imports, promotion, coverage gate)
audit: cover checkcore checkpromotion
	@which golangci-lint > /dev/null || $(MAKE) tools
	@which govulncheck > /dev/null || $(MAKE) tools
	go mod verify
	golangci-lint run ./...
	govulncheck ./...

## bench: run benchmarks with allocation counts, no tests
bench:
	go test -bench=. -benchmem -run=^$$ ./...

## build: compile every package
build:
	go build ./...

## checkcore: fail if internal/ depends on log/slog
checkcore:
	@./scripts/check-core-imports.sh

## checkpromotion: fail if our code imports a module go.mod marks indirect
checkpromotion:
	@./scripts/check-promotion.sh

## checklen: fail if any non-generated .go file exceeds LINE_LIMIT lines
checklen:
	@fail=0; \
	for f in $$(git ls-files '*.go' | grep -Ev '_test\.go$$|\.gen\.go$$|\.pb\.go$$'); do \
		n=$$(wc -l < "$$f"); \
		if [ "$$n" -gt "$(LINE_LIMIT)" ]; then echo "$$f: $$n lines > $(LINE_LIMIT)"; fail=1; fi; \
	done; \
	exit $$fail

## ci: run the full local CI pipeline (tidy, audit, fulltest)
ci: tidy audit fulltest

## clean: clean the Go cache and coverage output
clean:
	go clean
	rm -f coverage.out

## cover: run tests with coverage and fail below COVER_MIN
cover:
	go test -covermode=atomic -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | awk '/^total:/ {print "coverage: " $$3}'
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	awk -v t="$$total" -v min="$(COVER_MIN)" 'BEGIN { if (t+0 < min+0) { printf "FAIL: coverage %.1f%% < %d%%\n", t, min; exit 1 } }'

## fulltest: every test the repo owns, with race and coverage. The phase-closing gate.
fulltest:
	go test -race -cover ./...

## help: display this help message
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

## release: derive the next version from the commit log, gate, stamp, tag and push
release:
	@./scripts/release.sh $(VERSION)

## test: short unit tests only, seconds on a warm cache, for the red/green loop.
test:
	go test -short ./...

## tidy: format Go code and tidy the module file
tidy:
	go fmt ./...
	go mod tidy -v

## tools: install pinned Go development tools
tools:
	@echo "Installing Go tools..."
	@go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	@go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@echo "Tools installed in $(shell go env GOBIN || go env GOPATH)/bin"
