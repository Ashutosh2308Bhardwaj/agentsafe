# Threat model

agentsafe sits between an LLM agent and the systems it acts on (payment gateways, ledgers, databases). This document says what it protects, against whom, how, which test proves it, and what it deliberately doesn't do. Every mitigation names the test that fails if it stops working; "residual" is what remains after it.

## What is protected

| Asset | Why it matters |
|---|---|
| **Effects**: payments, records, any irreversible action a tool takes | Doing one twice, or without authority, is the loss this library exists to prevent. |
| **Approvals**: who decided what, about which operation | A forged or bypassed approval turns the human gate into decoration. |
| **The log**: the run's history | It is the source of truth for resume, idempotency and audit. A wrong history means confident wrong actions. |
| **Content**: tool arguments and results (payees, amounts, account numbers) | Personal and financial data, at rest and on its way to consoles and tracing vendors. |
| **Availability of the run** | A run that can be hung or crashed can't finish, or finishes in an unknown state. |

## Trust boundaries

- **The model is untrusted.** Everything it proposes is a proposal: arguments are decoded strictly, checked (`Check`), gated, and recorded before anything acts. Its text is never treated as a fact about the world (`Reconcile` compares claims with the systems of record).
- **Tool input from the model is untrusted**; tool *code* is trusted: it's yours.
- **Identities passed to `Approve` / `Reject` are trusted as authenticated**: agentsafe authorizes (`Authorizer`), your system authenticates (SSO, API keys, mTLS).
- **Log storage** is trusted for durability, and *not* trusted for integrity: the hash chain detects changes (with an HMAC key, even by someone who can rewrite the whole file).
- **The process running the agent** is trusted. An attacker running code as that user is out of scope.

## Threats

### T1. A mistaken model (the common case)

| Threat | Mitigation | Proof | Residual |
|---|---|---|---|
| Retries a write after an outcome it couldn't see, or under a new call id | Idempotency key from business identity, not the call id; the log replays a completed operation; the tool gets the key (`KeyFrom`) | `TestRepeatedOperationIsReplayedFromTheLog`, `TestFuncIdempotentPaysOncePerInvoiceAndPassesTheKey`, `TestPropertyRunnerUnderRandomCrashesPaysAtMostOnce` | Exactly-once needs the system you call to honour the key: check yours with `tooltest.SameKey` |
| Same operation, different values (amount changed) | Payload hash compared on replay: a conflict the model is told about, never a second effect | `TestSameKeyDifferentAmountIsAConflict` | — |
| Re-proposes a payout a human rejected | A rejection belongs to the operation: refused without asking again; invalid in the log | `TestRejectedOperationIsNotAskedAgain`, `TestPropertyCorruptionsAreRefused` | — |
| Wrong amount or payee (grounding) | `Check` against your source before anyone is asked; refused calls are told to the model | `TestInvalidCallNeverReachesTheApprover`, `TestFuncApprovalAndCheckEndToEnd` | Only as good as your `Check` |
| Invents an argument (`payee_account`) | Strict decoding: unknown fields are refused | `TestFuncRefusesUnknownFieldsBeforeRunning` | — |
| Claims success that didn't happen | `Reconcile` checks claims and effects against the systems of record, independently of the log | `TestEachFailureIsCaught`, `TestReconcileFlagsWhatShouldntExist` | Reconcile must be run, with real system-of-record adapters |
| Answer cut off (max tokens), or declined by safety classifiers | Mapped to `length` / `refusal`, never to a finished answer; unknown stop reasons are errors | `TestStopReasons` (Claude), `TestFinishReasons` (Gemini) | — |

### T2. A manipulated model (prompt injection, hostile content in tool results)

agentsafe does not detect injection: it bounds what an injected model can do. An injected model is a T1 model that is wrong on purpose, so every T1 mitigation applies, and:

| Threat | Mitigation | Proof | Residual |
|---|---|---|---|
| Steered into a payout to an attacker | Gated tools wait for an authorized human who sees the **checked** values; `Check` grounds against your data | `TestGatedCallWaitsDurablyThenRunsOnceOnApproval`, `TestAmountThresholdPolicySeesTheValidatedSummary` | A human who approves without reading; tools you didn't gate |
| Pushed into approval fatigue (re-proposing until someone clicks approve) | Rejections stick to the operation | `TestRejectedOperationIsNotAskedAgain` | — |
| Exhausts resources with crafted arguments (huge exponents, deep nesting) | Exponents beyond ±400 refused; strict decoding; tool timeouts | `TestLargeNumbersDontShareAKey`, `FuzzFuncArgs`, `FuzzCanonicalKeepsNumbersExact` | Argument size is bounded by the provider's output limit, not by agentsafe |
| Hangs a tool so the run stalls | Per-tool timeouts; an unknown outcome is in doubt, never "failed" | `TestChargedThenHungIsRetriedWithTheSameKeyAndPaysOnce`, `TestStillUnknownAfterAllAttemptsIsLeftInDoubtNotFailed` | A tool that ignores its context keeps running in its goroutine |

### T3. An insider without approval rights

| Threat | Mitigation | Proof | Residual |
|---|---|---|---|
| Approves under a name they type | `Authorizer` decides; no `Authorizer` means every decision is refused | `TestNoAuthorizerRefusesEveryDecision`, `TestOutsiderCannotApproveAndTheAttemptIsRecorded` | Authentication of the name is yours |
| Approves their own request | `NotRequester` (maker-checker), refusing when the requester is unknown | `TestMakerCannotCheckTheirOwnRun`, `TestNotRequesterRefusesARunWithNoRecordedRequester` | — |
| Tries repeatedly, quietly | Every refused attempt is logged (`approval_denied`) and traced | `TestDenialShowsUpInTheTrace`, `TestDenialCantBeForgedInTheLog` | — |

### T4. Someone who can write the log storage

| Threat | Mitigation | Proof | Residual |
|---|---|---|---|
| Edits, inserts, deletes or reorders events (a forged approval) | Hash chain verified on every read; a tampered run is refused, never resumed | `TestTamperingIsDetected`, `TestEveryLineLinksToTheOneBefore` | — |
| Rewrites the whole file with every link recomputed | HMAC chain (`FileLog.Key`), key kept off the log's host | `TestForgedApprovalIsRefusedWithAKey`, `TestReadingAKeyedLogWithoutTheKeyFails` | **Without a key, undetectable** |
| Edits or truncates the last event | Anchor `Head()` somewhere they can't write (your database, the approval record) | `TestWithoutAKeyAForgedTailNeedsAnAnchoredHead` | Only if you anchor it |
| Writes a history that never asks for approval | `Reconcile`: every gated effect needs an approved key in the log; the runner always gates | `TestEachFailureIsCaught`, `TestReconcileGatedEffectWithoutAKey` | **`Rebuild` alone accepts it**: the state machine can't know which tools are gated. Use a key, and reconcile |
| Moves a sealed payload onto another line | Each payload is bound to its position (seq, type, call id) | `TestSealedPayloadCantBeMovedToAnotherEvent` | — |
| Damages bytes in the middle | Refused as corrupt, never skipped | `TestDamageInTheMiddleIsRefused`, `FuzzReadLog` | — |

### T5. Faults: crashes, power loss, full disks, network partitions

| Threat | Mitigation | Proof | Residual |
|---|---|---|---|
| Crash between an effect and its record | The log records intent first (`tool_started`); resume retries with the same key | `TestCrashAfterEffectDoesNotDoubleExecute`, `scripts/money_sweep.sh` (kill -9 at 8 points, in CI), `TestPropertyRunnerUnderRandomCrashesPaysAtMostOnce` | Non-idempotent tools are at-least-once by design |
| Power loss mid-write | A torn last line is dropped and repaired; acknowledged lines are never lost | `TestTornAtEveryByte`, `TestZeroFilledTailIsTorn` | — |
| A log write fails (disk full) | The run stops before acting | `TestEveryLogWriteFailureStopsTheRunBeforeItActs` | — |
| fsync fails | The log handle is poisoned (no fsync retry: "fsyncgate"); a new process continues from what's on disk | `TestFailedWritesPoisonTheStore`, `TestFsyncFailureBeforeAPayoutMeansNoPayout` | Whether the in-doubt line survives is unknown; both outcomes are valid histories |
| A provider times out, rate-limits or errors | Retries only what's safe to retry, honouring `Retry-After`; model calls are side-effect free | `TestOpenAIRetriesTransientFailuresHonouringRetryAfter`, `TestOpenAIClientErrorsAreNotRetried` | — |

### T6. Concurrent or stale runners

| Threat | Mitigation | Proof | Residual |
|---|---|---|---|
| Two processes resume one run | A lease (file lock, or a row on the database clock) | `TestSecondRunnerIsLockedOut`, `TestConcurrentRunnersOnlyOneDrives`, `TestLeaseExpiresWhenItsHolderDies` | — |
| A runner pauses (GC, VM migration), loses its lease, wakes up and writes | The conditional append is a fencing token: its next write is refused, and every action is preceded by a write | `TestPausedRunnerThatLostTheRunCannotAct`, `TestHolderThatLostItsLeaseIsFenced`, `storetest` | Both runners may call the model before one is fenced (cost, not correctness) |
| Clock skew between machines | Postgres lease times use the database's clock | `postgres` lease tests | SQLite is single-machine: one clock |

### T7. Exposure of content

| Threat | Mitigation | Proof | Residual |
|---|---|---|---|
| Logs at rest read by someone with file access | Sealing (AES-256-GCM, key rotation, crypto-shredding by deleting a key) | `TestSealedRunNeverWritesContentInTheClear`, `TestSealedLogWithoutTheKeyIsRefusedNotReadAsEmpty` | The audit fields stay plain (who, decision, tool, keys); keys are SHA-256 of business fields, so a low-entropy field (a short reference, a round amount) can be guessed by hashing candidates |
| Console output or traces shipped to a vendor | `Runner.Redact`, `Trace.Redact` | `TestConsoleOutputIsRedactedButTheRunUsesRealValues`, `TestTraceRedactMasksContentKeepsStructure` | Free text needs a pattern redactor; field redaction can't see inside it |
| Another provider's private data (Claude thinking blocks) sent to a different API | `Message.Native` is stripped for other providers | `TestOpenAIDecideParsesAToolCallAndNeverSendsNative` | — |

## Out of scope

- **The model provider sees your prompts and tool data.** Redaction applies to what leaves *your* process for consoles and traces, not to the model requests themselves: the model needs the real values.
- **Detecting prompt injection, or judging whether the model's plan is wise.** agentsafe limits what any plan can do; it doesn't grade plans.
- **Authentication.** `By` must be an identity your system verified.
- **Key management.** Where `FileLog.Key` and `Codec` keys live, and who can read them.
- **Your tools and the systems they call.** Exactly-once needs the system you call to honour idempotency keys (`tooltest.SameKey` checks it); a non-idempotent tool is at-least-once across crashes.
- **An attacker with code execution as the agent's process user**, or root on its host.
- **Side channels** (timing, log sizes, the shape of a sealed log).

## Reporting

See [SECURITY.md](../SECURITY.md).
