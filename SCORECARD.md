# Scorecard

"Production grade" made measurable: 35 pass/fail items. **Score = items passing ÷ 35 × 100.** An item passes only when it's verifiable (a CI job, a test, a file, a public badge); "in progress" and "written but not yet run" count as not passing.

**Current: 7 / 35 → 20 / 100** (baseline, 2026-10-05)

## Correctness (2/5)
- [x] `go test -race` clean on every package
- [ ] Fuzz tests: log reader, `Rebuild`, `Canonical`
- [ ] Property-based tests on the state machine (random event sequences: valid histories rebuild, invalid ones are rejected)
- [ ] Coverage ≥ 90% of the core package (baseline 71.8%)
- [x] Crash harness (`scripts/money_sweep.sh`: kill -9 at 8 points, reconciled) exits non-zero on failure, wired into CI

## Production blockers (0/9)
- [ ] Run lease with fencing: two processes can't both drive one run
- [ ] Storage interface with SQLite and Postgres backends (separate modules; the core stays dependency-free)
- [ ] Versioned log format with a migration story
- [ ] Recovery from a torn last line (power loss mid-append)
- [ ] Tamper-evident log (hash chain), verifiable offline
- [ ] Approver authorization hook (who may approve what)
- [ ] Redaction hook for sensitive tool arguments/results before they're logged
- [ ] Per-tool timeouts and panic recovery
- [ ] Typed field comparison in `Reconcile` (no `"4200" == 4200`)

## Quality gates (3/7)
- [x] golangci-lint (strict config) clean
- [x] staticcheck clean
- [ ] govulncheck clean (baseline: 7 known vulns in the Go 1.24.9 stdlib, none reachable; fixed by toolchain upgrade)
- [x] gofmt clean
- [ ] Go Report Card A+ (needs the public repo)
- [ ] OpenSSF Scorecard ≥ 8 (needs the public repo, branch protection, pinned actions, SECURITY.md)
- [ ] CI green on Linux, macOS and Windows (workflow written; not yet run)

## Integration (1/7)
- [ ] Idiomatic API: context everywhere, functional options, `errors.Is`-able sentinel errors
- [ ] Tool middleware that wraps any existing tool function
- [x] Model adapter: OpenAI-compatible (Groq, OpenAI, Ollama, vLLM, …)
- [ ] Model adapter: Anthropic
- [ ] Model adapter: Gemini
- [ ] Runnable `Example` functions on pkg.go.dev for every primitive
- [ ] 5-minute quickstart in the README

## Release hygiene (1/7)
- [x] LICENSE (Apache-2.0)
- [ ] SECURITY.md (how to report a vulnerability)
- [ ] CONTRIBUTING.md
- [ ] CHANGELOG.md
- [ ] Semver tag (v0.x until the API is stable)
- [ ] Threat model
- [ ] Design doc (the state machine, the guarantees, what's out of scope)
