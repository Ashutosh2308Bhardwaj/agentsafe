# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version (0.x.0) may contain breaking changes; each is listed under **Breaking**.

Each module is versioned and tagged separately: `v0.1.0` is the core (`github.com/Ashutosh2308Bhardwaj/agentsafe`); `sqlite/v0.1.0`, `postgres/v0.1.0`, `anthropic/v0.1.0`, `gemini/v0.1.0` and `mcp/v0.1.0` are the modules in those directories. `mcp` is versioned on its own (it started at v0.1.0 with core v0.4.0).

## [Unreleased]

## [mcp/v0.5.0] - 2026-10-09

`agentsafe/mcp` only: the core stays at v0.5.0. Fixes from an outside review of mcp/v0.4.0: a suggested policy no
longer trusts what a server says about itself, and verify calls a tool exactly as the proxy does. **Upgrade from
v0.4.0** if you used `inspect --policy-out`: re-run it, or remove any `"key": "argument"` it wrote until `verify`
passes for that tool.

### Changed (breaking)

- **`inspect` never suggests a key.** v0.4.0 turned an argument named like an idempotency key (`request_id`,
  `client_token`) into `"key": "argument"`, which lets agentsafe retry an outcome it lost: safe only if the server
  deduplicates on it, which a name doesn't prove. A retry against a server that doesn't would make a second
  effect. The argument is now reported as a candidate key; `verify` checks it, and only then do you switch.
- **`inspect` doesn't trust annotations unless asked.** A tool marked read-only gets no policy (it stays hidden
  until reviewed) and every write waits for approval, whatever `destructiveHint` says: MCP calls the hints
  untrusted. `--trust-annotations` (`mcp.TrustAnnotations()`) restores v0.4.0's read-only pass and
  not-destructive-no-approval.
- `mcptest.OneEffect` and `CheckOneEffect` take the policy: the call is made as the proxy makes it, with a fresh key
  where the policy sends one. Before, it sent the agent's bare arguments, so a server that requires its key failed a
  check it would pass in service. Arguments the proxy would refuse (a missing identity field) are refused before any
  call.

### Added

- `mcp.ProxiedTool`: a tool as the proxy calls it under a policy, built by the same code as `Open`'s.
- `verify --timeout` (default 5m) bounds the whole run: a server or `--count` command that hangs ends it with an
  error.

### Fixed

- A `pass` policy's `timeout` is applied; it was accepted and ignored.
- `Hidden()` and `Unprotected()` return copies.
- `Open`'s comment said a nil policy map passes every tool through; it exposes none (fail closed since v0.2.0).

## [mcp/v0.4.0] - 2026-10-09

`agentsafe/mcp` only: the core stays at v0.5.0. Released with the [MCP retry scan](mcp/scan/README.md): 24 popular
MCP servers, 489 tools, none of the 266 that change something takes an idempotency key. **Breaking**: a policy
that protects a tool must name its `identity` (see below).

### Changed (breaking)

- **A policy that protects a tool now requires `identity`**, and one without it is refused at startup. Before, a
  policy such as `{}`, `{"key": "none"}` or `{"approval": "never"}` made every call to the tool the same operation:
  the first call ran, and every later one, for any arguments, was refused as a conflict (it failed safe, never
  twice, but the tool worked once). Give it the arguments that make a call one operation, `["*"]` for all of them,
  or `"pass": true`.

### Added

- `agentsafe-mcp inspect` (`mcp.Inspect`): lists a server's tools without calling any (read-only, destructive and
  idempotent hints, an argument that looks like an idempotency key, required arguments) and suggests a starting
  policy that fails closed: reads pass, writes get identity `["*"]`, a key argument if they have one, and approval
  unless the server says they aren't destructive. `--policy-out` writes it as a policy file, never over an
  existing one; `--json` prints the inspection; `--timeout` gives up on a server that never lists its tools.
- `"identity": ["*"]` (`mcp.AllArguments`): every argument the call has is the operation. An exact repeat is
  answered from the log, any difference is a new operation (never a conflict).
- `agentsafe-mcp verify` checks every tool that changes something, not only keyed ones: one call must make exactly
  one effect (`mcptest.OneEffect` / `CheckOneEffect`). Two effects means the server repeats the action inside a single
  call, which no proxy can stop; none means `--count` counts the wrong thing. A policy without a key is now accepted
  and reported as *not retry-safe*; with a key, the key checks follow as before.
- `verify --settle 5s` waits before each count, for an API whose lists lag its writes (GitHub's do): counted at
  once, an effect looked like none.
- `verify --json` prints the verdict (`one_effect`, `same_key`, `retry_safe`) for collecting across servers.
- `Duration` marshals as text ("10s"), and `Policy` omits empty fields: a policy written by Go loads back.

### Changed

- `verify` checks a keyed policy against the tool before making any call: a policy that doesn't fit the tool no
  longer costs a real effect.

### Fixed

- `LoadConfig` writes out the default key (`none`) for a policy that leaves `key` out. Before, `verify` read the
  empty value as a key and refused such a policy (found running it against GitHub's server).

## [mcp/v0.3.0] - 2026-10-08

`agentsafe/mcp` only: the core stays at v0.5.0.

### Added

- `agentsafe-mcp verify`: `mcptest.SameKey` from the command line, against a sandbox of the server. `--count` is a
  shell command printing how many effects exist; it refuses to run without `--sandbox`, and refuses a policy that
  sends no key. Catches a server that ignores the key and one that checks then acts without a lock, given a real
  gap between the two (a race only microseconds wide can pass).

## [0.5.0] - 2026-10-08

Hardening of the MCP proxy after an outside review: it fails closed, forwards the client's metadata (and replays it
on a retry), and can prove a server honours its key. Core v0.5.0 (`Gateway.Handle`, log format v6) and
**`mcp/v0.2.0`**; the other modules move to v0.5.0. **Breaking for `mcp`**: tools without a policy are no longer
exposed. **A v0.4 library refuses a v6 log.**

### Changed (breaking, `mcp`)

- **The proxy fails closed.** A tool without a policy is no longer passed through: it isn't exposed to the agent
  at all (not listed, not callable). `{"pass": true}` forwards a tool unprotected on purpose. `Proxy.Hidden` and
  `Proxy.Unprotected` list both kinds, and `agentsafe-mcp` warns about them at startup. A server that adds a tool
  (`delete_account`) doesn't get it forwarded until someone writes its policy.

### Added

- **Request metadata through a Gateway** (log format v6): `Gateway.Handle(ctx, Request{..., Meta})`; the tool reads it
  with `CallMetaFrom(ctx)`. It's recorded on `call_received` (sealed like the arguments), so a retry after a crash
  carries the same metadata, and it isn't part of the operation: the same call with a new trace id is a replay.
  `Gateway.Call` is unchanged. A v0.4 library refuses v6 logs.
- `mcp`: **the client's `_meta` is forwarded** to the upstream (it was dropped), merged under the proxy's idempotency
  key in `meta` mode (it was replaced). Keys under a reserved MCP prefix (the client's protocol version, client info
  and capabilities: forwarding them would present the proxy as the agent) and `progressToken` stay on the client's
  hop; agentsafe's namespace is written only by the proxy. Recorded with the call, so a retry sends the same.
- `mcptest.SameKey` / `CheckSameKey`: checks that an MCP server really deduplicates on the key a policy sends it
  (one call: one effect; 20 at once with one key: one effect, one answer), through the proxy's own adapter
  (`mcp.KeyedTool`). Catches a server that ignores the key and one that checks and acts without a lock.

### Documentation

- `mcp` is described as what it is: a tool proxy for one stdio upstream. Resources and prompts aren't proxied, and
  several upstreams and Streamable HTTP are planned, not built (`docs/MCP_PROXY.md` claimed otherwise).

## [0.4.0] - 2026-10-08

agentsafe under any agent framework: the core gains `Gateway`, for tool calls that arrive from outside an agent
loop, and the new module **`agentsafe/mcp` v0.1.0** puts it between any MCP client (LangGraph, the OpenAI Agents
SDK, Claude Code, …) and the MCP servers it uses: `agentsafe-mcp --log calls.jsonl --policy policy.json -- <server>`.
See [docs/MCP_PROXY.md](docs/MCP_PROXY.md) and [mcp/README.md](mcp/README.md). The other modules move to v0.4.0.
**A v0.3 library refuses a v5 log** (proxy runs): don't roll a library back under a log a newer one wrote.

### Added

- **Proxy runs** (log format v5): `run_started` with `kind: "proxy"` and the `call_received` event, for calls that
  arrive from outside a `Runner` (the MCP proxy, [docs/MCP_PROXY.md](docs/MCP_PROXY.md)). A proxy run stays
  `open` between calls; agent and proxy events can't mix in one run. A v0.3 library refuses v5 logs.
- `Gateway` (`OpenGateway`, `Call`, `Close`): tool calls that arrive from outside an agent loop get the
  Runner's guarantees: logged before they run, a repeated operation answered from the log, conflicts, keys.
  It settles a call a crash left unfinished: refused if it was never forwarded, retried with its key if the
  tool takes one, otherwise recorded as an unknown outcome and never run again.
- **Approvals through a Gateway.** A gated call is answered at once with `pending_approval` and its key, and the run
  stays open: other calls go on. `Gateway.Approve` / `Reject` decide by key (through the `Authorizer`, with
  maker-checker against `WithStartedBy`), and `Pending` lists what waits. The next call for the operation runs if
  approved (once; new values are a conflict), is refused if rejected, and is answered pending until then.
- `trace.Build` renders proxy runs: tools at the top level with the client that sent each call, approvals waiting
  by key, and `agentsafe.run.kind` / `agentsafe.client` in the OTLP export (agent-run traces are unchanged).
  `reconcile.Audit` reads proxy logs: a gated effect needs an approval for its key, as in an agent run.
- The Gateway takes the log's lease per call, not for its lifetime: a decision can be written from another process,
  and gateways in several processes can share one log.
- `KeyHonouring`: an `IdempotentTool` can report that the system it calls can't deduplicate on its key. The log
  still answers an operation it saw finish, but an unknown outcome is never retried: it's recorded as "outcome
  unknown" under the key, so the operation asked for again gets that answer, never a second attempt. Applies
  during a call (one attempt) and after a crash (a started call isn't re-run).
- **`agentsafe/mcp` v0.1.0** (new module, Go 1.25, official MCP Go SDK v1.8.0): `mcp.Open` / `Proxy.Server` and the
  `agentsafe-mcp` binary. Per-tool policies (`mcp.Config`): identity fields make the key; the key reaches the
  upstream in `_meta`, as a tool argument, or not at all; `approval: always`; approvers. `agentsafe-mcp pending`,
  `approve`, `reject` decide as the OS account running them. Tested by kill tests per key mode and, in CI, a
  LangGraph agent (`mcp/examples/langgraph`).
- Examples: [subscription](examples/subscription) (an HTTP call that times out or crashes after a billed change),
  [refund](examples/refund) (grounding, approval, maker-checker, a crash after the money moved) and
  [outbox](sqlite/examples/outbox) (a database write, its key and its email in one transaction), each with a
  process-level test with real kills and a control run without the key.

### Fixed

- Console lines mark where they cut a value ("…"); a cut amount could read as a different one.
- The examples' crash hooks never return after sending SIGKILL: the signal is delivered asynchronously, and a
  process could write one more log line after its "kill point". The money sweep's hook too.

## [0.3.0] - 2026-10-07

Correctness fixes from an outside review, and the bugs the new benchmarks found. Every module moves to v0.3.0.
**Read Breaking before upgrading:** keys are longer for new runs, and the log format is v4.

### Breaking

- **Idempotency keys are 128 bits (32 hex characters) for new runs**, up from 64 (16): at 64 bits, a key collision
  between two operations sent to the same provider would silently replay the first one's result for the second.
  **Runs started before this version keep their 64-bit keys to the end** (log format v4 records `key_bits` on
  `run_started`; a pre-v4 run reads as 64), so a run paused for approval across the upgrade still pays once,
  under the key that was approved. If you store or match keys yourself (e.g. a column sized for 16
  characters, or a provider with a short idempotency-key limit), allow 32.
- Log format v4. A v0.2 library refuses v4 logs (`ErrNewerLogFormat`): don't roll back a library under a
  run that a newer one has written to.

### Added

- Benchmarks (`bench_test.go`, `reconcile/bench_test.go`, `sqlite/bench_test.go`) and
  [docs/PERFORMANCE.md](docs/PERFORMANCE.md): about 16 µs of CPU per tool call plus three durable writes.

### Fixed

- `openai.Model` is safe for concurrent use: runners sharing one Model raced on its rate-limit state
  (`TestOpenAIModelIsSafeForConcurrentUse`, under `-race`).
- A `Journal` over a store without a lease could never run, even with `WithoutLease`: `Start` called its
  `Lock` anyway and failed with `ErrNoLocker`. Found by the new benchmarks.
- `sqlite`: `Open` sets `fullfsync`. On macOS, SQLite's `synchronous=FULL` issues a plain `fsync`, which
  leaves a commit in the drive's cache, where a power cut loses it. Other platforms ignore the pragma.
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

[Unreleased]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.4.0...v0.5.0
[mcp/v0.5.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/mcp/v0.4.0...mcp/v0.5.0
[mcp/v0.4.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/mcp/v0.3.0...mcp/v0.4.0
[mcp/v0.3.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/mcp/v0.2.0...mcp/v0.3.0
[0.4.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Ashutosh2308Bhardwaj/agentsafe/releases/tag/v0.1.0
