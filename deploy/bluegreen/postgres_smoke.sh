#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
pgdata="$work/postgres"
port=55432
cleanup() {
  pg_ctl -D "$pgdata" -m immediate stop >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

initdb -D "$pgdata" -A trust -U multica --no-locale >/dev/null
pg_ctl -D "$pgdata" -o "-h 127.0.0.1 -p $port" -w start >/dev/null
createdb -h 127.0.0.1 -p "$port" -U multica multica_per376
export DATABASE_URL="postgres://multica@127.0.0.1:$port/multica_per376?sslmode=disable"

cd "$root/server"
go build -o "$work/migrate" ./cmd/migrate
go build -o "$work/releasectl" ./cmd/releasectl
go build -o "$work/server" ./cmd/server
mkdir "$work/migrations"
cp "$root/server/migrations/319_deployment_release_control.up.sql" "$work/migrations/"
cp "$root/server/migrations/319_deployment_release_control.down.sql" "$work/migrations/"
cp "$root/server/migrations/320_deployment_release_control_singleton_index.up.sql" "$work/migrations/"
cp "$root/server/migrations/320_deployment_release_control_singleton_index.down.sql" "$work/migrations/"
(cd "$work" && "$work/migrate" up >/dev/null)

if MULTICA_PROCESS_ROLE=worker MULTICA_RELEASE_GENERATION=1 MULTICA_WORKER_OWNER=worker-a \
  "$work/server" >"$work/no-redis.log" 2>&1; then
  echo "split worker unexpectedly started without Redis" >&2
  exit 1
fi
grep -q 'REDIS_URL is required for web/worker split roles' "$work/no-redis.log"

"$work/releasectl" status | jq -e \
  '.active_generation == 1 and .claims_enabled == false and .admission_open == false' >/dev/null
"$work/releasectl" enable-claims --generation 1 --owner worker-a >/dev/null
if "$work/releasectl" enable-claims --generation 1 --owner worker-b >/dev/null 2>&1; then
  echo "duplicate worker owner unexpectedly enabled" >&2
  exit 1
fi
"$work/releasectl" drain --generation 1 --owner worker-a >/dev/null
"$work/releasectl" advance-generation --generation 1 | jq -e \
  '.active_generation == 2 and .claims_enabled == false and .admission_open == false' >/dev/null
if "$work/releasectl" enable-claims --generation 1 --owner worker-a >/dev/null 2>&1; then
  echo "fenced generation unexpectedly enabled" >&2
  exit 1
fi

digest=0000000000000000000000000000000000000000000000000000000000000000
"$work/releasectl" generate-manifest \
  --release-id PER-376-isolated \
  --from-digest "$digest" \
  --web-digest "$digest" \
  --worker-digest "$digest" \
  --config-file "$root/deploy/bluegreen/release.env.example" \
  --feature-flags-file "$root/.env.example" \
  --rollback-script "$root/deploy/bluegreen/release.sh" \
  --migration-dir "$work/migrations" \
  --migration-classes "$root/deploy/bluegreen/migration-classes.example.json" \
  --output "$work/manifest.json" >/dev/null
"$work/releasectl" verify-manifest \
  --manifest "$work/manifest.json" \
  --artifact-dir "$work" \
  --migration-dir "$work/migrations" \
  --check-database >/dev/null

cp -R "$work/migrations" "$work/migrations-drift"
printf '\n-- injected drift\n' >>"$work/migrations-drift/320_deployment_release_control_singleton_index.up.sql"
if "$work/releasectl" verify-manifest \
  --manifest "$work/manifest.json" \
  --artifact-dir "$work" \
  --migration-dir "$work/migrations-drift" \
  --check-database >/dev/null 2>&1; then
  echo "migration checksum drift unexpectedly passed" >&2
  exit 1
fi

echo "PostgreSQL generation fence, owner exclusion, manifest, and drift injection: PASS"
