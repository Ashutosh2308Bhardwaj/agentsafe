# agentsafe/mcp: an MCP proxy

**Status: in development, phases 1–2 of [the design](../docs/MCP_PROXY.md). Not released**, and it needs the
core's `Gateway`, which isn't in a release yet either.

Put `agentsafe-mcp` in front of any MCP server in your agent's configuration, and every tool call goes through
agentsafe first. The agent's code and the server don't change.

```jsonc
// before
{ "command": "npx", "args": ["-y", "@acme/billing-mcp"] }
// after
{ "command": "agentsafe-mcp", "args": ["--log", "calls.jsonl", "--policy", "policy.json", "--", "npx", "-y", "@acme/billing-mcp"] }
```

`policy.json` says which tools are protected, what makes a call one operation, and how its key reaches the server:

```json
{ "tools": {
    "charge": { "identity": ["ticket_id"], "key": "argument", "key_argument": "idempotency_key", "timeout": "10s" }
} }
```

## What it does

- The agent sees the upstream server's tools exactly as the server describes them.
- **Every call is logged before it's forwarded** (write-ahead) and **logged with what came back**: one log,
  a hash-chained proxy run (format v5) that continues across restarts. `go run ./cmd/trace` and
  `reconcile.Audit` read it.
- **A crash mid-call is settled, never repeated.** A call the proxy hadn't forwarded is recorded as refused
  (nothing was done); a call it had forwarded is recorded as an unknown outcome and **not** retried, because
  MCP has no idempotency key and the upstream can't deduplicate.
- One proxy per log, enforced by the log's lease.

With a policy for a tool:

- **A repeated operation is answered from the log**: the same ticket asked for again never reaches the server.
  The same ticket with a different amount is a **conflict**, never a second charge.
- **The key reaches the server** as the policy says: `meta` (in the call's `_meta`, under
  `io.github.ashutosh2308bhardwaj.agentsafe/idempotency-key`), `argument` (a tool argument the server already
  deduplicates on; a key the model made up is replaced), or `none`.
- **After a crash mid-call**, a new proxy retries with the same key (`meta`, `argument`), and the server
  answers with the first result; with `none` the outcome is recorded as unknown and **never retried**.
  `TestKilledAfterTheUpstreamChargedThenRestarted` kills the proxy right after the server charged: one
  charge in every mode, two without a policy.
- A call missing an identity field is refused before it's forwarded. Policies are checked against the
  server's real tools at startup: a misspelt tool or argument fails loudly instead of leaving a tool bare.

## Not yet

Approvals (phase 3), YAML and the Python end-to-end example (phase 4). See the design.
