#!/usr/bin/env bash
# The same "create issue" call sent twice, as an MCP client does when the first answer is lost to a timeout:
# straight to GitHub's MCP server, then through agentsafe-mcp. Counts the issues through GitHub's API each time.
# Needs a scratch repository (SANDBOX=owner/repo) and a token for it (AGENTSAFE_SANDBOX_GITHUB_TOKEN).
set -euo pipefail
cd "$(dirname "$0")"
: "${AGENTSAFE_SANDBOX_GITHUB_TOKEN:?a token for the scratch repository only}"
: "${SANDBOX:?owner/repo of the scratch repository}"
export GITHUB_PERSONAL_ACCESS_TOKEN=$AGENTSAFE_SANDBOX_GITHUB_TOKEN GH_TOKEN=$AGENTSAFE_SANDBOX_GITHUB_TOKEN
mkdir -p bin work/retry
go build -o bin/retry ./retry
go build -o bin/agentsafe-mcp ../cmd/agentsafe-mcp
GOBIN=$PWD/bin go install github.com/github/github-mcp-server/cmd/github-mcp-server@latest
count() { # GitHub's lists lag its writes by a few seconds
  sleep 5
  gh api "repos/$SANDBOX/issues?state=all&per_page=100" --paginate --jq '.[] | select(.pull_request == null) | .number' | wc -l | tr -d ' '
}
args="{\"method\":\"create\",\"owner\":\"${SANDBOX%/*}\",\"repo\":\"${SANDBOX#*/}\",\"title\":\"Refund ticket T-77 ($(date +%s))\"}"

echo "== directly to GitHub's MCP server"
before=$(count)
bin/retry --tool issue_write --args "$args" -- bin/github-mcp-server stdio 2>/dev/null
echo "issues created: $(($(count) - before))"

echo "== through agentsafe-mcp"
rm -f work/retry/calls.jsonl
echo '{"tools": {"issue_write": {"identity": ["*"]}}}' > work/retry/policy.json
before=$(count)
bin/retry --tool issue_write --args "$args" -- bin/agentsafe-mcp --quiet --log work/retry/calls.jsonl \
  --policy work/retry/policy.json -- bin/github-mcp-server stdio 2>/dev/null
echo "issues created: $(($(count) - before))"
