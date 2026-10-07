# Design: agentsafe as an MCP proxy

**Status: proposed (2026-10-07).** Nothing here is built yet. Decisions marked **(decide)** need the maintainer's call before code.

## The problem this solves

agentsafe today is a Go library whose `Runner` owns the agent loop. Most agents aren't written that way: they're Python or TypeScript (LangGraph, the OpenAI Agents SDK, CrewAI), or tools like Claude Code, and they get their tools from **MCP servers**. None of them can use agentsafe.

An MCP proxy changes that. It sits between any MCP client and the MCP servers it uses, and every tool call passes through agentsafe's log, idempotency keys and approval gate. Adopting it is one line in the client's configuration:

```jsonc
// before: the agent talks to the payments MCP server directly
{ "command": "npx", "args": ["-y", "@acme/payments-mcp"] }

// after: through agentsafe, with a policy file
{ "command": "agentsafe-mcp", "args": ["--policy", "agentsafe.yaml", "--", "npx", "-y", "@acme/payments-mcp"] }
```

The agent's code doesn't change, and neither does the server's.

## What it guarantees, and what it can't

The proxy sees **tool calls**, not the model's reasoning or decisions. That splits agentsafe's guarantees in two:

| Guarantee | Library (`Runner`) | Proxy |
|---|---|---|
| Every effect is logged before it happens (write-ahead), and the log is hash-chained, optionally sealed | yes | **yes** |
| A completed operation is never re-run: a repeat (same key) gets the logged result back | yes | **yes** |
| Same operation, different values (amount changed) is a conflict, never a second effect | yes | **yes** |
| Nothing gated runs without an authorized human approval; a rejection sticks to the operation | yes | **yes** |
| An unknown outcome (timeout, crash) is retried with the same key | yes | **only if the upstream server honours the key** (below) |
| `reconcile.Audit` checks effects against systems of record and the log | yes | **yes** |
| A crashed **run** resumes from the log: model decisions replayed, budget enforced | yes | **no**: the agent's own loop owns that |
| Arguments are grounded (`Check`) before approval | yes | **library mode only** (Go code); not in YAML |

The honest line for the README: *the proxy makes every tool call exactly-once-or-reported, approved where required, and auditable. Whether the agent itself survives a crash is up to the agent's framework.*

### The upstream key problem

MCP's `tools/call` has **no idempotency key** (checked against the 2026-07-28 schema). The only related field is a tool's `idempotentHint` annotation, which is self-reported and means "repeating this is harmless", not "this deduplicates by key". So after an unknown outcome (the upstream timed out, or the proxy crashed after forwarding), the proxy can't know whether the effect happened, and a blind retry is exactly the double-payment this project exists to prevent.

Proposal: per-tool policy says how the key reaches the upstream, if at all:

- `key: meta`: sent as `_meta["io.github.ashutosh2308bhardwaj.agentsafe/idempotency-key"]` (a valid, non-reserved `_meta` prefix). Useful for servers you write or that adopt it.
- `key: argument <name>`: injected into a tool argument the upstream already has, if it has one (an MCP server wrapping an API with idempotency keys can pass it straight through).
- `key: none` (default): the upstream can't deduplicate. **An in-doubt call is never retried automatically.** The agent gets an "outcome unknown" error naming the key, and a human resolves it (`agentsafe-mcp resolve <key> --happened|--did-not-happen`, which is logged, by whom), or a reconciliation does.

So: exactly-once where the upstream honours a key; **at-most-once with every unknown outcome surfaced** where it doesn't. Never a silent duplicate.

## Architecture

Two deliverables, in a new module `github.com/Ashutosh2308Bhardwaj/agentsafe/mcp` (it depends on the official Go SDK, `github.com/modelcontextprotocol/go-sdk` v1.8.0, which supports protocol 2026-07-28 and negotiates every earlier version; the core stays dependency-free):

1. **Package `mcp`**: the proxy as a library. Policies are Go values, so a Go user can add `Check` functions, an `Authorizer`, any `Log` backend (file, SQLite, Postgres).
2. **Command `agentsafe-mcp`**: the proxy as a binary, configured by a YAML policy file, for everyone else.

```
 agent (any language)          agentsafe-mcp                       upstream MCP server(s)
 ───────────────────   stdio   ───────────────────────────   stdio / Streamable HTTP   ──────────────
 LangGraph / Agents  ───────►  tools/list: upstream tools  ─────────────────────────►  payments-mcp
 SDK / Claude Code             tools/call: policy → log →                              crm-mcp
                               key → approve? → forward
                                       │
                                       ▼
                               log (file / SQLite / Postgres)
```

v1 transports: **stdio towards the agent** (what every client supports) and **stdio or Streamable HTTP towards upstreams**. Several upstreams can be proxied at once; their tools are listed with a server prefix when names collide.

## The call path

For each `tools/call`:

1. **Policy.** No policy for the tool: see *unconfigured tools* below.
2. **Validate** the arguments against the tool's input schema. A failure goes back to the agent as a tool error; nothing is logged as an attempt.
3. **Key** = hash(scope, tool, the policy's identity fields), 128 bits, exactly as the library does (`Idempotent("invoice_id")` becomes `identity: [invoice_id]`).
4. **Look the key up in the log**:
   - completed with the same payload → **return the logged result**, marked as a replay. Nothing is forwarded;
   - completed with a different payload → **conflict** error to the agent;
   - rejected → refused, without asking anyone again;
   - in doubt (started, no result) → see the upstream key problem: retry with the key, or surface "outcome unknown".
5. **Approval**, if the policy gates it: see below.
6. **Write-ahead**: `call_received` and `tool_started` are appended, durably, **before** anything is forwarded.
7. **Forward** to the upstream, with the key as the policy says, and a timeout.
8. **Log the result** (`tool_result`), then return it to the agent.

Calls are handled **one at a time per log**, as the library does: after a crash at most one call is in doubt. Throughput is then bounded by the durable writes, about three per call: about 25 calls/s on the Mac laptop measured in `docs/PERFORMANCE.md` (`F_FULLFSYNC`); server disks are typically faster, but that's unmeasured. Either way it's well above an LLM agent's call rate, but it's a real limit, and the reason is written down.

## Approvals

An MCP call is a request waiting for a response. A human approval can take hours. Options:

| Option | Verdict |
|---|---|
| Hold the request open until someone decides | **No.** Client SDKs put timeouts on requests (typically around a minute), and a held request dies with the process. |
| **Elicitation**: ask the client's user | **No, for money.** The user of the agent is usually the requester, so it breaks maker-checker, and the identity is unverified. Possibly fine as an opt-in for low-risk confirmations. |
| MCP **tasks** extension / **multi round-trip** (`InputRequiredResult` + `requestState`, 2026-07-28) | **Later.** The right protocol shape (the call becomes a task, completed after the decision), but client support is new and uneven. Adopt when the major clients advertise it. |
| **Return "pending" as the tool result**, and let the agent call again | **v1.** Works with every client today. |

v1 in detail: a gated call is logged as `approval_requested` and returns a normal tool result:

```json
{ "status": "pending_approval", "key": "1520a7e1bc6ea3caac5bb435a697b556",
  "message": "Waiting for a human to approve. Nothing was done. Call send_payout again with the same arguments to get the outcome." }
```

The model typically tells its user it's waiting, and calls the tool again later. The same arguments give the same key: approved → it runs (once); rejected → refused; still waiting → the same pending result. **The decision is made outside the agent**: `agentsafe-mcp approve <key> --as <identity>` or `reject`, checked by the policy's `Authorizer` (an allow-list plus maker-checker against the configured `started_by` identity), and logged with who decided. An HTTP approval endpoint with real authentication is a later phase.

## Unconfigured tools (decide)

A tool without a policy can be:

- **(a) passed through, logged, unprotected**: no key, no gate. It's in the audit log; nothing more.
- **(b) refused**: only tools with a policy can be called.
- **(c) read-only passed through, writes refused**: trust `readOnlyHint`. But it's self-reported by the server, so a mislabelled write slips through.

**Recommendation: (a) by default, `strict: true` for (b)**, and a startup warning listing every unprotected tool that isn't marked read-only. (b) is safer, but it makes the first run a wall of refusals, and people turn safety off when the first experience is friction.

## The log: tool-only runs

A proxy log has no model decisions, so the current state machine (which expects `model_decided` before any `tool_started`) doesn't fit. Rather than invent fake model decisions, **format v5 adds a run kind and one event**:

- `run_started` gains `kind: "proxy"` (absent = the existing `"agent"` kind);
- `call_received` (`call_id`, `tool`, `args`, the client's self-reported name) plays the part of `model_decided` for one call;
- everything after it is unchanged: `tool_started`, `tool_result`, `tool_refused`, `approval_*`, `key_bits`, the hash chain, sealing, redaction.

A proxy run has no budget and no final answer: it's open for the proxy's lifetime, keyed by **scope**. **(decide)** One log per scope (e.g. `support-agent-prod`) that grows forever, or rotation with the key index carried forward? Rebuilding 10k events takes about 27 ms, so a busy proxy reaches seconds of startup within weeks. v1 can ship with one log per scope and a documented limit; snapshots of the key index are the fix. The same lease rules apply: one proxy process per log, enforced by the lease.

## Policy file

```yaml
scope: support-agent-prod          # keys are unique within a scope
started_by: support-agent          # the requester, for maker-checker
log: { sqlite: /var/lib/agentsafe/runs.db }   # or file:, postgres:
seal_key_env: AGENTSAFE_LOG_KEY    # optional: seal arguments and results at rest
approvers: [finance@example.com, ops-lead@example.com]

tools:
  create_refund:
    identity: [ticket_id, charge_id]   # what makes it one operation
    approval: always                   # or never; thresholds need library mode
    key: { argument: idempotency_key } # how the upstream deduplicates
    timeout: 10s
  get_charge:
    read_only: true                    # pass through, logged, never gated
strict: false                          # true: refuse tools without a policy
```

## What's deliberately out of v1

- Grounding checks in YAML. A declarative check language is a project of its own; Go users get `Check` in library mode.
- Proxying resources, prompts, sampling: tools only. Everything else passes through untouched.
- HTTP transport towards the agent, an approval web UI, multi-tenant scopes from `_meta`.

## Testing

The same standard as the library:

- A fake upstream MCP server (Go SDK) with crash points and a key-honouring switch.
- Process-level kill tests: the proxy killed after forwarding and before logging, for `key: argument` (retry: one effect), and for `key: none` (outcome unknown surfaced: never two effects).
- A client test with the official SDK's client, so the proxy is tested as clients see it, across protocol versions 2025-06-18 and 2026-07-28.
- Property tests over the proxy state machine (random calls, decisions, crashes: never two effects per key).
- One end-to-end example driven by a real Python MCP client (LangGraph's adapter or the OpenAI Agents SDK), since "works underneath your framework" is the claim.

## Phases

1. **Pass-through and log**: `call_received` / format v5, stdio both ways, the library package, a fake upstream, client tests.
2. **Keys**: replay, conflict, in-doubt handling for each `key` mode, `resolve`, kill tests.
3. **Approvals**: pending results, `approve` / `reject` with an Authorizer, maker-checker.
4. **The binary and YAML policy**, `reconcile.Audit` over proxy logs, the Python end-to-end example.
5. **Release** as `mcp/v0.1.0`.

## Decisions needed

1. Unconfigured tools: (a) pass through by default with `strict` opt-in (recommended), or (b) refuse by default?
2. v1 approvals as "pending" tool results, with tasks / multi round-trip later: agreed?
3. Log growth: ship v1 with one log per scope and a documented limit, snapshots later: agreed?
4. Scope of v1 transports: stdio towards the agent only: agreed?
