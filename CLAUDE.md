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
export CCLOAD_PASS=test123  # 必填
go run -tags go_json .
```

## 核心架构

```
internal/
├── app/           # HTTP层+业务逻辑 (proxy_*.go, admin_*.go, selector.go, key_selector.go)
├── cooldown/      # 冷却决策引擎 (manager.go)
├── storage/sql/   # 数据持久层 (SQLite/MySQL统一实现)
├── validator/     # 渠道验证器
└── util/          # 工具库 (classifier.go错误分类, models_fetcher.go)
```

**故障切换策略**:
- Key级错误(401/403/429) → 重试同渠道其他Key
- 渠道级错误(5xx/520/524) → 切换到其他渠道
- 客户端错误(404/405) → 不重试,直接返回
- **软错误检测(597)**: 识别200状态码但响应体为错误的情况 → 渠道级冷却
- **1308配额错误(596)**: 专用处理,不计入渠道健康度 → Key级冷却
- 指数退避: 2min → 4min → 8min → 30min(上限)

**关键入口**:
- `cooldown.Manager.HandleError()` - 冷却决策引擎
- `util.ClassifyHTTPStatus()` - HTTP错误分类器
- `util.ClassifyHTTPStatusWithBody()` - 带响应体的错误分类（支持软错误检测）
- `app.KeySelector.SelectAvailableKey()` - Key负载均衡

## 开发指南

### Serena MCP 工具规范

**代码浏览**:
- 优先用符号化工具: `get_symbols_overview` → `find_symbol`
- **禁止**直接读取整文件，先了解结构
- 查找引用: `find_referencing_symbols`

**代码编辑**:
- 替换符号: `replace_symbol_body`
- 插入代码: `insert_after_symbol` / `insert_before_symbol`
- 小改动用 `Edit` 工具

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
