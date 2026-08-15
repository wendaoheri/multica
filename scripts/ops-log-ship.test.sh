#!/usr/bin/env bash
# Fixture-mode tests for ops/scripts/log-redact.sh and ops/scripts/log-ship.sh
# (PER-284). No real SSH: log-ship runs with MULTICA_OPS_LOG_SHIP_FIXTURE.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OPS_DIR="$ROOT_DIR/ops/scripts"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  local file=$1
  local needle=$2
  grep -Fq "$needle" "$file" || fail "$file: expected to contain '$needle'"
}

assert_not_contains() {
  local file=$1
  local needle=$2
  if grep -Fq "$needle" "$file"; then
    fail "$file: must NOT contain '$needle' (secret leaked)"
  fi
}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/ops-log-ship-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

SECRET_GHP="ghp_abcdefghijklmnopqrstuvwxyz0123456789"
SECRET_SK="sk-abcdefabcdefabcdefabcdef12"
SECRET_JWT="eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

# ---------- 1. log-redact.sh scrubs every pattern family ----------

INPUT="$WORK/input.log"
OUT="$WORK/redacted.log"
cat > "$INPUT" <<EOF
normal line stays
token leaked: $SECRET_GHP
openai key $SECRET_SK in prose
jwt=$SECRET_JWT
Authorization: Bearer abc123def456
postgres://dbuser:dbpass@db.internal:5432/multica connecting
API_KEY=supersecretvalue123
"access_token":"jsonsecretvalue99"
-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA7v5x
-----END RSA PRIVATE KEY-----
path /Users/someuser/workspace/file.txt
AKIAIOSFODNN7EXAMPLE aws key id
EOF

bash "$OPS_DIR/log-redact.sh" < "$INPUT" > "$OUT"

assert_contains "$OUT" "normal line stays"
assert_not_contains "$OUT" "$SECRET_GHP"
assert_not_contains "$OUT" "$SECRET_SK"
assert_not_contains "$OUT" "$SECRET_JWT"
assert_not_contains "$OUT" "abc123def456"
assert_not_contains "$OUT" "dbpass"
assert_not_contains "$OUT" "supersecretvalue123"
assert_not_contains "$OUT" "jsonsecretvalue99"
assert_not_contains "$OUT" "MIIEpAIBAAKCAQEA7v5x"
assert_not_contains "$OUT" "/Users/someuser"
assert_not_contains "$OUT" "AKIAIOSFODNN7EXAMPLE"
assert_contains "$OUT" "[REDACTED GITHUB TOKEN]"
assert_contains "$OUT" "[REDACTED API KEY]"
assert_contains "$OUT" "[REDACTED JWT]"
assert_contains "$OUT" "Bearer [REDACTED]"
assert_contains "$OUT" "[REDACTED CONNECTION STRING]@"
assert_contains "$OUT" "[REDACTED CREDENTIAL]"
assert_contains "$OUT" "[REDACTED PRIVATE KEY]"
assert_contains "$OUT" "/Users/****"
assert_contains "$OUT" "[REDACTED AWS KEY]"

# ---------- 2. log-ship.sh fixture run: redact-before-persist + watermark ----------

SHIP_DIR="$WORK/ship"
mkdir -p "$SHIP_DIR"
ENV_FILE="$WORK/ops.env"
cat > "$ENV_FILE" <<EOF
MULTICA_OPS_SSH_HOST=ship-test.invalid
MULTICA_OPS_SSH_PORT=22
MULTICA_OPS_LOG_SHIP_CONTAINERS=backend-test
MULTICA_OPS_LOG_SHIP_DIR=$SHIP_DIR
MULTICA_OPS_LOG_SHIP_RETAIN_DAYS=7
EOF

FIXTURE="$WORK/fixture.log"
cat > "$FIXTURE" <<EOF
2026-08-15T10:00:00Z server starting port=8080
2026-08-15T10:00:01Z auth failed with $SECRET_GHP
EOF

MULTICA_OPS_CONFIG="$ENV_FILE" MULTICA_OPS_LOG_SHIP_FIXTURE="$FIXTURE" \
  bash "$OPS_DIR/log-ship.sh" >/dev/null 2>"$WORK/ship.stderr" || {
    cat "$WORK/ship.stderr" >&2
    fail "log-ship.sh exited non-zero"
  }

DAY="$(date '+%Y-%m-%d')"
SHIPPED="$SHIP_DIR/$DAY/backend-test.log"
[ -f "$SHIPPED" ] || fail "expected shipped file $SHIPPED"
assert_contains "$SHIPPED" "server starting port=8080"
assert_not_contains "$SHIPPED" "$SECRET_GHP"
assert_contains "$SHIPPED" "[REDACTED GITHUB TOKEN]"
[ -s "$SHIP_DIR/.state/backend-test.since" ] || fail "watermark not written"
assert_contains "$SHIP_DIR/log-ship.log" "OK shipped container=backend-test lines=2"

# ---------- 3. retention: only whole stale date-dirs are pruned ----------

STALE_DIR="$SHIP_DIR/2020-01-01"
mkdir -p "$STALE_DIR" "$SHIP_DIR/.state"
echo stale > "$STALE_DIR/backend-test.log"
touch -t 202001010000 "$STALE_DIR"

MULTICA_OPS_CONFIG="$ENV_FILE" MULTICA_OPS_LOG_SHIP_FIXTURE="$FIXTURE" \
  bash "$OPS_DIR/log-ship.sh" >/dev/null 2>&1 || fail "second log-ship run failed"
[ ! -d "$STALE_DIR" ] || fail "stale date dir was not pruned"
[ -f "$SHIPPED" ] || fail "current date dir must survive pruning"
[ -d "$SHIP_DIR/.state" ] || fail ".state must survive pruning"

echo "ops log-ship ok"
