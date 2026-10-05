# Evidence

Every number in the README, with the run that produced it. Real-model runs used `openai/gpt-oss-120b` on Groq; "scripted" runs use the deterministic `ScriptedModel`.

## Idempotency: the blind retry, before and after
Same fault in both: the write succeeded, the model was told "timeout, outcome unknown", and it retried blindly both times.
| | rows | the model's claim |
|---|---|---|
| no idempotency | **5** (T1004 twice) | 4 |
| idempotency keyed on what the operation means | **4**; the retry got `already_recorded: true, discrepancy_id: 1` | 4 |
The model's behaviour didn't change. Retrying became safe.

## Where the transaction boundary sits (scripted, SEED=7, identical 172-kill schedule over 100 runs)
| design | duplicate writes | runs not ending with 4 rows |
|---|---|---|
| effect written, then key recorded | **27** | **25 / 100** |
| effect and key in one transaction | **0** | **0 / 100** |

## Kill -9 at every point of the money flow, then resume (real model)
| kill point | ledger | gateway payments |
|---|---|---|
| after the model responded, before logging | 4/4, 0 dup | 1 |
| after logging the decision | 4/4, 0 dup | 1 |
| before a tool runs | 4/4, 0 dup | 1 |
| **after a tool ran, before its result was logged** | 4/4, 0 dup | 1 |
| after the result was logged | 4/4, 0 dup | 1 |
| the moment approval is requested | 4/4, 0 dup | 1 |
| after approval, before sending | 4/4, 0 dup | 1 |
| **after the gateway charged, before anything was logged** | 4/4, 0 dup | **1** |
Control: the same kill at "after a tool ran" with the idempotency key disabled → **5 rows, 1 duplicate**.

## Pre-write validation (real model, same 8 runs)
6 of 8 runs proposed at least one wrong value (e.g. `T1011 missing_in_ledger` with both amounts null; truth: bank 3000). Each was refused with *"the files say ledger_amount=null, bank_amount=3000; you sent null, null"*, and the next call was correct. **0 wrong values written.**

## Silent failures (real model)
| fault | agent's claim | tool errors | trace problems | checker |
|---|---|---|---|---|
| gateway moves 90%, confirms 100% | 4 ✓ | 0/7 | 0 | ERROR: T1007 paid 9900, should be 11000 |
| ledger acknowledges a write, stores nothing | 4 (ledger has 3) | 0/7 | 0 | ERROR: T1004 missing · WARN: claim 4 vs records 3 |
| gateway ignores the idempotency key; crash + retry | 4 ✓ | 0/7 | 0 | CRITICAL: T1007 paid twice, one approval |
| payment made outside the agent | 4 ✓ | 0/7 | 0 | CRITICAL: payment with no approval and no logged cause |
In 3 of the 4 runs the model also proposed a wrong value that validation refused. The agent was careful, and correct about everything it could see. Money still went wrong in 3 of 4 runs, and only reconciliation saw it.
