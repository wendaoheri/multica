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
: "${COMPOSE_FILE:?set COMPOSE_FILE}"
: "${CADDY_CONFIG:?set CADDY_CONFIG}"
: "${CADDY_MANAGED_FRAGMENT:?set CADDY_MANAGED_FRAGMENT}"
: "${CADDY_BIN:=caddy}"
: "${RELEASECTL:=releasectl}"
: "${MULTICA_WORKER_ADMIN_TOKEN:?set MULTICA_WORKER_ADMIN_TOKEN}"

case "$CADDY_MANAGED_FRAGMENT" in /*) ;; *) echo "CADDY_MANAGED_FRAGMENT must be absolute" >&2; exit 1 ;; esac
case "$CADDY_CONFIG" in /*) ;; *) echo "CADDY_CONFIG must be absolute" >&2; exit 1 ;; esac

verify_artifacts() {
  "$RELEASECTL" verify-manifest \
    --manifest "$RELEASE_MANIFEST" \
    --artifact-dir "$RELEASE_ARTIFACT_DIR"
  docker compose -f "$COMPOSE_FILE" config --quiet
  "$CADDY_BIN" validate --config "$CADDY_CONFIG"
}

admin_post() {
  url=$1
  action=$2
  case "$url" in http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
    *) echo "worker admin URL must be loopback: $url" >&2; exit 1 ;;
  esac
  curl --fail --silent --show-error \
    -H "Authorization: Bearer $MULTICA_WORKER_ADMIN_TOKEN" \
    -X POST "$url/$action"
}

wait_drained() {
  url=$1
  attempts=0
  while [ "$attempts" -lt 30 ]; do
    status=$(curl --fail --silent --show-error \
      -H "Authorization: Bearer $MULTICA_WORKER_ADMIN_TOKEN" "$url/status")
    if printf '%s' "$status" | jq -e \
      '(.control.claims_enabled == false) and ((.worker.accepting_new // false) == false)' >/dev/null; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  echo "worker did not drain within 60 seconds" >&2
  return 1
}

switch_fragment() {
  candidate=$1
  test -f "$candidate"
  staging=$(mktemp "${CADDY_MANAGED_FRAGMENT}.candidate.XXXXXX")
  previous=$(mktemp "${CADDY_MANAGED_FRAGMENT}.previous.XXXXXX")
  trap 'rm -f "$staging" "$previous"' EXIT HUP INT TERM
  cp "$CADDY_MANAGED_FRAGMENT" "$previous"
  cp "$candidate" "$staging"
  chmod --reference="$CADDY_MANAGED_FRAGMENT" "$staging" 2>/dev/null || chmod 0644 "$staging"
  mv "$staging" "$CADDY_MANAGED_FRAGMENT"
  if ! "$CADDY_BIN" validate --config "$CADDY_CONFIG" || ! "$CADDY_BIN" reload --config "$CADDY_CONFIG"; then
    mv "$previous" "$CADDY_MANAGED_FRAGMENT"
    "$CADDY_BIN" validate --config "$CADDY_CONFIG"
    "$CADDY_BIN" reload --config "$CADDY_CONFIG" || true
    trap - EXIT HUP INT TERM
    return 1
  fi
  rm -f "$previous"
  trap - EXIT HUP INT TERM
}

rollback_candidate() {
  failed_generation=$1
	admin_post "$GREEN_ADMIN_URL" drain || true
	wait_drained "$GREEN_ADMIN_URL"
  rollback_json=$("$RELEASECTL" advance-generation --generation "$failed_generation")
  rollback_generation=$(printf '%s' "$rollback_json" | jq -er '.active_generation')
  BLUE_GENERATION=$rollback_generation
  export BLUE_GENERATION
  docker compose -f "$COMPOSE_FILE" up -d blue-worker
  switch_fragment "$BLUE_FRAGMENT"
  sh -c "$BLUE_SMOKE_COMMAND"
  admin_post "$BLUE_ADMIN_URL" enable-claims
  admin_post "$BLUE_ADMIN_URL" admission/open
}

verify_artifacts
if [ "$mode" = validate ]; then
  echo "release artifacts and candidate Caddy configuration are valid"
  exit 0
fi
if [ "$execute" != "--execute" ]; then
  echo "dry-run complete; pass --execute only in an approved release window" >&2
  exit 3
fi

: "${RELEASE_LOCK_FILE:=/var/lock/multica-release.lock}"
case "$RELEASE_LOCK_FILE" in /*) ;; *) echo "RELEASE_LOCK_FILE must be absolute" >&2; exit 1 ;; esac
exec 9>"$RELEASE_LOCK_FILE"
if ! flock -n 9; then
  echo "another release operation holds $RELEASE_LOCK_FILE" >&2
  exit 1
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
    admin_post "$BLUE_ADMIN_URL" admission/close
    admin_post "$BLUE_ADMIN_URL" drain
    wait_drained "$BLUE_ADMIN_URL"
    next_json=$("$RELEASECTL" advance-generation --generation "$ACTIVE_GENERATION")
    next_generation=$(printf '%s' "$next_json" | jq -er '.active_generation')
    test "$next_generation" -eq $((ACTIVE_GENERATION + 1))
	GREEN_GENERATION=$next_generation
	export GREEN_GENERATION
    docker compose -f "$COMPOSE_FILE" up -d green-worker
	if ! switch_fragment "$GREEN_FRAGMENT" || ! sh -c "$GREEN_SMOKE_COMMAND" || ! admin_post "$GREEN_ADMIN_URL" enable-claims || ! admin_post "$GREEN_ADMIN_URL" admission/open; then
	  echo "green cutover failed; executing fenced blue rollback" >&2
	  rollback_candidate "$next_generation"
	  exit 1
	fi
    ;;
  rollback-blue)
    : "${GREEN_ADMIN_URL:?set GREEN_ADMIN_URL}"
    : "${BLUE_ADMIN_URL:?set BLUE_ADMIN_URL}"
    : "${BLUE_FRAGMENT:?set BLUE_FRAGMENT}"
	: "${BLUE_SMOKE_COMMAND:?set BLUE_SMOKE_COMMAND}"
    # Mandatory order: drain K1 -> generation fence -> start K0 claims-disabled
    # -> Caddy back -> smoke -> enable K0 -> reopen admission.
    admin_post "$GREEN_ADMIN_URL" admission/close
    admin_post "$GREEN_ADMIN_URL" drain
    wait_drained "$GREEN_ADMIN_URL"
    next_json=$("$RELEASECTL" advance-generation --generation "$ACTIVE_GENERATION")
    next_generation=$(printf '%s' "$next_json" | jq -er '.active_generation')
    test "$next_generation" -eq $((ACTIVE_GENERATION + 1))
	BLUE_GENERATION=$next_generation
	export BLUE_GENERATION
    docker compose -f "$COMPOSE_FILE" up -d blue-worker
    switch_fragment "$BLUE_FRAGMENT"
	sh -c "$BLUE_SMOKE_COMMAND"
    admin_post "$BLUE_ADMIN_URL" enable-claims
    admin_post "$BLUE_ADMIN_URL" admission/open
    ;;
esac
