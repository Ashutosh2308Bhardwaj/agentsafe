# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version (0.x.0) may contain breaking changes; each is listed under **Breaking**.

Each module is versioned and tagged separately: `v0.1.0` is the core (`github.com/Ashutosh2308Bhardwaj/agentsafe`); `sqlite/v0.1.0`, `postgres/v0.1.0`, `anthropic/v0.1.0` and `gemini/v0.1.0` are the modules in those directories.

## [Unreleased]

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

[Unreleased]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/releases/tag/v0.1.0
