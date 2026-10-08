# agentsafe

[![CI](https://github.com/Ashutosh2308Bhardwaj/agentsafe/actions/workflows/ci.yml/badge.svg)](https://github.com/Ashutosh2308Bhardwaj/agentsafe/actions/workflows/ci.yml) [![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/Ashutosh2308Bhardwaj/agentsafe/badge)](https://scorecard.dev/viewer/?uri=github.com/Ashutosh2308Bhardwaj/agentsafe) [![Go Reference](https://pkg.go.dev/badge/github.com/Ashutosh2308Bhardwaj/agentsafe.svg)](https://pkg.go.dev/github.com/Ashutosh2308Bhardwaj/agentsafe)

> **Status: v0.5.0, pre-1.0** ([CHANGELOG](CHANGELOG.md)). The guarantees below are proven under the conditions stated; the gaps to production are tracked item by item in [SCORECARD.md](SCORECARD.md).

```bash
go get github.com/Ashutosh2308Bhardwaj/agentsafe
```

**Correctness primitives for LLM agents that act on money.** Idempotent tool execution, durable resume, human approval gates, pre-write validation, and reconciliation of what the agent claimed against what actually happened. Go; the core uses the standard library only, and storage backends and model adapters are separate modules.

I own a payouts platform that disburses ₹400M a month to 40,000+ people. The failure I've spent the most time on is the gateway timeout that arrives *after* the money moved. I wanted to know what that failure looks like when the caller is an LLM agent, so I built an agent, broke it deliberately, wrote down every failure I saw, and built this library from that list. Nothing in it is speculative: every primitive answers a failure I reproduced, and every guarantee below is a test or a run you can repeat.

## Quickstart (5 minutes, no API key)

An agent that can pay an invoice **at most once**, and **only after a human approves**, surviving restarts in between:

<!-- quickstart:start -->
```go
// Quickstart: an agent that can pay an invoice at most once, and only after a human approves.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// Payout is what the model sends to pay one invoice.
type Payout struct {
	InvoiceID string            `json:"invoice_id"`
	Amount    agentsafe.Decimal `json:"amount"`
}

// sendPayout is your code. KeyFrom(ctx) is the operation's idempotency key: give it to your payment
// provider, and a retry after a crash or a timeout can't pay twice.
func sendPayout(ctx context.Context, p Payout) (string, error) {
	_ = agentsafe.KeyFrom(ctx)
	fmt.Println("paying", p.InvoiceID, p.Amount)
	return "paid", nil
}

func main() {
	ctx := context.Background()
	pay := agentsafe.Func("send_payout", "Pay an approved invoice", sendPayout,
		agentsafe.Idempotent("invoice_id"),                       // once per invoice, ever
		agentsafe.NeedsApproval(func(p Payout) any { return p })) // a human decides first

	// An offline stand-in for the model. For Claude: anthropic.New(sdk.NewClient()).
	model := &agentsafe.ScriptedModel{Final: "INV-1 paid.", Plan: []agentsafe.FunctionCall{
		{Name: "send_payout", Arguments: `{"invoice_id":"INV-1","amount":"4200.50"}`}}}

	r, err := agentsafe.New(model, &agentsafe.FileLog{Path: "payout-run.jsonl"},
		agentsafe.WithTools(pay), agentsafe.WithAuthorizer(agentsafe.AllowList("you@example.com")))
	must(err)

	if len(os.Args) > 1 && os.Args[1] == "approve" {
		st, err := r.Continue(ctx) // rebuilt from the log: a new process knows what's waiting
		must(err)
		if st.Waiting == nil {
			fmt.Println("nothing to approve; the run is", st.Status)
			return
		}
		st, err = r.Approve(ctx, st.Waiting.Key, "you@example.com")
		must(err)
		fmt.Println(st.Status, "-", st.Text)
		return
	}
	st, err := r.Start(ctx, "You pay approved invoices.", "Pay invoice INV-1.")
	must(err)
	fmt.Printf("%s: %s\napprove it, now or after a reboot: go run . approve\n", st.Status, st.Waiting.Summary)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```
<!-- quickstart:end -->

```console
$ go run .
awaiting_approval: {"invoice_id":"INV-1","amount":"4200.50"}
approve it, now or after a reboot: go run . approve

$ go run . approve          # a new process: it finds the waiting payout in the log
paying INV-1 4200.50
finished - INV-1 paid.

$ go run . approve          # again: nothing is paid twice
nothing to approve; the run is finished
```

To use a real model, replace the `ScriptedModel` with `anthropic.New(sdk.NewClient())` (`go get github.com/Ashutosh2308Bhardwaj/agentsafe/anthropic`), `gemini.New(client, model)`, or `&openai.Model{...}` (`agentsafe/openai`) for Groq, OpenAI, Ollama or vLLM. Then add `agentsafe.Check(...)` to ground arguments against your data before anyone is asked to approve, and `reconcile.Audit` (`agentsafe/reconcile`) to check the run against your systems of record. This exact program is [examples/quickstart](examples/quickstart), and CI runs it as three processes.

## What goes wrong when the caller is a model

From [docs/FAILURES.md](docs/FAILURES.md), observed against real models (Gemini, then `openai/gpt-oss-120b` on Groq):

- **It retries blindly.** Told "timeout, outcome unknown" after a write that *had* succeeded, it retried after 19 tokens of thought. The ledger got 5 rows, and its report said 4.
- **It retries under a new identity.** When a tool result was lost, the model re-issued the same call with a **new call id**, so deduplicating on call ids misses it.
- **It assumes success.** When a result was never delivered, it reasoned *"already recorded"* and reported it as fact.
- **It's confidently wrong in specific places.** In one batch, **6 of 8 runs** proposed at least one wrong amount, every one on a row where one side is legitimately empty.
- **It notices, then hides it.** Given another account's balance, its reasoning said *"That's odd… possibly a mistake"*, and its answer approved the payout anyway, dropping the account id.
- **The APIs enforce almost nothing.** Two results for one call, a call with no result, schema-valid JSON with a made-up `amount: 0`: all accepted.

The model can't tell "failed" from "succeeded but unconfirmed", and it isn't its job to. **Delivery from this caller is at-least-once whatever you do; exactly-once has to come from the effect.**

## Framework-independent, through MCP: the proxy

Most agents aren't Go programs: they're LangGraph, the OpenAI Agents SDK, Claude Code, getting their tools from MCP
servers. Put `agentsafe-mcp` in front of a server, in the agent's MCP configuration, and every tool call goes through
agentsafe: logged before it's forwarded, a repeated operation answered from the log, an unknown outcome retried only
with a key the server honours, and `approval: always` tools waiting for a person. The agent's code doesn't change.

Today it's a **tool** proxy for **one stdio MCP server**, and it **fails closed**: tools without a policy aren't exposed.
Streamable HTTP and several servers are planned.

```bash
go install github.com/Ashutosh2308Bhardwaj/agentsafe/mcp/cmd/agentsafe-mcp@latest
```

```jsonc
{ "command": "agentsafe-mcp", "args": ["--log", "calls.jsonl", "--policy", "policy.json", "--", "npx", "-y", "@acme/billing-mcp"] }
```

[mcp/README.md](mcp/README.md) has the policy file and the approval commands; [mcp/examples/langgraph](mcp/examples/langgraph)
is a LangGraph agent doing it, run in CI; [docs/MCP_PROXY.md](docs/MCP_PROXY.md) is the design, including what the proxy
can't guarantee (it sees calls, not the model's decisions).

## The primitives

```go
r, err := agentsafe.New(model, &agentsafe.FileLog{Path: "run.jsonl"}, agentsafe.WithTools(tools...),
	agentsafe.WithStartedBy("scheduler"), // who asked for the run
	agentsafe.WithAuthorizer(agentsafe.All(agentsafe.AllowList("ops@company"), agentsafe.NotRequester()))) // who may decide
st, err := r.Start(ctx, system, task)     // or r.Continue(ctx) after a crash, in any process
r.Approve(ctx, key, "ops@company")         // a gated call waits durably until someone ALLOWED decides
rep := reconcile.Audit(expected, actual, events, claims)   // did it do what it claimed?
```

| primitive | what it does | file |
|---|---|---|
| **Wrap any function** | `agentsafe.Func("send_payout", "...", sendPayout, agentsafe.Idempotent("invoice_id"), agentsafe.NeedsApproval(summary), agentsafe.Check(againstLedger), agentsafe.Timeout(10*time.Second))`: the schema comes from the input struct, unknown fields are refused, and the function reads its idempotency key with `KeyFrom(ctx)` to pass to the system it calls. `tooltest.SameKey` proves your system really acts once per key, even for simultaneous calls. | `functool.go`, `tooltest/` |
| **Model adapters** | OpenAI-compatible (Groq, OpenAI, Ollama, vLLM, …) in `agentsafe/openai` (standard library only); Claude in `agentsafe/anthropic` and Gemini in `agentsafe/gemini` (official SDKs). A run resumed after a crash or an approval pause sends Claude exactly the conversation it sent before, thinking blocks / thought signatures included, which current models require. | `openai/`, `anthropic/`, `gemini/` |
| **Event log + state machine** | Every decision and result is fsync'd **before** anything acts on it, and a failed write or fsync stops the run and poisons that log handle (no retrying an fsync: PostgreSQL's "fsyncgate"); a new process continues from what is actually on disk. State is rebuilt from the log; the runner holds none. A transition function rejects impossible histories: finishing with a call unresolved, a result for something never started, a second result for one call, skipping an approval. | `eventlog.go`, `state.go`, `runner.go` |
| **Pluggable storage, fenced** | Backends implement a two-method `LineStore` (read lines; append line N only if N-1 exist); hash chain, sealing and verification live in the shared `Journal`, so no backend can get them wrong. The conditional append is the fencing token: a runner whose lease expired while it was paused is refused (`ErrConflict`) before it can act. `storetest.Run` checks any backend against the contract. Backends: file (built in), SQLite (`agentsafe/sqlite`, pure Go) and Postgres (`agentsafe/postgres`, for runs shared between machines; lease times on the database clock). | `journal.go`, `filestore.go`, `storetest/`, `sqlite/`, `postgres/` |
| **Idempotent tools** | A write tool declares which fields *define* the operation. The key is derived from those, never the model's call id and never free text. A repeat is replayed from the log, same key + different values is a conflict (never an upsert), and the key is passed **into** the effect so a crash between "done" and "logged" can't double it. | `idempotent.go` |
| **Pre-write validation** | `Validate` checks every consequential value against the source **before** anything runs or anyone is asked. The refusal says what the source says, so the model fixes it in one step. | `gate.go` |
| **Approval gate** | Irreversible calls pause as `awaiting_approval` in the log. Approve or reject later, from any process, addressed by the operation's key. Approving twice is a no-op; a decision can't be flipped, and a rejected operation the model proposes again is refused without asking the human again; the approver sees validated values only. An `Authorizer` decides who may approve or reject (allowlist, maker-checker, amount thresholds); with none set, every decision is refused, and refused attempts are logged. | `gate.go`, `authz.go` |
| **Budget as a pause** | Running out of steps pauses the run instead of ending it; extending it is a logged decision with who made it. | `state.go` |
| **Traces from the log** | OpenTelemetry GenAI-convention spans (`invoke_agent`, `chat`, `execute_tool`) *derived* from the log, so the trace can't disagree with it, and the same log gives the same trace. OTLP/JSON export. | `trace/`, `cmd/trace` |
| **Timeouts and panics** | A call that times out, panics, or returns `ErrOutcomeUnknown` is *in doubt*, never *failed*: a timed-out payout may have been charged. Idempotent tools are retried with the same key (the gateway dedupes); still unknown → `ErrInDoubt`, nothing logged, and `Continue` resolves it later. Other tools: the model is told the outcome is unknown. A panicking tool never takes the process down. | `exec.go` |
| **Sealed at rest** | Arguments, results, prompts and approval summaries are encrypted in the log (AES-256-GCM, key rotation; or your own `Codec`, e.g. a KMS), each bound to its line. Who approved what, keys, and the hash chain stay readable for audit; resume decrypts, so a crashed run continues with the real values. Deleting a key erases its runs. Console output and exported traces are masked separately (`Runner.Redact`, `Trace.Redact`), so nothing sensitive reaches a log aggregator or tracing vendor. | `seal.go`, `redact.go` |
| **Reconciliation** | Compares **what should exist** (computed from source data by plain code), **what the log says**, and **what the systems of record hold**: field by field, every payment tied to exactly one approval. | Values are compared by type, exactly: `4200` ≠ `"4200"`, a missing field ≠ null, `Decimal("4200.50")` = `4200.5`, float drift is reported, never tolerated. | `reconcile/` |

## What's proven

Full tables in [docs/EVIDENCE.md](docs/EVIDENCE.md).

- **Crash anywhere, pay once.** `kill -9` at all 8 points of a real run (inside tools, mid-approval, right after the gateway charged) → resume → **8/8 correct, 0 duplicates, exactly 1 payment each**.
- **The fix is what works, not luck.** Same 172 random kills, same model decisions: **27 duplicate writes → 0** when the effect and its key can't be separated. Disable the key at the gap point and the duplicate comes straight back.
- **Wrong values never land.** 6 of 8 real runs proposed a wrong amount; **0 were written**. Each was refused and corrected in the next step.
- **Silent failures get caught.** A gateway that under-pays, a ledger that drops a write, a gateway that ignores idempotency keys, a payment made outside the agent: in every case the agent's report, the tool results and the trace all looked clean. **The checker caught 4/4.** In 3 of the 4, the fault was in the *other* system.
- **The safety is cheap where it counts.** About **16 µs of CPU per tool call**, plus three durable log writes; the writes cost what your disk's `fsync` costs. Resuming a 1,000-event run takes about 3 ms. Numbers and how to reproduce them: [docs/PERFORMANCE.md](docs/PERFORMANCE.md).

## What it doesn't do

- **A transaction can't span someone else's system.** For external effects the guarantee depends on the downstream system honouring an idempotency key, or offering a status lookup by key. If it offers neither, the best you can honestly promise is at-least-once plus reconciliation.
- **Idempotency stops doing something twice, not doing it wrong.** A wrong first write would be locked in; that's why validation runs before the write.
- **A trace is the agent's account, not evidence about the world.** It can't tell a safe retry from a double charge. Only the gateway's records can.
- **Calls run one at a time**, by design: after a crash, at most one call is ever in the "may have executed" state.
- The GenAI semantic conventions are still at *Development* status upstream.

## Run it

```bash
go test ./...                                   # state machine, idempotency, gate, trace, checker
go run ./examples/reconcile -mock               # scripted model: free, deterministic
GROQ_API_KEY=… go run ./examples/reconcile      # a real model: reconciles, proposes a payout, waits at the gate
go run ./examples/reconcile -run <id> -approve <key>
go run ./examples/reconcile -run <id> -check    # reconcile any run against its systems of record
go run ./cmd/trace examples/reconcile/out/<id>-log.jsonl
```

### Examples

Each runs offline (scripted model, fake external system), and each has a test that runs it as real processes.

| Example | What goes wrong | What agentsafe does |
|---|---|---|
| [quickstart](examples/quickstart) | A payout must wait for a human, across restarts | The run waits durably at the approval gate; approving in a new process pays once |
| [refund](examples/refund) | The model proposes the wrong amount; the wrong person approves; the process dies right after the money moved | `Check` refuses the amount against the real charge; the `Authorizer` and maker-checker refuse the approvers; the refund is retried with its key: one refund. Without the key: two |
| [langgraph](mcp/examples/langgraph) | A Python LangGraph agent bills through an MCP server; charging needs approval | `agentsafe-mcp` in its MCP config, no agentsafe code: pending, a person approves from the command line, charged once, repeats answered from the log |
| [outbox](sqlite/examples/outbox) | Killed after a database write, before the agent logged it; killed between the write and its email | The credit, its key and its email commit in one transaction; the outbox worker resends with a key; reconciliation fails the control run |
| [subscription](examples/subscription) | An HTTP call times out, or the process dies, right after the API made a billed change | The unknown outcome is retried with the same idempotency key: one invoice. Without the key: two |
| [reconcile](examples/reconcile) | A model reconciling a ledger gets values wrong; a gateway under-pays or drops a write | Grounding refuses wrong values; the approval gate; reconciliation catches what every report missed |

`examples/reconcile` is a payout-reconciliation agent: it compares a ledger with a bank settlement file, records each discrepancy once, and re-issues payouts the bank never settled, behind an approval. `gateway.go` is a file-backed fake payment gateway with idempotency keys, status lookup, lost responses, a crash point, and four silent-fault modes.
