# 配置说明

**[English](configuration.md) | 简体中文** · [← 返回 README](../../README.zh-CN.md)

可通过以下配置项调整运行行为：

## 环境变量

环境变量只承载引导期配置——ccLoad 在数据库连接建立之前就需要的值。启动之后的策略类配置（限额、冷却时长、超时、健康度排序等）是系统设置，在管理界面修改。特别地，`SQLITE_PATH`、`SQLITE_JOURNAL_MODE`、`CCLOAD_MYSQL`、`CCLOAD_POSTGRES`、`CCLOAD_ENABLE_SQLITE_REPLICA` 和 `CCLOAD_SQLITE_LOG_DAYS` 决定的是*数据库怎么打开*，因此不可能存在那个数据库里，会一直保持为环境变量。

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `CCLOAD_PASS` | 无 | 管理界面密码（**必填**，未设置将退出） |
| `CCLOAD_API_TOKENS` | 无 | 启动时预置 API 访问令牌，格式：`token1,token2` 或 `token1\|生产,token2\|开发`；已存在的 token 不会被覆盖 |
| `API_TOKENS` | 无 | `CCLOAD_API_TOKENS` 的兼容别名；两个变量同时设置且值不一致时启动失败 |
| `CCLOAD_MYSQL` | 无 | MySQL DSN（可选，格式: `user:pass@tcp(host:port)/db?charset=utf8mb4`）<br/>**与 `CCLOAD_POSTGRES` 互斥** |
| `CCLOAD_POSTGRES` | 无 | PostgreSQL DSN（可选，支持 URL 或 libpq 关键字，例如 `postgres://user:pass@host:5432/db?sslmode=disable`）<br/>**与 `CCLOAD_MYSQL` 互斥** |
| `CCLOAD_ENABLE_SQLITE_REPLICA` | `0` | 混合存储模式开关（`1`=启用，需要 MySQL 或 PostgreSQL 主库 DSN） |
| `CCLOAD_SQLITE_LOG_DAYS` | `7` | 混合模式 SQLite 日志为空时的首次导入天数（-1=全量）；`0` 关闭所有启动日志导入，其他值在后续启动只导入 SQLite 最后时间之后的日志 |
| `CCLOAD_ALLOW_INSECURE_TLS` | `0` | 禁用上游 TLS 证书校验（`1`=启用；⚠️仅用于临时排障/受控内网环境） |
| `PORT` | `8080` | 服务端口 |
| `GIN_MODE` | `release` | 运行模式（`debug`/`release`） |
| `GIN_LOG` | `true` | Gin 访问日志开关（`false`/`0`/`no`/`off` 关闭） |
| `TRUSTED_PROXIES` | 私有网段 + Loopback + `100.64.0.0/10` | 可信代理 CIDR 列表（逗号分隔）；`none`=不信任任何代理 |
| `SQLITE_PATH` | `data/ccload.db` | SQLite 数据库文件路径（纯 SQLite 与混合模式） |
| `CURSOR_SDK_BRIDGE_BIN` | 自动发现 | 显式指定 Cursor SDK Bridge 可执行文件；存在 Cursor 渠道时，无效覆盖会导致启动失败 |
| `SQLITE_JOURNAL_MODE` | `WAL` | SQLite Journal 模式（WAL/TRUNCATE/DELETE 等，容器环境建议 TRUNCATE） |
| `CCLOAD_HOST_OVERRIDES` | 无 | DNS 覆盖：将上游域名钉到固定 IP，绕过 DNS 解析。格式：`host1=ip1,host2=ip2`，例如 `anyrouter.top=47.246.23.200`。不影响 TLS SNI/证书/Host 头 |
| `CCLOAD_MODEL_CATALOG_CACHE` | 无 | models.dev 模型目录缓存文件路径（默认 `data/model-catalog.json`，默认目录不可写时回退临时目录） |
| `CCLOAD_ANTHROPIC_CLI_VERSION_SYNC` | `true` | 每小时从 GitHub 同步最新 Claude Code CLI 版本，作为 Anthropic OAuth 指纹的版本下限；设为 `false` 只使用内置与已缓存版本 |
| `CCLOAD_ANTHROPIC_CLI_VERSION_CACHE` | 无 | Claude Code CLI 版本缓存文件路径（默认与模型目录缓存同目录的 `anthropic-cli-version.json`；容器部署须位于持久化卷） |

> 如果你的服务挂在反向代理或负载均衡后面，建议显式设置 `TRUSTED_PROXIES`，避免伪造 `X-Forwarded-For` 干扰客户端 IP 识别和登录限速。
> 可通过 `GET /admin/runtime-metrics` 查看 Responses WebSocket、日志队列/落库失败，以及混合存储主库待同步、失败、丢弃与最后成功时间。

### 混合存储模式（SQLite 权威库 + 主库异步副本）

HuggingFace Spaces 等环境重启后本地数据会丢失，远程 MySQL/PostgreSQL 又可能存在网络延迟。混合模式兼顾持久化和本地读性能：

- **SQLite 权威库**：配置、凭据、Key、冷却、设置和日志都同步读写本地 SQLite；SQLite 成功就是请求成功
- **主库异步副本**：进程内 worker 按实体合并最终状态；失败任务等待 10 秒后重试，主库慢或临时故障不阻塞请求；脏实体过多时折叠为一次全量状态对账，避免内存无界增长
- **启动语义**：SQLite 文件首次创建时从主库导入配置；`CCLOAD_SQLITE_LOG_DAYS=0` 关闭启动日志导入，否则每次启动都要求主库可用，已有日志时只从主库增量导入 `time > MAX(sqlite.logs.time)` 的尾部，日志为空时才按该变量限制导入窗口；已有 SQLite 配置和日志都不会被删除覆盖
- **日志语义**：主库日志与日志清理只做一次 best-effort 尝试，不进入 10 秒重试；较慢时新批次会替换旧批次并计入 dropped，允许主库日志缺失
- **健康检查**：只检查权威 SQLite；主库同步状态通过 runtime metrics 观察
- **仅本地数据**：Web Session 与原始 DebugData 只保存在当前实例的 SQLite
- **明确边界**：这是单实例、单写者方案。进程重启会丢失尚在内存中的同步任务，不使用 outbox，也不支持多实例混合写

```bash
# MySQL 主库
export CCLOAD_MYSQL="user:pass@tcp(host:3306)/db?charset=utf8mb4"
export CCLOAD_ENABLE_SQLITE_REPLICA=1
export CCLOAD_SQLITE_LOG_DAYS=7  # SQLite 日志为空时先导入最近 7 天，后续启动只补尾部日志

# 或 PostgreSQL 主库
export CCLOAD_POSTGRES="postgres://user:pass@host:5432/db?sslmode=disable"
export CCLOAD_ENABLE_SQLITE_REPLICA=1
```

**存储模式**：
| 模式 | 配置 | 适用场景 |
|------|------|---------|
| 纯 SQLite | 不设置主库 DSN | 本地开发、单机部署 |
| 纯 MySQL | 设置 `CCLOAD_MYSQL` | 标准生产环境 |
| 纯 PostgreSQL | 设置 `CCLOAD_POSTGRES` | PostgreSQL 生产环境 |
| 混合模式 | 主库 DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1` | HuggingFace Spaces / 高延迟主库 |

## Web 管理配置（数据库存储，保存后自动重启）

这些配置存在数据库中，在 Web 界面 `/web/settings.html` 修改。保存会先写库，随后约 2 秒自动重启进程——重启本身就是生效机制，进行中的请求会先跑完再退出。`model_multimodal_fallback` 是唯一热重载的设置——只保存该映射时原子替换内存快照、无需重启：

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `log_retention_days` | `7` | 日志保留天数（-1永久保留，1-365天） |
| `max_key_retries` | `3` | 单个渠道内最大Key重试次数 |
| `max_concurrency` | `1000` | 最大并发请求数，限制同时处理的代理请求数量 |
| `http_read_timeout_seconds` | `0` | 下游请求读取超时（秒）；`0` 使用内建 120 秒。覆盖请求头和请求体的完整读取，超时返回 408，与请求体大小限制独立。 |
| `max_body_bytes` | `33554432` | 请求体最大字节数，默认 32MB |
| `max_image_body_bytes` | `20971520` | Images API 请求体最大字节数，默认 20MB |
| `cooldown_auth_seconds` | `300` | 认证错误（401/402/403）初始冷却时间（秒） |
| `cooldown_server_seconds` | `120` | 服务器错误（5xx）初始冷却时间（秒） |
| `cooldown_timeout_seconds` | `60` | 超时错误（597/598）初始冷却时间（秒） |
| `cooldown_rate_limit_seconds` | `60` | 限流错误（429）初始冷却时间（秒） |
| `codex_map_429_to_503` | `false` | 将返回给官方 Codex 客户端的最终上游 429 映射为 503，使其按 5xx 重试；不影响其他 Responses 客户端和 ccLoad 自身限额 |
| `cooldown_min_seconds` | `10` | 指数退避冷却下限（秒） |
| `cooldown_max_seconds` | `1800` | 指数退避冷却上限（秒；下限大于上限时整对回退默认值） |
| `cooldown_fallback_enabled` | `true` | 所有渠道都在冷却时，兜底选取「最早恢复」的渠道继续服务（Key 同样选最早恢复的）；设为 `false` 则直接拒绝请求 |
| `global_cooldown_detection_rules` | `{}` | 全局冷却探测规则，渠道未配置自身 `cooldown_detection_rules` 时继承 |
| `TypeSafe_enabled` | `false` | 启用 TypeSafe（Jev）错误分析兜底；需配置密钥，保存后重启生效 |
| `TypeSafe_api_key` | 空 | TypeSafe API 密钥；设置接口不回显，不修改则保留，重置时清除并关闭 TypeSafe |
| `upstream_connection_reuse_limit_seconds` | `0` | 上游连接最长复用时间（秒，`0`=不限制）；统一约束 HTTP/1.1、HTTP/2 和 WebSocket，达到时限后不再接收新请求，在途请求跑完再关闭，下次按需重连 |
| `antigravity_sensitive_words` | `["API","proxy","Claude","Anthropic"]` | JSON 字符串数组；命中的词在 Antigravity `systemInstruction` 和 CodeBuddy system/developer 消息文本中用零宽字符替换 |
| `upstream_first_byte_timeout` | `0` | 流式请求首个有效内容超时（秒，0=禁用） |
| `stream_timeout` | `0` | 流式请求总超时（秒，0=禁用） |
| `stream_idle_timeout` | `0` | 流式请求上游连续无数据超时，超过即中止本次尝试（秒，0=禁用） |
| `non_stream_timeout` | `600` | 非流式请求超时（秒，0=禁用） |
| `anthropic_first_byte_timeout` | `0` | Anthropic 流式请求首个有效内容超时（秒，0=使用全局 `upstream_first_byte_timeout`） |
| `anthropic_non_stream_timeout` | `0` | Anthropic 非流式请求超时（秒，0=使用全局 `non_stream_timeout`） |
| `anthropic_stream_idle_timeout` | `180` | Anthropic 流式请求上游连续无数据超时（秒，0=使用全局 `stream_idle_timeout`） |
| `codex_first_byte_timeout` | `0` | Codex 流式请求首个有效内容超时（秒，0=使用全局 `upstream_first_byte_timeout`） |
| `codex_non_stream_timeout` | `0` | Codex 非流式请求超时（秒，0=使用全局 `non_stream_timeout`） |
| `openai_first_byte_timeout` | `0` | OpenAI 流式请求首个有效内容超时（秒，0=使用全局 `upstream_first_byte_timeout`） |
| `openai_non_stream_timeout` | `0` | OpenAI 非流式请求超时（秒，0=使用全局 `non_stream_timeout`） |
| `gemini_first_byte_timeout` | `0` | Gemini 流式请求首个有效内容超时（秒，0=使用全局 `upstream_first_byte_timeout`） |
| `gemini_non_stream_timeout` | `0` | Gemini 非流式请求超时（秒，0=使用全局 `non_stream_timeout`） |
| `enable_health_score` | `false` | 启用基于健康度的渠道动态排序 |
| `success_rate_penalty_weight` | `100` | 成功率惩罚权重（见下方说明） |
| `health_score_window_minutes` | `30` | 成功率统计时间窗口（分钟） |
| `health_score_update_interval` | `30` | 成功率缓存更新间隔（秒） |
| `health_min_confident_sample` | `20` | 置信样本量阈值（样本量达到此值时惩罚全额生效） |
| `enable_ttfb_score` | `false` | 启用渠道首字相对延迟惩罚，需同时开启 `enable_health_score` |
| `ttfb_penalty_weight` | `20` | 首字惩罚权重（平均首字为候选中位数 2 倍且满置信度时的惩罚值） |
| `ttfb_max_slow_ratio` | `2` | 首字相对慢速比上限（`平均首字 / 候选中位首字 - 1`） |
| `ttfb_min_confident_sample` | `10` | 首字置信样本量阈值 |
| `channel_test_content` | `sonnet 4.0的发布日期是什么` | 渠道手动测试与定时检测的默认内容；多个内容用竖线 `\|` 分隔，每次测试取其中一个；不能为空 |
| `model_catalog_sync_interval_hours` | `6` | 每 6 小时从 models.dev 同步模型目录；`0` 禁用网络同步。启动时使用最近一次成功的缓存，失败时回退内嵌目录；渠道 `cost_multiplier` 仍然适用。 |
| `auto_update_interval_hours` | `12` | 非容器部署的版本检查间隔（小时，0=禁用，启用时最低 1 小时）；容器中不可用 |
| `auto_update_channel` | `stable` | 非容器部署的发布渠道：`stable` 只接收稳定版；`preview` 同时接收稳定版和测试版，并选择语义版本最高者；容器中不可用 |
| `model_multimodal_fallback` | `{}` | JSON 映射 `{"非视觉模型":"回退模型"}`（最多 64 条 / 8 KB）；唯一保存后立即生效、无需重启的设置 |
| `model_fuzzy_match` | `false` | 模型名精确匹配未命中时，回退到子串匹配 + 版本排序 |
| `model_custom_pricing` | `{}` | JSON 对象，覆盖模型价格（单位：美元/百万 Token）；优先级高于 models.dev 目录和内嵌定价 |
| `responses_ws_max_connections` | `128` | 下游 Responses WebSocket 全局最大并发连接数；`0` 使用内建默认值 |
| `responses_ws_max_connections_per_token` | `64` | 单个认证 Token 的下游 Responses WebSocket 最大并发连接数；`0` 使用内建默认值 |
| `responses_ws_max_sessions` | `256` | 整个进程保留的 Responses WebSocket 执行会话数上限；`0` 使用内建默认值 |
| `responses_ws_session_ttl_minutes` | `15` | 空闲执行会话保留时长（分钟）；`0` 使用内建默认值 |
| `responses_ws_max_transcript_bytes` | `268435456` | 整个进程保留的 transcript 有效载荷总预算（256 MiB）；`0` 使用内建默认值 |
| `debug_log_enabled` | `false` | 记录上游请求/响应调试日志 |
| `debug_log_retention_minutes` | `2` | 调试日志保留时长（分钟） |
| `api_token_login_enabled` | `false` | 允许 API 访问令牌登录 Web 管理界面；不影响 API 调用 |
| `api_token_show_channels` | `false` | 向令牌登录的 Web 用户显示渠道名和实际模型名；禁用时隐藏两者，调用统计仍保留；不开放渠道配置，保存后重启生效 |
| `log_channel_click_action` | `edit` | 日志页点击渠道名后的操作（`edit` 打开渠道编辑器，`filter` 按该渠道过滤） |
| `channel_stats_range` | `today` | 渠道管理页费用统计时间范围（`today`/`yesterday`/`day_before_yesterday`/`this_week`/`last_week`/`this_month`/`last_month`） |
| `auto_refresh_interval_seconds` | `0` | Web 页面自动刷新间隔（秒，`0`=禁用，建议 `>= 30`）；有对话框打开时跳过本次刷新 |
| `CODEX_BASE_URL` | 空 | Codex OAuth 渠道的全局上游地址（完整 Responses URL；官方默认 `https://chatgpt.com/backend-api/codex/responses`） |
| `ANTHROPIC_BASE_URL` | 空 | Anthropic OAuth 渠道的全局 API 根地址（官方默认 `https://api.anthropic.com`） |
| `XAI_BASE_URL` | 空 | xAI OAuth 渠道的全局 API 根地址（通常以 `/v1` 结尾；官方默认 `https://cli-chat-proxy.grok.com/v1`） |
| `ANTIGRAVITY_URL` | 空 | Antigravity OAuth 渠道的全局上游地址（官方默认 `https://daily-cloudcode-pa.googleapis.com`，备用 `https://cloudcode-pa.googleapis.com`） |

分协议超时按“实际转发到的上游协议”生效：协议转换后转发到 OpenAI，就读取 `openai_*_timeout`；对应值为 `0` 时回退全局超时。

渠道定时检测是渠道级配置而非全局配置：渠道编辑器持有 `scheduled_check_enabled`、`scheduled_check_interval_minutes`（1–1440，默认 `300`）和 `scheduled_check_start_time`（`HH:MM`，默认 `00:00`），每个渠道可按各自节奏检测。旧的全局设置 `channel_check_interval_hours` 已移除：升级后首次启动会把它的值折算进每个渠道的间隔，并删除该行记录。

四个 OAuth 全局上游地址默认为空，表示使用各提供商官方地址。设置后，对应 OAuth 渠道的数据请求、模型发现、渠道测试和额度查询只使用该全局地址并忽略渠道 URL；OAuth 授权和 Token 交换/刷新仍走提供商官方地址，API Key 渠道不受影响。

### 渠道动态排序说明

启用 `enable_health_score` 后，ccLoad 会根据近期渠道健康数据计算有效优先级。成功率惩罚始终参与计算；只有同时设置 `enable_ttfb_score=true` 时，才叠加首字相对延迟惩罚：

```
失败置信度 = min(1.0, 样本量 / health_min_confident_sample)
失败惩罚 = 失败率 × success_rate_penalty_weight × 失败置信度

相对慢速比 = clamp(平均首字 / 当前候选渠道首字中位数 - 1, 0, ttfb_max_slow_ratio)
首字置信度 = min(1.0, 首字样本量 / ttfb_min_confident_sample)
首字惩罚 = 相对慢速比 × ttfb_penalty_weight × 首字置信度

有效优先级 = 基础优先级 - 失败惩罚 - 首字惩罚
```

**置信度因子**用于避免新渠道或低流量渠道因少量样本被全额惩罚。首字排序只统计成功请求，并与当前候选渠道的首字中位数比较；达到或快于中位数的渠道不受首字惩罚，少于两个候选渠道有有效首字数据时不启用该项惩罚。

**仅成功率惩罚示例**（`enable_ttfb_score=false`，`success_rate_penalty_weight = 100`，`health_min_confident_sample = 20`）：

| 渠道 | 基础优先级 | 成功率 | 样本量 | 置信度 | 惩罚值 | 有效优先级 |
|------|-----------|--------|--------|--------|--------|-----------|
| A | 100 | 95% | 100 | 1.0 | 5 | **95** |
| B | 90 | 70% | 80 | 1.0 | 30 | **60** |
| C | 80 | 60% | 4 | 0.2 | 8 | **72** |
| D | 70 | 100% | 50 | 1.0 | 0 | **70** |

基础优先级排序：A > B > C > D
**有效优先级排序：A (95) > C (72) > D (70) > B (60)**

**动态排序效果**：
- 渠道 B 原本排第二，但 70% 成功率导致惩罚 30，降至最后
- 渠道 D 原本排最后，但 100% 成功率使其超越 B 和 C
- 渠道 C 成功率仅 60%，但样本量 4（置信度 0.2）使惩罚从 40 降为 8，避免新渠道被过早淘汰

**权重调优建议**：
- 默认值 100 适合渠道优先级间隔为 10 的场景
- 权重 100 时：10% 失败率 = 降一档优先级（满置信度时）
- 若优先级间隔为 5，可调整为 50
- `health_min_confident_sample` 建议根据日均请求量调整，默认 20 适合中等流量场景

### API 访问令牌配置

**重点**：API 令牌默认在 Web 界面管理；Docker/CI 迁移场景可用环境变量预置：

- 访问 `http://localhost:8080/web/tokens.html` 进行令牌管理
- 启动时可设置 `CCLOAD_API_TOKENS=token1|生产,token2|开发` 自动创建缺失令牌
- 预置逻辑是幂等的：已存在的 token 保留原描述、限额、模型/渠道限制和统计数据
- 支持添加、删除、查看令牌
- 所有令牌存储在数据库中，支持持久化
- 未配置任何令牌时，所有 `/v1/*` 与 `/v1beta/*` API 返回 `401 Unauthorized`

⚠️ **安全提示**：
- 生产环境优先使用 Docker Secrets、Kubernetes Secrets 或平台加密 Secrets，避免把 token 明文写进普通环境变量
- CI/CD 中不要打印完整环境变量，避免日志泄露
- 预置完成后如不再需要自动恢复，可从部署配置中移除 `CCLOAD_API_TOKENS`
- 限制容器 inspect、编排平台控制台和部署配置的访问权限

**令牌高级功能**：
- **费用限额**：通过 `cost_limit_usd`、`cost_daily_limit_usd`、`cost_monthly_limit_usd` 分别设置总、日、月费用上限（美元）；`0` 表示不限制。任一已启用限额达到上限即返回 429；设置费用限额时还必须将 `max_concurrency` 设为正数。
- **模型限制**：限制令牌可访问的模型列表，增强访问控制
- **渠道限制**：`allowed_channel_ids` 配合 `channel_restriction_mode`——`allow` 为白名单，`deny` 为黑名单；两种模式下空列表均表示不限制
- **并发限制**：`max_concurrency` 限制单令牌同时在飞的请求数（`0`=不限制）
- **首字节时间**：记录流式请求的 TTFB（毫秒），便于诊断上游延迟

### 行为摘要

行为摘要：

- 未设置 `CCLOAD_PASS`：程序启动失败并退出（安全第一）
- 未配置 API 访问令牌：所有 `/v1/*` 与 `/v1beta/*` API 返回 `401 Unauthorized`，去Web界面 `/web/tokens.html` 配置令牌
- 公开端点：`GET /health`（健康检查）和 `GET /public/summary`（统计摘要）无需认证，其他都要授权

## Token 认证系统

Token 认证系统：

**认证方式**：
- **Web界面**：可用管理员密码或 API Token 登录，获取 24 小时有效的 Web 会话令牌
- **API端点**：支持 `Authorization: Bearer <token>` 头认证

**核心特性**：
- ✅ **作用域会话**：API Token 会话只读，服务端强制限制为当前 Token 的数据
- ✅ **即时失效**：API Token 被禁用、删除或过期后，其 Web 会话立即失效
- ✅ **凭据隔离**：浏览器存储中不会保存 API Token 明文
- ✅ **服务端授权**：渠道、令牌、设置和调试数据始终只允许管理员访问

**使用示例**：

```bash
# 1. 登录获取Token
curl -X POST http://localhost:8080/login \
  -H "Content-Type: application/json" \
  -d '{"mode":"admin","password":"your_admin_password"}' | jq

# 响应示例：
# {
#   "status": "success",
#   "token": "abc123...",  # 64字符十六进制Token
#   "expiresIn": 86400     # 24小时（秒）
# }

# 2. 使用Token访问管理API
curl http://localhost:8080/admin/channels \
  -H "Authorization: Bearer <your_token>"

# 3. 登出（可选，Token会在24小时后自动过期）
curl -X POST http://localhost:8080/logout \
  -H "Authorization: Bearer <your_token>"
```
