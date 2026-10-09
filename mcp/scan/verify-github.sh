#!/usr/bin/env bash
# Checks GitHub's MCP server's write tools against a scratch repository, for real: agentsafe-mcp verify makes one
# call per tool and counts the effect through GitHub's API before and after (one call, one effect). GitHub's
# lists lag its writes by a few seconds, hence --settle.
#
#   AGENTSAFE_SANDBOX_GITHUB_TOKEN  a fine-grained token for that one repository (Issues, Contents: read and write)
#   SANDBOX                         owner/repo of the scratch repository
#
# Writes results/github-verify.jsonl, one verdict per line.
set -euo pipefail
cd "$(dirname "$0")"
: "${AGENTSAFE_SANDBOX_GITHUB_TOKEN:?set it to a token for the scratch repository only}"
: "${SANDBOX:?owner/repo of the scratch repository}"
owner=${SANDBOX%/*} repo=${SANDBOX#*/}
export GITHUB_PERSONAL_ACCESS_TOKEN=$AGENTSAFE_SANDBOX_GITHUB_TOKEN GH_TOKEN=$AGENTSAFE_SANDBOX_GITHUB_TOKEN
mkdir -p bin work
go build -o bin/agentsafe-mcp ../cmd/agentsafe-mcp
server=(go run github.com/github/github-mcp-server/cmd/github-mcp-server@latest stdio --toolsets all)
run=$(date +%s)
api="gh api"

# A policy per tool: no key (GitHub's API has none), every argument the identity.
policy=work/github-policy.json
cat > "$policy" <<JSON
{"tools": {"issue_write": {"identity": ["*"]}, "add_issue_comment": {"identity": ["*"]},
           "create_branch": {"identity": ["*"]}, "create_or_update_file": {"identity": ["*"]}}}
JSON

# An issue to comment on, made through the API (not through the tool being checked).
target=$($api "repos/$SANDBOX/issues" -f title="agentsafe verify: comment target $run" --jq .number)

check() { # tool, args, count command
  bin/agentsafe-mcp verify --json --sandbox --settle 5s --policy "$policy" --tool "$1" --args "$2" --count "$3" \
    -- "${server[@]}" || true
}
out=results/github-verify.jsonl
: > "$out"
check issue_write "{\"method\":\"create\",\"owner\":\"$owner\",\"repo\":\"$repo\",\"title\":\"agentsafe verify $run\"}" \
  "$api 'repos/$SANDBOX/issues?state=all&per_page=100' --paginate --jq '.[] | select(.pull_request == null) | .number' | wc -l" >> "$out"
check add_issue_comment "{\"owner\":\"$owner\",\"repo\":\"$repo\",\"issue_number\":$target,\"body\":\"agentsafe verify $run\"}" \
  "$api 'repos/$SANDBOX/issues/$target/comments?per_page=100' --paginate --jq '.[].id' | wc -l" >> "$out"
check create_branch "{\"owner\":\"$owner\",\"repo\":\"$repo\",\"branch\":\"agentsafe-verify-$run\"}" \
  "$api 'repos/$SANDBOX/branches?per_page=100' --paginate --jq '.[].name' | wc -l" >> "$out"
check create_or_update_file "{\"owner\":\"$owner\",\"repo\":\"$repo\",\"branch\":\"agentsafe-verify-$run\",\"path\":\"verify-$run.txt\",\"content\":\"agentsafe verify\",\"message\":\"agentsafe verify $run\"}" \
  "$api 'repos/$SANDBOX/commits?sha=agentsafe-verify-$run&per_page=100' --paginate --jq '.[].sha' | wc -l" >> "$out"
cat "$out"
