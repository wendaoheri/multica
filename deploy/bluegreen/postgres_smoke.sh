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
"$work/releasectl" activate --generation 1 --owner worker-a >/dev/null
"$work/releasectl" status | jq -e '.claims_enabled == true and .admission_open == true and .worker_owner == "worker-a"' >/dev/null
if "$work/releasectl" activate --generation 1 --owner worker-b >/dev/null 2>&1; then
  echo "duplicate worker owner unexpectedly activated" >&2
  exit 1
fi
"$work/releasectl" drain --generation 1 --owner worker-a >/dev/null
if "$work/releasectl" enable-claims --generation 1 --owner worker-b >/dev/null 2>&1; then
  echo "replacement worker enabled while old owner was draining" >&2
  exit 1
fi
if "$work/releasectl" complete-drain --generation 1 --owner worker-a >/dev/null 2>&1; then
  echo "drain completed without the continuous observation window" >&2
  exit 1
fi
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c \
  "UPDATE deployment_release_control SET drain_zero_since = NOW() - INTERVAL '61 seconds' WHERE singleton" >/dev/null
"$work/releasectl" complete-drain --generation 1 --owner worker-a >/dev/null
"$work/releasectl" advance-generation --generation 1 | jq -e \
  '.active_generation == 2 and .claims_enabled == false and .admission_open == false' >/dev/null
if "$work/releasectl" enable-claims --generation 1 --owner worker-a >/dev/null 2>&1; then
  echo "fenced generation unexpectedly enabled" >&2
  exit 1
fi

"$work/releasectl" enable-claims --generation 2 --owner worker-c >/dev/null
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c \
  "UPDATE deployment_release_control SET worker_in_flight = 1 WHERE singleton" >/dev/null
"$work/releasectl" drain --generation 2 --owner worker-c >/dev/null
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c \
  "UPDATE deployment_release_control SET drain_zero_since = NOW() - INTERVAL '61 seconds' WHERE singleton" >/dev/null
if "$work/releasectl" complete-drain --generation 2 --owner worker-c >/dev/null 2>&1; then
  echo "drain completed with an in-flight operation" >&2
  exit 1
fi
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c \
  "UPDATE deployment_release_control SET worker_in_flight = 0, drain_zero_since = NOW() - INTERVAL '61 seconds' WHERE singleton" >/dev/null
"$work/releasectl" complete-drain --generation 2 --owner worker-c >/dev/null
"$work/releasectl" advance-generation --generation 2 >/dev/null
if "$work/releasectl" enable-claims --generation 2 --owner worker-c >/dev/null 2>&1; then
  echo "old generation operation became valid again" >&2
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
if "$work/releasectl" verify-manifest \
  --manifest "$work/manifest.json" \
  --artifact-dir "$work" \
  --require-combination W1K1S1 >/dev/null 2>&1; then
  echo "all-DENY manifest unexpectedly authorized the target" >&2
  exit 1
fi

psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c \
  "INSERT INTO schema_migrations(version) VALUES ('999_unknown_per376')" >/dev/null
if "$work/releasectl" verify-manifest \
  --manifest "$work/manifest.json" \
  --artifact-dir "$work" \
  --migration-dir "$work/migrations" \
  --check-database >/dev/null 2>&1; then
  echo "unknown database migration version unexpectedly passed" >&2
  exit 1
fi
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c \
  "DELETE FROM schema_migrations WHERE version = '999_unknown_per376'" >/dev/null

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

echo "PostgreSQL generation/owner/drain fence, strict migration set, manifest target DENY, and drift injection: PASS"
