# 日志外送选型与实现说明（PER-284）

来源：PER-270「远程 Multica 服务安全防护方案」§3.4 审计检测层——日志不得单点
存于服务器，外送本身不得外泄敏感内容。

## 1. 现状（勘察结论）

- 后端/前端日志写 stderr，由 Docker 默认 json-file 驱动落在服务器
  `/var/lib/docker/containers/*/*-json.log`，无轮转、无外送、单点存于服务器
  （见 `SELF_HOSTING_ADVANCED.md`「The backend writes to stderr and keeps
  nothing itself」）。服务器被攻破或磁盘故障即丢失全部运行时证据。
- 应用日志的凭据泄露面已由源码侧治理基线覆盖（`server/pkg/redact` 用于
  agent 输出等路径）。

## 2. 选型：阿里云 SLS vs Mac 侧收集

| 维度 | 阿里云 SLS | Mac 侧拉取（本方案） |
| --- | --- | --- |
| 可信域 | 与服务器同云账号。方案 T4 路径含「云账号攻破」——账号失陷时 SLS 中日志同样可读/可删 | Mac/NAS 为独立信任域，云账号失陷不波及 |
| 开通状态 | **待确认**：本机 aliyun CLI profile（cn-shanghai）对 SLS API 返回 `401 Access denied by access policy`（RAM 策略拒绝），无法核实开通状态与授权，当前凭据无法写入 | 不依赖云账号状态，复用既有 SSH 通道（patrol/backup 同款） |
| 新增密钥管理 | 需在 Mac 或服务器持有 SLS AK/SK——新增一份长期凭据，本身成为被窃取目标 | 无新增凭据，复用已收敛的 SSH 密钥 |
| 检索/告警能力 | 强（索引、查询、内置告警） | 弱（本地文件 grep；告警经 patrol 既有通道） |
| 成本/运维 | SLS 计费 + 投递配置 | 零云成本；磁盘占用由保留期控制 |

**结论：Mac 侧拉取为主方案，即刻实施。** 理由：① 与威胁模型一致——审计副本
必须与被审计对象（服务器/云账号）信任域分离；② 零新增凭据、零云依赖，复用
patrol/backup 已验证的 SSH + NAS 模式；③ SLS 开通/授权状态未确认，不阻塞。

**SLS 作为后续增强项保留**：待 Liu Xiang 确认 SLS 开通并授权后，可在
`log-ship.sh` 落地环节追加「脱敏文件 → SLS」投递（届时仍需先过 redact）。
确认前不实现、不引入 AK/SK。

## 3. 安全设计：双层脱敏

要求：日志外送本身不得外泄敏感内容（先过 redact，参考 `server/pkg/redact`）。

1. **源侧（Go，主保障）**：`server/internal/logger/redact_handler.go` 将
   slog handler 包一层 `redact.Text`——所有经 slog 输出的 message/属性在
   落 stderr 前即完成脱敏。后端容器日志从此不再含已知模式凭据，不再依赖
   每个调用点自觉（并结构性覆盖 PER-282 一类点状泄露）。
2. **外送侧（awk，纵深防御）**：`ops/scripts/log-redact.sh` 移植 redact 的
   模式族（AWS/GitHub/OpenAI/Slack/GitLab/Google/Stripe/JWT/Bearer/连接串/
   KEY=value/PEM/home 路径），拉取内容**管道内直接脱敏后落地**，原始日志
   在 Mac 不落盘。覆盖前端/postgres 等非 Go 日志流。
3. **诚实的边界**：两层均为模式匹配，未知形态凭据仍可能漏网（redact 包
   文档亦如此声明）；故敏感信息治理的根仍是「凭据不落 issue」（P0-1/P0-5）
   与字段加密（P2-1/P2-2）。

## 4. 实现组件

| 文件 | 职责 |
| --- | --- |
| `server/internal/logger/redact_handler.go` | 源侧 slog 脱敏层（含单测） |
| `ops/scripts/log-redact.sh` | 外送侧脱敏过滤器（stdin→stdout） |
| `ops/scripts/log-ship.sh` | Mac 侧拉取：SSH `docker logs --since 水位` → 脱敏 → 按天落地 → 保留期轮转 → 可选 NAS 双写；失败不推进水位（宁重复不丢失） |
| `ops/scripts/ssh-login-anomaly.sh` | SSH 登录失败异常检测（计数水位法，抗 logrotate），由 patrol 周期调用 |
| `ops/ops.env.example` | 新增 `MULTICA_OPS_LOG_SHIP_*` / `MULTICA_OPS_SSH_LOGIN_FAIL_THRESHOLD` / `MULTICA_OPS_SSH_ANOMALY_STATE_DIR` 键区 |

## 5. 运行接入（Mac 侧）

- `ops.env` 补齐上述键区；`log-ship.sh` 与 `ssh-login-anomaly.sh` 加入
  launchd（10min 节奏，与 patrol 一致），或由 patrol 内联调用异常检测。
- 告警通道复用 patrol 既有口径（Mac 本地日志 + NAS alerts 目录）。
- 验证：`scripts/ops-log-ship.test.sh`、`scripts/ops-ssh-anomaly.test.sh`
  以 fixture 模式离线跑通脱敏/水位/告警逻辑（不打真实 SSH）。

## 6. 事后定损用法

- 服务器日志存量为 `docker logs`（受容器重建影响）；外送副本在 Mac
  `MULTICA_OPS_LOG_SHIP_DIR/<日期>/<容器>.log`（及 NAS），是入侵调查的
  权威时间线：`grep -r 'audit_event' <日期>/` 可定位 workspace 删除等
  仅存在于应用日志的审计事件（见 §7）。

## 7. 与审计事件扩展（本 issue 同批交付）的关系

- 工作区级审计（成员角色/移除、邀请、agent 归档、批量增删、去重后的
  bulk_export）写 `activity_log`（forensic 行，无 issue_id，不进时间线 UI）。
- **workspace 删除是唯一例外**：拆除事务会连同该工作区的 activity_log 行
  一起删除，故删除事件以结构化应用日志 `audit_event action=workspace_deleted`
  记录——这正是日志外送必须存在的理由：该审计行只有外送副本能留住。
