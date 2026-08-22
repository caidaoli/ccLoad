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

## 测试策略

- 迭代只跑受影响包:`go test -tags sonic ./internal/app -run 'TestXxx'`;提交前全量 `./internal/...`。Go 测试、`make verify-web`、`make build`、lint 相互独立,可并行
- 不用 `-count=1`,除非排查缓存或不稳定测试
- 积极 `t.Parallel()`:并行测试必须独立 Store/Server、随机监听端口、局部 mock,不共享可变 fixture;调用 `t.Setenv`/`t.Chdir`,或修改进程环境、工作目录、`http.DefaultTransport`、全局模型目录、包级 session/cache、全局 goroutine 计数的测试必须串行
- 优先并行化含 sleep、deadline、轮询、异步日志等待的高耗时测试;不为凑并行数改纯解析微测试
- 并行化前后同命令计时(`/usr/bin/time -p go test -tags sonic -count=1 ./internal/app`),收益属噪声就撤销;新增/调整并行测试后跑 `go test -race -tags sonic -count=1 -shuffle=on ./internal/app`

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

## Responses WebSocket 会话与资源

- **执行身份**:同 Token 下以 `Session-Id` 标识顶层会话;存在 `Thread-Id` 时组合两者,隔离 Codex 主/子代理的 transcript、Response ID、turn lock;无 `Thread-Id` 回退原 `Session-Id` 契约。禁止改用请求体 `session_id`、`prompt_cache_key` 或每回合变化的 request/turn/window ID
- **默认限制**(新安装):下游连接全局 128、单 Token 64;执行会话 256;transcript payload 总预算 256 MiB。所有 `responses_ws_*` 整数配置保存 `0` 用内建默认,负数非法;已有数据库记录不迁移
- **生命周期**:上游每 45s 发 Ping,连续 5 min 无帧/Pong 判失活;下游全断满 5 min 后由每分钟清理器关上游物理连接(实际约 5–6 min);稳定逻辑会话与已提交 transcript 在 `responses_ws_session_ttl_minutes`(默认 15,小内存机器可设 10)到期前不因容量/预算压力被逐出
- **超限语义**:达 `responses_ws_max_sessions` 只拒绝新会话身份;已提交 payload 超 `responses_ws_max_transcript_bytes` 后,所有新回合在触达上游前以 `429/rate_limit_error/rate_limit` 拒绝,已准入回合仍可提交,有限最坏超量 `max_sessions × max_body_bytes`
- **连接轮换**:达到 `upstream_connection_reuse_limit_seconds` 的空闲连接立即关闭,在途 turn 完成后再关;下一轮优先原渠道/Key/URL,按需重连并重放完整 transcript——Response ID 只在原物理 WebSocket 上有效
- **指标**:`/admin/runtime-metrics` 的 `transcript_bytes` 只统计有效 payload,不是 Go 堆占用;另有 `ttl_expired`/`capacity_rejected`/`budget_rejected`/`previous_response_misses` 进程累计计数

## 故障切换(`util/classifier.go` + `cooldown/detection.go`)

- Key 级(401/403)→ 冷却当前 Key,重试同渠道其他 Key;所有启用 Key 均冷却时自动升级渠道冷却
- 模型级(`model_cooldown`,上游 HTTP 400/499/5xx/520/524/429,597 服务类 SSE 错误,598/599 流故障,连接重置/HTTP2 流关闭/空响应/网络超时,404 模型不可用,410 明确模型退役)→ 写入 `(channel_id, 实际上游模型)` 冷却;直接切渠道,不再尝试同渠道其他 Key/URL,不影响其他模型;所有配置模型均冷却时自动升级渠道冷却
- 渠道级(DNS/连接拒绝/网络或路由不可达)→ 切渠道
- 原生协议能力不支持(响应未提交的 HTTP 400、非模型 404/405,或结构化 500 明确返回 `convert_request_failed` + `not implemented`)→ 能力协商事件,不记失败日志、不冷却 Key/模型/渠道/URL;auto 模式可转换时同渠道/Key/URL 探测其他协议,不可转换时切 URL/渠道
- 客户端错误(406/413,404 非模型 `does not exist`)→ 直接返回,不重试
- 成本限额达到 → 跳过该渠道
- Key/模型/渠道共用指数退避:按错误类型取初始值(默认认证 5 min、服务端 2 min、超时/限流 1 min),翻倍并 30 min 封顶;上游或自定义规则给出精确 reset 截止时间时优先使用
- **冷却探测规则**(`cooldown/detection.go`):渠道 `cooldown_detection_rules` 为空时继承系统设置 `global_cooldown_detection_rules`;按 rules 数组顺序(提交后重编号 0..N-1)匹配 status+正则,命名捕获组可解析精确 reset 时间。网络故障故意不进匹配器(没有可信上游错误体);规则命中但不可执行时回退内置分类器,不猜冷却时长。`EvaluateCooldownDetectionRules` 无副作用,代理链路与 admin 规则测试端点共用
- **全冷却兜底**(`selector_cooldown.go`,`cooldown_fallback_enabled` 默认 true):所有渠道都冷却时不直接拒绝,挑「最早恢复」渠道打 `CooldownFallback` 标记继续正常流程,Key 也改选最早恢复的(`SelectCooldownFallbackKey`)。排查「明明全冷却了为什么还在发请求」先看这里;设 false 才直接拒绝
- **OAuth 凭证终态拒绝**(`proxy_forward.go:disableTerminalOAuthCredential`+`admin_oauth_cleanup.go:oauthRefreshTokenRejected`):刷新被上游明确拒绝(Token 端点 401,或 400/403 且错误码为 `invalid_grant`/`invalid_token`/`invalid_refresh_token`/`refresh_token_expired`/`refresh_token_revoked`/`expired_token`,或静态 PAT 本就不可刷新)时直接**禁用渠道**并清零其冷却,再按 `ActionRetryChannel` 切下一个渠道。禁用是凭证快照 CAS(`Store.DisableOAuthChannelIfCredentialMatches`:`WHERE id=? AND enabled=1 AND auth_type=? AND oauth_credential=?`),期间已重新授权的渠道快照不匹配,只打 INFO 跳过,绝不误禁;判定不成立的刷新失败仍走普通冷却
- **Responses WebSocket 特例**:
  - 首个语义输出前:非 WS→非 WS、原生 WS→非 WS/原生 WS 均可网关内部切换,WS→非 WS 用 execution session 完整 transcript;非 WS 故障且下一候选为原生 WS 时返回 `status=502` 的 `server_error/upstream_unavailable` 并以 close 1011 断开,让 Codex 客户端完整 replay
  - 已有语义输出后:禁止网关内部切换或重放;成功响应流在终结事件前中断 → `status=502` 的 `server_error/upstream_stream_interrupted` + close 1011,客户端重连并完整 replay 当前 turn;已完成的工具调用先提交 execution session,普通残缺文本不提交
  - 原生上游 WS 的 close 1006、心跳故障、嵌套网关返回的 `upstream_stream_interrupted`,统一按具体 WS 目标连续计数:首次不冷却,10 min 内第二个新物理连接仍失败才冷却 2 min,成功终结事件清零;不得升级为模型冷却

## 自定义状态码(改相关代码前先读语义)

- **499** 客户端取消:不计失败、不冷却;上游直接返回 499:模型级冷却
- **596** 1308 配额超限 → Key 级冷却,不计健康度
- **597** SSE error(HTTP 200+错误体)→ `classifySSEError` 按 error.type 动态判级
- **598** 首字节超时 → 模型级;**599** 流式中断 → 模型级
- **`fwResult.StreamDiagMsg` 是 599 的判定开关,不只是日志字段**:非空即被 `forwardAttempt` 判为流不完整,置 599 并走模型级冷却。所以只有真实上游故障才允许写入,客户端断开必须先过 `isClientDisconnectError`(`buildStreamDiagnostics` 与 Codex 非流式收集器 `codex_wire.go` 各有一处),漏一处就会把 499 误升成 599。`markIncompleteStreamForwardResult` 不覆盖已经是 598 的状态码——两者冷却初值不同
- **429** 统计页/健康时间线计入 ErrorCount 与成功率,`rate_limited` 是 ErrorCount 子集;健康度排序(`GetChannelSuccessRates`/effective priority)排除 429,真实渠道级限流交给冷却过滤。全局设置 `codex_map_429_to_503` 默认关闭;开启后只把所有候选耗尽时返回给官方 Codex 客户端的最终 429 改为 503,内部冷却/统计、其他 Responses 客户端和 ccLoad 自身限额仍保留真实状态

## 关键机制(要点,细节读对应文件)

- **选择顺序**:成本限额检查 → 可用时段过滤 → 冷却过滤(正确性优先;模型冷却按各渠道解析重定向/模糊匹配后的实际上游模型过滤)→ 排序二选一:`enable_health_score` 默认 **false** 走渠道平滑加权轮询(按有效 Key 数),开启才走健康度排序(`calculateEffectivePriority`:`P_eff = Priority - 失败惩罚 - TTFB惩罚`,两种惩罚各按样本量打置信度折扣,TTFB 部分还要 `enable_ttfb_score` 单独开)
- **渠道可用时段**(`model/config.go:IsAvailableAt`+`selector_cooldown.go:filterAvailableTimeChannels`):`available_time_start/end`(HH:MM,服务器本地时间)必须成对设置,均空=全天;支持跨午夜(如 22:00–08:00),半开区间,起止相等视为全天。硬路由约束:在冷却过滤与全冷却兜底**之前**过滤,时段外渠道绝不因「所有渠道冷却」被兜底选中
- **多 URL**:探索优先 → 1/EWMA 加权随机,失败 URL 独立退避;延迟数据只来自真实请求的 TTFB。`ChannelURL.Exact` 派生运行时 `#` 标记实现精确转发,持久化 URL 本身不含标记。Antigravity OAuth 例外:固定 provider 回退顺序(daily → daily sandbox),不被延迟重排,仅跳过手动禁用的 URL(`orderURLsInConfiguredOrder`)
- **路由候选是只读快照**(`cache.go:GetEnabledChannelsSnapshotByModel`):选择链路拿到的 `*model.Config` 归缓存所有,只有外层 slice 是请求私有的——可以过滤、排序、原地重排,但**禁止改 Config 字段**,要改先 `Clone()`(见 `selectAlphaSearchCandidates`);需要可变副本的路径继续用深拷贝的 `GetEnabledChannelsByModel`。`filterCooledChannels`/`selectByWeight`/`selectWithCooldownInPlace` 都原地压缩或重排入参,调用方只能用返回值,不能再按原长度复用入参
- **模型停用**(`ModelEntry.Disabled`):`disabled=true` 的模型对外完全不存在——`GetModels`/`modelIndex`/`FuzzyMatchModel`/`channelModelCooldownKeys` 一律跳过。刷新模型列表的 `replace` 模式会按原名、归一化别名、重定向目标三种键把停用标记传播回新拉取的条目,避免刷新一次就把停用状态洗掉
- **渠道级限流**(`channel_rpm_limiter.go`+`channel_concurrency_limiter.go`):`rpm_limit`/`max_concurrency` 都是 0=无限。注意 `max_concurrency` 这个名字在系统设置(全局信号量)、Auth Token、渠道三处各有一份,互不相干,改代码前先认准层级
- **多协议处理**:每个渠道默认接受四种客户端协议,`protocol_transform_mode` 选择策略:`auto`(默认)、`upstream`(只直通客户端协议)、`local`(只本地转换)。实际上游能力只由 `ChannelURL.Protocols` 声明:非空声明是权威配置,不兼容 URL 无请求、无冷却地跳过。local 优先有声明的 URL 并保持声明顺序;仅当全部 URL 未声明时按 Anthropic → Codex → OpenAI → Gemini 请求。auto 先试客户端协议,再按 OpenAI → Anthropic → Codex → Gemini 自动探测并跳过已试协议;未提交响应的 HTTP 400、非模型 404/405、明确未实现 500、请求到达 API 前的 Cloudflare 403 拦截页或当前转换无法表示请求时才继续下一协议。成功协议按 URL+请求族缓存到进程重启或渠道配置变更;全部协议不支持时 10 分钟后重新探测
- **自定义请求规则**(`custom_rules.go`):`channels.custom_request_rules` JSON;header remove/override/append、body remove/override(点分路径);`validateCustomRequestRules` 强制认证头黑名单 + 禁 CRLF
- **Codex 上游 Header 契约**(`codex_credentials.go`+`codex_upstream_websocket.go`):不走通用反代透传;HTTP 只接收 Codex 客户端白名单,静态 Key 与 OAuth 都只用 `Authorization: Bearer`,固定官方 `User-Agent`/`Originator`,认证与身份头在自定义规则后重建。HTTP 与原生 WebSocket 都保留规范 `Session-Id`+`Thread-Id`;WebSocket 额外接收 turn state/timing/`OpenAI-Beta`,握手前删除 HTTP 传输头并归一 `OpenAI-Beta`,同时从规范会话值同步旧 `Session_id`/`Conversation_id` 别名;渠道自定义 Header 规则仍可显式增加非认证 Header
- **Codex 凭证两种形态**(`codexauth/credential.go`+`admin_codex_auth.go:HandleCreateCodexPersonalAccessToken`,`POST /admin/codex/personal-access-token`):OAuth 与个人访问令牌(PAT)共用 `auth_type=codex_oauth`,靠凭证内 `AuthMode=personalAccessToken` 区分,别新造 auth_type。PAT 必须 `at-` 前缀,导入时走 `whoami` 端点校验并解析账号身份,按账号复用或新建渠道(上游 401/403 返回 400,其余网络/服务端错误返回 502);PAT 是静态令牌,`Refresh` 直接返回 `ErrPersonalAccessTokenCannotRefresh`,一旦被上游拒绝即终态,触发上面的渠道禁用
- **Responses 路径别名**(`protocol/types.go`+`server.go`+`codex_wire.go:normalizeCodexClientPath`):下游 `/v1/responses`、`/v1/codex/responses`、`/backend-api/codex/responses` 是同一 canonical 端点,GET 走 WebSocket 升级、POST 走 SSE fallback,`DetectRequestFamily` 一律识别为 Responses。上游协议为 Codex 时转发前把别名归一回 `/v1/responses`,避免别名泄漏进上游 URL;`Exact` 标记的 OAuth 官方 URL 不拼接路径,不受影响。新增 Responses 入口路径必须同时登记这三处
- **Codex 上游 TLS**(`codex_utls_transport.go`):普通 HTTP 请求仅对 `https://chatgpt.com` 使用 Chrome uTLS,按 HTTP/2 → uTLS HTTP/1.1 → 标准 HTTP/1.1 降级并保留请求体重放;其他 Host 走标准 Transport。uTLS 继承环境/渠道代理、Host 覆盖、证书校验和握手超时,连接池跟随 `upstream_connection_reuse_limit_seconds` 整代轮换;原生 WebSocket 保持独立 Dialer
- **Codex 多代理 v2**(`codex_multi_agent_v2.go`):对官方 Codex 客户端(User-Agent 判定)默认启用,有意不设配置开关;请求侧把 collaboration 工具命名空间(spawn_agent/send_message/followup_task)改写为 `collaboration-optimize__*`,响应侧还原命名;不改写任意 OpenAI 兼容调用方。改 `proxy_forward`/`codex_wire`/`responses_execution_session` 前先认清这层改写-还原对
- **Cursor SDK Bridge 契约**(`cursorauth/`+`app/cursor_wire.go`+`app/cursor_credentials.go`+`app/admin_cursor_auth.go`):`auth_type=cursor_oauth`。只接受 `POST /admin/cursor/credentials/import` 导入 User API Key，并通过 `POST /auth/exchange_user_api_key` 换取控制面会话；浏览器 PKCE 和直接粘贴 `accessToken` 不支持，因为两者不能提供推理必需的 API Key。身份/额度走 Connect JSON 一元 RPC：`DashboardService/GetMe`、`DashboardService/GetCurrentPeriodUsage`；额度按官网名称与顺序、与其他渠道统一展示可用比例：`autoPercentUsed`=Cursor Models、`apiPercentUsed`=Other Models、`totalSpend/limit`=按量 Monthly Limit，顺序固定如此；上游 `You've hit your usage limit` 不展示。模型目录只走 SDK Bridge `SdkCursorService/ListModels`，原样保存返回的 `SdkModel.id`，禁止扩展思考等级或重写模型名。渠道 URL 固定 `https://api2.cursor.sh`，协议 `anthropic,openai` + local 转换。推理**不 HTTP 转发**：启动时若存在 Cursor 渠道，在后台按环境覆盖→同目录→数据目录→PATH 查找固定版本 `cursor-sdk-bridge`；缺失时从官方 Release 下载锁定 archive、校验内置 SHA-256 并原子安装，再启动并探活 Bridge，全程不阻塞 HTTP 服务启动。初始化完成前 Cursor 请求返回 Bridge 不可用；初始化失败只记 WARN，不拖垮其他渠道。Bridge 异常退出后由进程级 watcher 立即单飞重启，连续崩溃指数退避且最长 5 秒；只有无副作用的 `ListModels` 在确认旧 client 已脱离后重试一次，`CreateAgent`/`Send`/已开始的流禁止自动重放，避免重复推理和计费。经 loopback Connect 执行 `CreateAgent → Send → DeleteAgent`；每个 RPC 显式传 User API Key，缺 Key 直接失败。Agent 内建工具列表显式为空，避免 Cursor 在网关本机执行 shell/写文件。客户端 `tools`/`tool_use`/`function_call`/`tool_result` 仍走 prompt 映射：把工具目录写进 prompt，模型输出 `<cc_tool_call>{"name","arguments"}</cc_tool_call>`，再译回 Anthropic `tool_use` 或 OpenAI `tool_calls`；下一轮把 `tool_result`/`role=tool` 写回 prompt。工具名按家族对齐 Grok Build、OpenCode、Codex CLI 和 Claude Code/Cursor 别名，参数键也按客户端 schema 改写。不实现自定义工具回调，也不在网关执行客户端工具。额度快照写入凭证 `oauth_usage`，不记 `quota_cost_usage`
- **Z.ai Coding Plan 契约**(`zaiauth/`+`app/zai_wire.go`+`app/zai_credentials.go`+`app/admin_zai_{auth,oauth}.go`):`auth_type=zai_oauth`,凭证里真正用于转发的是长期有效的 Coding Plan API Key(`id.secret`),`access_token` 只用于在 Key 被 401 拒绝后重新派生(`/api/auth/z/login` → `getCustomerInfo` → 复用或新建名为 `zcode-api-key` 的 Key → copy secretKey),没有过期时间也没有定时刷新。渠道 URL 是 ZCode 路由后的端点 `https://zcode.z.ai/api/v1/ultra-zai/anthropic`(创建渠道时按 `/api/v1/agent/configs` 的 `proxyEndpoint.mapping` 解析,失败回退内置常量),协议固定 anthropic + local 转换;该 URL 含 `/v1`,`validateChannelBaseURL` 对 `zai_oauth` 专门放行。转发时重建请求头:只用 `x-api-key`(删除 Authorization)、`User-Agent: ZCode/<ver>`+`X-ZCode-App-Version`+`X-Title`+`X-ZCode-Agent`+`HTTP-Referer`+平台头,并把 body 的 `metadata.user_id` 覆盖为 ZCode 指纹 `{"device_id","account_uuid":"","session_id"}`(字段顺序是契约的一部分;device_id 由账号身份派生,session_id 优先取请求自带的)。模型目录动态拉取,三级来源:①账号目录 `https://api.z.ai/api/coding/paas/v4/models`(Coding Plan 自己的目录,新模型先到这里)+通用 API `/api/paas/v4/models` 补齐;②models.dev 的 `zai-coding-plan` provider(无需 Key,仓库已为定价同步该站);③`zaiauth.DefaultModels` 兜底。导入渠道时即按实时目录建模型列表,不靠发版更新。额度查询走 Z.ai 订阅面板内部端点 `https://api.z.ai/api/monitor/usage/quota/limit`(未公开文档,接受 Coding Plan Key):`data.limits[]` 每项给 `type`(`TOKENS_LIMIT` 等)+`unit/number`(3/5=5 小时,6/1=周)+`percentage`(已用 0-100)+`nextResetTime`(毫秒),该端点即使 Key 无效也返回 HTTP 200,成败以信封 `success`/`code` 为准;归一成 `oauthUsageWindow` 后按普通 OAuth 渠道流程持久化进凭证 `oauth_usage`,但**不记 `quota_cost_usage`**(Coding Plan 按额度计费,标准成本窗口对它没有意义)。登录是轮询式:`POST /admin/zai/oauth/start` 起 flow 后由服务端后台轮询 `/oauth/cli/poll/{id}`,不需要粘贴回调;上游该端点当前返回空 404 时统一报 `ErrOAuthFlowUnavailable`(503),此时走 `POST /admin/zai/credentials/import` 直接导入 Coding Plan Key
- **Antigravity 上游契约**(`server.go`+`antigravityauth/service.go`+`antigravity_wire.go`+`upstream_connection_age.go`):启动时以 `electron-builder` UA 从官方 Hub manifest 读取三段式版本,失败回退 `antigravity/hub/2.8.1 darwin/arm64`;非流请求走 `/v1internal:generateContent`,流式请求走 `/v1internal:streamGenerateContent?alt=sse`,但两者共用 daily → daily sandbox 的地址回退策略;Cloud Code 数据请求及项目/模型/额度查询共用该 UA 和独立的标准 HTTP/1.1 连接池,OAuth Token 端点仍按原生契约使用 `Go-http-client/2.0`;不用 uTLS、不先试 HTTP/2,避免在 Google Cloud Code 内部端点上重放失败的 POST;无代理与渠道代理池都和普通渠道按传输配置隔离,并继续服从 `upstream_connection_reuse_limit_seconds`
- **OAuth 全局上游地址**:`CODEX_BASE_URL`(完整 Responses URL)、`XAI_BASE_URL`(API 根地址,通常以 `/v1` 结尾)、`ANTIGRAVITY_URL`、`ANTHROPIC_BASE_URL`(API 根地址)默认均为空;设置页分别以 `https://chatgpt.com/backend-api/codex/responses`、`https://cli-chat-proxy.grok.com/v1`、`https://daily-cloudcode-pa.googleapis.com`、`https://api.anthropic.com` 显示默认渠道地址占位提示,Antigravity 另有 `https://cloudcode-pa.googleapis.com` 备用地址。非空时对应 OAuth 渠道的数据请求、模型发现、测试和额度查询只使用该全局地址并忽略渠道 URL;OAuth 授权、Token 交换/刷新仍使用提供商官方地址,API Key 渠道不受影响。属于系统设置,修改后重启生效
- **Antigravity 容量错误**:精确 `MODEL_CAPACITY_EXHAUSTED` 首次出现时立即按原始 503 冷却当前实际上游模型(默认 2 min),默认在 daily 与 daily sandbox 间重试;自定义 URL 列表仍最多尝试 3 个。重试成功立即清除该模型冷却,全部失败保留上游 503 供诊断、对外统一为 429。容量/URL 回退失败的日志延迟到重试器定论后只落一条,避免一次逻辑请求产生多条失败日志;模型发现不因该错误冷却 URL
- **系统设置无热重载**(`config_service.go`+`admin_settings.go`):`LoadDefaults` 启动读一次进内存,运行期只读;单改/重置/批量三个写入口都是写库后 `go s.triggerRestart()`,2 秒后重启进程生效。重启回调属于 `Server` 实例并由锁保护,禁止恢复为包级可变全局。别在 `AdminUpdateSetting` 里加"顺手刷新缓存"——重启才是生效机制
- **引导期配置只能是环境变量**:`ConfigService` 依赖已建好的 `storage.Store`,建库阶段消费的配置不可能迁进系统设置(要读设置得先开库,要开库得先知道设置)。`SQLITE_PATH`/`SQLITE_JOURNAL_MODE`(拼 DSN,`factory.go:buildSQLiteDSN`)、`CCLOAD_MYSQL`/`CCLOAD_POSTGRES`/`CCLOAD_ENABLE_SQLITE_REPLICA`/`CCLOAD_SQLITE_LOG_DAYS`(`factory.go:NewStore`)全属这一类,保持环境变量;运行期策略才进系统设置
- **全局限额与冷却时长**(`server.go:loadServerRuntimeConfig`):均为系统设置,启动读一次,改后重启生效。`max_concurrency`(全局并发信号量;三层同名警告见渠道级限流条)、`max_body_bytes`/`max_image_body_bytes`(Images 路径独立上限,同时约束 Responses WS 帧与 transcript,注入见 `newRequestBodyLimits`)、`cooldown_{auth,server,timeout,rate_limit,min,max}_seconds`(`loadCooldownSettings` 读出 `util.CooldownSettings`,经 `Store.ConfigureCooldown` 注入;下限>上限时整对回退默认)。旧 `CCLOAD_MAX_CONCURRENCY`/`CCLOAD_MAX_BODY_BYTES`/`CCLOAD_COOLDOWN_*` 已废弃,仍设置时启动打 WARN
- **下游请求读取超时**(`config/defaults.go`+`server.go:loadHTTPReadTimeout`+`main.go`):系统设置 `http_read_timeout_seconds`(秒,0=内建默认 120 秒,负数回退默认),启动读一次注入 `http.Server.ReadTimeout`,改后重启生效。它覆盖**请求头+请求体的整段读取**,和 `max_body_bytes` 是两件事:体积超限立即 413(`errBodyTooLarge`),读取超时是 408(`errBodyReadTimeout`),两条错误文案分别点名对应设置,别再互相误判——注意调大体积上限反而会让原本快速 413 的请求改为等到读取超时才失败
- **上游超时**(`server.go:loadProtocolTimeouts`):`upstream_first_byte_timeout`(0=禁用,仅流式)、`stream_timeout`(0=禁用,流式总时长)、`non_stream_timeout`(120s),首字节与非流式超时可按实际上游协议 `{protocol}_*` 覆盖;写回前调 `disableResponseWriteTimeout` 防 `WriteTimeout` 截断响应体
- **上游连接最长复用时间**(`upstream_connection_age.go`+`codex_upstream_websocket.go`):`upstream_connection_reuse_limit_seconds`(默认 0=不限制)统一约束直连及渠道代理池中的 HTTP/1.1、HTTP/2、WebSocket 物理连接;达到时限后不再接收新请求,空闲连接立即关闭,在途请求/turn 完成后关闭,新请求自动建连。原生 WS 重连语义见「Responses WebSocket 会话与资源」;计划轮换不记失败、不触发冷却
- **模型名思考后缀**(`thinking_suffix.go`+`model/thinking_suffix.go`):`gpt-5.6-luna(max)` 只是「客户端在 body 里写思考参数」的语法糖。入口先用 `applyThinkingSuffix` 在**客户端协议的原始 body** 上写字段,位置在协议转换**之前**;选定渠道后 `prepareRequestBody` 再用请求原始后缀和重定向/模糊匹配后的实际上游模型收敛等级,原生 Responses WS 的完整 transcript 与增量体必须使用同一实际模型。入口共三个:HTTP 代理 handler、Responses WS turn、管理测试 `buildChannelTestRequestPlan`——三处必须同步登记,漏一处该入口的后缀就是静默空操作。跨协议管理测试还要在 `upstreamTester.Build` 之后再写一次上游协议 body,否则 `patchUpstreamTestFields` 会用 Codex 模板默认 `reasoning.effort=medium` 盖掉后缀。**别挪进 `prepareTranslatedUpstreamBody`**:那里的 body 形态取决于上游,Antigravity 的 `{"request":{...}}` 信封会把顶层字段整段丢弃。跨协议映射与模型能力裁剪归 registry 转换器;同协议直通按客户端协议写字段,OpenAI/Codex 的等级按实际上游模型查 catalog `thinking.levels` 收敛(有 max 就保留,没有则夹到最近档;`auto` 删字段)。Anthropic 等级走 `adaptive`+`MapToClaudeEffort`,数字预算走 `enabled`+`budget_tokens`;Gemini 收敛到 low/medium/high,`none` 用 `thinkingBudget=0`。后缀不是模型身份:`model.RoutingModelName` 是唯一剥离入口,`GetModels()`/`FuzzyMatchModel`/模型索引按基名归一(条目字面写 `x(max)` 也能按 `x` 路由,显式基名条目优先不被别名覆盖),选路/鉴权/冷却/上游模型名与路径一律基名。渠道 `custom_request_rules` 在上游边界应用,晚于后缀,所以渠道规则覆盖后缀——与其它 body 字段一致
- **Anthropic thinking**:项目生成的 Anthropic 请求用 `thinking.type=adaptive` + `output_config.effort`;anyrouter `/v1/messages` 兜底补 adaptive 并归一旧 `enabled`;anyrouter 额外注入 `anthropic-beta: context-1m`
- **Claude Code CLI 指纹**(`anthropic_wire.go`,无开关):套不套 CLI 指纹只由 `isAnthropicClaudeCodeMessagesRequest`(anthropic 上游 + `/v1/messages` 且非 Z.ai Coding Plan)决定,**与凭证形态无关**——OAuth、第一方 API Key、第三方 Anthropic 网关一律走同一条 `finalizeAnthropicClaudeCodeMessagesBody` + `applyAnthropicClaudeCodeHeaders`,认证差异只落在认证头上(OAuth `Authorization: Bearer`,API Key 走 `applyAnthropicAPIKeyAuth`:官方 origin 只发 `x-api-key`,第三方网关两种都发)。API Key 渠道没有真实 device/account,由 `synthesizeAnthropicAPIKeyCredential` 从 Key 稳定派生。**别拆成「OAuth 版」和「API Key 版」两套形态**:body 形态与 `anthropic-beta` 必须同源,拆开就会出现 body 用了 `cache_control.ttl=1h` 而 header 少了 `extended-cache-ttl-2025-04-11` 的 400。三类请求例外直通(只重签 CCH,不重写 body):`nativeAnthropicHaikuHelperShape` 命中的内部 Haiku 辅助请求、`isNativeAnthropicClaudeCodeRequest` 命中的原生 Claude Code 请求(判定含 header 与 body 的 session id 等式)、以及 Z.ai。副作用:该路径清空全部下游 header 后重建,所以 `buildProxyRequest`/管理测试在重建之后**再跑一次** `applyHeaderRules`——渠道自定义 header 规则必须最终生效,认证头仍由 `authHeaderBlacklist` 拦下,规则可以改写 CLI 身份头(重跑对 override/remove 幂等,append 也不重复,因为前一次的产物已被清空);同理 `applyHeaderRules` 会先把规则名对齐到请求里已存在的同名键,否则 canonical 化会让 `anthropic-beta` 这类小写指纹头以两种大小写并存。CCH 无条件重签(`finalizeAnthropicCCH` 对无 billing header 的 body 是 no-op),别加第二个谓词
- **Anthropic 缓存窗口归调用方**(`anthropic_wire.go`):5m 还是 1h 由原始请求决定,网关不主动升级。`extended-cache-ttl-2025-04-11` 按 body 里**实际存在**的 `cache_control.ttl` 条件声明(`anthropicRequestHasCacheControl`),双向同源——没用 ttl 就不发这个 beta。但网关注入的 breakpoint(CLI system 提示 + `ensureAnthropicCloakedCacheBreakpoints`)必须**跟随**调用方:Anthropic 按 tools → system → messages 顺序评估,前面出现 5m 会让后面的 1h 被 `normalizeAnthropicCacheControlTTL` 删掉 ttl,网关注入的 system breakpoint 排在调用方 block 之前,保持 5m 就会把调用方的 1h 一起降级。所以 finalize **在改写 system 之前**先探测调用方有没有 1h,有就让自己注入的 breakpoint 也用 1h(`anthropicCloakCacheControl`);调用方没要 1h 时一律无 ttl。别把它写回成无条件 `upgrade...("1h")`,那才是主动改写窗口
- **定时检测**(`channel_check_scheduler.go`):全局 `channel_check_interval_hours`(0=禁用,启动读一次,改后重启生效)+ 渠道级开关

## 发布与更新

- 发布必须使用仓库 Skill:Codex 调 `$ccload-release`,Claude Code 调 `/ccload-release`;唯一源码在 `.agents/skills/ccload-release/`,`.claude/skills/ccload-release` 只是软链接
- 无参数默认 Beta;只有显式 `stable` 才发稳定版。Tag 只允许 `vX.Y.Z-beta.N` / `vX.Y.Z`
- `.github/workflows/test.yml` 是提交级唯一发布门禁:`master` 的完整 SHA 必须通过后端测试、Web 验证、构建、lint 和 PostgreSQL 集成测试,发布脚本才允许打 Tag。`.github/workflows/release.yml` 只校验 Tag、构建多平台产物并生成 Release 和 GHCR 镜像;Beta=`prerelease=true` 且不改 GitHub latest,镜像发布精确版本 Tag+`beta`;稳定版更新 GitHub latest,镜像发布精确版本 Tag+`latest`,且该稳定版为 SemVer 最高版本时同步把 `beta` 别名推进到它(存在更高 Beta Tag 时不动,禁止降级)——`beta` 别名语义=全渠道 SemVer 最高版本,与 `preview` 更新渠道一致
- 官方容器直接打包同一 Release 的 Linux 二进制;`CCLOAD_CONTAINER=1` 时不启动版本检查或进程内更新,`auto_update_*` 设置只读;稳定版/测试版分别通过 `latest`/`beta` 镜像标签切换
- 非容器部署的单一更新管理器同时负责前端版本提示和可选自动应用;默认 `auto_update_channel=stable`,`preview` 同时考虑稳定版/测试版并按 SemVer 取最高版本;`auto_update_interval_hours=0` 关闭全部版本检查

## 协议转换核心(改前必读)

- 同步/审查转换核心与 provider 纯请求/响应适配器必须使用仓库 Skill:Codex 调 `$sync-cliproxy-core`,Claude Code 调 `/sync-cliproxy-core`;一次操作固定同一上游 commit 并原子完成全部登记范围,唯一 Skill 源码在 `.agents/skills/`,`.claude/skills/` 只放发现链接
- `protocol/registry.go` 是唯一契约/调度边界:同协议原样透传;跨协议只走 `builtin/register.go` 注册的 12 个有向转换对
- `builtin/cliproxy_adapter.go` 只处理 ccLoad 通用边界(输入验证、JSON/SSE 规范化、流帧封装);`protocol/cliproxy/` 只允许放从 CLIProxyAPI 同步的纯转换核心和 allowlist provider adapter,实际已导入状态以 `protocol/cliproxy/UPSTREAM.md` 为准
- 不要把上游 auth/config/routing/cache service/plugin/executor/network 代码搬进来,也不要改成运行时 Go module 依赖;来源 commit、provider allowlist、许可证和同步步骤以 `protocol/cliproxy/UPSTREAM.md` 与仓库 Skill 为准
- `RequestTranslationError` 是客户端语义错误:代理返回 HTTP 400,不切渠道、不冷却;不要把无法表示的请求伪装成上游故障
- Registry 边界测试定义 ccLoad 线协议契约,上游同步测试守住转换行为;改协议后先跑命令区快照审计,再跑全量 `internal/...`

## 计费与限额

- **渠道倍率** `cost_multiplier`(≤0 归 1):× 标准成本 = `effective_cost`,写日志时快照到 `logs.cost_multiplier` 避免历史污染
- **Auth Token**:`cost_*_microusd`(微美元整数避浮点);`cost_limit` 是总限额,`cost_daily_*`/`cost_monthly_*` 按服务器本地自然日/自然月累计,任一限额启用时必须同时设正数 `max_concurrency`;仅 2xx 累加费用,失败只计次,允许「超额一个请求」;`CCLOAD_API_TOKENS` 启动预置
- **Auth Token 访问控制**(`model/auth_token.go`、`auth_service.go`):`allowed_models` 模型白名单(空=无限制);`allowed_channel_ids`+`channel_restriction_mode`(`allow` 白名单/`deny` 黑名单,空 mode 视为 allow,空列表始终无限制),`ChannelRestriction.Allows` 封装极性,选择链路走 `FilterAllowedChannels`;`max_concurrency` 令牌级并发上限(0=无限),`acquireTokenConcurrencySlot` 获取槽位
- **渠道每日限额** `daily_cost_limit`(美元,0=无限);`CostCache` 内存缓存按天重置
- **OAuth 配额成本**(`internal/oauthcost/usage.go`+`app/oauth_quota_cost.go`+`sql/log.go:AddLogWithOAuthQuotaCost`):Codex/Anthropic/Antigravity/xAI 凭证 JSON 的 `quota_cost_usage.windows[]` 内持久化**每个上游额度窗口**的**标准成本**(不乘 `cost_multiplier`,与 `effective_cost` 无关),随日志写入在**同一事务**累加,失败即整条日志回滚。槽位身份是上游的 `limit_name|kind`(`oauthcost.Key`),**不是窗口时长**——同一时长可以对应多个互不相干的窗口(Antigravity 的 gemini/3p 周额度、Anthropic 的三个 7 天窗口),按时长归并必然错位。每个槽位带模型族 `Family`,`AddStandardCost` 按日志的实际上游模型(`ActualModel` 优先)逐槽判族累加:Antigravity 按模型名前缀分 `gemini`/`non_gemini`,Anthropic 的 `seven_day_sonnet`/`seven_day_fable` 各自成族,Codex 的 `codex-spark` 只吃 Spark 模型,其余窗口是 `FamilyAll` 吃全部。窗口边界只来自上游额度采样(月=28–31 天区间,按自然月+`ResetDay` 锚点推进)后由 `Reconcile` 对账,没采到就没有窗口、不累加;采样里消失的槽位丢弃,但**一个有效窗口都没采到时保留已累计数据**(缺信息≠窗口消失)。只有落在 `[max(StartedAt, CountFromAt), ResetAt)` 半开区间的日志计入,迟到的旧日志不会污染新周期。混合存储副本走 `AddLogReplica` 不累加,凭证由 `markChannelDirty`→`syncChannelReplica` 异步复制到主库
- **Codex 手动配额重置**(`admin_codex_quota_reset.go`,`POST /admin/channels/:id/codex-quota-reset`):先查上游 reset credit,无可用额度返回 409;credit 一经消费不可退,之后的本地清理(成本窗口重置、渠道全部冷却清除、超额窗口清理、额度刷新)全部 best-effort,失败只进响应 `warnings` 而不回滚。同渠道并发重置由 `codexQuotaResetInFlight` 直接 409 拒绝
- **定价细节**(service_tier 倍率、GPT-5.4/Qwen-Plus 分层降档、Gemini 长上下文翻倍、缓存读折扣/写乘数):读 `cost_calculator.go`

## 存储

- 存储相关配置全是引导期环境变量,不进系统设置(原因见「关键机制」引导期配置条)
- 模式:纯 SQLite(默认)/ 纯 MySQL(`CCLOAD_MYSQL`)/ 纯 PostgreSQL(`CCLOAD_POSTGRES`)/ 混合(主库 DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1`)
- 互斥:`CCLOAD_MYSQL` 与 `CCLOAD_POSTGRES` 同时设置 → `log.Fatal`
- PG DSN:URL(`postgres://user:pass@host:5432/db?sslmode=disable`)或 libpq 关键字串;驱动 `pgx/stdlib`
- 混合数据流:SQLite 是权威库,配置/鉴权/Key/冷却/设置/日志都同步读写 SQLite,提交成功即返回;主库只由进程内 write-behind worker 写入,同一实体合并最终状态,失败 10 秒后重试。分析读默认 SQLite,本地分析读取失败才允许回退主库。Web session 与 DebugData 仅存 SQLite
- 混合启动:仅首次创建 SQLite 文件时从主库一致性快照导入配置;`CCLOAD_SQLITE_LOG_DAYS=0` 全局关闭启动日志导入,否则每次启动都要求主库可用,SQLite 有日志时从主库增量导入 `time > MAX(sqlite.logs.time)` 的尾部日志,SQLite 日志为空时才按该变量限制首次日志窗口;已有 SQLite 配置禁止被启动恢复覆盖。SQLite DSN 必须启用 `PRAGMA foreign_keys=1`
- 混合边界:单实例、单写者;不支持外部直接修改主库或多个混合实例。无 outbox,进程退出允许丢失待同步内存任务;日志写入/清理仅单次 best-effort,不进入 10 秒重试,新日志批次可替换旧批次并计入 dropped
- 混合健康:Ping 只检查权威 SQLite;`RuntimeMetrics` 暴露主库 pending/failures/dropped/last_success
- 混合队列:按实体合并内存终态;高基数脏实体达到 10000 时折叠为一次 SQLite→主库全量状态对账,不静默丢失运行中配置任务
- 模型冷却与 URL 禁用状态写 SQLite 后作为渠道聚合终态异步复制主库,渠道删除时级联清理

## 前端(Playwright MCP)

截图必须 `type:"jpeg"`,优先 `browser_snapshot`(文本),避免 `fullPage:true`。
