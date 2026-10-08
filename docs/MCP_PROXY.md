# Design: agentsafe as an MCP proxy

**Status: phases 1–4 built (2026-10-08), unreleased.** Written as a proposal on 2026-10-07; where the build differs, an **As built** note says so, and [Decisions](#decisions) records what was chosen. The code is `agentsafe.Gateway` (core) and the `agentsafe/mcp` module; usage is in [mcp/README.md](../mcp/README.md).

## The problem this solves

agentsafe today is a Go library whose `Runner` owns the agent loop. Most agents aren't written that way: they're Python or TypeScript (LangGraph, the OpenAI Agents SDK, CrewAI), or tools like Claude Code, and they get their tools from **MCP servers**. None of them can use agentsafe.

An MCP proxy changes that. It sits between any MCP client and the MCP servers it uses, and every tool call passes through agentsafe's log, idempotency keys and approval gate. Adopting it is one line in the client's configuration:

```jsonc
// before: the agent talks to the payments MCP server directly
{ "command": "npx", "args": ["-y", "@acme/payments-mcp"] }

// after: through agentsafe, with a policy file
{ "command": "agentsafe-mcp", "args": ["--log", "calls.jsonl", "--policy", "policy.json", "--", "npx", "-y", "@acme/payments-mcp"] }
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

Proposed: **stdio towards the agent**, and **stdio or Streamable HTTP towards upstreams**, several at once, their tools prefixed when names collide.

**As built (v0.1):** a **tool** proxy for **one upstream**, **stdio on both sides** in the binary (`mcp.Open` takes any `*ClientSession`, so a Go program can connect it to a Streamable HTTP upstream itself). Only tools are proxied: resources, prompts, completions and the rest are not, and the agent doesn't see them through the proxy. Several upstreams, prefixing, and Streamable HTTP in either direction are planned, not built.

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

The model typically tells its user it's waiting, and calls the tool again later. The same arguments give the same key: approved → it runs (once); rejected → refused; still waiting → the same pending result. **The decision is made outside the agent**: `agentsafe-mcp approve <key>` or `reject`, checked by the policy's `Authorizer` (an allow-list plus maker-checker against the configured `started_by` identity), and logged with who decided. An HTTP approval endpoint with real authentication is a later phase.

**As built:** the approver is the OS account running `agentsafe-mcp approve` (`user:` + username, from the process's uid), not an `--as` flag, which anyone could set. In the log, a proxy run never pauses: `approval_requested` answers the call and the run stays `open`; decisions arrive between calls, addressed by key (format v5, `FORMAT.md`). The pending result is not an MCP error (`isError` false), and carries `{status, key}` as structured content.

## Unconfigured tools (decided: fail closed)

A tool without a policy can be:

- **(a) passed through, logged, unprotected**: no key, no gate. It's in the audit log; nothing more.
- **(b) refused**: only tools with a policy can be called.
- **(c) read-only passed through, writes refused**: trust `readOnlyHint`. But it's self-reported by the server, so a mislabelled write slips through.

The proposal recommended (a), for an easy first run. **Changed in mcp v0.2.0 to (b), fail closed**, after an outside review: a safety gateway is an allow-list. A server that adds `delete_account` tomorrow must not have it forwarded, unprotected, on the next restart. A tool without a policy **isn't exposed at all** (not listed, not callable); `"pass": true` forwards a tool unprotected on purpose; and at startup the binary warns about every hidden tool and every passed-through tool not marked read-only. The upstream's tools are read once, at startup: one added later appears only after a restart, and only with a policy.

## The log: tool-only runs

A proxy log has no model decisions, so the current state machine (which expects `model_decided` before any `tool_started`) doesn't fit. Rather than invent fake model decisions, **format v5 adds a run kind and one event**:

- `run_started` gains `kind: "proxy"` (absent = the existing `"agent"` kind);
- `call_received` (`call_id`, `tool`, `args`, the client's self-reported name) plays the part of `model_decided` for one call;
- everything after it is unchanged: `tool_started`, `tool_result`, `tool_refused`, `approval_*`, `key_bits`, the hash chain, sealing, redaction.

A proxy run has no budget and no final answer: it's open for the proxy's lifetime, keyed by **scope**. (Decided: see Decisions.) One log per scope (e.g. `support-agent-prod`) that grows forever, or rotation with the key index carried forward? Rebuilding 10k events takes about 27 ms, so a busy proxy reaches seconds of startup within weeks. v1 can ship with one log per scope and a documented limit; snapshots of the key index are the fix.

**As built:** the lease is taken **per call**, not for the proxy's lifetime (phase 3: an approval is written by another process). Each call re-reads the log (a few ms at 1k events, `PERFORMANCE.md`); in return, proxies in several processes can share one log, their calls taking turns. A gateway settles an interrupted call only if it serves that call's tool, so an approver's tool-less gateway never mis-settles one.

## Policy file

**As built:** JSON, read with unknown fields refused (a misspelt `"aproval"` fails loudly rather than leaving a tool unprotected), and checked against the upstream's real tools at startup (`mcp.Config`):

```json
{ "approvers": ["user:alice", "user:bob"],
  "tools": {
    "create_refund": { "identity": ["ticket_id", "charge_id"], "key": "argument", "key_argument": "idempotency_key",
                       "approval": "always", "timeout": "10s" } } }
```

Scope, started-by and the log are command-line flags (`--scope`, `--started-by`, `--log`). A read is passed through with `{"pass": true}`. Not built yet: choosing SQLite / Postgres / sealing from the binary (the library takes any `agentsafe.Log`, all three included).

## What's deliberately out of v1

- Grounding checks in YAML. A declarative check language is a project of its own; Go users get `Check` in library mode.
- Proxying resources, prompts, completions: **not proxied at all** (the agent doesn't see them through the proxy). The product is about consequential tool calls; the rest can come later.
- HTTP transport towards the agent, an approval web UI, multi-tenant scopes from `_meta`.

## Testing

The same standard as the library:

- A fake upstream MCP server (Go SDK) with crash points and a key-honouring switch.
- Process-level kill tests: the proxy killed after forwarding and before logging, for `key: argument` (retry: one effect), and for `key: none` (outcome unknown surfaced: never two effects).
- A client test with the official SDK's client, so the proxy is tested as clients see it, across protocol versions 2025-06-18 and 2026-07-28.
- Property tests over the proxy state machine (random calls, decisions, crashes: never two effects per key).
- One end-to-end example driven by a real Python MCP client (LangGraph's adapter or the OpenAI Agents SDK), since "works underneath your framework" is the claim.

## Phases

1. **Pass-through and log**: done. `call_received` / format v5, `Gateway`, stdio both ways, the library package, a fake upstream, in-memory and process tests.
2. **Keys**: done. `KeyHonouring` in the core, per-tool policies (`meta`, `argument`, `none`), replay, conflict, in-doubt handling per mode, kill tests (one charge per mode, two without a policy). **Not built:** `resolve` (a person recording what became of an unknown outcome).
3. **Approvals**: done. By key in proxy runs, pending results, `pending` / `approve` / `reject` with an allow-list and maker-checker, as processes with the real OS identity.
4. **Audit and a real framework**: done. Traces and `reconcile.Audit` over proxy logs; a LangGraph agent (`mcp/examples/langgraph`) run in CI. The policy file is JSON, not YAML (above).
5. **Release**: the core's next minor version with `mcp/v0.1.0`.

## Decisions

Made on 2026-10-07, when building started (the maintainer took the recommended defaults):

1. ~~Unconfigured tools pass through~~ → **fail closed** (2026-10-08, mcp v0.2.0, the maintainer's call after an outside review): no policy, not exposed; `"pass": true` to forward unprotected on purpose.
2. **Approvals as "pending" tool results**; MCP tasks / multi round-trip when the major clients support them.
3. **One log per scope, growth documented**; snapshots of the key index later.
4. **stdio towards the agent** only, for now.
5. (2026-10-08) **The policy file is JSON**: built, no dependency, strict about unknown fields.
