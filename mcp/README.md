# agentsafe/mcp: an MCP proxy

**Status: in development, phase 1 of [the design](../docs/MCP_PROXY.md). Not released**, and it needs the core's
`Gateway`, which isn't in a release yet either.

Put `agentsafe-mcp` in front of any MCP server in your agent's configuration, and every tool call goes through
agentsafe first. The agent's code and the server don't change.

```jsonc
// before
{ "command": "npx", "args": ["-y", "@acme/billing-mcp"] }
// after
{ "command": "agentsafe-mcp", "args": ["--log", "calls.jsonl", "--", "npx", "-y", "@acme/billing-mcp"] }
```

## What phase 1 does

- The agent sees the upstream server's tools exactly as the server describes them.
- **Every call is logged before it's forwarded** (write-ahead) and **logged with what came back**: one log,
  a hash-chained proxy run (format v5) that continues across restarts. `go run ./cmd/trace` and
  `reconcile.Audit` read it.
- **A crash mid-call is settled, never repeated.** A call the proxy hadn't forwarded is recorded as refused
  (nothing was done); a call it had forwarded is recorded as an unknown outcome and **not** retried, because
  MCP has no idempotency key and the upstream can't deduplicate.
- One proxy per log, enforced by the log's lease.

## Not yet

Idempotency keys and replays (phase 2), approvals (phase 3), the YAML policy file (phase 4). See the design.
