#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir "$work/artifacts" "$work/bin" "$work/execution" "$work/migrations"

config_sentinel=compose-fixture-secret-do-not-log
flags_sentinel=flags-fixture-secret-do-not-log
assert_cli_output_clean() {
  label=$1
  stdout_file=$2
  stderr_file=$3
  shift 3
  for forbidden in "$config_sentinel" "$flags_sentinel" \
    --expected-attempt-id --config-file --feature-flags-file "$@"; do
    if [ -n "$forbidden" ] && grep -F -- "$forbidden" "$stdout_file" "$stderr_file" >/dev/null; then
      echo "$label leaked protected CLI input" >&2
      exit 1
    fi
  done
}

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
printf 'transition_t: true\nfixture_marker: flags-fixture-secret-do-not-log\n' >"$flags"
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
  (($doc.services.migrator.command | join(" ")) as $command |
    ($command | index("begin-migrator-attempt")) < ($command | index("verify-manifest")) and
    ($command | index("verify-manifest")) < ($command | index("migrate up")) and
    ($command | index("migrate up")) < ($command | index("record-migrator-execution")) and
    ($command | contains("--attempt-id-file")) and
    ($command | contains("--expected-attempt-id"))) and
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

"$work/releasectl" begin-migrator-attempt \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --attempt-id-file "$work/attempt-a.private" \
  --output "$work/execution/current-attempt.json" \
  >"$work/begin.stdout" 2>"$work/begin.stderr"
attempt_a=$(tr -d '\n' <"$work/attempt-a.private")
assert_cli_output_clean begin "$work/begin.stdout" "$work/begin.stderr" "$attempt_a"
"$work/releasectl" record-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --current-attempt "$work/execution/current-attempt.json" \
  --expected-attempt-id "$attempt_a" \
  --output "$work/execution/migrator-execution.json" \
  >"$work/record.stdout" 2>"$work/record.stderr"
assert_cli_output_clean record "$work/record.stdout" "$work/record.stderr" "$attempt_a"
"$work/releasectl" verify-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --current-attempt "$work/execution/current-attempt.json" \
  --execution-record "$work/execution/migrator-execution.json" \
  >"$work/verify.stdout" 2>"$work/verify.stderr"
assert_cli_output_clean verify "$work/verify.stdout" "$work/verify.stderr" "$attempt_a"
jq -e '(.attempt_id | not) and .status == "verified" and
  (.deployment.migrator_image | test("@sha256:[0-9a-f]{64}$"))' \
  "$work/verify.stdout" >/dev/null

# Simulate A having migrated, then B beginning before A completes. Completion
# must use A's private nonce rather than re-reading and blessing shared B.
"$work/releasectl" begin-migrator-attempt \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --attempt-id-file "$work/attempt-race-a.private" \
  --output "$work/execution/current-attempt.json" >/dev/null
attempt_race_a=$(tr -d '\n' <"$work/attempt-race-a.private")
"$work/releasectl" begin-migrator-attempt \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --attempt-id-file "$work/attempt-b.private" \
  --output "$work/execution/current-attempt.json" >/dev/null
attempt_b=$(tr -d '\n' <"$work/attempt-b.private")
[ "$attempt_race_a" != "$attempt_b" ]
if "$work/releasectl" record-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --current-attempt "$work/execution/current-attempt.json" \
  --expected-attempt-id "$attempt_race_a" \
  --output "$work/execution/migrator-execution.json" \
  >"$work/race-complete.stdout" 2>"$work/race-complete.stderr"; then
  echo "attempt A completion blessed later attempt B" >&2
  exit 1
fi
assert_cli_output_clean race-complete "$work/race-complete.stdout" "$work/race-complete.stderr" \
  "$attempt_a" "$attempt_race_a" "$attempt_b"
if "$work/releasectl" verify-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --current-attempt "$work/execution/current-attempt.json" \
  --execution-record "$work/execution/migrator-execution.json" \
  >"$work/stale-verify.stdout" 2>"$work/stale-verify.stderr"; then
  echo "old success authorized a new current attempt" >&2
  exit 1
fi
assert_cli_output_clean stale-verify "$work/stale-verify.stdout" "$work/stale-verify.stderr" \
  "$attempt_a" "$attempt_race_a" "$attempt_b"

for expected in missing malformed wrong; do
  expected_arg=""
  expected_value=""
  case "$expected" in
    missing) ;;
    malformed) expected_value=not-a-nonce; expected_arg="--expected-attempt-id $expected_value" ;;
    wrong) expected_value=$(printf '%064d' 0); expected_arg="--expected-attempt-id $expected_value" ;;
  esac
  # shellcheck disable=SC2086 -- deliberately exercises a missing flag as well.
  if "$work/releasectl" record-migrator-execution \
    --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
    --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
    --config-file "$config" --feature-flags-file "$flags" \
    --current-attempt "$work/execution/current-attempt.json" $expected_arg \
    --output "$work/execution/migrator-execution.json" \
    >"$work/expected-$expected.stdout" 2>"$work/expected-$expected.stderr"; then
    echo "$expected expected attempt id unexpectedly passed" >&2
    exit 1
  fi
  assert_cli_output_clean "expected-$expected" "$work/expected-$expected.stdout" \
    "$work/expected-$expected.stderr" "$attempt_a" "$attempt_race_a" "$attempt_b" "$expected_value"
done

if "$work/releasectl" record-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --current-attempt "$work/execution/current-attempt.json" \
  --expected-attempt-id "$attempt_b" \
  --output "$work/missing/migrator-execution.json" \
  >"$work/write-failure.stdout" 2>"$work/write-failure.stderr"; then
  echo "completion record write failure was not propagated" >&2
  exit 1
fi
assert_cli_output_clean write-failure "$work/write-failure.stdout" "$work/write-failure.stderr" \
  "$attempt_a" "$attempt_race_a" "$attempt_b"
if "$work/releasectl" verify-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --current-attempt "$work/execution/current-attempt.json" \
  --execution-record "$work/execution/migrator-execution.json" \
  >"$work/incomplete-verify.stdout" 2>"$work/incomplete-verify.stderr"; then
  echo "old success authorized after completion write failure" >&2
  exit 1
fi
assert_cli_output_clean incomplete-verify "$work/incomplete-verify.stdout" \
  "$work/incomplete-verify.stderr" "$attempt_a" "$attempt_race_a" "$attempt_b"
"$work/releasectl" record-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --deployment-identity "$work/artifacts/deployment.actual.json" \
  --config-file "$config" --feature-flags-file "$flags" \
  --current-attempt "$work/execution/current-attempt.json" \
  --expected-attempt-id "$attempt_b" \
  --output "$work/execution/migrator-execution.json" >/dev/null
"$work/releasectl" verify-migrator-execution \
  --manifest "$work/artifacts/manifest.json" --artifact-dir "$work/artifacts" \
  --combination W1K1S1 --current-attempt "$work/execution/current-attempt.json" \
  --execution-record "$work/execution/migrator-execution.json" >/dev/null

echo "real Compose identity, one-shot attempt lifecycle, and CLI output redaction: PASS"
