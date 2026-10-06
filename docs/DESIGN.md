# Design

How agentsafe works, why it was built this way, and what it leaves out. The threats it answers are in [THREAT_MODEL.md](THREAT_MODEL.md); the failures it was built from are in [FAILURES.md](FAILURES.md).

## The problem in one paragraph

An LLM agent that calls tools is a distributed system with an unreliable, non-deterministic client. It retries after outcomes it couldn't see, under new call ids; it can't tell "failed" from "succeeded but unconfirmed"; it proposes wrong values with confidence and reports actions it didn't take. Its process crashes like any other. agentsafe makes that safe for consequential actions (payments first) by treating the agent like any untrusted client of a payments system: every action is recorded before it happens, identified by what it *is* rather than by who asked, gated by people who are allowed to decide, and checked afterwards against the systems of record.

## Architecture

```
            ┌──────────── your process ────────────────────────────────────────┐
 model ◀──▶ │ Runner (holds no run state)                                       │
 adapter    │   Start / Continue / Approve / Reject / Extend                    │
            │     │  every event: validate (State.Apply) → append → then act    │
            │     ▼                                                             │
            │ Journal  (encoding · sequence · hash chain · sealing · verify)    │
            │     │                                                             │
            │     ▼                                                             │
            │ LineStore: file │ SQLite │ Postgres │ yours (must pass storetest)  │
            │                                                                   │
            │ Tools: Func(...) → Validator · IdempotentTool · Gated · Timeout   │
            └──────────────────────────────────────────────────────────────────┘
 offline:  Rebuild(events) → State      reconcile.Audit(expected, actual, events, claims)
           trace.Build(events) → OpenTelemetry spans
```

- **The log is the only state.** A `Runner` reads the log, rebuilds the run (`Rebuild`), and acts. Any process can pick a run up: after a crash, after an approval that took a week, on another machine.
- **The state machine (`State.Apply`) is the only code that changes state**, and it refuses impossible histories (`ErrInvalidTransition`).
- **The core module is standard library only.** Storage backends and model adapters with dependencies are separate modules.

## The event log

Eleven event types are the whole vocabulary of a run:

| Event | Means |
|---|---|
| `run_started` | the system prompt, the task, the budget, who started it |
| `model_decided` | the model's full response, logged before anything acts on it |
| `tool_started` | the runner is about to execute a call: from here it **may have executed** |
| `tool_result` | the call's outcome, as given to the model (or replayed from an earlier one) |
| `tool_refused` | resolved without being attempted: failed `Check`, rejected, or a rejected operation re-proposed |
| `approval_requested` | a gated call waits for a human, durably |
| `approval_decided` | approved or rejected, by whom, why |
| `approval_denied` | someone the `Authorizer` refused tried to decide |
| `run_paused` | the budget is spent: stopped, not finished |
| `budget_extended` | someone gave a paused run more steps, on the record |
| `run_finished` | terminal: nothing follows it |

The run's status is derived, never stored:

```
new ──run_started──▶ awaiting_model ──model_decided (calls)──▶ executing ──last call resolved──▶ awaiting_model
                        │    │                                  │    ▲
                        │    └─model_decided (no calls)─▶ answered ──run_finished──▶ finished
                        │                                       │    │
                        └─run_paused─▶ paused ─budget_extended─┘    approval_requested ▼ │ approval_decided
                                                                   awaiting_approval ───┘
```

What `Apply` refuses, by design (each is a test in `state_test.go`, `gate_test.go` or `prop_test.go`): a result for a call that never started; a second result for one call or one idempotency key; finishing with a call unresolved; starting a call whose approval is pending or rejected; refusing a call that already started (it may have run); asking again about an operation already rejected in this run; anything after `run_finished`. `TestPropertyLegalHistoriesAndEveryPrefixRebuild` checks that every reachable history and every prefix of it (a crash leaves a prefix) is accepted, and `TestPropertyCorruptionsAreRefused` that eight kinds of corruption never are.

## The write-ahead rule

`Runner.emit` validates an event against a copy of the state, appends it (durably: `fsync` before returning), and only then applies it. **Every action is preceded by the record of the intent to act**: `tool_started` is written before the tool is called. So for any crash point, the log says enough to recover:

| Crash right after | The log shows | Resume does |
|---|---|---|
| the model answered | nothing new | asks again (model calls are side-effect free) |
| `model_decided` | proposed calls, none started | runs them |
| `tool_started` | started, no result | **may have executed**: calls again **with the same key**; the tool dedupes |
| the tool acted | started, no result | the same: the key goes back to the tool |
| `tool_result` | the outcome | continues |
| `approval_requested` / `_decided` | the request / decision | waits / proceeds |

`scripts/money_sweep.sh` kills the process with SIGKILL at each of these points on every CI run (plain and sealed), and `TestPropertyRunnerUnderRandomCrashesPaysAtMostOnce` crashes at random points in random runs. A write that fails is treated the same way: the run stops before acting (`TestEveryLogWriteFailureStopsTheRunBeforeItActs`).

## Idempotency: exactly-once from at-least-once

Delivery from a model is at-least-once whatever you do, so exactly-once has to come from the effect:

- **The key is the operation's business identity** (`Idempotent("invoice_id")`), hashed with the tool name and a scope, never the call id: a model retrying under a new call id gets the same key.
- **The log is the idempotency store for outcomes it has seen**: a completed key is replayed from the log, and nothing runs. The same key with a different payload (the amount changed) is a conflict the model is told about.
- **The tool is the idempotency store for outcomes the log never saw** (a crash after the effect, before its record): it receives the key (`KeyFrom(ctx)`) and must pass it to the system it calls. `tooltest.SameKey` checks that system for check-then-act races.
- **An unknown outcome is in doubt, never failed**: a timeout, a panic, or `ErrOutcomeUnknown` leaves no result in the log for an idempotent call; it is retried with the same key, and if still unknown the run stops with `ErrInDoubt`, exactly as after a crash. Logging "timed out" would be worse than nothing: the log would replay it as the outcome of a payment that went through.
- **Keys must never change for existing operations.** `Canonical` keeps every number exactly (a number float64 would round keeps its decimal text), and every key it produced before stays byte-identical (`TestCanonicalUnchangedForExactNumbers`).

## Approvals

The gate runs in this order: `Check` (grounding: is the call right?) → was this operation already rejected in this run? (refused without asking) → `approval_requested` with the **checked** values as the summary → wait, durably → `Approve`/`Reject` by operation key → `Authorizer` (who may decide: allowlists, maker-checker, thresholds; with none, every decision is refused) → `approval_decided` → `tool_started`. Refused attempts are logged (`approval_denied`). A decision can't be flipped, and a rejection belongs to the operation, not to one proposal of it.

## Concurrency: leases for efficiency, fencing for correctness

A run has one driver at a time: a lease (an OS file lock released by the kernel on any exit; a row with a heartbeat and expiry on SQLite and Postgres, on the database's clock). A lease can be lost while its holder still runs (a GC pause, a VM migration). So every storage backend's append is conditional: **line N is stored only if exactly N-1 lines exist**, atomically against every writer (a unique `(run, seq)` constraint, or a lock around check-and-write). The sequence number is the fencing token: a runner that lost its lease is refused (`ErrConflict`) at its next write, and since every action is preceded by a write, before it can act. The lease only saves wasted model calls.

## Durability and integrity

- **fsync before acknowledging**; an unacknowledged (torn) last line is dropped and repaired; damage before valid lines is refused (`ErrCorruptLog`), never skipped.
- **A failed write or fsync poisons the log handle** (`ErrStorePoisoned`): no fsync retry, because after a failed fsync the data may be lost while the cache still serves it (PostgreSQL's "fsyncgate"). A new process continues from what is on disk; since the runner never acted on the failed append, both outcomes for that line are valid histories.
- **A hash chain** links every line to the previous one (HMAC with a key); every read verifies it. `Head()` lets you anchor the last line elsewhere.
- **Sealing** encrypts content at rest (arguments, results, prompts, summaries) while the audit trail stays readable; each payload is bound to its position.
- **The format is versioned** (`FormatVersion`), older events are upgraded on read, newer ones refused, and golden logs prove every format ever written still rebuilds ([FORMAT.md](../FORMAT.md)).

## Model adapters

An adapter turns the conversation into a request and the response into a `Decision`. The adapters for Claude and Gemini store each model turn as the provider returned it (`Message.Native`) and replay it verbatim, because their thinking blocks / thought signatures are bound to the exact conversation before them: a resumed run must send exactly what it sent before (`TestReplayIsAppendOnlyAcrossApprovalRestartAndCrash`, in each adapter). Every adapter maps truncation and refusals to explicit finish reasons, never to a finished answer.

## Reconciliation

`reconcile.Audit` compares three independent sources: what **should** exist (computed by plain code from source data), what **does** exist (the systems of record), and what the **log** says, plus the agent's claims. Four checks: every expected effect exists exactly once with equal values (compared by type: `4200` is not `"4200"`); every gated effect has an approved key in the log; every approval took effect at most once; every effect traces to a logged result, and every claim matches the records. It's the backstop for what the runner can't see: a history forged without approvals, a payment made outside the agent, a gateway that silently charged the wrong amount.

## Decisions and the alternatives we didn't take

| Decision | Alternative | Why |
|---|---|---|
| Event-sourced log; the runner holds no state | Snapshot the state | A snapshot can't explain itself, can't be audited, and a crash between "act" and "snapshot" loses the truth. The log is also the trace, the audit trail and the idempotency store. |
| Keys from business identity | The model's call id | The model mints a new call id on every retry (FAILURES.md, F9). |
| Write intent before acting | Record after acting | After-the-fact recording has a window where the effect happened and nothing knows. |
| Unknown outcome = in doubt | Treat a timeout as a failure | The money may have moved; the log would replay "failed" forever. |
| Fencing by sequence number | A separate fencing token | The append-only log already has a monotonic number the storage can check atomically. |
| Poison after a failed fsync | Retry the fsync | A retried fsync can succeed on data the kernel already dropped. |
| A rejection sticks to the operation | Ask again on each proposal | Re-asking until someone approves is how a rejection gets undone. |
| Strict argument decoding | Ignore unknown fields | For money, an unexpected field is a model mistake, not noise. |
| Stateless model APIs (Gemini generateContent) | Server-side conversation state | The history must live in the log, so a run paused for weeks or moved to another machine resumes. |
| Standard library only in the core | Use the official SDKs everywhere | The primitives are small; dependencies belong to the modules that need them. |
| No default Gemini model | Pick one | Its model names change faster than this library, and Google's own sources disagree. |

## What isn't tested, and why

Coverage of the core package is 97.2%. The rest falls into three groups; anything not on this list that carries a rule has a test.

**Can't fail by construction** (forcing it would need fault hooks in production code for things that can't happen):
- `json.Marshal` / `json.Unmarshal` of values this code just built or already validated: in `Canonical`, `Journal.Append`, `Seal`, `RedactFields`, `maskKeys`, `schemaOf`, `idempotentFuncTool.Identity`;
- `crypto/rand` failing (`AESGCM.Seal`), and `aes.NewCipher` on a key whose length was just checked;
- a `big.Rat` that can't parse a number the JSON decoder already accepted (`canonicalNumber`), and the non-decimal fallback in `decimalText`;
- `default:` branches of switches over closed sets (`compare.go` kinds, `Runner.loop` statuses, `trace.Trace.Tree` span names) and an upgrade step failing (`Upgrade`: every step is the identity today; the first real one comes with its own tests).

**Reachable, untested, low risk** (each returns the error, and nothing acts):
- operating-system errors other than those tested: `stat`/`open`/read failing with something other than "not found", and the blocking lock failing (`fileStore`); releasing a lease failing (only logged);
- failing to log a refused approval (`Runner.authorize` returns both errors);
- an invalid `BaseURL` or tool schema in `openai.Model`; a custom `IdempotentTool` whose identity can't be canonicalized (`keyFor`); top-level struct fields skipped by `jsonFields`;
- trace rendering helpers (`Tree` and OTLP formatting for values the runner never produces).

**Timing-dependent**: in `invoke`, a tool's result arriving at the same instant as its timeout. Without that branch the call is retried with the same key, which is still safe; testing it deterministically isn't possible.

## Out of scope

Detecting prompt injection, judging the model's plan, authentication, key management, and the correctness of your own tools; see [THREAT_MODEL.md](THREAT_MODEL.md#out-of-scope).
