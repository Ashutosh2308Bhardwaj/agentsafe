# Scorecard

"Production grade" made measurable: 35 pass/fail items. **Score = items passing ÷ 35 × 100.** An item passes only when it's verifiable (a CI job, a test, a file, a public badge); "in progress" and "written but not yet run" count as not passing.

**Current: 29 / 35 → 83 / 100** (after step 6.1, SECURITY.md + CONTRIBUTING.md)

History: baseline 20 → 2.1 lease 29 → 2.2 log versions 31 → 2.3 torn tails 34 → 2.4 hash chain 37 → 2.5 authorizer 40 → 2.6 sealing + redaction 43 → 2.7 timeouts 46 → 2.8 typed reconcile 49 → 3 storage backends 51 → 4.1 idiomatic API 54 → 4.2 tool middleware 57 → 4.3 Anthropic 60 → 4.4 Gemini 63 → 4.5 examples 66 → 4.6 quickstart 69 → 5.1 fuzzing 71 → 5.2 property tests 74 → 5.3 coverage 77 → 6.1 security + contributing 83

## Correctness (5/5)
- [x] `go test -race` clean on every package
- [x] Fuzz tests: log reader, `Rebuild`, `Canonical`: 7 targets (`fuzz_test.go`: log reading + torn tails, Rebuild, Canonical exactness and stability, sealed payloads, Same, wrapped tools' arguments), each fuzzed 30s per CI run; found a real key collision in `Canonical` (numbers above 2^53), fixed without changing any other key
- [x] Property-based tests on the state machine (random event sequences: valid histories rebuild, invalid ones are rejected): `prop_test.go`: random legal histories and all their prefixes rebuild with invariants after every event; 8 corruption classes always refused (with a guard that each actually occurs); the real runner under random plans, decisions and crashes never pays a key twice or pays a rejected one. Found: a rejected payout re-proposed by the model was put to the human again and could be approved; now refused automatically, and invalid in the log
- [x] Coverage ≥ 90% of the core package: 96.7% (baseline 71.8%); every reachable error path tested, including injected write/fsync/close/truncate failures; the rest can't fail by construction (listed in the design doc). Gained by testing error paths, not padding: the OpenAI-compatible adapter (was 0%: retries, Retry-After, pacing, 4xx not retried, cancellation, Native never sent), Canonical's exact-number path, every schema input kind, a failing model, an unknown tool, trace outcomes
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

## Integration (7/7)
- [x] Idiomatic API: context everywhere, functional options, `errors.Is`-able sentinel errors: every I/O call takes a ctx (Log, Locker, Journal, Codec; storetest checks a cancelled append stores nothing); `New(model, log, opts...)` validates the whole setup at once (`ErrConfig`); every error a caller may act on is a sentinel (`errors.go`, `TestSentinelErrors`)
- [x] Tool middleware that wraps any existing tool function: `Func(name, desc, fn, Idempotent(...), NeedsApproval(...), Check(...), Timeout(...))`, schema generated from the input struct, strict decoding, key passed via `KeyFrom(ctx)`; `tooltest.SameKey` checks users' own tools for check-then-act races
- [x] Model adapter: OpenAI-compatible (Groq, OpenAI, Ollama, vLLM, …)
- [x] Model adapter: Anthropic: `agentsafe/anthropic` on the official Go SDK; assistant turns stored verbatim (`Message.Native`) so a resumed run replays Claude's thinking blocks byte for byte; a fake Messages API that enforces append-only history proves it across an approval pause, a restart and a crash
- [x] Model adapter: Gemini: `agentsafe/gemini` on the official Go SDK (stateless generateContent, so the history lives in agentsafe's log); model turns replayed verbatim with their thought signatures; deterministic ids for calls Gemini sends without one, never sent back; same fake-API journey as Claude (approval pause, restart, crash)
- [x] Runnable `Example` functions on pkg.go.dev for every primitive: core (`example_test.go`: the payout flow, New, Func, crash resume, authorized approvals, sealed log, Reconcile, Same, redaction, traces), `tooltest`, `sqlite` (all output-checked by `go test`); `postgres`, `anthropic`, `gemini` (compiled; they need a server or a key)
- [x] 5-minute quickstart in the README: offline, no API key; pay-once-after-approval across three processes; `examples/quickstart` is tested as three real processes, and a test fails if the README block differs from it

## Release hygiene (3/7)
- [x] LICENSE (Apache-2.0)
- [x] SECURITY.md (how to report a vulnerability): private reporting via GitHub advisories; what counts (duplicate effects, approval bypass, history forgery, sealed-data exposure, wrong reconciliation, exhaustion from model input, fencing failure) and what doesn't
- [x] CONTRIBUTING.md: setup, the gates, "a test that fails without it" (and that the broken version compiles), no dependencies in the core, format and key compatibility rules, backends must pass storetest
- [ ] CHANGELOG.md
- [ ] Semver tag (v0.x until the API is stable)
- [ ] Threat model
- [ ] Design doc (the state machine, the guarantees, what's out of scope)
