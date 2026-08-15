#!/usr/bin/env bash
#
# ssh-login-anomaly.sh — 服务器 SSH 登录失败异常检测（PER-284）
# 由 patrol.sh 在其 SSH 可达分支内周期性调用，输出契约供 patrol 翻译为告警。
#
# 原理（计数水位法，兼容 logrotate）：
#   - 站内 auth 日志（/var/log/auth.log 或 /var/log/secure）中
#     「Failed password / Invalid user / authentication failure」的总条数
#     作为单调水位；本次总数 - 上次水位 = 新增失败数。
#   - 日志轮转会使总数回落：总数 < 水位 时视为轮转，基线归零、以当前总数为增量
#     （宁可误报一次，不可漏报暴力破解）。
#   - 首跑只建基线不告警（历史失败不属于「新增异常」）。
#
# 输出契约（stdout，供 patrol 逐行解析）：
#   SSH_ANOMALY_STATS new_failures=<N> interval_min=<M> top_ips=<摘要>
#   ALERT <P0|P1> SSH_LOGIN_ANOMALY <证据摘要>   # 仅达阈值时出现
# 退出码：0 = 检测运行完成（无论是否告警）；1 = 基础设施失败（配置/SSH），
# 由 patrol 记录并按其自身告警口径处理。
#
# 阈值语义：新增失败 >= THRESHOLD → P1；>= 3×THRESHOLD → P0（疑似正在进行
# 的暴力破解）。THRESHOLD / WINDOW 由 ops.env 显式配置，无内置默认。
#
# 测试钩子：MULTICA_OPS_SSH_ANOMALY_FIXTURE=<文件> 时以该文件代替站内 SSH
# 采集（仅测试用，ops.env 中不存在该键）。文件内容契约与站内采集一致：
# 首行 "total <N>"，其后为最近的失败日志行。
#
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
. "${SCRIPT_DIR}/ops-config.sh"

ops_config_load

ops_config_require \
  MULTICA_OPS_SSH_HOST \
  MULTICA_OPS_SSH_PORT \
  MULTICA_OPS_SSH_LOGIN_FAIL_THRESHOLD \
  MULTICA_OPS_SSH_ANOMALY_STATE_DIR
ops_config_require_posint \
  MULTICA_OPS_SSH_PORT \
  MULTICA_OPS_SSH_LOGIN_FAIL_THRESHOLD
ops_config_require_writable_dir MULTICA_OPS_SSH_ANOMALY_STATE_DIR

SSH_HOST="$MULTICA_OPS_SSH_HOST"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -p "$MULTICA_OPS_SSH_PORT")
THRESHOLD="$MULTICA_OPS_SSH_LOGIN_FAIL_THRESHOLD"
STATE_FILE="$MULTICA_OPS_SSH_ANOMALY_STATE_DIR/ssh-anomaly-state.env"
# 尾部采样上限：够聚合出 top IPs 即可，避免大日志跨网传输。
TAIL_CAP=200

FAIL_PATTERN='Failed password|Invalid user|authentication failure'

FIXTURE=""
eval "fixture_is_set=\${MULTICA_OPS_SSH_ANOMALY_FIXTURE+set}"
# shellcheck disable=SC2154 # fixture_is_set 由上方 eval 赋值
[ "$fixture_is_set" = "set" ] && FIXTURE="$MULTICA_OPS_SSH_ANOMALY_FIXTURE"

NOW_EPOCH="$(date +%s)"

# ---------- 站内采集（单次 SSH，全部只读） ----------
collect() {
  if [ -n "$FIXTURE" ]; then
    cat "$FIXTURE"
    return
  fi
  # 未加引号的 heredoc 为有意为之：$FAIL_PATTERN / $TAIL_CAP 需在客户端展开。
  # shellcheck disable=SC2087
  ssh "${SSH_OPTS[@]}" "$SSH_HOST" bash -s <<REMOTE
LOGF=""
[ -f /var/log/auth.log ] && LOGF=/var/log/auth.log
[ -z "\$LOGF" ] && [ -f /var/log/secure ] && LOGF=/var/log/secure
if [ -z "\$LOGF" ]; then
  echo "total NA"
  exit 0
fi
echo "total \$(grep -cE '$FAIL_PATTERN' "\$LOGF" 2>/dev/null || echo 0)"
grep -E '$FAIL_PATTERN' "\$LOGF" 2>/dev/null | tail -n $TAIL_CAP
REMOTE
}

RAW="$(collect)" || {
  echo "[ssh-login-anomaly] ERROR: 站内采集失败（SSH 或远端命令）" >&2
  exit 1
}

TOTAL="$(printf '%s\n' "$RAW" | grep '^total ' | head -1 | cut -d' ' -f2)"
if [ -z "$TOTAL" ] || [ "$TOTAL" = "NA" ]; then
  # 日志文件不存在（如纯 journald 且无 auth.log/secure）：留痕但不告警。
  echo "SSH_ANOMALY_STATS new_failures=0 interval_min=0 top_ips=none note=auth_log_not_found"
  exit 0
fi
case "$TOTAL" in
  ''|*[!0-9]*)
    echo "[ssh-login-anomaly] ERROR: 无法解析失败总数: '$TOTAL'" >&2
    exit 1
    ;;
esac

LAST_TOTAL=""
LAST_EPOCH=""
if [ -f "$STATE_FILE" ]; then
  # shellcheck source=/dev/null
  . "$STATE_FILE"
fi

INTERVAL_MIN=0
if [ -n "$LAST_EPOCH" ]; then
  INTERVAL_MIN=$(( (NOW_EPOCH - LAST_EPOCH) / 60 ))
fi

# ---------- 判定 ----------
if [ -z "$LAST_TOTAL" ]; then
  # 首跑：建基线，不告警。
  {
    echo "LAST_TOTAL=$TOTAL"
    echo "LAST_EPOCH=$NOW_EPOCH"
  } > "$STATE_FILE"
  echo "SSH_ANOMALY_STATS new_failures=0 interval_min=0 top_ips=none note=baseline_created total=$TOTAL"
  exit 0
fi

if [ "$TOTAL" -lt "$LAST_TOTAL" ]; then
  # 日志轮转：基线归零，当前总数全部视为新增（宁误报不漏报）。
  NEW_FAILURES="$TOTAL"
  ROTATED=1
else
  NEW_FAILURES=$(( TOTAL - LAST_TOTAL ))
  ROTATED=0
fi

# top IPs：从采样尾部提取 "from <ip>" / "user ... from <ip>" 中的地址。
TOP_IPS="$(printf '%s\n' "$RAW" | grep -v '^total ' | tail -n "$TAIL_CAP" \
  | grep -oE 'from [0-9a-fA-F:.]+' | cut -d' ' -f2 \
  | sort | uniq -c | sort -rn | head -5 \
  | awk '{printf "%s(%s) ", $2, $1}')"
[ -n "$TOP_IPS" ] || TOP_IPS="none"

{
  echo "LAST_TOTAL=$TOTAL"
  echo "LAST_EPOCH=$NOW_EPOCH"
} > "$STATE_FILE"

echo "SSH_ANOMALY_STATS new_failures=$NEW_FAILURES interval_min=$INTERVAL_MIN top_ips=$TOP_IPS"

if [ "$NEW_FAILURES" -ge "$THRESHOLD" ]; then
  ROT_NOTE=""
  [ "$ROTATED" -eq 1 ] && ROT_NOTE="（检测到日志轮转，增量按轮转后全量计）"
  if [ "$NEW_FAILURES" -ge $(( THRESHOLD * 3 )) ]; then
    echo "ALERT P0 SSH_LOGIN_ANOMALY 近${INTERVAL_MIN}min 新增 SSH 登录失败 ${NEW_FAILURES} 次（阈值 ${THRESHOLD} 的 3 倍以上，疑似暴力破解）${ROT_NOTE} top_ips: ${TOP_IPS}"
  else
    echo "ALERT P1 SSH_LOGIN_ANOMALY 近${INTERVAL_MIN}min 新增 SSH 登录失败 ${NEW_FAILURES} 次（阈值 ${THRESHOLD}）${ROT_NOTE} top_ips: ${TOP_IPS}"
  fi
fi

exit 0
