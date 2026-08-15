#!/usr/bin/env bash
# ops-config.sh — MULTICA_OPS_* 共享配置加载与 fail-fast 校验段
#
# 方案依据：PER-267 合成终稿 v1.0 §2（配置管理）。服务器 backup.sh、Mac 拉取
# 脚本、patrol.sh 共用本文件，避免三处漂移。修改后需同步部署到服务器
# /opt/multica-ops/ops-config.sh。
#
# 用法（脚本内）：
#   source "<本文件路径>"
#   ops_config_load                              # 定位并加载 ops.env（缺失即 exit 1）
#   ops_config_require KEY...                    # 非空校验
#   ops_config_require_posint KEY...             # 正整数校验
#   ops_config_require_abs_path KEY...           # 绝对路径校验
#   ops_config_require_dir KEY...                # 绝对路径 + 目录存在
#   ops_config_require_writable_dir KEY...       # 目录存在 + 可写（写探测）
#   ops_config_dir_writable <路径>               # 非致命可写探测（返回 0/1，不 exit；
#                                                # 供运行期 NAS 降级判断使用）
#
# 规则：
#   1. 配置文件定位优先级：MULTICA_OPS_CONFIG 环境变量 > 规范位置（按本文件所在
#      位置推导：同目录 ops.env = 服务器布局；上级目录 ops.env = Mac 布局）。
#      该推导是方案允许的「唯一 bootstrap 常量」，其余路径一律来自配置。
#   2. 值优先级：进程环境变量 > 配置文件（已 export 的变量不被文件覆盖，
#      供一次性运维/演练临时覆盖）。
#   3. 配置文件格式：每行 KEY=VALUE，# 开头为注释；键必须以 MULTICA_OPS_ 开头；
#      严禁写入任何凭据（凭据只在服务器 /opt/multica-src/.env）。
#   4. 所有校验函数失败即点名报错并 exit 1（fail fast）；调用方必须在任何写操作
#      （dump/tar/rsync/rm 轮转）之前完成校验。脚本不得存在 ${VAR:-默认值} 形式
#      回退到硬编码路径执行写入的分支。

# ---------- 加载 ----------

ops_config_load() {
    local oc_self_dir oc_cfg oc_line oc_key oc_val oc_is_set

    oc_self_dir=$(cd "$(dirname "$BASH_SOURCE")" 2>/dev/null && pwd)
    if [ -z "$oc_self_dir" ]; then
        echo "[ops-config] ERROR: 无法解析 ops-config.sh 自身位置" >&2
        exit 1
    fi

    if [ -n "${MULTICA_OPS_CONFIG:-}" ]; then
        oc_cfg="$MULTICA_OPS_CONFIG"
    elif [ -f "$oc_self_dir/ops.env" ]; then
        oc_cfg="$oc_self_dir/ops.env"          # 服务器布局：/opt/multica-ops/
    else
        oc_cfg="$oc_self_dir/../ops.env"       # Mac 布局：ops/scripts -> ops/
    fi

    if [ ! -f "$oc_cfg" ] || [ ! -r "$oc_cfg" ]; then
        echo "[ops-config] ERROR: 配置文件缺失或不可读: $oc_cfg" >&2
        echo "[ops-config] hint: 从 ops.env.example 复制并填写；或用 MULTICA_OPS_CONFIG 指定路径" >&2
        exit 1
    fi

    while IFS= read -r oc_line || [ -n "$oc_line" ]; do
        case "$oc_line" in
            ''|\#*) continue ;;
        esac
        case "$oc_line" in
            *=*) ;;
            *)
                echo "[ops-config] ERROR: $oc_cfg: 非法行（应为 KEY=VALUE）: $oc_line" >&2
                exit 1
                ;;
        esac
        oc_key=${oc_line%%=*}
        oc_val=${oc_line#*=}
        # 去除键两侧空白
        oc_key=$(printf '%s' "$oc_key" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        # 去除值两侧的成对引号（如有）
        case "$oc_val" in
            \"*\") oc_val=${oc_val%\"}; oc_val=${oc_val#\"} ;;
            \'*\') oc_val=${oc_val%\'}; oc_val=${oc_val#\'} ;;
        esac
        case "$oc_key" in
            MULTICA_OPS_[A-Z0-9_]*) ;;
            *)
                echo "[ops-config] ERROR: $oc_cfg: 键 '$oc_key' 不符合 MULTICA_OPS_[A-Z0-9_]* 约定，拒绝加载（防漂移）" >&2
                exit 1
                ;;
        esac
        # 进程环境变量 > 配置文件：已设置的变量不覆盖
        eval "oc_is_set=\${$oc_key+set}"
        if [ "$oc_is_set" != "set" ]; then
            export "$oc_key=$oc_val"
        fi
    done < "$oc_cfg"

    MULTICA_OPS_CONFIG_LOADED="$oc_cfg"
    echo "[ops-config] loaded: $oc_cfg" >&2
}

# ---------- 校验（失败即 exit 1） ----------

ops_config_require() {
    local oc_key oc_val
    for oc_key in "$@"; do
        eval "oc_val=\${$oc_key:-}"
        if [ -z "$oc_val" ]; then
            echo "[ops-config] ERROR: 必填配置缺失或为空: $oc_key" >&2
            exit 1
        fi
    done
}

ops_config_require_posint() {
    local oc_key oc_val
    for oc_key in "$@"; do
        eval "oc_val=\${$oc_key:-}"
        case "$oc_val" in
            ''|*[!0-9]*)
                echo "[ops-config] ERROR: $oc_key 必须为正整数，当前值: '$oc_val'" >&2
                exit 1
                ;;
        esac
        if [ "$oc_val" -lt 1 ]; then
            echo "[ops-config] ERROR: $oc_key 必须 >= 1，当前值: $oc_val" >&2
            exit 1
        fi
    done
}

ops_config_require_abs_path() {
    local oc_key oc_val
    for oc_key in "$@"; do
        eval "oc_val=\${$oc_key:-}"
        if [ -z "$oc_val" ]; then
            echo "[ops-config] ERROR: 必填配置缺失或为空: $oc_key" >&2
            exit 1
        fi
        case "$oc_val" in
            /*) ;;
            *)
                echo "[ops-config] ERROR: $oc_key 必须为绝对路径，当前值: '$oc_val'" >&2
                exit 1
                ;;
        esac
    done
}

ops_config_require_dir() {
    local oc_key oc_val
    for oc_key in "$@"; do
        ops_config_require_abs_path "$oc_key"
        eval "oc_val=\${$oc_key}"
        if [ ! -d "$oc_val" ]; then
            echo "[ops-config] ERROR: $oc_key 目录不存在: $oc_val" >&2
            exit 1
        fi
    done
}

# 非致命可写探测：返回 0=可写，1=不可写/不存在。不 exit——供运行期降级判断
# （运行期 NAS 掉线 != 配置缺失，降级需留痕而非静默改道）。
ops_config_dir_writable() {
    local oc_dir oc_probe
    oc_dir="$1"
    [ -d "$oc_dir" ] || return 1
    oc_probe="$oc_dir/.ops-config-write-test.$$"
    if touch "$oc_probe" 2>/dev/null; then
        rm -f "$oc_probe" 2>/dev/null
        return 0
    fi
    return 1
}

ops_config_require_writable_dir() {
    local oc_key oc_val
    for oc_key in "$@"; do
        ops_config_require_dir "$oc_key"
        eval "oc_val=\${$oc_key}"
        if ! ops_config_dir_writable "$oc_val"; then
            echo "[ops-config] ERROR: $oc_key 目录不可写: $oc_val" >&2
            exit 1
        fi
    done
}
