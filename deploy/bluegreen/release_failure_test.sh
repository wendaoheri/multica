#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir "$work/bin" "$work/artifacts"
log="$work/actions.log"
live="$work/live.fragment"
blue="$work/blue.fragment"
green="$work/green.fragment"
config="$work/Caddyfile"
printf 'BLUE\n' >"$live"
printf 'BLUE\n' >"$blue"
printf 'GREEN\n' >"$green"
printf 'import %s\n' "$live" >"$config"
: >"$work/compose.yml"
: >"$work/config.actual"
: >"$work/flags.actual"
printf '{"release_id":"test-release","migration_manifest_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' >"$work/artifacts/manifest.json"

cat >"$work/bin/releasectl" <<'EOF'
#!/bin/sh
printf 'releasectl %s\n' "$*" >>"$ACTION_LOG"
if [ "${DENY_TARGET:-0}" = 1 ] && [ "$1" = verify-manifest ]; then exit 1; fi
case "$1" in
  checksum) printf '%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  advance-generation) printf '{"active_generation":2}\n' ;;
  *) printf '{"status":"ok"}\n' ;;
esac
EOF
cat >"$work/bin/docker" <<'EOF'
#!/bin/sh
printf 'docker %s\n' "$*" >>"$ACTION_LOG"
case "$*" in *'config --format json'*)
  d=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  printf '{"services":{"blue-web":{"image":"r/web@sha256:%s"},"green-web":{"image":"r/web@sha256:%s"},"blue-worker":{"image":"r/worker@sha256:%s"},"green-worker":{"image":"r/worker@sha256:%s"},"blue-frontend":{"image":"r/frontend@sha256:%s"},"green-frontend":{"image":"r/frontend@sha256:%s"},"migrator":{"image":"r/migrator@sha256:%s"}}}\n' "$d" "$d" "$d" "$d" "$d" "$d" "$d" ;;
esac
exit 0
EOF
cat >"$work/bin/caddy" <<'EOF'
#!/bin/sh
printf 'caddy %s\n' "$*" >>"$ACTION_LOG"
case "$1" in
  validate)
    if [ "${FAIL_CANDIDATE:-0}" = 1 ] && printf '%s' "$3" | grep -q candidate; then exit 1; fi
    ;;
  reload) [ "${FAIL_RELOAD:-0}" = 1 ] && exit 1 ;;
esac
exit 0
EOF
cat >"$work/bin/curl" <<'EOF'
#!/bin/sh
url=''
for arg in "$@"; do url=$arg; done
printf 'curl %s\n' "$url" >>"$ACTION_LOG"
case "$url" in
  */status) printf '%s\n' '{"control":{"claims_enabled":false,"admission_open":false,"drain_requested":true,"worker_in_flight":0,"worker_leases":0,"drain_zero_since":"2026-08-19T00:00:00Z"},"worker":{"accepting_new":false}}' ;;
  */admission/close) if [ "${FAIL_CLOSE:-0}" = 1 ] && [ -f "$ACTION_LOG.activation-failed" ]; then exit 22; fi ;;
  *green*/activate) if [ "${FAIL_GREEN_ACTIVATE:-0}" = 1 ]; then : >"$ACTION_LOG.activation-failed"; exit 22; fi ;;
  *) printf '%s\n' '{}' ;;
esac
EOF
chmod +x "$work/bin/"*

run_release() {
  PATH="$work/bin:$PATH" ACTION_LOG="$log" LIVE_FRAGMENT="$live" \
  RELEASE_ARTIFACT_DIR="$work/artifacts" TARGET_COMBINATION=W1K1S1 \
  RELEASE_CONFIG_FILE="$work/config.actual" RELEASE_FEATURE_FLAGS_FILE="$work/flags.actual" \
  COMPOSE_FILE="$work/compose.yml" CADDY_CONFIG="$config" \
  CADDY_MANAGED_FRAGMENT="$live" CADDY_BIN=caddy RELEASECTL=releasectl \
  MULTICA_WORKER_ADMIN_TOKEN=01234567890123456789012345678901 \
  RELEASE_LOCK_FILE="$work/release.lock" ACTIVE_GENERATION=1 \
  BLUE_ADMIN_URL=http://127.0.0.1:9000/blue \
  GREEN_ADMIN_URL=http://127.0.0.1:9001/green \
  BLUE_FRAGMENT="$blue" GREEN_FRAGMENT="$green" \
  BLUE_SMOKE_COMMAND=true GREEN_SMOKE_COMMAND=true \
  OBSERVATION_COMMAND="${TEST_OBSERVATION_COMMAND-true}" OBSERVATION_DURATION_SECONDS="${TEST_OBSERVATION_DURATION-1}" OBSERVATION_INTERVAL_SECONDS=1 \
  MULTICA_RELEASE_TEST_ONLY_SHORT_OBSERVATION=1 \
  "$root/release.sh" "$@"
}

DENY_TARGET=1; export DENY_TARGET
set +e
run_release validate >"$work/out" 2>&1
code=$?
set -e
test "$code" -ne 0
! grep -q 'curl ' "$log"
unset DENY_TARGET

: >"$log"
FAIL_CANDIDATE=1; export FAIL_CANDIDATE
set +e
run_release cutover-green --execute >"$work/out" 2>&1
code=$?
set -e
test "$code" -ne 0
test "$(cat "$live")" = BLUE
unset FAIL_CANDIDATE

: >"$log"
FAIL_GREEN_ACTIVATE=1; export FAIL_GREEN_ACTIVATE
FAIL_CLOSE=1; export FAIL_CLOSE
set +e
run_release cutover-green --execute >"$work/out" 2>&1
code=$?
set -e
test "$code" -ne 0
! grep -q 'green/admission/open\|green/enable-claims' "$log"
unset FAIL_GREEN_ACTIVATE
unset FAIL_CLOSE
rm -f "$log.activation-failed"

: >"$log"
printf 'BLUE\n' >"$live"
TEST_OBSERVATION_COMMAND=''; export TEST_OBSERVATION_COMMAND
set +e
run_release cutover-green --execute >"$work/out" 2>&1
code=$?
set -e
test "$code" -ne 0
grep -q 'green/activate' "$log" || { cat "$work/out" >&2; cat "$log" >&2; exit 1; }
grep -q 'blue/activate' "$log" || { cat "$work/out" >&2; cat "$log" >&2; exit 1; }
unset TEST_OBSERVATION_COMMAND

: >"$log"
printf 'BLUE\n' >"$live"
FAIL_RELOAD=1; export FAIL_RELOAD
set +e
run_release cutover-green --execute >"$work/out" 2>&1
code=$?
set -e
test "$code" -ne 0
if ! grep -q '/admission/close' "$log" || ! grep -q '/drain' "$log"; then
  echo "reload failure did not retain closed gates" >&2
  cat "$log" >&2
  cat "$work/out" >&2
  exit 1
fi

echo "release target DENY, candidate validation, atomic activation, reload/rollback fail-closed injection: PASS"
