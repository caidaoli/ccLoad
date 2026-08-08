# CLAUDE.md

> ccLoad:Claude/OpenAI/Gemini/Codex 多协议 API 网关(渠道/Key/URL 选择 + 故障切换 + 协议转换 + 成本计量)。
> 本文件是 AI 操作手册——只记命令、硬约束、反直觉机制与入口;展开细节读对应代码。

## 命令

必须 `-tags sonic`;环境变量见 `.env`。

```bash
make build          # 构建(注入版本号+strip)
make dev            # 开发运行
bash .agents/skills/sync-cliproxy-core/scripts/verify.sh --tests # 协议快照审计+定向测试
bash .agents/skills/ccload-release/scripts/release.sh --self-test # 发布脚本自检
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
internal/{model,config,version,testutil}/   web/  管理后台前端(HTML+assets/{css,js,locales})
www/                 独立介绍站(`make www-setup` 复制共享资源后可脱离仓库部署,别和 web/ 混淆)
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

Responses WebSocket execution identity：同 Token 下以 `Session-Id` 标识顶层会话；存在 `Thread-Id` 时组合两者，使 Codex 主代理/子代理 transcript、Response ID、turn lock 隔离；无 `Thread-Id` 时回退原 `Session-Id` 契约，禁止改用请求体 `session_id`、`prompt_cache_key` 或每回合变化的 request/turn/window ID。默认限制：下游连接全局 64、单 Token 16；上游每 45 秒发送 Ping，连续 5 分钟未收到任何帧/Pong 判定失活。下游全部断开满 5 分钟后由每分钟清理器关闭上游物理连接（实际约 5–6 分钟），稳定逻辑会话与已提交 transcript 在 `responses_ws_session_ttl_minutes` 到期前（新安装/重置默认 15 分钟，小内存机器可设 10；升级不改已有值）不会因容量/预算压力被逐出。`upstream_connection_reuse_limit_seconds`（默认 0，不限制）还可限制上游连接的复用时间；达到时限的空闲连接立即关闭，在途 turn 完成后再关闭，下一轮按需重连并重放完整 transcript，因为 Response ID 只在原物理 WebSocket 上有效。达到 `responses_ws_max_sessions` 只拒绝新会话身份；已提交 payload 超过 `responses_ws_max_transcript_bytes`（默认 128 MiB）后，所有新回合在触达上游前以 `429/rate_limit_error/rate_limit` 拒绝，已准入回合仍可提交，有限最坏超量为 `max_sessions × max_body_bytes`。`/admin/runtime-metrics` 的 `transcript_bytes` 只统计有效 payload，不是 Go 堆占用，并提供 `ttl_expired`、`capacity_rejected`、`budget_rejected`、`previous_response_misses` 进程累计计数。下一轮会优先原渠道/Key/URL 并按需重连。

## 故障切换(`util/classifier.go` + `cooldown/detection.go`)

- Key 级(401/403)→ 冷却当前 Key,重试同渠道其他 Key;所有启用 Key 均冷却时自动升级渠道冷却
- 模型级(`model_cooldown`,上游 HTTP 400/499/5xx/520/524/429,597 服务类 SSE 错误,598/599 流故障,连接重置/HTTP2 流关闭/空响应/网络超时,404 模型不可用,410 明确模型退役)→ 写入 `(channel_id, 实际上游模型)` 冷却;直接切渠道,不再尝试同渠道其他 Key/URL,不影响其他模型;所有配置模型均冷却时自动升级渠道冷却
- 渠道级(DNS/连接拒绝/网络或路由不可达)→ 切渠道
- 原生协议能力不支持(响应未提交的 HTTP 400、非模型 404/405,或结构化 500 明确返回 `convert_request_failed` + `not implemented`)→ 能力协商事件,不记失败日志、不冷却 Key/模型/渠道/URL;auto 模式可转换时同渠道/Key/URL 探测其他协议,不可转换时切 URL/渠道
- 客户端错误(406/413,404 非模型 `does not exist`)→ 直接返回,不重试
- 成本限额达到 → 跳过该渠道
- Key/模型/渠道共用指数退避策略:按错误类型取初始值(默认认证 5 min、服务端 2 min、超时/限流 1 min),随后翻倍并在 30 min 封顶;上游或自定义规则给出精确 reset 截止时间时优先使用
- **冷却探测规则**(`cooldown/detection.go`):渠道 `cooldown_detection_rules` 为空时继承系统设置 `global_cooldown_detection_rules`;按 rules 数组顺序(提交后重编号 0..N-1)匹配 status+正则,命名捕获组可解析出精确 reset 时间。网络故障故意不进这个匹配器(没有可信上游错误体);规则命中但不可执行时回退内置分类器,不猜冷却时长。`EvaluateCooldownDetectionRules` 无副作用,代理链路与 admin 规则测试端点共用
- **全冷却兜底**(`selector_cooldown.go`,`cooldown_fallback_enabled` 默认 true):所有渠道都冷却时不直接拒绝,而是挑「最早恢复」的渠道打 `CooldownFallback` 标记继续走正常流程,Key 也改选最早恢复的(`SelectCooldownFallbackKey`)。排查「明明全冷却了为什么还在发请求」先看这里;设 false 才直接拒绝
- Responses WebSocket 特例:首个语义输出前,非 WS→非 WS、原生 WS→非 WS/原生 WS 均可在网关内部切换,其中 WS→非 WS 使用 execution session 的完整 transcript;非 WS 故障且下一候选为原生 WS 时返回 `status=502` 的 `server_error/upstream_unavailable` 并用 close code `1011` 断开,让 Codex 客户端完整 replay。已有语义输出后禁止网关内部切换或重放;若成功响应流在终结事件前中断,返回 `status=502` 的 `server_error/upstream_stream_interrupted` 并用 `1011` 断开,让 Codex 客户端重连并完整 replay 当前 turn;已完成的工具调用先提交到 execution session,普通残缺文本不提交

## 自定义状态码(改相关代码前先读语义)

- **499** 客户端取消:不计失败、不冷却;上游直接返回 499:模型级冷却
- **596** 1308 配额超限 → Key 级冷却,不计健康度
- **597** SSE error(HTTP 200+错误体)→ `classifySSEError` 按 error.type 动态判级
- **598** 首字节超时 → 模型级;**599** 流式中断 → 模型级
- **`fwResult.StreamDiagMsg` 是 599 的判定开关,不只是日志字段**:非空即被 `forwardAttempt` 判为流不完整,置 599 并走模型级冷却。所以只有真实上游故障才允许写入,客户端断开必须先过 `isClientDisconnectError`(`buildStreamDiagnostics` 与 Codex 非流式收集器 `codex_wire.go` 各有一处),漏一处就会把 499 误升成 599。`markIncompleteStreamForwardResult` 不覆盖已经是 598 的状态码——两者冷却初值不同
- **429** 统计页/健康时间线计入 ErrorCount 与成功率,`rate_limited` 是 ErrorCount 子集;健康度排序(`GetChannelSuccessRates`/effective priority)排除 429,真实渠道级限流交给冷却过滤

## 关键机制(要点,细节读对应文件)

- **选择**:先冷却过滤(正确性优先),再二选一排序——`enable_health_score` 默认 **false** 走渠道平滑加权轮询(按有效 Key 数),开启才走健康度排序(`calculateEffectivePriority`:`P_eff = Priority - 失败惩罚 - TTFB惩罚`,两种惩罚各自按样本量打置信度折扣,TTFB 部分还要 `enable_ttfb_score` 单独开)。成本限额检查优先于冷却;模型冷却按每个渠道解析重定向/模糊匹配后的实际上游模型过滤;多 URL 探索优先→1/EWMA 加权随机,失败 URL 独立退避;`ChannelURL.Exact` 派生运行时 `#` 标记实现精确转发,持久化 URL 本身不含标记
- **路由候选是只读快照**(`cache.go:GetEnabledChannelsSnapshotByModel`):选择链路拿到的 `*model.Config` 归缓存所有,只有外层 slice 是请求私有的——可以过滤、排序、原地重排,但**禁止改 Config 字段**,要改先 `Clone()`(见 `selectAlphaSearchCandidates`);需要可变副本的路径继续用深拷贝的 `GetEnabledChannelsByModel`。`filterCooledChannels`/`selectByWeight`/`selectWithCooldownInPlace` 都原地压缩或重排入参,调用方只能用返回值,不能再按原长度复用入参
- **模型停用**(`ModelEntry.Disabled`):`disabled=true` 的模型对外完全不存在——`GetModels`/`modelIndex`/`FuzzyMatchModel`/`channelModelCooldownKeys` 一律跳过。刷新模型列表的 `replace` 模式会按原名、归一化别名、重定向目标三种键把停用标记传播回新拉取的条目,避免刷新一次就把停用状态洗掉
- **渠道级限流**(`channel_rpm_limiter.go`+`channel_concurrency_limiter.go`):`rpm_limit`/`max_concurrency` 都是 0=无限。注意 `max_concurrency` 这个名字在系统设置(全局信号量)、Auth Token、渠道三处各有一份,互不相干,改代码前先认准层级
- **多协议处理**:每个渠道默认接受四种客户端协议,`protocol_transform_mode` 选择策略:`auto`(默认)、`upstream`(只直通客户端协议)、`local`(只本地转换)。实际上游能力只由 `ChannelURL.Protocols` 声明:非空声明是权威配置,不兼容 URL 无请求、无冷却地跳过。local 优先有声明的 URL 并保持声明顺序;仅当全部 URL 未声明时按 Anthropic → Codex → OpenAI → Gemini 请求。auto 先试客户端协议,再按 OpenAI → Anthropic → Codex → Gemini 自动探测并跳过已试协议;未提交响应的 HTTP 400、非模型 404/405、明确未实现 500、请求到达 API 前的 Cloudflare 403 拦截页或当前转换无法表示请求时才继续下一协议。成功协议按 URL+请求族缓存到进程重启或渠道配置变更;全部协议不支持时 10 分钟后重新探测
- **自定义请求规则**(`custom_rules.go`):`channels.custom_request_rules` JSON;header remove/override/append、body remove/override(点分路径);`validateCustomRequestRules` 强制认证头黑名单 + 禁 CRLF
- **Codex 上游 Header 契约**(`codex_credentials.go`+`codex_upstream_websocket.go`):不走通用反代透传；HTTP 只接收 Codex 客户端白名单，静态 Key 与 OAuth 都只用 `Authorization: Bearer`，固定官方 `User-Agent`/`Originator`，认证与身份头在自定义规则后重建。原生 WebSocket 额外接收 turn state/timing/`OpenAI-Beta`，握手前删除 HTTP 传输头并归一 `OpenAI-Beta`、`Session_id`、`Conversation_id`；渠道自定义 Header 规则仍可显式增加非认证 Header
- **Codex 上游 TLS**(`codex_utls_transport.go`):普通 HTTP 请求仅对 `https://chatgpt.com` 使用 Chrome uTLS，按 HTTP/2 → uTLS HTTP/1.1 → 标准 HTTP/1.1 降级并保留请求体重放；其他 Host 继续走标准 Transport。uTLS 继承环境/渠道代理、Host 覆盖、证书校验和握手超时，连接池跟随 `upstream_connection_reuse_limit_seconds` 整代轮换；原生 WebSocket 保持独立 Dialer
- **OAuth 全局上游地址**:`CODEX_BASE_URL`(完整 Responses URL)、`XAI_BASE_URL`(API 根地址,通常以 `/v1` 结尾)、`ANTIGRAVITY_URL`(API 根地址)默认均为空；设置页分别以 `https://chatgpt.com/backend-api/codex/responses`、`https://cli-chat-proxy.grok.com/v1`、`https://daily-cloudcode-pa.googleapis.com` 显示默认渠道地址占位提示，Antigravity 另有 `https://cloudcode-pa.googleapis.com` 备用地址。非空时对应 OAuth 渠道只使用该全局地址并忽略渠道 URL，API Key 渠道不受影响。属于系统设置，修改后重启生效
- **系统设置无热重载**(`config_service.go`+`admin_settings.go`):`LoadDefaults` 启动读一次进内存,运行期只读;单改/重置/批量三个写入口都是写库后 `go triggerRestart()`,2 秒后重启进程生效。别在 `AdminUpdateSetting` 里加"顺手刷新缓存"——重启才是生效机制
- **引导期配置只能是环境变量**:`ConfigService` 依赖已建好的 `storage.Store`,所以建库阶段消费的配置不可能迁进系统设置(要读设置得先开库,要开库得先知道设置)。`SQLITE_PATH`/`SQLITE_JOURNAL_MODE`(拼 DSN,`factory.go:buildSQLiteDSN`)、`CCLOAD_MYSQL`/`CCLOAD_POSTGRES`/`CCLOAD_ENABLE_SQLITE_REPLICA`/`CCLOAD_SQLITE_LOG_DAYS`(`factory.go:NewStore`)全部属于这一类,保持环境变量;运行期策略才进系统设置
- **全局限额与冷却时长**(`server.go:loadServerRuntimeConfig`):均为系统设置,启动读一次,改后重启生效。`max_concurrency`(全局并发信号量,注意与 Auth Token、渠道的同名字段是三个独立层级)、`max_body_bytes`/`max_image_body_bytes`(Images 路径独立上限,同时约束 Responses WS 帧与 transcript,注入见 `newRequestBodyLimits`)、`cooldown_{auth,server,timeout,rate_limit,min,max}_seconds`(`loadCooldownSettings` 读出 `util.CooldownSettings`,经 `Store.ConfigureCooldown` 注入;下限>上限时整对回退默认)。旧 `CCLOAD_MAX_CONCURRENCY`/`CCLOAD_MAX_BODY_BYTES`/`CCLOAD_COOLDOWN_*` 已废弃,仍设置时启动打 WARN
- **上游超时**(`server.go:loadProtocolTimeouts`):`upstream_first_byte_timeout`(0=禁用,仅流式)、`stream_timeout`(0=禁用,流式总时长)、`non_stream_timeout`(120s),首字节与非流式超时可按实际上游协议 `{protocol}_*` 覆盖;写回前调 `disableResponseWriteTimeout` 防 `WriteTimeout` 截断响应体
- **上游连接最长复用时间**(`upstream_connection_age.go`+`codex_upstream_websocket.go`):`upstream_connection_reuse_limit_seconds`(默认 0=不限制)统一约束直连及渠道代理池中的 HTTP/1.1、HTTP/2、WebSocket 物理连接；达到时限后不再接收新请求，空闲连接立即关闭，在途请求/turn 完成后关闭，新请求自动建连。原生 WS 新物理连接必须用 execution session 完整 transcript 重放，不能携带旧 socket 的 `previous_response_id`；计划轮换不记失败、不触发冷却
- **Anthropic thinking**:项目生成的 Anthropic 请求用 `thinking.type=adaptive` + `output_config.effort`;anyrouter `/v1/messages` 兜底补 adaptive 并归一旧 `enabled`;anyrouter 额外注入 `anthropic-beta: context-1m`
- **定时检测**(`channel_check_scheduler.go`):全局 `channel_check_interval_hours`(0=禁用,启动读一次,改后重启生效)+ 渠道级开关

## 发布与更新

- 发布必须使用仓库 Skill：Codex 调 `$ccload-release`，Claude Code 调 `/ccload-release`；唯一源码在 `.agents/skills/ccload-release/`，`.claude/skills/ccload-release` 只是软链接
- 无参数默认 Beta；只有显式 `stable` 才发稳定版。Tag 只允许 `vX.Y.Z-beta.N` / `vX.Y.Z`
- `.github/workflows/test.yml` 是提交级唯一发布门禁：`master` 的完整 SHA 必须通过后端测试、Web 验证、构建、lint 和 PostgreSQL 集成测试，发布脚本才允许打 Tag。`.github/workflows/release.yml` 只校验 Tag、构建多平台产物并生成 Release 和 GHCR 镜像；Beta=`prerelease=true` 且不改 GitHub latest，镜像发布精确版本 Tag+`beta`；稳定版更新 GitHub latest，镜像发布精确版本 Tag+`latest`
- 官方容器直接打包同一 Release 的 Linux 二进制；`CCLOAD_CONTAINER=1` 时不启动版本检查或进程内更新，`auto_update_*` 设置只读；稳定版/测试版分别通过 `latest`/`beta` 镜像标签切换
- 非容器部署的单一更新管理器同时负责前端版本提示和可选自动应用；默认 `auto_update_channel=stable`，`preview` 同时考虑稳定版/测试版并按 SemVer 取最高版本；`auto_update_interval_hours=0` 关闭全部版本检查

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
