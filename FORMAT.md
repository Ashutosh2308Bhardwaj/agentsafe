# Event log format and compatibility

A run's log outlives the code that wrote it: a run paused for an approval can be resumed weeks later by a newer version of agentsafe. A log that can't be read loses the run; a log that's read *wrong* is worse, because the runner would confidently act on a history that never happened. These rules prevent both.

## Versions

Every event carries `"v"`, the format version it was written in (`agentsafe.FormatVersion`). Events written before versions existed have no `"v"` and are **version 0**.

| version | since | change |
|---|---|---|
| 0 | first release | (no `v` field) |
| 1 | step 2.2 | adds `v`; otherwise identical to 0 |
| 1 | step 2.4 | adds optional `prev` (hash chain, see `chain.go`). No version bump: rule 2 below. A log may start unchained (older lines) and become chained; once chained, a missing link is tampering. **Consequence:** don't let a library older than this append to a chained log; its unlinked lines would read as tampering. |
| 2 | step 2.5 | adds the `approval_denied` event (an Authorizer refused a decision: `by`, `decision`, `key`, and `reason` = why it was refused) and `run_started.by` (who started the run, for maker-checker). Bumped, not rule 2: a v1 library would reject `approval_denied` as an unknown type, and must not skip it, since it's an audit fact. v1 events read unchanged. |
| 3 | step 2.6 | adds `sealed`: the content fields (`system`, `task`, `message`, `args`, `result`, `summary`, `text`, `reason`) encrypted by a `Codec` (`seal.go`), with those fields empty. Bumped, not rule 2: a v2 library would read a sealed event as one with empty content, a different history. A sealed event read without its Codec is refused (`ErrSealed`). |
| 3 | step 4.3 | adds optional `message.native`: a provider's own form of an assistant message (Claude: the response's content blocks, thinking included), replayed verbatim so a resumed run sends exactly what it sent before. No version bump: rule 2 (an older library ignores it; a run resumed without it on Claude fails loudly with a 400, never silently). |
| 3 | step 5.1 | Idempotency keys: `Canonical` keeps a number that doesn't round-trip through float64 (an integer above 2^53, say) as its exact decimal text instead of rounding it. Keys change **only** for such numbers, which previously collided (two invoices, one key); every other key is byte-identical (`TestCanonicalUnchangedForExactNumbers`). Numbers with an exponent beyond ±400 are refused. |

## Reading

- **Older events are upgraded on read**, one event at a time, by the steps in `format.go`. A run can mix versions (started by an old library, resumed by a new one).
- **Newer events are refused** with `ErrNewerLogFormat`. Go's JSON decoder silently drops fields it doesn't know; an old library that skipped a field which mattered would act on the wrong history, so it stops instead.
- `Rebuild`, `trace.Build` and `Reconcile` all read upgraded events. A log that can't be read is a CRITICAL reconciliation finding.

## Changing the format

1. **Never rename, remove or change the meaning of a field.** Only add.
2. **An optional field whose absence is harmless** may be added without a version bump. Older libraries ignore it, and that's safe by definition.
3. **Anything else** bumps `FormatVersion` and adds an upgrade step `upgrades[old] = func(Event) (Event, error)`.
4. **Commit a golden log** of the new version to `testdata/` and add it to `TestGoldenLogsStillRebuild`. Golden logs are never edited or deleted: they are the proof that every log ever written still rebuilds to the same state.

Current golden logs:
- `golden_v0_real_gateway_crash.jsonl`: a real `openai/gpt-oss-120b` run with a grounding refusal, an approval, and a kill -9 right after the payment gateway charged.
- `golden_v1_scripted_approved.jsonl`: the scripted example through its approval gate.
- `golden_v1_chained_approved.jsonl`: the same, with the hash chain on every line.
- `golden_v2_denied_then_approved.jsonl`: the example's approval refused twice by its Authorizer (approver not on the list), then approved by an allowed approver.
- `golden_v3_sealed.jsonl`: the example sealed at rest (AES-256-GCM; the test key is in `format_test.go`), with one refused approval.
