#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM

write_report() {
  errors=$1 cpu=$2 cpu_minutes=$3 memory=$4 memory_seconds=$5 relay=$6
  jq -n --argjson errors "$errors" --argjson cpu "$cpu" \
    --argjson cpu_minutes "$cpu_minutes" --argjson memory "$memory" \
    --argjson memory_seconds "$memory_seconds" --argjson relay "$relay" '
    {baseline:{request_count_15m:500,errors_5xx_rate:0.01,p95_ms:200},
     window:{request_count_2m:120,errors_5xx_2m:$errors,
       errors_5xx_rate_2m:($errors/120),request_count_5m:250,p95_ms_5m:250,
       consecutive_p95_breaches:0,cpu_1m_pct:$cpu,cpu_ge85_minutes:$cpu_minutes,
       mem_available_mib:$memory,mem_below768_seconds:$memory_seconds,
       db_connections:20,db_connections_seconds:0},
     signals:{auth_write_queue_ws_smoke_ok:true,duplicate_claim_or_run:false,
       relay_healthy:$relay,panic_oom_restart:false,data_discrepancy:false}}' >"$work/report.json"
}

write_report 0 20 0 2048 0 true
test "$("$root/observe.sh" "$work/report.json")" = CONTINUE

write_report 0 86 3 2048 0 true
set +e
"$root/observe.sh" "$work/report.json" >"$work/out"
code=$?
set -e
test "$code" -eq 10
grep -qx STOP "$work/out"

write_report 6 20 0 2048 0 true
set +e
"$root/observe.sh" "$work/report.json" >"$work/out"
code=$?
set -e
test "$code" -eq 20
grep -qx ROLLBACK "$work/out"

write_report 0 20 0 2048 0 false
set +e
"$root/observe.sh" "$work/report.json" >"$work/out"
code=$?
set -e
test "$code" -eq 20

echo "observation 5xx/resource/relay thresholds: PASS"
