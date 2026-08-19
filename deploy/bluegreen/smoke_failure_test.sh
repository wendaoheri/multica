#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")" && pwd); work=$(mktemp -d); trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir "$work/bin" "$work/artifacts"; : >"$work/body.json"
d=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
cat >"$work/artifacts/manifest.json" <<EOF
{"release_id":"r1","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d","combinations":{"W1K1S1":{"deployment":{"web_image":"r/web@sha256:$d","worker_image":"r/worker@sha256:$d","frontend_image":"r/front@sha256:$d","migrator_image":"r/migrate@sha256:$d","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d"}}}}
EOF
cat >"$work/runtime.good.json" <<EOF
{"release_id":"r1","combination":"W1K1S1","migration_manifest_checksum":"$d","deployment":{"web_image":"r/web@sha256:$d","worker_image":"r/worker@sha256:$d","frontend_image":"r/front@sha256:$d","migrator_image":"r/migrate@sha256:$d","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d"}}
EOF
attempt_b=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
attempt_a=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
cat >"$work/current-attempt.json" <<EOF
{"version":1,"attempt_id":"$attempt_b","release_id":"r1","combination":"W1K1S1","started_at":"2026-08-19T04:00:00Z","deployment":{"web_image":"r/web@sha256:$d","worker_image":"r/worker@sha256:$d","frontend_image":"r/front@sha256:$d","migrator_image":"r/migrate@sha256:$d","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d"}}
EOF
cat >"$work/migrator-execution.json" <<EOF
{"version":1,"result":"PASS","attempt_id":"$attempt_b","release_id":"r1","combination":"W1K1S1","started_at":"2026-08-19T04:00:00Z","completed_at":"2026-08-19T04:01:00Z","deployment":{"web_image":"r/web@sha256:$d","worker_image":"r/worker@sha256:$d","frontend_image":"r/front@sha256:$d","migrator_image":"r/migrate@sha256:$d","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d"}}
EOF
cat >"$work/bin/releasectl" <<'EOF'
#!/bin/sh
set -eu
[ "${1:-}" = verify-migrator-execution ] || exit 0
shift
while [ "$#" -gt 0 ]; do
  case "$1" in
    --manifest) manifest=$2; shift 2 ;;
    --combination) combination=$2; shift 2 ;;
    --current-attempt) current=$2; shift 2 ;;
    --execution-record) execution=$2; shift 2 ;;
    *) shift ;;
  esac
done
jq -e --arg combination "$combination" --slurpfile attempt "$current" --slurpfile record "$execution" '
  ($attempt[0]) as $a | ($record[0]) as $r |
  $a.version == 1 and ($a.attempt_id | test("^[0-9a-f]{64}$")) and
  $r.version == 1 and $r.result == "PASS" and $r.attempt_id == $a.attempt_id and
  $r.release_id == $a.release_id and $r.combination == $a.combination and
  $a.combination == $combination and $r.started_at == $a.started_at and
  $r.deployment == $a.deployment and
  (($a.started_at | fromdateiso8601) as $started |
   ($r.completed_at | fromdateiso8601) as $completed | $completed >= $started)
' "$manifest" >/dev/null
jq -n --slurpfile record "$execution" '{status:"verified",deployment:$record[0].deployment}'
EOF
cat >"$work/bin/curl" <<'EOF'
#!/bin/sh
case "$*" in *'/readyz'*) printf '{"ready":true}\n' ;; *'/status'*) printf '{"control":{"active_generation":2,"claims_enabled":false,"admission_open":false}}\n' ;; *'write-out'*) printf '200' ;; *) printf '200' ;; esac
EOF
cat >"$work/bin/docker" <<'EOF'
#!/bin/sh
d=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
if [ "$1" = compose ]; then
  for arg in "$@"; do service=$arg; done
  [ "$service" != migrator ] || { echo "cleaned one-shot migrator must not be queried" >&2; exit 1; }
  printf '%s\n' "$service"
  exit 0
fi
container=$2
case "$*" in
  *RepoDigests*) case "$container" in green-web) image=r/web ;; green-worker) image=r/worker ;; green-frontend) image=r/front ;; *) exit 1 ;; esac; printf '["%s@sha256:%s"]\n' "$image" "$d" ;;
  *release-id*) printf 'r1\n' ;; *combination*) printf 'W1K1S1\n' ;;
  *config-checksum*|*flags-checksum*|*migration-checksum*) printf '%s\n' "$d" ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$work/bin/"*
PATH="$work/bin:$PATH" COMBINATION=W1K1S1 COMPOSE_FILE="$work/compose.yml" \
  RELEASE_MANIFEST="$work/artifacts/manifest.json" RUNTIME_IDENTITY_REPORT="$work/runtime.collected.json" \
  MIGRATOR_CURRENT_ATTEMPT="$work/current-attempt.json" \
  MIGRATOR_EXECUTION_RECORD="$work/migrator-execution.json" \
  "$root/runtime_identity.sh"
jq -e --slurpfile expected "$work/runtime.good.json" '. == $expected[0]' "$work/runtime.collected.json" >/dev/null
run() {
  PATH="$work/bin:$PATH" COMBINATION=W1K1S1 WEB_URL=http://127.0.0.1:1 FRONTEND_URL=http://127.0.0.1:2 \
  WORKER_ADMIN_URL=http://127.0.0.1:3 CRITICAL_WRITE_URL=http://127.0.0.1:4 MULTICA_WORKER_ADMIN_TOKEN=x \
  RELEASE_ARTIFACT_DIR="$work/artifacts" RELEASE_MANIFEST="$work/artifacts/manifest.json" \
  RELEASECTL=releasectl CRITICAL_WRITE_BODY_FILE="$work/body.json" \
  COMPOSE_FILE="$work/compose.yml" MIGRATOR_CURRENT_ATTEMPT="$work/current-attempt.json" \
  MIGRATOR_EXECUTION_RECORD="$work/migrator-execution.json" \
  REPORT="$work/report.json" RUNTIME_IDENTITY_REPORT="$work/runtime.json" RUNTIME_IDENTITY_COMMAND="$1" "$root/smoke.sh"
}
run "cp '$work/runtime.good.json' '$work/runtime.json'"
jq -e '.runtime_identity.release_id == "r1"' "$work/report.json" >/dev/null
sed 's/"r1"/"wrong"/' "$work/runtime.good.json" >"$work/runtime.bad.json"
if run "cp '$work/runtime.bad.json' '$work/runtime.json'" >/dev/null 2>&1; then echo "mismatched runtime identity accepted" >&2; exit 1; fi
if run true >/dev/null 2>&1; then echo "missing runtime identity accepted" >&2; exit 1; fi

# Old success A cannot authorize after current attempt B begins, even when all
# release/combination/deployment fields are otherwise identical.
jq --arg attempt "$attempt_a" '.attempt_id=$attempt' "$work/migrator-execution.json" >"$work/old-success.json"
mv "$work/old-success.json" "$work/migrator-execution.json"
if run "$root/runtime_identity.sh" >/dev/null 2>&1; then echo "old success A authorized attempt B" >&2; exit 1; fi

# A failed completion write leaves an incomplete B and must remain closed.
printf '{\n' >"$work/migrator-execution.json"
if run "$root/runtime_identity.sh" >/dev/null 2>&1; then echo "incomplete attempt B authorized" >&2; exit 1; fi

write_success_b() {
  cat >"$work/migrator-execution.json" <<EOF
{"version":1,"result":"PASS","attempt_id":"$attempt_b","release_id":"r1","combination":"W1K1S1","started_at":"2026-08-19T04:00:00Z","completed_at":"2026-08-19T04:01:00Z","deployment":{"web_image":"r/web@sha256:$d","worker_image":"r/worker@sha256:$d","frontend_image":"r/front@sha256:$d","migrator_image":"r/migrate@sha256:$d","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d"}}
EOF
}
write_success_b
for mutation in epoch early invalid; do
  case "$mutation" in
    epoch) value=1970-01-01T00:00:00Z ;;
    early) value=2026-08-19T03:59:59Z ;;
    invalid) value='2026-08-19 04:01:00' ;;
  esac
  jq --arg value "$value" '.completed_at=$value' "$work/migrator-execution.json" >"$work/mutated.json"
  mv "$work/mutated.json" "$work/migrator-execution.json"
  if run "$root/runtime_identity.sh" >/dev/null 2>&1; then echo "$mutation timestamp authorized" >&2; exit 1; fi
  write_success_b
done
run "$root/runtime_identity.sh"
echo "runtime release/image/config/flags/migration identity fail-closed smoke: PASS"
