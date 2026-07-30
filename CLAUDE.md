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
| Responses WebSocket | `proxy_responses_websocket.go:HandleResponsesWebsocket` → `executeResponsesWebsocketTurn` → `runProxyAttemptLoopWithFailureBoundary`;会话状态见 `responses_execution_session.go` |
| 渠道/Key/URL 选择 | `selector*.go`、`key_selector.go`、`smooth_weighted_rr.go`、`url_selector.go` |
| 错误分类/冷却 | `util/classifier.go`、`cooldown/manager.go` |
| 协议转换 | `protocol/registry.go` → `protocol/builtin/register.go` → `protocol/builtin/cliproxy_adapter.go`;核心实现/同步规则见 `protocol/cliproxy/{UPSTREAM.md,...}` |
| 定价/成本 | `util/cost_calculator.go` |
| 加 Admin API | `admin_types.go` 定类型 → `admin_<feature>.go` 实现 → `server.go:SetupRoutes` 注册 |
| 数据库 | Schema 启动自动 `migrate.go`;事务 `(*SQLStore).WithTransaction`;改后失效 `InvalidateChannelListCache`/`InvalidateAPIKeysCache` |

Responses WebSocket execution identity：同 Token 下以 `Session-Id` 标识顶层会话；存在 `Thread-Id` 时组合两者，使 Codex 主代理/子代理 transcript、Response ID、turn lock 隔离；无 `Thread-Id` 时回退原 `Session-Id` 契约，禁止改用请求体 `session_id`、`prompt_cache_key` 或每回合变化的 request/turn/window ID。默认限制：下游连接全局 64、单 Token 16；上游每 45 秒发送 Ping，连续 5 分钟未收到任何帧/Pong 判定失活。下游全部断开满 5 分钟后由每分钟清理器关闭上游物理连接（实际约 5–6 分钟），稳定逻辑会话与已提交 transcript 在 `responses_ws_session_ttl_minutes` 到期前（新安装/重置默认 15 分钟，小内存机器可设 10；升级不改已有值）不会因容量/预算压力被逐出。达到 `responses_ws_max_sessions` 只拒绝新会话身份；已提交 payload 超过 `responses_ws_max_transcript_bytes`（默认 128 MiB）后，所有新回合在触达上游前以 `429/rate_limit_error/rate_limit` 拒绝，已准入回合仍可提交，有限最坏超量为 `max_sessions × max_body_bytes`。`/admin/runtime-metrics` 的 `transcript_bytes` 只统计有效 payload，不是 Go 堆占用，并提供 `ttl_expired`、`capacity_rejected`、`budget_rejected`、`previous_response_misses` 进程累计计数。下一轮会优先原渠道/Key/URL 并按需重连。

## 故障切换(`util/classifier.go`)

- Key 级(401/403)→ 冷却当前 Key,重试同渠道其他 Key;所有启用 Key 均冷却时自动升级渠道冷却
- 模型级(`model_cooldown`,上游 HTTP 400/499/5xx/520/524/429,597 服务类 SSE 错误,598/599 流故障,连接重置/HTTP2 流关闭/空响应/网络超时,404 模型不可用)→ 写入 `(channel_id, 实际上游模型)` 冷却;直接切渠道,不再尝试同渠道其他 Key/URL,不影响其他模型;所有配置模型均冷却时自动升级渠道冷却
- 渠道级(DNS/连接拒绝/网络或路由不可达)→ 切渠道
- 原生协议能力不支持(响应未提交的非模型 404/405,或结构化 500 明确返回 `convert_request_failed` + `not implemented`)→ 能力协商事件,不记失败日志、不冷却 Key/模型/渠道/URL;可转换时同渠道/Key/URL 转到 `ChannelType` 重试,不可转换时切 URL/渠道
- 客户端错误(406/413,404 非模型 `does not exist`)→ 直接返回,不重试
- 成本限额达到 → 跳过该渠道
- Key/渠道级默认指数退避:2 → 4 → 8 → 30 min;模型级优先使用上游 reset 截止时间,缺失时固定 5 min
- Responses WebSocket 特例(仅首个语义输出前):非 WS→非 WS、原生 WS→非 WS/原生 WS 均在网关内部切换,其中 WS→非 WS 使用 execution session 的完整 transcript;非 WS 故障且下一候选为原生 WS 时返回 `status=502` 的 `server_error/upstream_unavailable` 并用 close code `1011` 断开,让 Codex 客户端完整 replay;已有语义输出后一律不切换或重放

## 自定义状态码(改相关代码前先读语义)

- **499** 客户端取消:不计失败、不冷却;上游直接返回 499:模型级冷却
- **596** 1308 配额超限 → Key 级冷却,不计健康度
- **597** SSE error(HTTP 200+错误体)→ `classifySSEError` 按 error.type 动态判级
- **598** 首字节超时 → 模型级;**599** 流式中断 → 模型级
- **429** 统计页/健康时间线计入 ErrorCount 与成功率,`rate_limited` 是 ErrorCount 子集;健康度排序(`GetChannelSuccessRates`/effective priority)排除 429,真实渠道级限流交给冷却过滤

## 关键机制(要点,细节读对应文件)

- **选择**:渠道平滑加权轮询(按有效 Key 数)+ 渠道/Key/模型冷却感知,成本限额检查优先于冷却;模型冷却按每个渠道解析重定向/模糊匹配后的实际上游模型过滤;多 URL 探索优先→1/EWMA 加权随机,失败 URL 独立退避;`ChannelURL.Exact` 派生运行时 `#` 标记实现精确转发,持久化 URL 本身不含标记
- **多协议处理**:渠道用 `ChannelType` 定本地转换目标,用 `protocol_transform_mode` 选择策略:`auto`(默认)、`upstream`(只原生直通)、`local`(只本地转换)。每个 `ChannelURL.Protocols` 可显式声明端点线协议能力:非空声明是权威配置,直接选择兼容协议,不兼容 URL 无请求、无冷却地跳过;空声明按 Anthropic → OpenAI → Codex → Gemini 固定顺序协商。未提交响应的非模型 404/405、明确未实现 500、请求到达 API 前的 Cloudflare 403 拦截页或当前转换无法表示请求时才继续下一协议,成功协议按 URL+请求族缓存 10 分钟。auto 下未声明协议的 Exact URL 跨协议直接本地转换
- **自定义请求规则**(`custom_rules.go`):`channels.custom_request_rules` JSON;header remove/override/append、body remove/override(点分路径);`validateCustomRequestRules` 强制认证头黑名单 + 禁 CRLF
- **系统设置无热重载**(`config_service.go`+`admin_settings.go`):`LoadDefaults` 启动读一次进内存,运行期只读;单改/重置/批量三个写入口都是写库后 `go triggerRestart()`,2 秒后重启进程生效。别在 `AdminUpdateSetting` 里加"顺手刷新缓存"——重启才是生效机制
- **引导期配置只能是环境变量**:`ConfigService` 依赖已建好的 `storage.Store`,所以建库阶段消费的配置不可能迁进系统设置(要读设置得先开库,要开库得先知道设置)。`SQLITE_PATH`/`SQLITE_JOURNAL_MODE`(拼 DSN,`factory.go:buildSQLiteDSN`)、`CCLOAD_MYSQL`/`CCLOAD_POSTGRES`/`CCLOAD_ENABLE_SQLITE_REPLICA`/`CCLOAD_SQLITE_LOG_DAYS`(`factory.go:NewStore`)全部属于这一类,保持环境变量;运行期策略才进系统设置
- **全局限额与冷却时长**(`server.go:loadServerRuntimeConfig`):均为系统设置,启动读一次,改后重启生效。`max_concurrency`(全局并发信号量,注意与 Auth Token 同名字段不是一回事)、`max_body_bytes`/`max_image_body_bytes`(Images 路径独立上限,同时约束 Responses WS 帧与 transcript,注入见 `setMaxBodyBytesLimits`)、`cooldown_{auth,server,timeout,rate_limit,min,max}_seconds`(注入 `util.ApplyCooldownSettings`;下限>上限时整对回退默认)。旧 `CCLOAD_MAX_CONCURRENCY`/`CCLOAD_MAX_BODY_BYTES`/`CCLOAD_COOLDOWN_*` 已废弃,仍设置时启动打 WARN
- **上游超时**(`server.go:loadChannelTypeTimeouts`):`upstream_first_byte_timeout`(0=禁用,仅流式)、`stream_timeout`(0=禁用,流式总时长)、`non_stream_timeout`(120s),首字节与非流式超时可按渠道类型 `{type}_*` 覆盖;写回前调 `disableResponseWriteTimeout` 防 `WriteTimeout` 截断响应体
- **Anthropic thinking**:项目生成的 Anthropic 请求用 `thinking.type=adaptive` + `output_config.effort`;anyrouter `/v1/messages` 兜底补 adaptive 并归一旧 `enabled`;anyrouter 额外注入 `anthropic-beta: context-1m`
- **定时检测**(`channel_check_scheduler.go`):全局 `channel_check_interval_hours`(0=禁用,启动读一次,改后重启生效)+ 渠道级开关

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
- **Auth Token 访问控制**(`model/auth_token.go`、`auth_service.go`):`allowed_models` 模型白名单(空=无限制);`allowed_channel_ids`+`channel_restriction_mode`(`allow` 白名单/`deny` 黑名单,空 mode 视为 allow,空列表始终无限制),`ChannelRestriction.Allows` 封装极性,选择链路走 `FilterAllowedChannels`;`max_concurrency` 令牌级并发上限(0=无限),`acquireTokenConcurrencySlot` 获取槽位
- **渠道每日限额** `daily_cost_limit`(美元,0=无限);`CostCache` 内存缓存按天重置
- **定价细节**(service_tier 倍率、GPT-5.4/Qwen-Plus 分层降档、Gemini 长上下文翻倍、缓存读折扣/写乘数):读 `cost_calculator.go`

## 存储

- 存储相关配置全是引导期环境变量,不进系统设置(原因见"关键机制"引导期配置条)
- 模式:纯 SQLite(默认)/ 纯 MySQL(`CCLOAD_MYSQL`)/ 纯 PostgreSQL(`CCLOAD_POSTGRES`)/ 混合(主库 DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1`)
- 互斥:`CCLOAD_MYSQL` 与 `CCLOAD_POSTGRES` 同时设置 → `log.Fatal`
- PG DSN:URL(`postgres://user:pass@host:5432/db?sslmode=disable`)或 libpq 关键字串;驱动 `pgx/stdlib`
- 混合数据流:写主库(MySQL/PG)→同步 SQLite,读 SQLite,日志先 SQLite 后异步主库;`CCLOAD_SQLITE_LOG_DAYS` 默认 7
- 模型冷却状态:`channel_model_cooldowns(channel_id, model, cooldown_until)`;写主库后同步 SQLite,启动自动建表/恢复,渠道删除时级联清理
- URL 禁用状态(`channel_url_states` 表)双写,重启 `URLSelector.LoadDisabled` 回填

## 前端(Playwright MCP)

截图必须 `type:"jpeg"`,优先 `browser_snapshot`(文本),避免 `fullPage:true`。
