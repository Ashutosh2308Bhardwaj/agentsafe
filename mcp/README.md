# agentsafe/mcp: an MCP proxy

**Status: in development, phases 1–3 of [the design](../docs/MCP_PROXY.md). Not released**, and it needs the
core's `Gateway`, which isn't in a release yet either.

Put `agentsafe-mcp` in front of any MCP server in your agent's configuration, and every tool call goes through
agentsafe first. The agent's code and the server don't change.

```jsonc
// before
{ "command": "npx", "args": ["-y", "@acme/billing-mcp"] }
// after
{ "command": "agentsafe-mcp", "args": ["--log", "calls.jsonl", "--policy", "policy.json", "--", "npx", "-y", "@acme/billing-mcp"] }
```

`policy.json` says which tools are protected, what makes a call one operation, how its key reaches the server,
which need a human's approval, and who may give it:

```json
{ "approvers": ["user:alice", "user:bob"],
  "tools": {
    "charge": { "identity": ["ticket_id"], "key": "argument", "key_argument": "idempotency_key",
                "approval": "always", "timeout": "10s" }
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
- The log's lease is taken per call: proxies in several processes can share one log, their calls taking turns.

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

With `approval: always`:

- **The call is answered at once with `pending_approval`** and the operation's key, not as an error: nothing was
  done yet. The agent can carry on with other tools: a proxy run never pauses.
- **A person decides from the command line**, as their own OS account (`user:` + username, from the process's
  uid, never `$USER`):

  ```bash
  agentsafe-mcp pending --log calls.jsonl --policy policy.json            # what's waiting, with its values
  agentsafe-mcp approve --log calls.jsonl --policy policy.json KEY
  agentsafe-mcp reject  --log calls.jsonl --policy policy.json --reason "duplicate ticket" KEY
  ```

  Only `approvers` may decide, and never the identity the proxy was started with (`--started-by`): maker-checker.
  Refused attempts are logged.
- **The agent's next call for the operation gets the outcome**: approved, it runs once (with the approved
  values: different ones are a conflict); rejected, it's refused every time; undecided, it's pending again.

## Not yet

YAML policies and the Python end-to-end example (phase 4). See the design.
