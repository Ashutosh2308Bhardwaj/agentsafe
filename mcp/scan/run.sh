#!/usr/bin/env bash
# Inspects every server in servers.tsv (agentsafe-mcp inspect --json: lists tools, calls none) into results/,
# then summarizes. Needs Go, Node (npx) and uv (uvx). npx/uvx download and run each server's code on this machine.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh
mkdir -p bin results work/fsroot
[ -d work/gitrepo ] || git init -q work/gitrepo
go build -o bin/agentsafe-mcp ../cmd/agentsafe-mcp
failed=0
while IFS=$'\t' read -r id _ cmd; do
  [[ -z "$id" || "$id" == \#* ]] && continue
  # shellcheck disable=SC2086 # the command is a word list on purpose
  if bin/agentsafe-mcp inspect --timeout 2m --json -- $cmd > "results/$id.json" 2> "work/$id.err"; then
    echo "ok    $id"
  else
    echo "FAIL  $id (see work/$id.err)"; rm -f "results/$id.json"; failed=1
  fi
done < servers.tsv
python3 summarize.py > RESULTS.md
echo "wrote RESULTS.md"
exit $failed
