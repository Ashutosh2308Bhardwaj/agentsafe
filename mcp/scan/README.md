# Can an agent safely retry a call to a popular MCP server?

An agent's tool call can time out, or the agent's process can crash mid-call, and then nobody knows whether the
action happened. The only safe way to retry is with an **idempotency key**: the server remembers the key and
answers a repeat with the first result instead of acting twice. Payment APIs have done this for years. MCP has no
key in `tools/call` ([spec issue #3394](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/3394)),
so a server can only offer one as a tool argument.

This directory checks how many popular MCP servers do. [RESULTS.md](RESULTS.md) has the numbers; `run.sh`
reproduces them.

## Method

`agentsafe-mcp inspect --json` starts each server with dummy credentials (`env.sh`), lists its tools, and **calls
none of them**. For each tool it records:

- **Not read-only**: the tool doesn't set MCP's `readOnlyHint`. These hints are the server's own word (MCP calls them
  untrusted), and a tool without annotations is, by MCP's defaults, a destructive write.
- **With a key argument**: the tool has an argument named like an idempotency key (`idempotency_key`,
  `request_id`, `client_token`, `dedup_key` and variants). Before relying on the name-based count, every schema was
  also searched by hand for `idempot`, `request id`, `dedup`, `nonce`, `client token`, `unique`, `reference` and
  `receipt`: the only matches were Razorpay's `receipt` and `reference_id` (merchant references, not documented as
  idempotency keys) and HubSpot's `hasUniqueValue` (a property setting).
- **Annotated**: the server sent annotations at all.
- **Read-named, not read-only**: named like a read (`get_`, `fetch_`, `list_`, `search_`…) but not marked
  read-only: a sign the annotations don't describe the tools.

## What it found

The numbers are in [RESULTS.md](RESULTS.md); in short:

1. **No tool that changes something takes an idempotency key.** Creating an issue, sending a message, writing a
   file, creating a refund or a payment link, deploying an app: after a timeout, none of them can be retried
   without risking doing it twice, and the server gives the agent no way to ask "did that happen?" by key. Often
   the API underneath has no keys either (GitHub, Slack, Jira, Notion); where it does (Square's API asks for an
   `idempotency_key` on creates), the MCP server doesn't surface it as an argument.
2. **Annotations are uneven.** Most tools carry them, but some servers send none, and Razorpay marks all 41 of its
   tools identically as destructive and not read-only, including its 25 `fetch_*` reads. A client that gates on
   `destructiveHint` either asks about everything or, trusting a server that omits hints the other way, about
   nothing.
3. **Generic tools defeat per-tool policies.** Square's server exposes the whole API through one
   `make_api_request(service, method, request)`: listing customers and charging a card are the same tool, so any
   per-tool rule (approval, rate limit, key) applies to both or neither.

## What this does and doesn't show

- It shows what servers **declare**. It doesn't show what they **do**: a server could deduplicate without saying
  so, or (as [atlassian-mcp-server#132](https://github.com/atlassian/atlassian-mcp-server/issues/132) reports)
  create two tickets for one call. `agentsafe-mcp verify` checks behaviour, against a sandbox: one call must make
  one effect, and with a key, twenty simultaneous calls must make one.
- Tool lists can depend on credentials, toolsets and flags (GitHub's `--toolsets all` is used here). Servers run at
  `@latest` on the date in RESULTS.md; versions are the ones the servers report.
- Not scanned: Stripe (its npm package forwards to a hosted server that won't list tools without a real key),
  Twilio and Google Drive (wouldn't start without real credentials), and hosted-only servers such as Atlassian's
  and Linear's official ones. `atlassian` and `linear` here are community servers.
- `run.sh` downloads and runs each server's code (`npx`, `uvx`, `go run`) on your machine.

## What agentsafe does about it

For a tool without a key, `agentsafe-mcp` answers an exact repeat from its log, and when a call's outcome is lost
to a crash or a timeout it records it as **unknown and never retries it**, rather than guessing. For a tool with
one, it sends the same key on a retry. `inspect --policy-out` writes that policy for any server above.
