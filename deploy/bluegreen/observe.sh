#!/bin/sh
set -eu

: "${1:?usage: observe.sh REPORT.json}"
report=$1

decision=$(jq -er '
  def required:
    (.baseline.request_count_15m|type == "number") and
    (.baseline.errors_5xx_rate|type == "number") and
    (.baseline.p95_ms|type == "number") and
    (.window.request_count_2m|type == "number") and
    (.window.errors_5xx_2m|type == "number") and
    (.window.errors_5xx_rate_2m|type == "number") and
    (.window.request_count_5m|type == "number") and
    (.window.p95_ms_5m|type == "number") and
    (.window.consecutive_p95_breaches|type == "number") and
    (.window.cpu_1m_pct|type == "number") and
    (.window.cpu_ge85_minutes|type == "number") and
    (.window.mem_available_mib|type == "number") and
    (.window.mem_below768_seconds|type == "number") and
    (.window.db_connections|type == "number") and
    (.window.db_connections_seconds|type == "number") and
    (.signals.auth_write_queue_ws_smoke_ok|type == "boolean") and
    (.signals.duplicate_claim_or_run|type == "boolean") and
    (.signals.relay_healthy|type == "boolean") and
    (.signals.panic_oom_restart|type == "boolean") and
    (.signals.data_discrepancy|type == "boolean");
  if (required|not) then error("invalid observation report")
  elif (.signals.auth_write_queue_ws_smoke_ok|not)
    or .signals.duplicate_claim_or_run
    or (.signals.relay_healthy|not)
    or .signals.panic_oom_restart
    or .signals.data_discrepancy
    or (.window.cpu_1m_pct >= 95)
    or (.window.mem_available_mib < 512)
    or ((.window.db_connections >= 80) and (.window.db_connections_seconds >= 60))
    or ((.window.request_count_2m >= 100)
      and ((.window.errors_5xx_rate_2m >= 0.05)
        or ((.window.errors_5xx_2m >= 5)
          and (.window.errors_5xx_rate_2m >= (.baseline.errors_5xx_rate * 2)))))
    or ((.window.request_count_2m < 100) and (.window.errors_5xx_2m >= 3))
    or ((.window.request_count_5m >= 200)
      and (.window.consecutive_p95_breaches >= 2)
      and (.window.p95_ms_5m > ([.baseline.p95_ms * 2, .baseline.p95_ms + 500] | max)))
  then "ROLLBACK"
  elif ((.window.cpu_1m_pct >= 85) and (.window.cpu_ge85_minutes >= 3))
    or ((.window.mem_available_mib < 768) and (.window.mem_below768_seconds >= 60))
  then "STOP"
  else "CONTINUE" end
' "$report") || exit 2

printf '%s\n' "$decision"
case "$decision" in
  CONTINUE) exit 0 ;;
  STOP) exit 10 ;;
  ROLLBACK) exit 20 ;;
  *) exit 2 ;;
esac
