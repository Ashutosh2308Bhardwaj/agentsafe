# Scorecard

"Production grade" made measurable: 35 pass/fail items. **Score = items passing ÷ 35 × 100.** An item passes only when it's verifiable (a CI job, a test, a file, a public badge); "in progress" and "written but not yet run" count as not passing.

**Current: 12 / 35 → 34 / 100** (after step 2.3, torn-tail recovery)

History: baseline 20 → 2.1 lease 29 → 2.2 log versions 31 → 2.3 torn tails 34

## Correctness (2/5)
- [x] `go test -race` clean on every package
- [ ] Fuzz tests: log reader, `Rebuild`, `Canonical`
- [ ] Property-based tests on the state machine (random event sequences: valid histories rebuild, invalid ones are rejected)
- [ ] Coverage ≥ 90% of the core package (baseline 71.8%, now 77.1%)
- [x] Crash harness (`scripts/money_sweep.sh`: kill -9 at 8 points, reconciled) exits non-zero on failure, wired into CI

## Production blockers (3/9)
- [x] Run lease: two processes can't both drive one run. File log: OS lock (flock / LockFileEx), released by the kernel even on kill -9 (`lock_test.go`). Database backends: lease + fencing tokens, verified by the conformance suite in phase 3
- [ ] Storage interface with SQLite and Postgres backends (separate modules; the core stays dependency-free)
- [x] Versioned log format with a migration story: every event carries `v`; older events upgraded on read, newer refused (`ErrNewerLogFormat`); golden logs incl. a real v0 run must keep rebuilding ([FORMAT.md](FORMAT.md))
- [x] Recovery from a torn last line (power loss mid-append): unacknowledged tail dropped and repaired on next append; damage before valid lines refused (`ErrCorruptLog`); proven by cutting a real log at every byte (`torn_test.go`)
- [ ] Tamper-evident log (hash chain), verifiable offline
- [ ] Approver authorization hook (who may approve what)
- [ ] Redaction hook for sensitive tool arguments/results before they're logged
- [ ] Per-tool timeouts and panic recovery
- [ ] Typed field comparison in `Reconcile` (no `"4200" == 4200`)

## Quality gates (5/7)
- [x] golangci-lint (strict config) clean
- [x] staticcheck clean
- [x] govulncheck clean (CI, latest Go)
- [x] gofmt clean
- [ ] Go Report Card A+ (needs the public repo)
- [ ] OpenSSF Scorecard ≥ 8 (needs the public repo, branch protection, pinned actions, SECURITY.md)
- [x] CI green on Linux, macOS and Windows × Go 1.23 and stable

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
