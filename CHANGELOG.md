# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version (0.x.0) may contain breaking changes; each is listed under **Breaking**.

Each module is versioned and tagged separately: `v0.1.0` is the core (`github.com/Ashutosh2308Bhardwaj/agentsafe`); `sqlite/v0.1.0`, `postgres/v0.1.0`, `anthropic/v0.1.0` and `gemini/v0.1.0` are the modules in those directories.

## [Unreleased]

### Breaking

- **Idempotency keys are 128 bits (32 hex characters) for new runs**, up from 64 (16): at 64 bits, a key collision
  between two operations sent to the same provider would silently replay the first one's result for the second.
  **Runs started before this version keep their 64-bit keys to the end** (log format v4 records `key_bits` on
  `run_started`; a pre-v4 run reads as 64), so a run paused for approval across the upgrade still pays once,
  under the key that was approved. If you store or match keys yourself (e.g. a column sized for 16
  characters, or a provider with a short idempotency-key limit), allow 32.
- Log format v4. A v0.2 library refuses v4 logs (`ErrNewerLogFormat`): don't roll back a library under a
  run that a newer one has written to.

### Fixed

- `openai.Model` is safe for concurrent use: runners sharing one Model raced on its rate-limit state
  (`TestOpenAIModelIsSafeForConcurrentUse`, under `-race`).
- `New` refuses a `Journal` whose store has no lease (unless `WithoutLease`). A Journal always has a `Lock`
  method, so it passed validation and failed at the first `Start` instead. `Journal.CanLock` reports it.

### Security

- `gemini`: indirect dependencies upgraded past known advisories: `golang.org/x/crypto` v0.56.0 (GO-2026-6354,
  GO-2026-6355 and 13 earlier ones), `golang.org/x/net` v0.58.0 (GO-2026-5942), `google.golang.org/grpc` v1.83.2
  (GO-2026-6443). None was reachable from agentsafe's code (govulncheck), but they showed up in every user's
  dependency scan. **`gemini` now requires Go 1.26** (x/crypto v0.56.0 does); Go 1.25 left upstream support
  when Go 1.27 was released. GO-2026-5932 (x/crypto `openpgp`, no fix will ever exist) is ignored in
  `gemini/osv-scanner.toml`: nothing imports `openpgp`.
- `postgres`: `golang.org/x/text` v0.39.0 (GO-2026-5970).

## [0.2.0] - 2026-10-06

A restructure before the API settles: the core package is split, the largest functions are broken up, and a
`Runner` written as a struct literal is checked like one built with `New`. Every module moves to v0.2.0.

### Breaking

The core package is split so each part can be found and used on its own. To migrate:

| v0.1.0 | now |
|---|---|
| `agentsafe.OpenAICompatible{...}` | `openai.Model{...}` (`github.com/Ashutosh2308Bhardwaj/agentsafe/openai`), same fields |
| `agentsafe.BuildTrace(events, agent)` | `trace.Build(events, agent)` (`.../agentsafe/trace`) |
| `agentsafe.Trace`, `Span`, `SpanEvent`, `Attr` | `trace.Trace`, `trace.Span`, `trace.SpanEvent`, `trace.Attr`; methods unchanged (`Tree`, `OTLPJSON`, `Redact`) |
| `agentsafe.Reconcile(expected, actual, events, claims)` | `reconcile.Audit(expected, actual, events, claims)` (`.../agentsafe/reconcile`) |
| `agentsafe.Effect`, `Claim`, `Report`, `Finding`, `Severity`, `Critical`, `Error`, `Warn`, `Same` | the same names in `reconcile` |
| `agentsafe.Decimal` | unchanged (used by `Func` schemas and by `reconcile`) |
| `agentsafe.Seal(ctx, e, c)`, `agentsafe.Open(ctx, e, c)` | `agentsafe.SealEvent`, `agentsafe.OpenEvent` (the `Codec` interface's `Seal`/`Open` are unchanged) |

### Changed

- A `Runner` written as a struct literal is validated like one built with `New`, before it reads or writes
  anything; a run whose gated tools nobody may approve is now refused at `Start` (`ErrConfig` wrapping
  `ErrNoAuthorizer`) instead of at its first decision.
- `Reconcile` reports findings of equal severity in a stable order.

### Modules

- **`sqlite/v0.2.0`, `postgres/v0.2.0`**: the lease heartbeat is shared code (`internal/heartbeat`); behaviour
  unchanged. They require core v0.2.0.
- **`anthropic/v0.2.0`, `gemini/v0.2.0`**: no changes besides requiring core v0.2.0.

## [0.1.0] - 2026-10-06

The first release: correctness primitives for LLM agents that act on money, hardened from a four-week study of how real models fail (see [docs/FAILURES.md](docs/FAILURES.md)).

### Core (`v0.1.0`)

- **Runner**: a stateless agent loop over an event log. `New(model, log, opts...)` checks the whole configuration at startup and reports every problem at once (`ErrConfig`). `Start`, `Continue`, `Approve`, `Reject`, `Extend`; every I/O call takes a `context.Context`.
- **Event log and state machine**: eleven event types; `Rebuild` refuses impossible histories (`ErrInvalidTransition`); every action is recorded, durably, before it happens.
- **Idempotent tools**: keys from business identity, never the model's call id; completed operations replayed from the log; the same key with different values is a conflict; the key is passed to your code (`KeyFrom`) for the system you call.
- **`Func`**: wrap any `func(ctx, In) (Out, error)` as a tool, with a JSON schema generated from `In`, strict argument decoding, and the options `Idempotent`, `NeedsApproval`, `ApprovalIf`, `Check`, `Timeout`.
- **Approval gate**: durable waits addressed by operation key; `Authorizer` (`AllowList`, `NotRequester`, `All`, your own); with none, every decision is refused; refused attempts are logged; a rejection sticks to the operation.
- **Timeouts and panics**: an unknown outcome is in doubt, never failed: idempotent calls are retried with the same key, then `ErrInDoubt`; panics are recovered, never retried automatically.
- **Storage**: `LineStore` + `Journal`; the file store (`FileLog`, `NewFileStore`); a conditional append that fences stale writers (`ErrConflict`); an OS-lock lease; torn-tail recovery; a failed write or fsync poisons the handle (`ErrStorePoisoned`).
- **Integrity**: a hash chain verified on every read (HMAC with a key), `Head()` for anchoring; a versioned log format (v3) with upgrades on read and golden logs.
- **Confidentiality**: sealing at rest (`Codec`, `AESGCM` with key rotation), and redaction of console output and traces (`RedactFields`, `RedactPattern`).
- **Reconciliation**: `Reconcile` checks a run against the systems of record (exactly once, authorized, once per approval, provenance and claims), comparing values by type (`Same`, `Decimal`).
- **Traces**: `BuildTrace` derives OpenTelemetry GenAI spans from the log; OTLP/JSON export; a tree view; `cmd/trace`.
- **Model adapter**: `OpenAICompatible` for Groq, OpenAI, Ollama, vLLM and others.
- **Testing helpers**: `storetest` (conformance suite for storage backends), `tooltest` (`SameKey`: does your system really act once per key, even for simultaneous calls?).

### Modules

- **`sqlite/v0.1.0`**: many runs in one SQLite file (pure Go); a lease row with heartbeat and expiry. Go 1.26+.
- **`postgres/v0.1.0`**: runs shared between machines; fencing by primary key under READ COMMITTED; lease times on the database's clock. Go 1.25+.
- **`anthropic/v0.1.0`**: Claude via the official SDK; each turn stored and replayed verbatim, so a resumed run sends exactly what it sent before (thinking blocks are bound to the conversation). Defaults: `claude-opus-5-5`, effort `high`, server-side refusal fallback. Go 1.25+.
- **`gemini/v0.1.0`**: Gemini via the official SDK, on stateless generateContent; thought signatures replayed verbatim. Go 1.25+.

### Fixed during hardening (never released)

- Two different numbers above 2^53 shared one idempotency key, so a second payout could be treated as already done (found by fuzzing).
- A payout a human rejected could be approved if the model proposed it again under a new call id (found by property tests).
- The example gateway charged once per key only for sequential calls, not simultaneous ones.

[Unreleased]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/releases/tag/v0.1.0
