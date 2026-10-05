#!/usr/bin/env bash
# kill -9 the example agent at every point of the money flow (reconcile -> gate -> approve -> gateway),
# drive each run to completion in fresh processes (resume / approve as needed), then reconcile it against
# the systems of record. Exits non-zero if any point fails.
#
#   scripts/money_sweep.sh          scripted model (free, deterministic) - this is what CI runs
#   scripts/money_sweep.sh real     a real model via GROQ_API_KEY (~70 requests)
#
# Pass per point: the run finishes AND reconciliation passes (4 discrepancies exactly once, exactly one
# authorised payment, claim matches the ledger).
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

bin="$(mktemp -d)/reconcile"
go build -o "$bin" ./examples/reconcile || exit 1
mode=(-mock)
[[ "${1:-}" == "real" ]] && mode=()
out=examples/reconcile/out
stamp=$(date +%H%M%S)
points=(
  after_model_call:3 after_model_logged:3 before_tool_executed:3 after_tool_executed:3 after_result_logged:3
  approval_requested:1 # dies the moment it asks for approval
  approval_decided:1   # approved, dies before anything is sent
  gateway_charged:1    # money moved, dies before anything is logged
)

key_of() { grep '"type":"approval_requested"' "$out/$1-log.jsonl" | grep -o '"key":"[a-f0-9]*"' | cut -d'"' -f4; }

failed=0
summary=()
for pn in "${points[@]}"; do
  p=${pn%%:*} n=${pn##*:} run="m$stamp-$p" kills=0 result=""
  printf '\n######## %s (occurrence %s) ########\n' "$p" "$n"
  for attempt in 1 2 3 4 5 6; do
    envs=()
    ((kills == 0)) && envs=(KILL_AT="$p" KILL_NTH="$n")
    if ((attempt == 1)); then
      args=(-run "$run")
    elif grep -q '"type":"approval_requested"' "$out/$run-log.jsonl" 2>/dev/null &&
      ! grep -q '"type":"approval_decided"' "$out/$run-log.jsonl"; then
      args=(-run "$run" -approve "$(key_of "$run")")
    else
      args=(-run "$run" -resume)
    fi
    result=$(env ${envs[@]+"${envs[@]}"} "$bin" ${mode[@]+"${mode[@]}"} "${args[@]}" 2>&1)
    code=$?
    grep -E "💀|APPROVE|RESUME|↻|status=" <<<"$result" | sed 's/^/  /'
    if ((code == 137)); then kills=$((kills + 1)); continue; fi
    grep -q "status=finished" <<<"$result" && break
  done
  verdict=$(grep -oE "reconciliation: (PASS|FAIL)" <<<"$result" | cut -d' ' -f2)
  [[ $kills -eq 1 && $verdict == PASS ]] || failed=1
  summary+=("$(printf '%-22s killed=%s  reconciliation=%s' "$p" "$kills" "${verdict:-none}")")
done

printf '\n=== kill -9 across the money flow ===\n'
printf '%s\n' "${summary[@]}"
exit $failed
