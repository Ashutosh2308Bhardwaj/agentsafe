# Scorecard

"Production grade" made measurable: 35 pass/fail items. **Score = items passing ÷ 35 × 100.** An item passes only when it's verifiable (a CI job, a test, a file, a public badge); "in progress" and "written but not yet run" count as not passing.

**Current: 19 / 35 → 54 / 100** (after step 4.1, idiomatic API)

History: baseline 20 → 2.1 lease 29 → 2.2 log versions 31 → 2.3 torn tails 34 → 2.4 hash chain 37 → 2.5 authorizer 40 → 2.6 sealing + redaction 43 → 2.7 timeouts 46 → 2.8 typed reconcile 49 → 3 storage backends 51 → 4.1 idiomatic API 54

## Correctness (2/5)
- [x] `go test -race` clean on every package
- [ ] Fuzz tests: log reader, `Rebuild`, `Canonical`
- [ ] Property-based tests on the state machine (random event sequences: valid histories rebuild, invalid ones are rejected)
- [ ] Coverage ≥ 90% of the core package (baseline 71.8%, now 78.2%)
- [x] Crash harness (`scripts/money_sweep.sh`: kill -9 at 8 points, reconciled) exits non-zero on failure, wired into CI

## Production blockers (9/9)
- [x] Run lease: two processes can't both drive one run. File log: OS lock (flock / LockFileEx), released by the kernel even on kill -9 (`lock_test.go`). Database backends: lease + fencing tokens, verified by the conformance suite in phase 3
- [x] Storage interface with SQLite and Postgres backends (separate modules; the core stays dependency-free): `LineStore` + `Journal`; `sqlite/`, `postgres/` (Postgres 14 and 17 in CI); every backend passes `storetest`; leases renew and expire on the database clock, and the conditional append fences a holder that lost its lease
- [x] Versioned log format with a migration story: every event carries `v`; older events upgraded on read, newer refused (`ErrNewerLogFormat`); golden logs incl. a real v0 run must keep rebuilding ([FORMAT.md](FORMAT.md))
- [x] Recovery from a torn last line (power loss mid-append): unacknowledged tail dropped and repaired on next append; damage before valid lines refused (`ErrCorruptLog`); proven by cutting a real log at every byte (`torn_test.go`)
- [x] Tamper-evident log: every line links to the hash of the one before (optional HMAC key); every Read verifies, a tampered run is refused (`ErrTampered`); `Head()` for anchoring the last line (`chain.go`, `chain_test.go`)
- [x] Approver authorization hook (who may approve what): `authz.go`, `authz_test.go`; refused attempts logged as `approval_denied`; safe default refuses
- [x] Redaction hook for sensitive tool arguments/results before they're logged. At rest: sealed log (`seal.go`: AES-256-GCM, rotation, crypto-shredding; resume still sees real values). Leaving the process: `Runner.Redact` for console output, `Trace.Redact` before export (`redact.go`)
- [x] Per-tool timeouts and panic recovery: `exec.go`, `exec_test.go`; unknown outcome ≠ failure (idempotent: same-key retry, then `ErrInDoubt` with nothing logged; others: model told it's unknown); panics recovered, never auto-retried
- [x] Typed field comparison in `Reconcile` (no `"4200" == 4200`): `compare.go` `Same` + `Decimal`; missing ≠ null; float drift reported; claims typed too

## Quality gates (5/7)
- [x] golangci-lint (strict config) clean
- [x] staticcheck clean
- [x] govulncheck clean (CI, latest Go)
- [x] gofmt clean
- [ ] Go Report Card A+ (needs the public repo)
- [ ] OpenSSF Scorecard ≥ 8 (needs the public repo, branch protection, pinned actions, SECURITY.md)
- [x] CI green on Linux, macOS and Windows × Go 1.23 and stable

## Integration (2/7)
- [x] Idiomatic API: context everywhere, functional options, `errors.Is`-able sentinel errors: every I/O call takes a ctx (Log, Locker, Journal, Codec; storetest checks a cancelled append stores nothing); `New(model, log, opts...)` validates the whole setup at once (`ErrConfig`); every error a caller may act on is a sentinel (`errors.go`, `TestSentinelErrors`)
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
