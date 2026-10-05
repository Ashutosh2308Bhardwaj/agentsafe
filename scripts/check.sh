#!/usr/bin/env bash
# Every local quality gate, in CI order. Stops at the first failure (set -e), so a commit can't go out
# with a gate red. Run before every commit: scripts/check.sh
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PATH:$(go env GOPATH)/bin"

step() { printf '\n== %s\n' "$1"; }
step gofmt;            test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
step "go vet (unix)";  go vet ./...
step "go vet (windows)"; GOOS=windows go vet ./...
step "golangci-lint (unix)";    golangci-lint run ./...
step "golangci-lint (windows)"; GOOS=windows golangci-lint run ./...
step staticcheck;      staticcheck ./...
# Vulnerabilities are judged against the latest Go release (what CI and users should run), not whatever
# toolchain happens to be installed locally; the go command downloads it on demand.
step govulncheck
latest="$(curl -sf 'https://go.dev/VERSION?m=text' | head -1)"
GOTOOLCHAIN="${latest:-auto}" go run golang.org/x/vuln/cmd/govulncheck@latest ./... >/dev/null
step "tests (race)";   go test -race -count=1 ./...
step coverage;         go test -coverprofile=coverage.out . >/dev/null && go tool cover -func=coverage.out | tail -1
step "crash harness";  scripts/money_sweep.sh >/dev/null
step "crash harness (sealed log)"; AGENTSAFE_LOG_KEY=4242424242424242424242424242424242424242424242424242424242424242 scripts/money_sweep.sh >/dev/null
printf '\nall gates green\n'
