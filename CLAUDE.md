# CLAUDE.md

> ccLoad:Claude/OpenAI/Gemini/Codex 多协议 API 网关(渠道/Key/URL 选择 + 故障切换 + 协议转换 + 成本计量)。
> 本文件是 AI 操作手册——只记命令、硬约束、反直觉机制与入口;展开细节读对应代码。

## 命令

必须 `-tags sonic`;环境变量见 `.env`。

```bash
make build          # 构建(注入版本号+strip)
make dev            # 开发运行
bash .agents/skills/sync-cliproxy-core/scripts/verify.sh --tests # 协议快照审计+定向测试
go test -tags sonic ./internal/...
make race-fast      # 高价值 race 子集
make race           # 全量 race(可用 RACE_P/RACE_PARALLEL 调并行度)
make verify-web     # 前端验证(含 node:test)
golangci-lint run ./...   # 提交前必须零警告
```

## 代码规范(硬约束)

- 必须 `-tags sonic`;用 `any`,不用 `interface{}`
- YAGNI,拒绝过度工程;Fail-Fast:配置错误 `log.Fatal()` 退出
- Context:`defer cancel()` 无条件调用,用 `context.AfterFunc` 监听取消
- lint 启用 errcheck/govet/staticcheck/unused/revive/bodyclose(gosec 已禁)

## 架构与入口

```
internal/app/        HTTP+业务:proxy_* / admin_* / selector_* / *_cache / *_service
internal/protocol/   协议契约与注册;builtin/ 是 ccLoad 适配层;cliproxy/ 是上游转换核心快照
internal/storage/    存储(factory/hybrid_store/sync_manager/migrate;sql/ sqlite/)
internal/cooldown/   冷却决策   internal/util/  classifier/cost_calculator/money/...
internal/{model,config,version,testutil}/   web/  前端(HTML+assets/{css,js,locales})
```

| 任务 | 入口 |
|------|------|
| 代理主链路 | `proxy_handler.go:HandleProxyRequest` → `runProxyAttemptLoop` → `proxy_forward.go` → `proxy_stream.go` |
| 渠道/Key/URL 选择 | `selector*.go`、`key_selector.go`、`smooth_weighted_rr.go`、`url_selector.go` |
| 错误分类/冷却 | `util/classifier.go`、`cooldown/manager.go` |
| 协议转换 | `protocol/registry.go` → `protocol/builtin/register.go` → `protocol/builtin/cliproxy_adapter.go`;核心实现/同步规则见 `protocol/cliproxy/{UPSTREAM.md,...}` |
| 定价/成本 | `util/cost_calculator.go` |
| 加 Admin API | `admin_types.go` 定类型 → `admin_<feature>.go` 实现 → `server.go:SetupRoutes` 注册 |
| 数据库 | Schema 启动自动 `migrate.go`;事务 `(*SQLStore).WithTransaction`;改后失效 `InvalidateChannelListCache`/`InvalidateAPIKeysCache` |

## 故障切换(`util/classifier.go`)

- Key 级(401/403/429)→ 重试同渠道其他 Key
- 渠道级(5xx/520/524,404/405 无客户端语义)→ 切渠道
- 客户端错误(406/413,404+`model_not_found`)→ 直接返回,不重试
- 成本限额达到 → 跳过该渠道
- 指数退避:2 → 4 → 8 → 30 min

## 自定义状态码(改相关代码前先读语义)

- **499** 客户端取消:不计失败、不冷却
- **596** 1308 配额超限 → Key 级冷却,不计健康度
- **597** SSE error(HTTP 200+错误体)→ `classifySSEError` 按 error.type 动态判级
- **598** 首字节超时 → 渠道级;**599** 流式中断 → 渠道级
- **429** 统计页/健康时间线计入 ErrorCount 与成功率,`rate_limited` 是 ErrorCount 子集;健康度排序(`GetChannelSuccessRates`/effective priority)排除 429,真实渠道级限流交给冷却过滤

## 关键机制(要点,细节读对应文件)

- **选择**:渠道平滑加权轮询(按有效 Key 数)+ 冷却感知,成本限额检查优先于冷却;多 URL 探索优先→1/EWMA 加权随机,失败 URL 独立退避;渠道 URL 末尾 `#`(`ExactUpstreamURLMarker`)= 精确转发,不自动追加路径
- **协议转换**:四协议 12 个有向转换对,覆盖请求、流式响应、非流式响应;`upstream`(原生)/`local`(本地翻译)两模式;渠道配 `ProtocolTransformMode`+`ProtocolTransforms`
- **自定义请求规则**(`custom_rules.go`):`channels.custom_request_rules` JSON;header remove/override/append、body remove/override(点分路径);`validateCustomRequestRules` 强制认证头黑名单 + 禁 CRLF
- **上游超时**(`server.go:loadChannelTypeTimeouts`):`upstream_first_byte_timeout`(0=禁用,仅流式)、`non_stream_timeout`(120s),按渠道类型 `{type}_*` 覆盖;写回前调 `disableResponseWriteTimeout` 防 `WriteTimeout` 截断响应体
- **Anthropic thinking**:项目生成的 Anthropic 请求用 `thinking.type=adaptive` + `output_config.effort`;anyrouter `/v1/messages` 兜底补 adaptive 并归一旧 `enabled`;anyrouter 额外注入 `anthropic-beta: context-1m`
- **定时检测**(`channel_check_scheduler.go`):全局 `channel_check_interval_hours`(0=禁用,热重载)+ 渠道级开关

## 协议转换核心(改前必读)

- 同步/审查转换核心必须使用仓库 Skill：Codex 调 `$sync-cliproxy-core`，Claude Code 调 `/sync-cliproxy-core`；唯一源码在 `.agents/skills/`，`.claude/skills/` 只放发现链接
- `protocol/registry.go` 是唯一契约/调度边界:同协议原样透传;跨协议只走 `builtin/register.go` 注册的 12 个有向转换对
- `builtin/cliproxy_adapter.go` 只处理 ccLoad 边界(输入验证、JSON/SSE 规范化、流帧封装);`protocol/cliproxy/` 只放从 CLIProxyAPI 同步的纯转换核心
- 不要把上游 auth/config/routing/cache/plugin/network 代码搬进来,也不要改成运行时 Go module 依赖;来源 commit、许可证和同步步骤以 `protocol/cliproxy/UPSTREAM.md` 为准
- `RequestTranslationError` 是客户端语义错误:代理返回 HTTP 400,不切渠道、不冷却;不要把无法表示的请求伪装成上游故障
- Registry 边界测试定义 ccLoad 线协议契约,上游同步测试守住转换行为;改协议后先跑命令区快照审计,再跑全量 `internal/...`

## 计费与限额

- **渠道倍率** `cost_multiplier`(≤0 归 1):× 标准成本 = `effective_cost`,写日志时快照到 `logs.cost_multiplier` 避免历史污染
- **Auth Token**:`cost_*_microusd`(微美元整数避浮点);仅 2xx 累加费用,失败只计次,允许「超额一个请求」;`CCLOAD_API_TOKENS` 启动预置
- **渠道每日限额** `daily_cost_limit`(美元,0=无限);`CostCache` 内存缓存按天重置
- **定价细节**(service_tier 倍率、GPT-5.4/Qwen-Plus 分层降档、Gemini 长上下文翻倍、缓存读折扣/写乘数):读 `cost_calculator.go`

## 存储

- 模式:纯 SQLite(默认)/ 纯 MySQL(`CCLOAD_MYSQL`)/ 纯 PostgreSQL(`CCLOAD_POSTGRES`)/ 混合(主库 DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1`)
- 互斥:`CCLOAD_MYSQL` 与 `CCLOAD_POSTGRES` 同时设置 → `log.Fatal`
- PG DSN:URL(`postgres://user:pass@host:5432/db?sslmode=disable`)或 libpq 关键字串;驱动 `pgx/stdlib`
- 混合数据流:写主库(MySQL/PG)→同步 SQLite,读 SQLite,日志先 SQLite 后异步主库;`CCLOAD_SQLITE_LOG_DAYS` 默认 7
- URL 禁用状态(`channel_url_states` 表)双写,重启 `URLSelector.LoadDisabled` 回填

## 前端(Playwright MCP)

截图必须 `type:"jpeg"`,优先 `browser_snapshot`(文本),避免 `fullPage:true`。
