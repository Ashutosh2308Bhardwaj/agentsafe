# The failure list that specified this library

Observed first-hand in week 1 of building it, against real models (F0–F4 on Gemini's free tier, F5–F14 on `openai/gpt-oss-120b` via Groq). Every entry had a saved transcript. The full entries, with the model's own reasoning quoted, are in the lab notes this was built from.

## What broke, and which primitive it demands
| # | Failure (observed) | Money consequence | Primitive |
|---|---|---|---|
| F0 | Model endpoint 503s; 4 of 5 models down at once | batch dies at step k of n, unknown which steps took effect | checkpoint; retries only safe with idempotent tools |
| F1 | HTTP 200 + `MAX_TOKENS`: fluent, silently cut-off answer | 37 of 50 payouts processed, 13 vanish | reconciliation (counts vs source); check `finish_reason` |
| F2 | No client timeout (Gemini SDK); OpenAI SDK has 2 *silent* retries | stalled run killed mid-step → outcome unknown | explicit deadline + retry policy; checkpoint |
| F3 | Hidden thinking re-sent as input every turn (~2× prompt) | budgets set from visible output are wrong | budget on measured prompt size |
| F4 | Retries burn per-minute and per-day quota; 9 requests per answer | a throttle becomes an outage (retry storm) | classify errors; honour `retryDelay`; run budget |
| F5 | Stream complete only at its last chunk; usage delivered twice | acting mid-stream = acting on a partial; double-counted cost | act only on committed output; idempotent metering |
| F6 | Killed mid-stream: partial looks finished, cost unknowable, rerun differs | partial output drives effects; no resume, only re-decide | record model calls as activities; replay, never re-ask |
| F7 | Required schema field → model makes up `amount: 0`; all checks pass | invoice closed as "₹0 pending" | nullable + provenance; **grounding** check vs source |
| F8 | API accepts 2 results for 1 call | harmless on reads, double-count on writes | harness enforces 1 call ↔ 1 result |
| F9 | Result lost → model re-issues the same call under a **new id** | double execution that dedupe-by-call-id misses | idempotency keyed on **intent**, not call id |
| F10 | Model notices wrong-account result, approves anyway, hides it | underfunded payout approved confidently | harness validates result against call; reconcile claims |
| F11 | Right txn + kind, **wrong amount** (copied from the next row); key-only checker said 4/4 | ₹3,700 of a ₹11,000 gap disappears | grounding at write time; **field-level** reconciliation |
| F12 | Same input, different paths; IDs from write order unstable; budget 8/8 | replay renumbers effects; half-written run on budget-out | content-derived identity; budget-out = resumable failure |
| F13 | "Outcome unknown" → blind retry → **duplicate write**; claim 4, ledger 5 | **the double payment, reproduced** | idempotency at the tool boundary; effect lookup; reconcile claim vs ledger |
| F14 | No result delivered → model assumes success, reports it as fact | missed payout reported as done | exactly one recorded outcome per call; never trust the claim |

**What `agentsafe` must provide, from this list:**
1. **Idempotent tool execution keyed on intent** (F8, F9, F13), with effect lookup (F13).
2. **Durable checkpointing of model calls and tool outcomes** (F0, F2, F6, F12, F14): replay recorded outputs, never re-ask; budget-out and crashes are resumable states.
3. **Harness-side validation before effects** (F7, F10, F11): grounding and entity checks at write time.
4. **Run reconciliation**: claims vs recorded effects vs system of record, per field, every run (F1, F11, F13, F14).

<!--
## F<n>: <short name>
Fault / trigger:
What the model did:
What my harness did:
Consequence if this were a payout:
Transcript: transcripts/<file>
Primitive it demands:  (idempotency / checkpoint / gate / reconciliation / other)
-->
