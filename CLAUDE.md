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

**核心原则**: Serena 工具优先于内置工具。资源高效、按需获取，避免读取不必要的内容。

**代码浏览策略**:
- **禁止**直接读取整文件，采用渐进式信息获取
- 工作流: `get_symbols_overview` → `find_symbol(depth=1)` → `find_symbol(include_body=True)`
- 符号定位不确定时: 先用 `search_for_pattern` 找候选，再用符号化工具
- 查找引用关系: `find_referencing_symbols`
- 限制搜索范围: 始终传 `relative_path` 参数缩小搜索目录

**符号路径 (name_path) 语法**:
- 简单名称: `method` - 匹配任意同名符号
- 相对路径: `class/method` - 匹配后缀
- 绝对路径: `/class/method` - 精确匹配
- 重载索引: `MyClass/method[0]` - 指定特定重载

**代码编辑**:
- 整符号替换: `replace_symbol_body`
- 文件尾部插入: `insert_after_symbol` (最后一个顶级符号)
- 文件头部插入: `insert_before_symbol` (第一个顶级符号)
- 小改动 (几行代码): 用 `Edit` 工具
- 编辑前必须用 `find_referencing_symbols` 检查影响范围

**辅助工具**:
- 目录结构: `list_dir`
- 文件查找: `find_file`
- 模式搜索: `search_for_pattern` (非代码文件或符号名未知时)
- 项目记忆: `read_memory` / `list_memories` (查阅架构文档等)

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
