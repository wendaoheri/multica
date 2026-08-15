#!/usr/bin/env bash
# Fixture-mode tests for ops/scripts/ssh-login-anomaly.sh (PER-284).
# No real SSH: the script runs with MULTICA_OPS_SSH_ANOMALY_FIXTURE.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT_DIR/ops/scripts/ssh-login-anomaly.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/ops-ssh-anomaly-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

ENV_FILE="$WORK/ops.env"
STATE_DIR="$WORK/state"
mkdir -p "$STATE_DIR"
cat > "$ENV_FILE" <<EOF
MULTICA_OPS_SSH_HOST=anomaly-test.invalid
MULTICA_OPS_SSH_PORT=22
MULTICA_OPS_SSH_LOGIN_FAIL_THRESHOLD=3
MULTICA_OPS_SSH_ANOMALY_STATE_DIR=$STATE_DIR
EOF

fail_line() {
  echo "Aug 15 10:2$1:45 host sshd[123$1]: Failed password for invalid user admin from 203.0.113.$1 port 22 ssh2"
}

fixture_total() {
  local total=$1
  {
    echo "total $total"
    fail_line 1
    fail_line 2
    fail_line 3
  } > "$WORK/fixture.log"
}

run_check() {
  MULTICA_OPS_CONFIG="$ENV_FILE" MULTICA_OPS_SSH_ANOMALY_FIXTURE="$WORK/fixture.log" \
    bash "$CHECK" 2>"$WORK/stderr.log"
}

# ---------- 1. first run creates baseline, never alerts ----------

fixture_total 5
OUT="$(run_check)" || { cat "$WORK/stderr.log" >&2; fail "first run exited non-zero"; }
echo "$OUT" | grep -q 'note=baseline_created' || fail "first run must create baseline: $OUT"
echo "$OUT" | grep -q 'ALERT' && fail "first run must not alert: $OUT"

# ---------- 2. +4 failures (>= threshold 3) -> P1 ----------

fixture_total 9
OUT="$(run_check)" || fail "second run exited non-zero"
echo "$OUT" | grep -q 'SSH_ANOMALY_STATS new_failures=4' || fail "expected 4 new failures: $OUT"
echo "$OUT" | grep -q 'ALERT P1 SSH_LOGIN_ANOMALY' || fail "expected P1 alert: $OUT"
echo "$OUT" | grep -q '203.0.113.' || fail "expected top_ips evidence: $OUT"

# ---------- 3. log rotation (total drops) -> treated as all-new ----------

fixture_total 2
OUT="$(run_check)" || fail "rotation run exited non-zero"
echo "$OUT" | grep -q 'SSH_ANOMALY_STATS new_failures=2' || fail "rotation delta wrong: $OUT"
echo "$OUT" | grep -q 'ALERT' && fail "2 < threshold 3 must not alert: $OUT"

# ---------- 4. jump to 3x threshold -> P0 ----------

fixture_total 12
OUT="$(run_check)" || fail "p0 run exited non-zero"
echo "$OUT" | grep -q 'SSH_ANOMALY_STATS new_failures=10' || fail "expected 10 new failures: $OUT"
echo "$OUT" | grep -q 'ALERT P0 SSH_LOGIN_ANOMALY' || fail "expected P0 alert: $OUT"

# ---------- 5. steady state (no new failures) -> no alert ----------

fixture_total 12
OUT="$(run_check)" || fail "steady run exited non-zero"
echo "$OUT" | grep -q 'SSH_ANOMALY_STATS new_failures=0' || fail "expected 0 new failures: $OUT"
echo "$OUT" | grep -q 'ALERT' && fail "steady state must not alert: $OUT"

echo "ops ssh-login-anomaly ok"
