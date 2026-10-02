# 架构

**[English](architecture.md) | 简体中文** · [← 返回 README](../../README.zh-CN.md)

## 协议路由

每个渠道默认接受四种客户端协议。实际上游协议由 `protocol_transform_mode` 和每个结构化 URL 的 `protocols` 声明共同决定：`upstream` 只直通客户端协议；`auto` 先尝试客户端协议，再按 OpenAI → Anthropic → Codex → Gemini 探测并跳过已试协议，仅在响应未提交的能力错误后继续；`local` 优先使用显式声明协议的 URL，并保持每个 URL 的声明顺序。来自官方 Codex 客户端的 Responses 请求在 `local` 模式下会在 URL 已声明 Codex 能力时优先选择 Codex 原生路径；未声明 Codex 的 URL 不会因此获得额外能力，其他客户端仍遵循原声明顺序。只有全部 URL 都未声明协议时，`local` 才按 Anthropic → Codex → OpenAI → Gemini 尝试。不兼容 URL 不发请求、不冷却。自动探测成功结果按 URL 和请求族缓存到进程重启或渠道配置变更；只有稳定的端点级非模型 404/405 才会缓存该 URL 与请求族的“全部协议不支持”结果，并在 10 分钟后重新探测。请求相关的 400/403/500 和本地转换失败会在下次请求时重新尝试。

![ccLoad 程序架构](../../images/ccload-architecture.jpg)

## 软错误检测

HTTP 200 但实际是错误的响应，和普通上游故障走同一条故障切换路径：

- `{"error": {...}}` 结构的 JSON 错误
- `type` 字段是 `"error"` 的响应
- SSE `error` 事件中的明确限流（`rate_limit_exceeded` / `too_many_requests`），按 `429` 处理
- `"当前模型负载过高"` 之类的纯文本告警

## 核心依赖

| 组件 | 版本 | 用途 | 性能优势 |
|------|------|------|----------|
| **Go** | 1.27.0+ | 运行时环境 | 原生并发支持，现代工具链 |
| **Gin** | v1.12.0 | Web框架 | 高性能HTTP路由 |
| **modernc/sqlite** | v1.59.0 | 嵌入式数据库 | 纯Go实现，零CGO依赖，单文件存储（默认） |
| **MySQL** | v1.10.1 | 关系型数据库 | 可选，适合高并发生产环境 |
| **PostgreSQL (pgx)** | v5.11.0 | 关系型数据库 | 可选，支持 URL 和 libpq DSN |
| **Sonic** | v1.15.4 | JSON库 | 高性能 JSON 编解码 |
| **gjson / sjson** | v1.19.0 / v1.2.5 | 协议 JSON 转换 | 定向读写字段，避免通用 map 转换 |
| **godotenv** | v1.5.1 | 环境配置 | 简化配置管理 |

## 架构特点

架构重点：

**模块化架构**（SOLID原则实践）:
- **proxy模块拆分**（SRP原则）：
  - `proxy_handler.go`：HTTP入口、并发控制、路由选择
  - `proxy_forward.go`：核心转发逻辑、请求构建、响应处理
  - `proxy_error.go`：错误处理、冷却决策、重试逻辑
  - `proxy_util.go`：常量、类型定义、工具函数
  - `proxy_stream.go`：流式响应、首字节检测
  - `proxy_gemini.go`：Gemini API特殊处理
  - `proxy_sse_parser.go`：SSE解析器（防御性处理，支持 Gemini/OpenAI 缓存 Token 解析）
  - `proxy_debug.go`：上游请求/响应调试捕获（含敏感头脱敏）
- **admin模块拆分**（SRP原则）：
  - `admin_channels.go`：渠道CRUD操作
  - `admin_stats.go`：统计分析API
  - `admin_cooldown.go`：冷却管理API
  - `admin_csv.go`：CSV导入导出
  - `admin_types.go`：管理API类型定义
  - `admin_auth_tokens.go`：API访问令牌CRUD（支持Token统计、费用限额、模型/渠道限制、并发限制）
  - `admin_settings.go`：系统设置管理
  - `admin_models.go`：模型列表管理
  - `admin_testing.go`：渠道测试功能（显式选择客户端请求协议）
  - `admin_debug_log.go`：调试日志API（敏感头脱敏+base64二进制编码）
  - `channel_check_scheduler.go`：渠道定时检测调度器
  - `detection_log.go`：检测日志构建（定时检测结果→LogEntry）
- **协议转换系统**：
  - `protocol/types.go`：四大协议定义（Anthropic/OpenAI/Gemini/Codex）
  - `protocol/registry.go`：请求、流式响应和非流式响应的契约边界；同协议请求不进入转换
  - `protocol/builtin/register.go`：注册全部 12 个跨协议有向组合
  - `protocol/builtin/cliproxy_adapter.go`：ccLoad 自有的输入验证、JSON/SSE 规范化与流帧封装
  - `protocol/cliproxy/`：仓库内维护的纯 [CLIProxyAPI](https://github.com/caidaoli/CLIProxyAPI) 四协议核心及 allowlist provider 请求/响应适配器快照边界；来源、同步规则和 provider 实际导入状态见 [`UPSTREAM.md`](../../internal/protocol/cliproxy/UPSTREAM.md)
  - 上游同步入口：Codex 调 `$sync-cliproxy-core`，Claude Code 调 `/sync-cliproxy-core`；一次原子操作固定一个 commit，同时同步核心和全部已登记 provider adapter
  - 无法表示为目标协议的请求返回 `400 Bad Request`，不会触发渠道故障切换或冷却
  - 自动检测仅在未提交响应的 HTTP 400、非模型 404/405、结构化 `convert_request_failed` + `not implemented` 500，或请求到达 API 前的 Cloudflare 403 拦截页后本地转换；未声明协议的 Exact URL 跨协议直接转换
- **冷却管理器**（DRY原则）：
  - `cooldown/manager.go`：统一冷却决策引擎
  - 消除重复代码，冷却逻辑统一管理
  - 区分网络错误和HTTP错误的分类策略
  - Key/模型/渠道使用独立动作；`ActionRetryModel` 不再尝试同渠道其他 Key 或 URL
  - 结构化 `model_cooldown`、上游 HTTP 5xx、Key 级 429 限流、模型不可用 404 和明确表示模型退役的 410 按 `(channel_id, 实际上游模型)` 持久化，同渠道其他模型仍可选
  - 所有配置模型或所有启用 Key 均冷却时，自动升级为渠道冷却
- **多URL选择器**（URLSelector）：
  - `url_selector.go`：单渠道多URL智能调度
  - 探索优先：未访问过的URL优先尝试，确保收集延迟数据
  - 加权随机：权重=1/EWMA延迟，延迟低的URL自动多分流
  - 独立冷却：故障URL指数退避，不影响同渠道其他URL
  - BaseURL追踪：活跃请求、日志和UI全链路携带上游URL
- **存储层**：
  - `storage/schema/`：统一Schema定义（支持 SQLite/MySQL/PostgreSQL 差异）
  - `storage/sql/`：SQLite、MySQL 和 PostgreSQL 共享的通用 SQL 实现层
  - `storage/factory.go`：工厂模式自动选择数据库
  - 复合索引优化，统计查询性能提升
- **OpenAI service_tier 定价**：
  - `util.OpenAIServiceTierMultiplier()`：返回 priority/flex/default 层级对应倍率
  - `LogEntry.ServiceTier`：持久化到数据库，日志成本列显示层级标注
  - 支持 GPT-5.4、GPT-5.4-pro 等最新模型定价
- **Responses image_generation 工具计费**：
  - 解析 Responses API 的 `tool_usage.image_gen` 与 `image_generation` 工具模型
  - `gpt-image-2` 按文本输入、图像输入、图像输出 token 分项计费
  - 流式/非流式代理链路与渠道测试共用同一 usage 解析器，避免费用口径漂移
- **分层定价（Tiered Pricing）**：
  - GPT-5.4：超过阈值 token 后输入价格自动降档
  - Qwen-Plus：超过阈值后触发低价区间
  - Gemini 长上下文：超过阈值后价格翻倍
  - 缓存折扣：Claude/Opus 独立乘数，OpenAI 缓存命中50%折扣

**多级缓存系统**:
- 渠道配置缓存（60秒TTL）- 减少数据库查询
- 轮询指针缓存（内存）- 毫秒级选择
- 渠道/Key 冷却内联在 `channels` / `api_keys`，模型冷却独立存入 `channel_model_cooldowns`
- 错误分类缓存（1000容量）- 重复错误秒判

**异步处理架构**:
- 日志系统（1000条缓冲 + 单worker，保证FIFO顺序）
- Token/日志清理（后台协程，定期维护）

**连接池优化**:
- SQLite: 内存模式10个连接/文件模式5个连接，5分钟生命周期
- HTTP客户端: 开启 keepalive；按启动时每渠道 2 条计算单 Transport 空闲容量（2–1024），每主机最多 20 条，空闲超时 90 秒
- TLS: 会话缓存（1024容量），减少握手耗时

## 数据库结构

数据库结构如下：

**存储架构（工厂模式）**:
```
storage/
├── store.go     # Store 接口（统一契约）
├── factory.go   # NewStore() 自动选择数据库
├── migrate*.go  # 启动时的表结构/列/数据迁移
├── hybrid_*.go  # 权威 SQLite + 异步主库复制
├── cache.go     # 渠道/API 密钥读缓存
├── schema/      # 表结构定义（DefineXxxTable）与 SQLite/MySQL/PostgreSQL 差异化 Schema 构建器
├── sql/         # 三种数据库共用的 SQL 实现：渠道配置、API 密钥、冷却、URL 状态、日志、
│                # 调试日志、指标统计、认证令牌及其统计、Web 会话、系统设置、
│                # OAuth 额度费用、事务、副本写入
└── sqlite/      # 仅 SQLite 专属测试
```

**数据库选择逻辑**:
- 设置 `CCLOAD_MYSQL` → 使用 MySQL 主库（与 `CCLOAD_POSTGRES` 同时设置时启动失败）
- 设置 `CCLOAD_POSTGRES` → 使用 PostgreSQL 主库
- 两者都未设置 → 使用 SQLite（默认）
- 主库 DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1` → 混合模式

**核心表结构**（SQLite / MySQL / PostgreSQL 共用）:
- `channels` - 渠道配置（渠道级冷却内联，UNIQUE 约束 name，含上游协议、定时检测配置、RPM/并发限制配置）
- `api_keys` - API 密钥（Key 级冷却内联，支持多 Key 策略与 `allowed_models` 模型白名单）
- `channel_models` - 渠道模型列表，含每个模型的重定向目标与禁用标记
- `channel_model_cooldowns` - 模型级运行时冷却，主键为渠道和实际上游模型
- `channel_url_states` - 多 URL 渠道的单 URL 启用/禁用状态，主键为渠道和 URL 哈希
- `logs` - 请求日志（含base_url上游URL追踪）
- `debug_logs` - 调试日志（上游请求/响应原始数据，独立清理策略）
- `auth_tokens` - 认证令牌（支持费用限额、模型/渠道限制、并发限制、首字节时间记录）
- `web_sessions` - 可绑定 API Token 的角色化 Web 会话
- `system_settings` - 系统配置（数据库存储，保存后自动重启生效）

**架构特性**:
- ✅ **统一SQL层**：SQLite、MySQL 和 PostgreSQL 共享 `storage/sql/` 实现
- ✅ **统一Schema定义**：`storage/schema/`定义表结构，支持数据库差异
- ✅ 工厂模式统一接口（OCP 原则，易扩展新存储）
- ✅ 渠道/Key 冷却数据内联；模型冷却独立存储，避免单模型不可用时冷却整个渠道
- ✅ 渠道选择与 Key 查找索引
- ✅ 复合索引优化（统计查询性能提升）
- ✅ 外键约束（级联删除，保证数据一致性）
- ✅ 多 Key 支持（sequential/round_robin 策略）
- ✅ 自动迁移（启动时自动创建/更新表结构）
- ✅ Token统计增强（支持时间范围选择、按令牌ID分类、缓存优化）
- ✅ **service_tier 成本计量**：日志持久化 service_tier 字段，成本列展示层级提示
- ✅ **Responses 图像工具成本计量**：`image_generation` 工具调用费用并入日志、统计和限额口径
- ✅ **分层定价引擎**：GPT-5.4/Qwen-Plus/Gemini 长上下文阶梯计价
- ✅ **日志体验优化**：成本格式化精度提升（3位小数/空值空串），IP列悬停显示完整地址
- ✅ **协议转换系统**：Anthropic/OpenAI/Gemini/Codex 四协议互转，支持 auto/upstream/local 三种模式
- ✅ **调试日志**：上游请求/响应原始数据捕获，敏感头脱敏，独立清理策略
- ✅ **渠道定时检测**：后台定时探测渠道可用性，支持指定检测模型
- ✅ **渠道RPM限制**：每渠道滚动60秒请求数上限，`0` 表示无限制，超限自动跳过该渠道
- ✅ **渠道并发限制**：每渠道同时在飞请求数上限，`0` 表示无限制，超限自动跳过该渠道
- ✅ **Key 模型白名单**：`api_keys.allowed_models` 限定每个 Key 可服务的渠道模型，空表示不限制；渠道内所有 Key 都不服务该模型时跳过该渠道

**向后兼容迁移**:
- 自动检测并修复重复渠道名称
- 智能添加 UNIQUE 约束，确保数据完整性
- 启动时自动执行，无需手动干预
- 日志数据库已合并到主数据库（单一数据源）
