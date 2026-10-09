# agentsafe/mcp: an MCP proxy

**Status: v0.4.0** ([the design](../docs/MCP_PROXY.md), what's built and what isn't). Requires agentsafe v0.5.0.

```bash
go install github.com/Ashutosh2308Bhardwaj/agentsafe/mcp/cmd/agentsafe-mcp@latest
```

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

**Start from what the server says about its tools.** `inspect` lists them without calling any, and writes a
starting policy that fails closed: read-only tools pass; a write is identified by all its arguments
(`"identity": ["*"]`), keyed on an argument that looks like an idempotency key if it has one, and waits for approval
unless the server says it isn't destructive. The annotations are the server's word: read the file before you use it.

```bash
agentsafe-mcp inspect --policy-out policy.json -- npx -y @modelcontextprotocol/server-filesystem ./data
```

```
secure-filesystem-server 0.2.0: 14 tools, 10 read-only, 4 that change something (0 with a key argument); 14 annotated.

TOOL              KIND                KEY ARGUMENT  SUGGESTED POLICY
read_file         read                -             pass
write_file        write, destructive  -             all arguments; no key (not retry-safe); approval
create_directory  write               -             all arguments; no key (not retry-safe)
...
```

Why all arguments: what makes two calls one operation is knowledge about your domain, and a schema doesn't carry it.
With `["*"]` an exact repeat is answered from the log and any difference is a new operation, so nothing legitimate
is ever refused. Name the fields yourself (`["ticket_id"]`) to also catch the same ticket asked for with a new
amount, as a conflict. `--json` prints the inspection; from Go, `mcp.Inspect`.

## What it does, and what it doesn't

**A tool proxy, for one upstream MCP server, over stdio.** Only tools are proxied: resources and prompts are not.
Streamable HTTP and several upstreams are planned ([the design](../docs/MCP_PROXY.md)).

**It fails closed: a tool without a policy isn't exposed** (not listed, not callable). `{"pass": true}` forwards a
tool unprotected on purpose. At startup, stderr lists every hidden tool and every passed-through tool that the
server doesn't mark read-only. The server's tools are read at startup: one it adds later appears after a restart,
and only with a policy.


- The agent sees the upstream server's tools exactly as the server describes them.
- **Every call is logged before it's forwarded** (write-ahead) and **logged with what came back**: one log,
  a hash-chained proxy run (format v5) that continues across restarts. `go run ./cmd/trace` and
  `reconcile.Audit` read it.
- **A crash mid-call is settled, never repeated.** A call the proxy hadn't forwarded is recorded as refused
  (nothing was done); a call it had forwarded is recorded as an unknown outcome and **not** retried, because
  MCP has no idempotency key and the upstream can't deduplicate.
- **The client's `_meta` travels to the server** (a trace context, say), recorded with the call so a retry after a
  crash sends the same. Not forwarded: what belongs to the client's own connection (keys under a reserved MCP
  prefix, such as its protocol version, client info and capabilities; `progressToken`), and agentsafe's namespace,
  which only the proxy writes. Metadata isn't part of the operation: a new trace id on a retry is still a replay.
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
- **`key: meta` or `argument` is your claim that the server deduplicates on the key**: agentsafe-mcp can't know. Check
  it against a **sandbox** of the server, calling the tool the way the proxy does. Every tool, keyed or not: one call
  makes exactly one effect (two means the server repeats the action inside one call, which no proxy can stop). With a
  key: one call with a new key (one effect), then 20 at once with one key (still one effect, one answer). Without a
  key, verify reports the tool as not retry-safe: after a crash its outcome is recorded as unknown, never retried.

  ```bash
  agentsafe-mcp verify --policy policy.json --tool charge --args '{"ticket_id":"VERIFY-1","amount":"0.01"}' \
      --count 'sqlite3 sandbox.db "select count(*) from charges"' --sandbox -- npx -y @acme/billing-mcp-sandbox
  ```

  `--count` is any shell command printing how many effects exist (a `count(*)`, `curl … | jq length`). It makes real
  calls, so it won't run without `--sandbox`; `--json` prints the verdict. From Go: `mcptest.OneEffect`, `mcptest.SameKey`. It catches a server that ignores the key,
  and one that checks and then acts without a lock **when there's a real gap between the two** (a database read, then
  a payment API call): a race only microseconds wide can pass. A pass is strong evidence, not proof.
- A policy that protects a tool needs `identity` (or it's `pass`): the fields that make a call one operation, or
  `["*"]` for all its arguments. Without it every call would be the same operation. A call missing an identity field is refused before it's forwarded. Policies are checked against the
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

`resolve` (a person recording what became of an unknown outcome), choosing SQLite / Postgres / sealing from the binary (the library takes any
`agentsafe.Log`), several upstreams, and Streamable HTTP. See the design.
