#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir "$work/artifacts" "$work/bin" "$work/execution" "$work/migrations"

command -v docker >/dev/null
docker compose version >/dev/null
(cd "$root/server" && go build -o "$work/releasectl" ./cmd/releasectl)
for removed in admission-open enable-claims; do
  if "$work/releasectl" "$removed" --generation 1 --owner fixture >/dev/null 2>&1; then
    echo "removed CLI command $removed unexpectedly succeeded" >&2
    exit 1
  fi
done

digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
config="$work/config.env"
flags="$work/flags.yaml"
cat >"$config" <<'EOF'
DATABASE_URL=postgres://fixture.invalid/multica
REDIS_URL=redis://fixture.invalid:6379/0
REALTIME_RELAY_MODE=sharded
REALTIME_RELAY_MAX_CONSUMER_LAG=30s
TEST_SECRET=compose-fixture-secret-do-not-log
EOF
printf 'transition_t: true\n' >"$flags"
config_sum=$("$work/releasectl" checksum --file "$config")
flags_sum=$("$work/releasectl" checksum --file "$flags")
rollback_sum=$("$work/releasectl" checksum --file "$root/deploy/bluegreen/release.sh")
migration_sum=4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945
jq -n --arg d "$digest" --arg config "$config_sum" --arg flags "$flags_sum" \
  --arg migration "$migration_sum" --arg rollback "$rollback_sum" '
  ["W0K0S0","W0K0S1","W0K0S2","W0K1S0","W0K1S1","W0K1S2",
   "W1K0S0","W1K0S1","W1K0S2","W1K1S0","W1K1S1","W1K1S2"] as $keys |
  {release_id:"PER-376-compose-fixture",from_digest:$d,web_digest:$d,worker_digest:$d,
   config_checksum:$config,feature_flags_checksum:$flags,migration_manifest_checksum:$migration,
   migrations:[],combinations:($keys | map({key:.,value:{decision:"DENY"}}) | from_entries),
   rollback_script_checksum:$rollback}' >"$work/artifacts/manifest.json"
migration_sum=$(jq -er .migration_manifest_checksum "$work/artifacts/manifest.json")
checks=$(jq -n '{auth_permissions:true,issue_comment_crud:true,attachment_lifecycle:true,websocket_compatibility:true,task_lifecycle:true,concurrent_claim:true,scheduler_lease:true,autopilot_state:true,webhook_fake_endpoint:true,pr_refresh_fake_api:true,heartbeat_sweeper:true,relay_cross_process:true,critical_write_path:true,no_duplicate_side_effects:true,external_network_blocked:true}')
jq -n --arg d "$digest" --arg config "$config_sum" --arg flags "$flags_sum" --arg migration "$migration_sum" --argjson checks "$checks" '
  {version:1,release_id:"PER-376-compose-fixture",combination:"W1K1S1",result:"PASS",
   web_digest:$d,worker_digest:$d,config_checksum:$config,feature_flags_checksum:$flags,
   migration_manifest_checksum:$migration,started_at:"2026-08-19T04:00:00Z",
   completed_at:"2026-08-19T04:01:00Z",checks:$checks}' >"$work/artifacts/smoke.json"
report_sum=$("$work/releasectl" checksum --file "$work/artifacts/smoke.json")
jq --arg d "$digest" --arg config "$config_sum" --arg flags "$flags_sum" \
  --arg migration "$migration_sum" --arg report "$report_sum" '
  .combinations.W1K1S1={decision:"ALLOW",report:"smoke.json",report_sha256:$report,
    deployment:{web_image:("fixture/backend@sha256:"+$d),worker_image:("fixture/backend@sha256:"+$d),
      frontend_image:("fixture/frontend@sha256:"+$d),migrator_image:("fixture/migrator@sha256:"+$d),
      config_checksum:$config,feature_flags_checksum:$flags,migration_manifest_checksum:$migration}}' \
  "$work/artifacts/manifest.json" >"$work/manifest.tmp"
mv "$work/manifest.tmp" "$work/artifacts/manifest.json"

live="$work/live.fragment"
printf 'reverse_proxy 127.0.0.1:3000\n' >"$live"
printf 'import %s\n' "$live" >"$work/Caddyfile"
cat >"$work/bin/caddy" <<'EOF'
#!/bin/sh
[ "$1" = validate ]
EOF
chmod +x "$work/bin/caddy"

export RELEASE_CONFIG_FILE="$config" RELEASE_FEATURE_FLAGS_FILE="$flags"
export RELEASE_ARTIFACT_DIR="$work/artifacts" RELEASE_EXECUTION_DIR="$work/execution"
export RELEASE_CONFIG_CHECKSUM="$config_sum" RELEASE_FLAGS_CHECKSUM="$flags_sum"
export RELEASE_MIGRATION_CHECKSUM="$migration_sum" RELEASE_ID=PER-376-compose-fixture
export TARGET_COMBINATION=W1K1S1 COMPOSE_FILE="$root/deploy/bluegreen/docker-compose.transition.yml"
export BLUE_BACKEND_IMAGE="fixture/backend@sha256:$digest" GREEN_BACKEND_IMAGE="fixture/backend@sha256:$digest"
export BLUE_WEB_IMAGE="fixture/frontend@sha256:$digest" GREEN_WEB_IMAGE="fixture/frontend@sha256:$digest"
export MIGRATOR_IMAGE="fixture/migrator@sha256:$digest"
export BLUE_GENERATION=1 GREEN_GENERATION=2 BLUE_WORKER_OWNER=blue-fixture GREEN_WORKER_OWNER=green-fixture
export MULTICA_WORKER_ADMIN_TOKEN=01234567890123456789012345678901
export MULTICA_UPLOADS_VOLUME=fixture_uploads MULTICA_STATE_NETWORK=fixture_state

default_count=$(docker compose -f "$COMPOSE_FILE" config --format json | jq '.services | length')
profile_count=$(docker compose --profile migration -f "$COMPOSE_FILE" config --format json | jq '.services | length')
[ "$default_count" -eq 6 ]
[ "$profile_count" -eq 7 ]
docker compose --profile migration -f "$COMPOSE_FILE" config --format json | jq -e \
  --arg config "$config" --arg flags "$flags" '
  . as $doc |
  $doc.services.migrator.profiles == ["migration"] and
  ([$doc.services[].image | test("@sha256:[0-9a-f]{64}$")] | all) and
  (["blue-web","green-web","blue-worker","green-worker","migrator"] | all(. as $name |
    ([$doc.services[$name].volumes[] | select(.source == $config and .target == "/run/multica-release/config.env" and .read_only == true)] | length == 1) and
    ([$doc.services[$name].volumes[] | select(.source == $flags and .target == "/run/multica-release/feature-flags.yaml" and .read_only == true)] | length == 1)))' >/dev/null

if ! PATH="$work/bin:$PATH" RELEASECTL="$work/releasectl" CADDY_BIN="$work/bin/caddy" \
  CADDY_CONFIG="$work/Caddyfile" CADDY_MANAGED_FRAGMENT="$live" \
  "$root/deploy/bluegreen/release.sh" validate >"$work/validate.log" 2>&1; then
  cat "$work/validate.log" >&2
  exit 1
fi
if grep -q 'compose-fixture-secret-do-not-log' "$work/validate.log"; then
  echo "release validation leaked a config value" >&2
  exit 1
fi

if docker compose --profile migration -f "$COMPOSE_FILE" config --format json | \
  jq --arg bad "$work/wrong.env" '.services["green-web"].volumes |= map(if .target == "/run/multica-release/config.env" then .source=$bad else . end)' | \
  "$work/releasectl" verify-deployment --manifest "$work/artifacts/manifest.json" \
    --artifact-dir "$work/artifacts" --combination W1K1S1 --compose-json - \
    --config-file "$config" --feature-flags-file "$flags" >/dev/null 2>&1; then
  echo "expanded config path mismatch unexpectedly passed" >&2
  exit 1
fi

old_image=$BLUE_BACKEND_IMAGE
BLUE_BACKEND_IMAGE=fixture/backend:latest; export BLUE_BACKEND_IMAGE
if PATH="$work/bin:$PATH" RELEASECTL="$work/releasectl" CADDY_BIN="$work/bin/caddy" \
  CADDY_CONFIG="$work/Caddyfile" CADDY_MANAGED_FRAGMENT="$live" \
  "$root/deploy/bluegreen/release.sh" validate >/dev/null 2>&1; then
  echo "tag image unexpectedly passed release validation" >&2
  exit 1
fi
BLUE_BACKEND_IMAGE=$old_image; export BLUE_BACKEND_IMAGE

printf 'DRIFT=true\n' >>"$config"
if PATH="$work/bin:$PATH" RELEASECTL="$work/releasectl" CADDY_BIN="$work/bin/caddy" \
  CADDY_CONFIG="$work/Caddyfile" CADDY_MANAGED_FRAGMENT="$live" \
  "$root/deploy/bluegreen/release.sh" validate >/dev/null 2>&1; then
  echo "actual config drift unexpectedly passed release validation" >&2
  exit 1
fi
sed -i.bak '$d' "$config" && rm "$config.bak"

"$work/releasectl" record-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --output "$work/execution/migrator-execution.json" >/dev/null
jq -e '.version == 1 and .result == "PASS" and .combination == "W1K1S1"' \
  "$work/execution/migrator-execution.json" >/dev/null

echo "real Compose default/profile expansion, 7-service immutable identity, actual config/flags binding, release validation, and durable one-shot record: PASS"
