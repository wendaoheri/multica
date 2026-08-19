#!/bin/sh
set -eu

: "${COMBINATION:?set COMBINATION, for example W1K1S1}"
: "${WEB_URL:?set WEB_URL to a loopback candidate endpoint}"
: "${FRONTEND_URL:?set FRONTEND_URL to a loopback candidate endpoint}"
: "${WORKER_ADMIN_URL:?set WORKER_ADMIN_URL}"
: "${MULTICA_WORKER_ADMIN_TOKEN:?set MULTICA_WORKER_ADMIN_TOKEN}"
: "${REPORT:?set REPORT output path}"

case "$COMBINATION" in W0K0S0|W0K0S1|W0K0S2|W0K1S0|W0K1S1|W0K1S2|W1K0S0|W1K0S1|W1K0S2|W1K1S0|W1K1S1|W1K1S2) ;;
  *) echo "unknown combination" >&2; exit 1 ;;
esac
case "$WEB_URL$FRONTEND_URL$WORKER_ADMIN_URL" in *localhost*|*127.0.0.1*|*\[::1\]*) ;;
  *) echo "smoke endpoints must be isolated loopback endpoints" >&2; exit 1 ;;
esac

ready=$(curl --fail --silent --show-error "$WEB_URL/readyz")
front_status=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "$FRONTEND_URL/")
worker=$(curl --fail --silent --show-error \
  -H "Authorization: Bearer $MULTICA_WORKER_ADMIN_TOKEN" "$WORKER_ADMIN_URL/status")
test "$front_status" = 200
printf '%s' "$worker" | jq -e '.control.active_generation > 0' >/dev/null

timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
jq -n \
  --arg combination "$COMBINATION" \
  --arg timestamp "$timestamp" \
  --argjson readiness "$ready" \
  --argjson worker "$worker" \
  '{combination:$combination,timestamp:$timestamp,result:"PASS",readiness:$readiness,worker:$worker,external_side_effects:0}' \
  >"$REPORT"
