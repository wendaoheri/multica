#!/bin/sh
set -eu

: "${COMBINATION:?set COMBINATION, for example W1K1S1}"
: "${WEB_URL:?set WEB_URL to a loopback candidate endpoint}"
: "${FRONTEND_URL:?set FRONTEND_URL to a loopback candidate endpoint}"
: "${WORKER_ADMIN_URL:?set WORKER_ADMIN_URL}"
: "${MULTICA_WORKER_ADMIN_TOKEN:?set MULTICA_WORKER_ADMIN_TOKEN}"
: "${RELEASE_ARTIFACT_DIR:?set RELEASE_ARTIFACT_DIR}"
: "${RELEASE_MANIFEST:=$RELEASE_ARTIFACT_DIR/manifest.json}"
: "${RELEASECTL:=releasectl}"
: "${CRITICAL_WRITE_URL:?set a loopback fake/isolated critical write endpoint}"
: "${CRITICAL_WRITE_BODY_FILE:?set a non-secret fake request body file}"
: "${REPORT:?set REPORT output path}"
: "${RUNTIME_IDENTITY_COMMAND:?set RUNTIME_IDENTITY_COMMAND to the running-container identity collector}"
: "${RUNTIME_IDENTITY_REPORT:?set RUNTIME_IDENTITY_REPORT output path}"

case "$COMBINATION" in W0K0S0|W0K0S1|W0K0S2|W0K1S0|W0K1S1|W0K1S2|W1K0S0|W1K0S1|W1K0S2|W1K1S0|W1K1S1|W1K1S2) ;;
  *) echo "unknown combination" >&2; exit 1 ;;
esac
for url in "$WEB_URL" "$FRONTEND_URL" "$WORKER_ADMIN_URL" "$CRITICAL_WRITE_URL"; do
  case "$url" in http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
    *) echo "smoke endpoint must be isolated loopback: $url" >&2; exit 1 ;;
  esac
done

"$RELEASECTL" verify-manifest \
  --manifest "$RELEASE_MANIFEST" \
  --artifact-dir "$RELEASE_ARTIFACT_DIR" \
  --require-combination "$COMBINATION" >/dev/null

rm -f "$RUNTIME_IDENTITY_REPORT"
sh -c "$RUNTIME_IDENTITY_COMMAND"
test -s "$RUNTIME_IDENTITY_REPORT"
jq -e --arg combination "$COMBINATION" --slurpfile runtime "$RUNTIME_IDENTITY_REPORT" '
  .release_id == $runtime[0].release_id and
  $runtime[0].combination == $combination and
  .migration_manifest_checksum == $runtime[0].migration_manifest_checksum and
  .combinations[$combination].deployment == $runtime[0].deployment
' "$RELEASE_MANIFEST" >/dev/null

successes=0
while [ "$successes" -lt 10 ]; do
  ready=$(curl --fail --silent --show-error "$WEB_URL/readyz")
  printf '%s' "$ready" | jq -e '(.status // "ok") == "ok" or (.ready == true)' >/dev/null
  successes=$((successes + 1))
  [ "$successes" -eq 10 ] || sleep 1
done
front_status=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "$FRONTEND_URL/")
worker=$(curl --fail --silent --show-error \
  -H "Authorization: Bearer $MULTICA_WORKER_ADMIN_TOKEN" "$WORKER_ADMIN_URL/status")
write_status=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  -H 'Content-Type: application/json' \
  -H 'X-Multica-Release-Smoke: isolated-no-side-effects' \
  --data-binary "@$CRITICAL_WRITE_BODY_FILE" "$CRITICAL_WRITE_URL")
test "$front_status" = 200
case "$write_status" in 200|201|202|204) ;; *) echo "critical write smoke failed: HTTP $write_status" >&2; exit 1 ;; esac
printf '%s' "$worker" | jq -e \
  '.control.active_generation > 0
   and (.control.claims_enabled == false)
   and (.control.admission_open == false)' >/dev/null

timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
jq -n \
  --arg combination "$COMBINATION" \
  --arg timestamp "$timestamp" \
  --argjson worker "$worker" \
  --argjson readiness_successes "$successes" \
  --arg critical_write_http "$write_status" \
  --slurpfile runtime "$RUNTIME_IDENTITY_REPORT" \
  '{combination:$combination,timestamp:$timestamp,result:"PASS",
    readiness_successes:$readiness_successes,worker:$worker,
    runtime_identity:$runtime[0],critical_write_http:($critical_write_http|tonumber),external_side_effects:0}' \
  >"$REPORT"
