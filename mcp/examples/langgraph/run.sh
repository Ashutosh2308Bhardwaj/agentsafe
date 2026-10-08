#!/usr/bin/env bash
# The whole story with real processes: a LangGraph agent, agentsafe-mcp, a billing MCP server, and a person
# approving from the command line. Exits non-zero if anything isn't as it should be.
#
#   python3 -m venv .venv && .venv/bin/pip install --require-hashes -r requirements.txt
#   PYTHON=.venv/bin/python ./run.sh
set -euo pipefail
cd "$(dirname "$0")"
PYTHON="${PYTHON:-python3}"
repo="$(cd ../../.. && pwd)"

# The binaries. agentsafe-mcp isn't released yet, so it's built against this checkout's core through a workspace.
rm -rf bin run && mkdir -p bin run
ws="$(mktemp -d)" && trap 'rm -rf "$ws"' EXIT
(cd "$ws" && GOTOOLCHAIN=auto go work init "$repo" "$repo/mcp" >/dev/null 2>&1)
(cd "$repo/mcp" && GOWORK="$ws/go.work" GOTOOLCHAIN=auto go build -o "$OLDPWD/bin/agentsafe-mcp" ./cmd/agentsafe-mcp \
  && GOWORK="$ws/go.work" GOTOOLCHAIN=auto go build -o "$OLDPWD/bin/billing-mcp" ./internal/fakeupstream)
(cd "$repo" && GOWORK="$ws/go.work" GOTOOLCHAIN=auto go build -o "$OLDPWD/bin/trace" ./cmd/trace)

# The policy: charge is one operation per ticket, keyed into the billing server's idempotency_key, and needs a
# person's approval. The approver is whoever runs this script; the agent runs as "support-agent".
cat > run/policy.json <<POLICY
{ "approvers": ["user:$(id -un)"],
  "tools": { "charge": { "identity": ["ticket_id"], "key": "argument", "key_argument": "idempotency_key", "approval": "always" } } }
POLICY

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
fail() { printf '\nFAILED: %s\n' "$*"; exit 1; }
agent() { "$PYTHON" agent.py T-77 1200.00 2>run/agent.err; }
mcp() { bin/agentsafe-mcp "$1" --log run/calls.jsonl --policy run/policy.json "${@:2}"; }

say "1. The agent asks to charge T-77. Charging needs approval:"
out="$(agent)" && echo "$out"
key="$(sed -n 's/.*"key":"\([0-9a-f]*\)".*/\1/p' <<<"$out")"
[[ "$out" == *pending_approval* && ${#key} -eq 32 ]] || fail "expected pending_approval with a key"

say "2. A person looks at what's waiting, and approves it:"
mcp pending
mcp approve "$key"

say "3. The agent asks again (any later run of it, or the same one retrying):"
out="$(agent)" && echo "$out"
[[ "$out" == *"charged 1200.00 (charge 1)"* ]] || fail "expected the charge"

say "4. And again: answered from agentsafe's log, the billing server isn't asked twice:"
out="$(agent)" && echo "$out"
[[ "$out" == *"charged 1200.00 (charge 1)"* ]] || fail "expected the same answer"
grep -q '"charges":1' run/books.json || fail "the billing server must have charged once: $(cat run/books.json)"

say "5. What happened, from the log:"
bin/trace --agent billing-agent run/calls.jsonl
say "OK: one approval, one charge, every call on the record."
