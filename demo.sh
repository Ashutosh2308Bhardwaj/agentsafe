#!/usr/bin/env bash
# The agentsafe demo: no credentials, no setup beyond Go. An agent charges a customer through a billing MCP server,
# the answer is lost, and the agent asks again: straight to the server, then through agentsafe-mcp.
set -euo pipefail
cd "$(dirname "$0")/mcp"
if ! command -v go >/dev/null; then
  echo "demo.sh needs Go 1.25 or later: https://go.dev/dl/" >&2
  exit 1
fi
work=$(mktemp -d)
echo "Building agentsafe-mcp and a fake billing MCP server (the first run downloads modules)..."
go build -o "$work/bin/agentsafe-mcp" ./cmd/agentsafe-mcp
go build -o "$work/bin/billing-mcp" ./internal/fakeupstream
echo
go run ./demo --proxy "$work/bin/agentsafe-mcp" --billing "$work/bin/billing-mcp" --work "$work"
