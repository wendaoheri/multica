#!/bin/sh
set -eu

usage() {
  echo "usage: release.sh validate|cutover-green|rollback-blue [--execute]" >&2
  exit 2
}

mode=${1:-}
execute=${2:-}
case "$mode" in validate|cutover-green|rollback-blue) ;; *) usage ;; esac

: "${RELEASE_ARTIFACT_DIR:?set RELEASE_ARTIFACT_DIR}"
: "${RELEASE_MANIFEST:=$RELEASE_ARTIFACT_DIR/manifest.json}"
: "${TARGET_COMBINATION:?set TARGET_COMBINATION to the actual W/K/S target}"
: "${COMPOSE_FILE:?set COMPOSE_FILE}"
: "${CADDY_CONFIG:?set CADDY_CONFIG}"
: "${CADDY_MANAGED_FRAGMENT:?set CADDY_MANAGED_FRAGMENT}"
: "${CADDY_BIN:=caddy}"
: "${RELEASECTL:=releasectl}"
: "${MULTICA_WORKER_ADMIN_TOKEN:?set MULTICA_WORKER_ADMIN_TOKEN}"
: "${RELEASE_CONFIG_FILE:?set RELEASE_CONFIG_FILE to the actual effective config}"
: "${RELEASE_FEATURE_FLAGS_FILE:?set RELEASE_FEATURE_FLAGS_FILE to the actual effective flags}"
: "${RELEASE_ACTUAL_IDENTITY_FILE:=$RELEASE_ARTIFACT_DIR/deployment.actual.json}"
: "${RELEASE_EXECUTION_DIR:?set RELEASE_EXECUTION_DIR for one-shot execution records}"

case "$CADDY_MANAGED_FRAGMENT" in /*) ;; *) echo "CADDY_MANAGED_FRAGMENT must be absolute" >&2; exit 1 ;; esac
case "$CADDY_CONFIG" in /*) ;; *) echo "CADDY_CONFIG must be absolute" >&2; exit 1 ;; esac

verify_artifacts() {
  "$RELEASECTL" verify-manifest \
    --manifest "$RELEASE_MANIFEST" \
    --artifact-dir "$RELEASE_ARTIFACT_DIR" \
    --require-combination "$TARGET_COMBINATION"
  RELEASE_CONFIG_CHECKSUM=$($RELEASECTL checksum --file "$RELEASE_CONFIG_FILE")
  RELEASE_FLAGS_CHECKSUM=$($RELEASECTL checksum --file "$RELEASE_FEATURE_FLAGS_FILE")
  RELEASE_MIGRATION_CHECKSUM=$(jq -er .migration_manifest_checksum "$RELEASE_MANIFEST")
  RELEASE_ID=$(jq -er .release_id "$RELEASE_MANIFEST")
  export RELEASE_CONFIG_CHECKSUM RELEASE_FLAGS_CHECKSUM RELEASE_MIGRATION_CHECKSUM RELEASE_ID TARGET_COMBINATION
  # The migration profile is part of the release identity. Stream the fully
  # expanded model directly to the verifier so env_file secrets are never
  # persisted in an artifact or log.
  docker compose --profile migration -f "$COMPOSE_FILE" config --format json | "$RELEASECTL" verify-deployment \
    --manifest "$RELEASE_MANIFEST" --artifact-dir "$RELEASE_ARTIFACT_DIR" \
    --combination "$TARGET_COMBINATION" --compose-json - \
    --config-file "$RELEASE_CONFIG_FILE" --feature-flags-file "$RELEASE_FEATURE_FLAGS_FILE" \
    --output "$RELEASE_ACTUAL_IDENTITY_FILE"
  "$CADDY_BIN" validate --config "$CADDY_CONFIG"
}

admin_post() {
  url=$1
  action=$2
  case "$url" in http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
    *) echo "worker admin URL must be loopback: $url" >&2; return 1 ;;
  esac
  curl --fail --silent --show-error \
    -H "Authorization: Bearer $MULTICA_WORKER_ADMIN_TOKEN" \
    -X POST "$url/$action"
}

force_closed() {
  failed=0
  for url in ${BLUE_ADMIN_URL:-} ${GREEN_ADMIN_URL:-}; do
    admin_post "$url" admission/close >/dev/null 2>&1 || failed=1
    admin_post "$url" drain >/dev/null 2>&1 || failed=1
  done
  return "$failed"
}

wait_drained() {
  url=$1
  attempts=0
  while [ "$attempts" -lt 50 ]; do
    status=$(curl --fail --silent --show-error \
      -H "Authorization: Bearer $MULTICA_WORKER_ADMIN_TOKEN" "$url/status") || status='{}'
    if printf '%s' "$status" | jq -e \
      '(.control.claims_enabled == false)
       and (.control.admission_open == false)
       and (.control.drain_requested == true)
       and (.control.worker_in_flight == 0)
       and (.control.worker_leases == 0)
       and (.control.drain_zero_since != null)
       and ((.worker.accepting_new // false) == false)' >/dev/null 2>&1; then
      # The server enforces the continuous 60-second zero-activity window.
      if admin_post "$url" complete-drain >/dev/null 2>&1; then
        return 0
      fi
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  echo "worker did not satisfy the fenced drain observation window" >&2
  return 1
}

ensure_generation_releasable() {
  url=$1
  status=$(curl --fail --silent --show-error \
    -H "Authorization: Bearer $MULTICA_WORKER_ADMIN_TOKEN" "$url/status") || return 1
  if printf '%s' "$status" | jq -e \
    '(.control.worker_owner == null or .control.worker_owner == "")
     and (.control.claims_enabled == false)
     and (.control.admission_open == false)
     and (.control.worker_in_flight == 0)
     and (.control.worker_leases == 0)' >/dev/null; then
    return 0
  fi
  admin_post "$url" admission/close >/dev/null \
    && admin_post "$url" drain >/dev/null \
    && wait_drained "$url"
}

switch_fragment() {
  candidate=$1
  test -f "$candidate"
  grep -F "$CADDY_MANAGED_FRAGMENT" "$CADDY_CONFIG" >/dev/null || {
    echo "Caddy root config does not import managed fragment" >&2
    return 1
  }
  staging=$(mktemp "${CADDY_MANAGED_FRAGMENT}.candidate.XXXXXX")
  previous=$(mktemp "${CADDY_MANAGED_FRAGMENT}.previous.XXXXXX")
  validate_config=$(mktemp "${CADDY_CONFIG}.candidate.XXXXXX")
  cp "$CADDY_MANAGED_FRAGMENT" "$previous"
  cp "$candidate" "$staging"
  escaped_fragment=$(printf '%s' "$CADDY_MANAGED_FRAGMENT" | sed 's/[&|]/\\&/g')
  escaped_staging=$(printf '%s' "$staging" | sed 's/[&|]/\\&/g')
  sed "s|$escaped_fragment|$escaped_staging|g" "$CADDY_CONFIG" >"$validate_config"

  # Validate the candidate while the live fragment is untouched.
  if ! "$CADDY_BIN" validate --config "$validate_config"; then
    rm -f "$staging" "$previous" "$validate_config"
    return 1
  fi
  chmod --reference="$CADDY_MANAGED_FRAGMENT" "$staging" 2>/dev/null || chmod 0644 "$staging"
  mv "$staging" "$CADDY_MANAGED_FRAGMENT"
  if ! "$CADDY_BIN" validate --config "$CADDY_CONFIG" || ! "$CADDY_BIN" reload --config "$CADDY_CONFIG"; then
    mv "$previous" "$CADDY_MANAGED_FRAGMENT"
    if ! "$CADDY_BIN" validate --config "$CADDY_CONFIG" || ! "$CADDY_BIN" reload --config "$CADDY_CONFIG"; then
      echo "Caddy rollback state is uncertain; gates remain closed" >&2
    fi
    rm -f "$staging" "$previous" "$validate_config"
    return 1
  fi
  rm -f "$previous" "$validate_config"
}

activate_target() {
  url=$1
  # One DB transition changes both gates; there is no half-open intermediate.
  admin_post "$url" activate >/dev/null
}

rollback_candidate() {
  failed_generation=$1
  if ! admin_post "$GREEN_ADMIN_URL" admission/close >/dev/null \
    || ! ensure_generation_releasable "$GREEN_ADMIN_URL"; then
    force_closed || true
    return 1
  fi
  rollback_json=$("$RELEASECTL" advance-generation --generation "$failed_generation") || {
    force_closed || true
    return 1
  }
  rollback_generation=$(printf '%s' "$rollback_json" | jq -er '.active_generation') || {
    force_closed || true
    return 1
  }
  BLUE_GENERATION=$rollback_generation
  export BLUE_GENERATION
  if ! docker compose --profile migration -f "$COMPOSE_FILE" up -d blue-worker \
    || ! switch_fragment "$BLUE_FRAGMENT" \
    || ! sh -c "$BLUE_SMOKE_COMMAND" \
    || ! activate_target "$BLUE_ADMIN_URL"; then
    force_closed || true
    return 1
  fi
}

observe_or_rollback() {
  if [ -z "${OBSERVATION_COMMAND:-}" ]; then
    echo "set OBSERVATION_COMMAND to the window collector" >&2
    return 1
  fi
  : "${OBSERVATION_DURATION_SECONDS:=3600}"
  : "${OBSERVATION_INTERVAL_SECONDS:=60}"
  case "$OBSERVATION_DURATION_SECONDS:$OBSERVATION_INTERVAL_SECONDS" in *[!0-9:]*|0:*|*:0) echo "observation durations must be positive integers" >&2; return 1 ;; esac
  if [ "${MULTICA_RELEASE_TEST_ONLY_SHORT_OBSERVATION:-0}" != 1 ] && [ "$OBSERVATION_DURATION_SECONDS" -lt 3600 ]; then
    echo "production observation must run continuously for at least 60 minutes" >&2
    return 1
  fi
  started=$(date +%s)
  deadline=$((started + OBSERVATION_DURATION_SECONDS))
  while :; do
    if ! sh -c "$OBSERVATION_COMMAND"; then
      echo "observation sample missing, failed, or requested rollback" >&2
      return 1
    fi
    now=$(date +%s)
    [ "$now" -ge "$deadline" ] && break
    sleep_for=$OBSERVATION_INTERVAL_SECONDS
    [ $((now + sleep_for)) -le "$deadline" ] || sleep_for=$((deadline - now))
    sleep "$sleep_for"
  done
}

verify_artifacts
if [ "$mode" = validate ]; then
  echo "release artifacts, target ALLOW report, and Caddy configuration are valid"
  exit 0
fi
if [ "$execute" != "--execute" ]; then
  echo "dry-run complete; pass --execute only in an approved release window" >&2
  exit 3
fi

: "${RELEASE_LOCK_FILE:=/var/lock/multica-release.lock}"
case "$RELEASE_LOCK_FILE" in /*) ;; *) echo "RELEASE_LOCK_FILE must be absolute" >&2; exit 1 ;; esac
if command -v flock >/dev/null 2>&1; then
  exec 9>"$RELEASE_LOCK_FILE"
  if ! flock -n 9; then
    echo "another release operation holds $RELEASE_LOCK_FILE" >&2
    exit 1
  fi
else
  lock_dir="${RELEASE_LOCK_FILE}.d"
  if ! mkdir "$lock_dir" 2>/dev/null; then
    echo "another release operation holds $lock_dir" >&2
    exit 1
  fi
  trap 'rmdir "$lock_dir" 2>/dev/null || true' EXIT HUP INT TERM
fi

: "${ACTIVE_GENERATION:?set ACTIVE_GENERATION}"
case "$mode" in
  cutover-green)
    : "${BLUE_ADMIN_URL:?set BLUE_ADMIN_URL}"
    : "${GREEN_ADMIN_URL:?set GREEN_ADMIN_URL}"
    : "${GREEN_FRAGMENT:?set GREEN_FRAGMENT}"
    : "${BLUE_FRAGMENT:?set BLUE_FRAGMENT}"
    : "${GREEN_SMOKE_COMMAND:?set GREEN_SMOKE_COMMAND}"
    : "${BLUE_SMOKE_COMMAND:?set BLUE_SMOKE_COMMAND}"
    admin_post "$BLUE_ADMIN_URL" admission/close >/dev/null
    admin_post "$BLUE_ADMIN_URL" drain >/dev/null
    wait_drained "$BLUE_ADMIN_URL"
    next_json=$("$RELEASECTL" advance-generation --generation "$ACTIVE_GENERATION")
    next_generation=$(printf '%s' "$next_json" | jq -er '.active_generation')
    test "$next_generation" -eq $((ACTIVE_GENERATION + 1))
    GREEN_GENERATION=$next_generation
    export GREEN_GENERATION
    if ! docker compose --profile migration -f "$COMPOSE_FILE" up -d green-worker \
      || ! switch_fragment "$GREEN_FRAGMENT" \
      || ! sh -c "$GREEN_SMOKE_COMMAND" \
      || ! activate_target "$GREEN_ADMIN_URL" \
      || ! observe_or_rollback; then
      echo "green cutover failed; executing fenced blue rollback" >&2
      force_closed || true
      rollback_candidate "$next_generation" || true
      exit 1
    fi
    ;;
  rollback-blue)
    : "${GREEN_ADMIN_URL:?set GREEN_ADMIN_URL}"
    : "${BLUE_ADMIN_URL:?set BLUE_ADMIN_URL}"
    : "${BLUE_FRAGMENT:?set BLUE_FRAGMENT}"
    : "${BLUE_SMOKE_COMMAND:?set BLUE_SMOKE_COMMAND}"
    admin_post "$GREEN_ADMIN_URL" admission/close >/dev/null
    admin_post "$GREEN_ADMIN_URL" drain >/dev/null
    wait_drained "$GREEN_ADMIN_URL"
    next_json=$("$RELEASECTL" advance-generation --generation "$ACTIVE_GENERATION")
    next_generation=$(printf '%s' "$next_json" | jq -er '.active_generation')
    test "$next_generation" -eq $((ACTIVE_GENERATION + 1))
    BLUE_GENERATION=$next_generation
    export BLUE_GENERATION
    if ! docker compose --profile migration -f "$COMPOSE_FILE" up -d blue-worker \
      || ! switch_fragment "$BLUE_FRAGMENT" \
      || ! sh -c "$BLUE_SMOKE_COMMAND" \
      || ! activate_target "$BLUE_ADMIN_URL"; then
      force_closed || true
      exit 1
    fi
    ;;
esac
