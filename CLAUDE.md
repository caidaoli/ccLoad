# CLAUDE.md

## 构建与测试

必须 `-tags go_json`。环境变量见 `.env`。

```bash
make build          # 构建（自动注入版本号+strip）
make web-test       # 前端 node:test
make verify-web     # 前端验证（含 web-test）
make dev            # 开发运行

go build -tags go_json -o ccload .
go test -tags go_json ./internal/... -v
go test -tags go_json -race ./internal/...
```

## 架构概览

```
internal/
├── app/           # HTTP 层 + 业务（proxy_*、admin_*、selector_*、url_selector、*_cache、*_service）
├── protocol/      # 协议转换（Anthropic/OpenAI/Gemini/Codex 互转，内置在 builtin/）
├── model/         # 数据模型
├── cooldown/      # 冷却决策引擎
├── storage/       # 存储层（factory/hybrid_store/schema/sql/sqlite/migrate）
├── util/          # 工具（classifier/cost_calculator/money/rate_limiter/uuid_local/...）
├── version/       # 版本信息
├── config/        # 配置与默认常量（defaults.go）
└── testutil/      # 测试辅助
web/               # 前端（HTML + assets/{css,js,locales}）
```

## 故障切换策略

- Key 级（401/403/429）→ 重试同渠道其他 Key
- 渠道级（5xx/520/524，以及 404/405 无明确客户端语义）→ 切换渠道
- 客户端错误（406/413，或 404 + `model_not_found`）→ 不重试直接返回
- 每日成本限额达到 → 跳过该渠道
- 指数退避：2min → 4min → 8min → 30min

## 自定义状态码（`util/classifier.go`）

- **499** 客户端取消，不计失败、不冷却
- **596** 1308 配额超限 → Key 级冷却，不计健康度
- **597** SSE error 事件（HTTP 200 + 错误体）→ `classifySSEError()` 按 error.type 动态判定（api_error/overloaded_error → 渠道级）
- **598** 上游首字节超时 → 渠道级
- **599** 流式响应中断 → 渠道级

## 渠道/Key/URL 选择

- **渠道**：平滑加权轮询（按有效 Key 数分配流量）；冷却感知；成本限额检查优先于冷却
- **多 URL**：探索优先 → 1/EWMA 延迟加权随机；失败 URL 独立指数退避冷却；BaseURL 全链路追踪（活跃请求/日志/UI）

## 协议转换（`internal/protocol/`）

- 四协议：Anthropic / OpenAI / Gemini / Codex
- 请求族：chat_completions / responses / messages / generate_content / completions / embeddings / images
- 两模式：`upstream`（默认，上游原生）/ `local`（本地翻译）
- `Registry` 注册 请求/流式响应/非流式响应 三类转换器
- 渠道配置：`ProtocolTransformMode` + `ProtocolTransforms`

## anyrouter 特殊处理

渠道名含 `anyrouter` 且是 Anthropic 类型：
- 注入 `anthropic-beta: context-1m-2025-08-07`（`injectAnthropicBetaFlag`）
- `/v1/messages` 且 body 无 `thinking`：注入 `thinking.type=adaptive`（`maybeInjectAnyrouterAdaptiveThinking`），代理链路与 `admin_testing.go` 测试接口同步启用

## 自定义请求规则（`custom_rules.go`）

- 存储：`channels.custom_request_rules` JSON = `CustomRequestRules{Headers[], Body[]}`
- **Header**：`remove`（多值头按 token 精确剔除）/ `override`（`Set`）/ `append`（`Add`）
- **JSON Body**：`remove`（点分路径删除 key/数组元素）/ `override`（按路径写值，自动创建中间节点）
- 路径语法：`thinking.budget_tokens`、`messages.0.role`
- **安全**（`validateCustomRequestRules`）：认证头黑名单（`Authorization`/`x-api-key`/`x-goog-api-key`）静默 + `slog.Warn`；禁 CRLF；非 JSON body 静默跳过；单渠道 header/body 各 ≤ 32 条、单条 value ≤ 8 KB
- **顺序**（`proxy_forward.go`）：body 规则先应用；header 规则在 anyrouter beta flag 注入之后，可覆盖/移除 beta flag

## 调试日志

- 捕获：`proxy_debug.go:captureDebugRequest`（脱敏敏感头）
- API：`admin_debug_log.go:HandleGetDebugLog`（base64 二进制）
- 独立清理：`DebugLogCleanupInterval=2min`，不受普通日志保留天数限制

## 渠道定时检测

- 调度：`channel_check_scheduler.go:startScheduledChannelCheckLoop`
- 配置：全局 `channel_check_interval_hours`（0=禁用，热重载）；渠道 `scheduled_check_enabled`/`scheduled_check_model`

## Auth Token 费用限额

- 存 `cost_used_microusd`/`cost_limit_microusd`（微美元整数，避浮点误差）
- 请求开始查限额、结束后记账 → 允许「最多超额一个请求」
- 仅 2xx 累加 Token/费用；失败只计次
- 字段：`allowed_models`（逗号分隔，空=无限制）、`first_byte_time_ms`、`PeakRPM`/`AvgRPM`/`RecentRPM`（`GetAuthTokenStatsInRange` 支持时间范围）

## 渠道每日成本限额

- `channels.daily_cost_limit`（美元，0=无限制）
- `channels.cost_multiplier`（默认 1，0=免费，负数回退 1）：渠道级倍率，限额按 **倍率后成本**（`cost × multiplier`）累加
- `CostCache` 内存缓存当日成本，按天自动重置，启动从数据库加载

## 混合存储（HuggingFace Spaces）

- 模式：纯 SQLite（默认）/ 纯 MySQL / 混合（`CCLOAD_MYSQL` + `CCLOAD_ENABLE_SQLITE_REPLICA=1`）
- 日志恢复：`CCLOAD_SQLITE_LOG_DAYS`（默认 7，-1=全量，0=不恢复）
- 数据流：写 MySQL 主→同步 SQLite 缓存；读 SQLite（低延迟）；日志先 SQLite 后异步 MySQL
- `StatsCache` TTL 30s~2h

## 定价

- **渠道倍率**：`channels.cost_multiplier` × 标准成本 = `effective_cost`；写日志时快照到 `logs.cost_multiplier`，避免渠道倍率变更污染历史；统计查询同时返回 `total_cost`（标准）与 `effective_cost`（倍率后）；`normalizeCostMultiplier` 兜底 ≤0→1
- **OpenAI service_tier**：`priority`/`flex`/`default` 倍率（`OpenAIServiceTierMultiplier`）；`LogEntry.ServiceTier` 持久化
- **分层定价**：GPT-5.4（`gpt54TierThreshold`）、Qwen-Plus（`qwenPlusTierThreshold`）超阈值降档；Gemini 长上下文（`geminiLongContextThreshold`）超阈值翻倍
- **缓存**：读折扣（Claude/Opus 单独乘数，OpenAI 50%）；写 5m×1.25 / 1h×2.0（基于 input 价格）

## 开发指南

### 添加 Admin API

1. `admin_types.go` 定类型
2. `admin_<feature>.go` 实现 Handler
3. `server.go:SetupRoutes()` 注册路由

### 数据库

- Schema：`storage/migrate.go` 启动自动执行
- 事务：`(*SQLStore).WithTransaction(ctx, func(tx) error)`
- 缓存失效：`InvalidateChannelListCache()` / `InvalidateAPIKeysCache()`

### Playwright MCP

- 截图**必须** `type: "jpeg"`；优先 `browser_snapshot`（文本），视觉验证才截图
- **避免** `fullPage: true`

## 代码规范

- **必须** `-tags go_json`
- **必须** `any` 替代 `interface{}`
- **禁止** 过度工程，YAGNI
- **Fail-Fast**：配置错误 `log.Fatal()` 退出
- **Context**：`defer cancel()` 无条件调用，用 `context.AfterFunc` 监听取消

### golangci-lint

提交前必须 `golangci-lint run ./...` 通过零警告。
启用：`errcheck`/`govet`/`staticcheck`/`unused`/`revive`/`bodyclose`
（`gosec` 在 v2.11.1+go1.26.1 下挂死，已禁用）
