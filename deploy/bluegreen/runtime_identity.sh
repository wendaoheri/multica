#!/bin/sh
set -eu

: "${COMBINATION:?set COMBINATION}"
: "${COMPOSE_FILE:?set COMPOSE_FILE}"
: "${RELEASE_MANIFEST:?set RELEASE_MANIFEST}"
: "${RUNTIME_IDENTITY_REPORT:?set RUNTIME_IDENTITY_REPORT}"
: "${MIGRATOR_EXECUTION_RECORD:?set MIGRATOR_EXECUTION_RECORD from the completed one-shot migrator}"

case "$COMBINATION" in
  W0*) web=blue-web; frontend=blue-frontend ;; W1*) web=green-web; frontend=green-frontend ;; *) exit 2 ;;
esac
case "$COMBINATION" in
  W?K0*) worker=blue-worker ;; W?K1*) worker=green-worker ;; *) exit 2 ;;
esac

inspect_digest() {
  service=$1
  container=$(docker compose --profile migration -f "$COMPOSE_FILE" ps -q "$service")
  test -n "$container"
  expected=$(jq -er --arg c "$COMBINATION" --arg field "$2" '.combinations[$c].deployment[$field]' "$RELEASE_MANIFEST")
  docker inspect "$container" --format '{{json .RepoDigests}}' | jq -e --arg expected "$expected" 'index($expected) != null' >/dev/null
  for pair in 'com.multica.release-id release_id' 'com.multica.combination combination' 'com.multica.config-checksum config_checksum' 'com.multica.flags-checksum feature_flags_checksum' 'com.multica.migration-checksum migration_manifest_checksum'; do
    label=${pair%% *}; field=${pair#* }
    actual=$(docker inspect "$container" --format "{{index .Config.Labels \"$label\"}}")
    case "$field" in combination) want=$COMBINATION ;; *) want=$(jq -er --arg field "$field" '.[$field]' "$RELEASE_MANIFEST") ;; esac
    test "$actual" = "$want"
  done
  printf '%s' "$expected"
}

web_image=$(inspect_digest "$web" web_image)
worker_image=$(inspect_digest "$worker" worker_image)
frontend_image=$(inspect_digest "$frontend" frontend_image)
jq -e --arg combination "$COMBINATION" --slurpfile manifest "$RELEASE_MANIFEST" '
  .version == 1 and .result == "PASS" and
  .release_id == $manifest[0].release_id and .combination == $combination and
  .deployment == $manifest[0].combinations[$combination].deployment
' "$MIGRATOR_EXECUTION_RECORD" >/dev/null
migrator_image=$(jq -er '.deployment.migrator_image' "$MIGRATOR_EXECUTION_RECORD")
jq -n --arg combination "$COMBINATION" --arg web "$web_image" --arg worker "$worker_image" \
  --arg frontend "$frontend_image" --arg migrator "$migrator_image" --slurpfile manifest "$RELEASE_MANIFEST" '
  {release_id:$manifest[0].release_id,combination:$combination,
   migration_manifest_checksum:$manifest[0].migration_manifest_checksum,
   deployment:($manifest[0].combinations[$combination].deployment
     | .web_image=$web | .worker_image=$worker | .frontend_image=$frontend | .migrator_image=$migrator)}' >"$RUNTIME_IDENTITY_REPORT"
