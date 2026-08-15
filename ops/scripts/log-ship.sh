#!/usr/bin/env bash
#
# log-ship.sh — Multica 服务日志外送（Mac 侧拉取；launchd 周期触发）
# PER-284 日志外送层。选型结论见 ops/LOG_SHIPPING.md。
#
# 原则：
#   - 服务器侧只读：唯一动作是 `docker logs` 拉取，不触碰服务器任何状态；
#   - 先脱敏后落地：拉取内容一律先过 log-redact.sh 再写入任何文件（Mac/NAS），
#     原始日志在本机不落盘（管道内直接脱敏）；
#   - 断点续传：每容器一个水位文件（.state/<容器名>.since，UTC RFC3339）；
#     拉取+写入全部成功后才推进水位，失败则下次全量补拉（宁可重复、不可丢失，
#     重复行对审计无碍，缺口对定损致命）；
#   - 首跑限流：无水位时只拉近 MULTICA_OPS_LOG_SHIP_INITIAL_WINDOW_MIN 的日志，
#     避免一次性倾泻容器全量历史；
#   - 保留期轮转：按天分目录（YYYY-MM-DD），超过 RETAIN_DAYS 的整天目录删除；
#   - NAS 双写为可选增强：NAS 不可达时降级仅写 Mac 本地并留痕（同 patrol 口径）；
#   - 退出码：0 = 本次运行完成（含「无新日志」）；1 = 脚本自身基础设施失败
#     （配置缺失/状态目录不可写），供 launchd KeepAlive.SuccessfulExit=false 重试。
#   - 测试钩子：MULTICA_OPS_LOG_SHIP_FIXTURE=<文件> 时以该文件内容代替 SSH 拉取
#     （仅测试用，ops.env 中不存在该键）。
#
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
. "${SCRIPT_DIR}/ops-config.sh"

ops_config_load

# ---- fail fast：任何拉取/写盘前完成校验 ----
ops_config_require \
  MULTICA_OPS_SSH_HOST \
  MULTICA_OPS_SSH_PORT \
  MULTICA_OPS_LOG_SHIP_CONTAINERS \
  MULTICA_OPS_LOG_SHIP_DIR \
  MULTICA_OPS_LOG_SHIP_RETAIN_DAYS
ops_config_require_posint MULTICA_OPS_SSH_PORT MULTICA_OPS_LOG_SHIP_RETAIN_DAYS
ops_config_require_writable_dir MULTICA_OPS_LOG_SHIP_DIR

SSH_HOST="$MULTICA_OPS_SSH_HOST"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -p "$MULTICA_OPS_SSH_PORT")
SHIP_DIR="$MULTICA_OPS_LOG_SHIP_DIR"
STATE_DIR="$SHIP_DIR/.state"
SHIP_LOG="$SHIP_DIR/log-ship.log"
RETAIN_DAYS="$MULTICA_OPS_LOG_SHIP_RETAIN_DAYS"
INITIAL_WINDOW_MIN="${MULTICA_OPS_LOG_SHIP_INITIAL_WINDOW_MIN:-60}"

FIXTURE=""
eval "fixture_is_set=\${MULTICA_OPS_LOG_SHIP_FIXTURE+set}"
# shellcheck disable=SC2154 # fixture_is_set 由上方 eval 赋值
[ "$fixture_is_set" = "set" ] && FIXTURE="$MULTICA_OPS_LOG_SHIP_FIXTURE"

NOW_H="$(date '+%Y-%m-%d %H:%M:%S')"
DAY_DIR="$SHIP_DIR/$(date '+%Y-%m-%d')"

ship_log() {
  echo "$NOW_H $*" >> "$SHIP_LOG"
}

mkdir -p "$STATE_DIR" "$DAY_DIR" || {
  echo "[log-ship] ERROR: 无法创建状态/落地目录: $STATE_DIR / $DAY_DIR" >&2
  exit 1
}

# 单容器拉取-脱敏-落地。成功返回 0 并推进水位；失败返回 1（水位不动）。
ship_container() {
  local container="$1"
  local state_file="$STATE_DIR/$container.since"
  local since next_since raw_line_count

  next_since="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  if [ -f "$state_file" ]; then
    since="$(cat "$state_file")"
  else
    # 首跑：只拉近 INITIAL_WINDOW_MIN 分钟，避免倾泻全量历史。
    since="$(date -u -v "-${INITIAL_WINDOW_MIN}M" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d "-${INITIAL_WINDOW_MIN} minutes" '+%Y-%m-%dT%H:%M:%SZ')"
  fi

  local out_file="$DAY_DIR/$container.log"
  local pulled

  if [ -n "$FIXTURE" ]; then
    pulled="$(cat "$FIXTURE")"
  else
    # docker logs 同时输出 stdout/stderr 两路；--timestamps 便于事后对齐。
    if ! pulled="$(ssh "${SSH_OPTS[@]}" "$SSH_HOST" \
        docker logs --since "$since" --timestamps "$container" 2>&1)"; then
      ship_log "ALERT P1 LOG_SHIP_PULL_FAIL container=$container since=$since（SSH/docker 拉取失败，水位未推进）"
      return 1
    fi
  fi

  if [ -z "$pulled" ]; then
    # 无新日志：只推进水位，不向日志文件写空行。
    printf '%s\n' "$next_since" > "$state_file"
    ship_log "OK shipped container=$container lines=0 since=$since"
    return 0
  fi

  raw_line_count="$(printf '%s\n' "$pulled" | grep -c . || true)"

  # 先脱敏、后落地：原始内容不经过任何中间文件。
  if ! printf '%s\n' "$pulled" | "${SCRIPT_DIR}/log-redact.sh" >> "$out_file"; then
    ship_log "ALERT P1 LOG_SHIP_WRITE_FAIL container=$container（脱敏或写入失败，水位未推进）"
    return 1
  fi

  printf '%s\n' "$next_since" > "$state_file"
  ship_log "OK shipped container=$container lines=$raw_line_count since=$since -> $out_file"
  return 0
}

FAILURES=0
for container in $MULTICA_OPS_LOG_SHIP_CONTAINERS; do
  ship_container "$container" || FAILURES=$((FAILURES + 1))
done

# ---- 保留期轮转：删除超期的整天目录（不碰 .state 与散文件） ----
find "$SHIP_DIR" -maxdepth 1 -type d -name '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]' -mtime "+$RETAIN_DAYS" -exec rm -rf {} + 2>/dev/null || true

# ---- 可选 NAS 双写（降级留痕，不静默） ----
eval "nas_is_set=\${MULTICA_OPS_LOG_SHIP_NAS_DIR+set}"
# shellcheck disable=SC2154 # nas_is_set 由上方 eval 赋值
if [ "$nas_is_set" = "set" ] && [ -n "$MULTICA_OPS_LOG_SHIP_NAS_DIR" ]; then
  NAS_DIR="$MULTICA_OPS_LOG_SHIP_NAS_DIR"
  if ops_config_dir_writable "$NAS_DIR"; then
    mkdir -p "$NAS_DIR/$(date '+%Y-%m-%d')" 2>/dev/null || true
    if cp "$DAY_DIR"/*.log "$NAS_DIR/$(date '+%Y-%m-%d')/" 2>/dev/null; then
      :
    else
      ship_log "ALERT P2 NAS_COPY_FAIL 当日文件复制 NAS 失败（Mac 本地副本仍在）"
    fi
  else
    ship_log "ALERT P2 NAS_UNAVAILABLE $NAS_DIR 不可写；本次仅落 Mac 本地"
  fi
fi

# 拉取失败不改写已落地内容；退出码语义：仅「本次运行是否完成」。
# 连续失败由 patrol 侧的 LOG_SHIP_PULL_FAIL 告警与 launchd 重试兜底。
if [ "$FAILURES" -gt 0 ]; then
  ship_log "SUMMARY failures=$FAILURES（失败容器水位未推进，下次补拉）"
fi
exit 0
