# Example: agentsafe under a LangGraph agent

A Python agent built with LangChain's `create_agent` (a LangGraph graph) bills customers through an MCP server. **It has no agentsafe code.** Putting agentsafe in front of the billing server is a change to the agent's MCP configuration only:

```diff
 {
   "billing": {
     "transport": "stdio",
-    "command": "billing-mcp",
-    "args": ["--state", "run/books.json"]
+    "command": "bin/agentsafe-mcp",
+    "args": ["--log", "run/calls.jsonl", "--policy", "run/policy.json", "--started-by", "support-agent", "--",
+             "bin/billing-mcp", "--state", "run/books.json"]
   }
 }
```

From then on, the billing server's `charge` tool is protected by the policy (`run.sh` writes it):

```json
{ "approvers": ["user:<you>"],
  "tools": { "charge": { "identity": ["ticket_id"], "key": "argument", "key_argument": "idempotency_key", "approval": "always" } } }
```

One charge per ticket, keyed into the server's own `idempotency_key`, and a person's approval before it happens.

## Run it

Python 3.12, Go, and this checkout. No API key: the model is scripted (`ScriptedModel` in `agent.py`), and everything else is real (LangGraph's agent loop, `langchain-mcp-adapters`, stdio, agentsafe-mcp, the billing server).

```bash
cd mcp/examples/langgraph
python3.12 -m venv .venv && .venv/bin/pip install --require-hashes -r requirements.txt
PYTHON=.venv/bin/python ./run.sh
```

What it does, and what you'll see:

1. **The agent asks to charge T-77** and is answered `pending_approval`, with the operation's key. Nothing was charged.
2. **A person runs `agentsafe-mcp pending`**, sees the charge with its values, and **`agentsafe-mcp approve KEY`**, as their own OS account.
3. **The agent asks again** (a later run, or the same one retrying): `charged 1200.00 (charge 1)`.
4. **And again**: the same answer, from agentsafe's log. The billing server is never asked twice.
5. **The trace** of the proxy's log: the approval, who decided, the charge, the replay.

`run.sh` exits non-zero if any step isn't as described, and CI runs it on every push (`langgraph-e2e`).

```
invoke_agent billing-agent  1.3s  [proxy run, open, 10 events]
├─ ⏸ approval charge  waited 96ms  approved by user:you
├─ execute_tool charge  5ms  ok  (from mcp)
├─ execute_tool charge  4ms  replayed  (from mcp)
```

`(from mcp)` is the client's name as it reports itself: the Python MCP SDK's default, which the adapter doesn't override. It's recorded, never trusted.

## With a real model

Replace `ScriptedModel(...)` with any LangChain chat model, for example `ChatAnthropic(model="claude-opus-5-5")` from `langchain-anthropic`. The model then decides when to charge, and what to tell the customer while the charge waits for approval. Nothing on the agentsafe side changes.

## Files

- `agent.py`: the agent. LangChain, LangGraph and the MCP adapter; no agentsafe.
- `mcp_servers.json`: the agent's MCP configuration: the only place agentsafe appears.
- `run.sh`: builds `agentsafe-mcp`, the billing server (`mcp/internal/fakeupstream`) and the trace tool, and plays the story.
- `requirements.in` / `requirements.txt`: the Python dependencies, every one pinned by hash (`pip-compile --generate-hashes`).
