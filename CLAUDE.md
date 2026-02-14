# CLAUDE.md

## 构建与测试

```bash
# 构建(必须 -tags go_json，注入版本号用于静态资源缓存)
go build -tags go_json -ldflags "\
  -X ccLoad/internal/version.Version=$(git describe --tags --always) \
  -X ccLoad/internal/version.Commit=$(git rev-parse --short HEAD) \
  -X 'ccLoad/internal/version.BuildTime=$(date '+%Y-%m-%d %H:%M:%S %z')' \
  -X ccLoad/internal/version.BuiltBy=$(whoami)" -o ccload .

# 测试(必须 -tags go_json)
go test -tags go_json ./internal/... -v
go test -tags go_json -race ./internal/...  # 竞态检测

# 开发运行(版本号为dev)
go run -tags go_json .
```
运行所需环境变量定义在.env文件中
## 核心架构

```
internal/
├── app/           # HTTP层+业务逻辑
│   ├── proxy_*.go       # 代理（handler/forward/stream/gemini/sse_parser/error）
│   ├── admin_*.go       # 管理API
│   ├── selector*.go     # 渠道选择（balancer/cooldown/model_matcher）
│   ├── *_cache.go       # 缓存（cost/health/stats）
│   ├── *_service.go     # 服务层（auth/config/log）
│   ├── key_selector.go  # Key负载均衡
│   ├── smooth_weighted_rr.go  # 平滑加权轮询实现
│   ├── request_context.go     # 请求上下文与超时控制
│   ├── token_counter.go       # Token计数（Anthropic count-tokens）
│   └── active_requests.go     # 活跃请求追踪
├── model/         # 数据模型（auth_token/config/log/stats）
├── cooldown/      # 冷却决策引擎
├── storage/       # 存储层
│   ├── factory.go       # 存储工厂（SQLite/MySQL/混合）
│   ├── store.go         # 统一存储接口
│   ├── hybrid_store.go  # 混合存储实现
│   ├── cache.go         # 渠道/Key缓存
│   ├── migrate.go       # Schema迁移
│   ├── sync_manager.go  # 启动数据恢复
│   ├── schema/          # Schema定义与构建器
│   ├── sqlite/          # SQLite特定实现
│   └── sql/             # SQL实现
├── util/          # 工具库（classifier/cost_calculator/money/rate_limiter/models_fetcher/channel_types）
├── version/       # 版本信息、启动banner、版本检查
├── config/        # 配置加载
└── testutil/      # 测试辅助
```

**故障切换策略**:
- Key级错误(401/403/429) → 重试同渠道其他Key
- 渠道级错误(5xx/520/524) → 切换到其他渠道
- 404/405（非明确客户端语义）→ 视为渠道级错误并切换渠道（常见于BaseURL/endpoint配置问题）
- 客户端错误（如406/413，或404且响应体明确`model_not_found`）→ 不重试,直接返回
- **软错误检测(597)**: 识别200状态码但响应体为错误的情况 → 渠道级冷却
- **1308配额错误(596)**: 专用处理,不计入渠道健康度 → Key级冷却
- **渠道每日成本限额**: 达到`daily_cost_limit`自动跳过该渠道
- 指数退避: 2min → 4min → 8min → 30min(上限)

**渠道选择算法**:
- **平滑加权轮询**: 替换加权随机，按有效Key数量分配流量，更均匀
- **冷却感知**: 实时排除冷却中的Key，权重反映实际可用容量
- **成本限额检查**: 优先于冷却检查，达到限额的渠道被排除

**关键入口**:
- `cooldown.Manager.HandleError()` - 冷却决策引擎
- `util.ClassifyHTTPStatus()` - HTTP错误分类器
- `util.ClassifyHTTPResponseWithMeta()` - 带响应体的错误分类（返回完整元数据）
- `app.KeySelector.SelectAvailableKey()` - Key负载均衡
- `app.SmoothWeightedRR.SelectWithCooldown()` - 平滑加权轮询选择

**Token费用限额（Auth Token）**:
- 存储：`auth_tokens.cost_used_microusd/cost_limit_microusd`（微美元整数），避免浮点误差
- 语义：在请求开始处做限额检查；费用在请求结束后记账，因此允许"最多超额一个请求"的窗口
- 计费：仅成功请求（2xx）累加费用与Token统计；失败请求只计失败次数
- **模型限制**：`auth_tokens.allowed_models`（逗号分隔），空值表示无限制
- **首字节时间**：`auth_tokens.first_byte_time_ms`（毫秒），记录流式请求TTFB

**渠道每日成本限额**:
- 存储：`channels.daily_cost_limit`（美元），0表示无限制
- 缓存：`CostCache`组件在内存中缓存当日成本，按天自动重置
- 启动加载：从数据库加载当日已消耗成本

**混合存储模式**（HuggingFace Spaces 场景）:
- 三种模式：纯SQLite（默认）/ 纯MySQL / 混合（MySQL主+SQLite缓存）
- 启用：`CCLOAD_MYSQL` + `CCLOAD_ENABLE_SQLITE_REPLICA=1`
- 日志恢复天数：`CCLOAD_SQLITE_LOG_DAYS`（默认7天，-1=全量，0=不恢复）
- 核心组件：
  - `HybridStore`: MySQL主存储 + SQLite本地缓存（读加速）
  - `SyncManager`: 启动时从MySQL恢复数据到SQLite
  - `StatsCache`: 统计结果缓存（TTL: 30秒~2小时）
- 数据流：
  - 写操作：先写MySQL（主），成功后同步到SQLite（缓存）
  - 读操作：从SQLite读取（本地缓存，低延迟）
  - 日志特殊：先写SQLite（快），再异步同步到MySQL（备份）

## 开发指南

### Serena MCP 工具

Serena 优先于内置工具。按需获取，不读整文件。

**读取**: `get_symbols_overview` → `find_symbol(include_body=True)`
- 始终传 `relative_path` 限制范围
- 符号名不确定时先 `search_for_pattern`

**符号路径**: `Struct/Method` (Go), `/pkg/func` (绝对), `Method[0]` (重载)

**编辑**:
- 整函数/方法: `replace_symbol_body`
- 几行代码: `Edit` 工具
- 编辑前 `find_referencing_symbols` 检查影响

### Playwright MCP 工具策略

- 截图**必须** JPEG: `type: "jpeg"`
- 优先 `browser_snapshot`（文本），视觉验证才截图
- **避免** `fullPage: true`

### 添加 Admin API
1. `admin_types.go` - 定义类型
2. `admin_<feature>.go` - 实现Handler
3. `server.go:SetupRoutes()` - 注册路由

### 数据库操作
- Schema更新: `storage/migrate.go` 启动自动执行
- 事务: `(*SQLStore).WithTransaction(ctx, func(tx) error)`
- 缓存失效: `InvalidateChannelListCache()` / `InvalidateAPIKeysCache()`

## 代码规范

- **必须** `-tags go_json` 构建和测试
- **必须** `any` 替代 `interface{}`
- **禁止** 过度工程，YAGNI原则
- **Fail-Fast**: 配置错误直接 `log.Fatal()` 退出
- **Context**: `defer cancel()` 必须无条件调用，用 `context.AfterFunc` 监听取消

### 代码质量检查 (golangci-lint)

```bash
# 运行 lint 检查
golangci-lint run ./...

# 仅检查指定目录
golangci-lint run ./internal/app/...

# 自动修复可修复的问题
golangci-lint run --fix ./...
```

**启用的 Linters**:
- `errcheck` - 检查未处理的错误返回值
- `govet` - Go 官方静态分析工具
- `staticcheck` - 包含 gosimple 的静态逻辑检查
- `unused` - 检查未使用的代码
- `gosec` - 安全漏洞审计
- `revive` - 代码风格检查
- `bodyclose` - HTTP response body 关闭检查

**提交前必须**: `golangci-lint run ./...` 通过，零警告
