# Event log format and compatibility

A run's log outlives the code that wrote it: a run paused for an approval can be resumed weeks later by a newer version of agentsafe. A log that can't be read loses the run; a log that's read *wrong* is worse, because the runner would confidently act on a history that never happened. These rules prevent both.

## Versions

Every event carries `"v"`, the format version it was written in (`agentsafe.FormatVersion`). Events written before versions existed have no `"v"` and are **version 0**.

| version | since | change |
|---|---|---|
| 0 | first release | (no `v` field) |
| 1 | step 2.2 | adds `v`; otherwise identical to 0 |
| 1 | step 2.4 | adds optional `prev` (hash chain, see `chain.go`). No version bump: rule 2 below. A log may start unchained (older lines) and become chained; once chained, a missing link is tampering. **Consequence:** don't let a library older than this append to a chained log; its unlinked lines would read as tampering. |

## Reading

- **Older events are upgraded on read**, one event at a time, by the steps in `format.go`. A run can mix versions (started by an old library, resumed by a new one).
- **Newer events are refused** with `ErrNewerLogFormat`. Go's JSON decoder silently drops fields it doesn't know; an old library that skipped a field which mattered would act on the wrong history, so it stops instead.
- `Rebuild`, `BuildTrace` and `Reconcile` all read upgraded events. A log that can't be read is a CRITICAL reconciliation finding.

## Changing the format

1. **Never rename, remove or change the meaning of a field.** Only add.
2. **An optional field whose absence is harmless** may be added without a version bump. Older libraries ignore it, and that's safe by definition.
3. **Anything else** bumps `FormatVersion` and adds an upgrade step `upgrades[old] = func(Event) (Event, error)`.
4. **Commit a golden log** of the new version to `testdata/` and add it to `TestGoldenLogsStillRebuild`. Golden logs are never edited or deleted: they are the proof that every log ever written still rebuilds to the same state.

Current golden logs:
- `golden_v0_real_gateway_crash.jsonl`: a real `openai/gpt-oss-120b` run with a grounding refusal, an approval, and a kill -9 right after the payment gateway charged.
- `golden_v1_scripted_approved.jsonl`: the scripted example through its approval gate.
- `golden_v1_chained_approved.jsonl`: the same, with the hash chain on every line.
