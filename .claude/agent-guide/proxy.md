# 代理、故障切换与流终态

修改代理重试、冷却、取消、SSE 或 Responses WebSocket 时读取相关章节。路径相对仓库根目录；省略目录的 app 文件位于 `internal/app/`。

## Responses WebSocket 会话与资源

- **执行身份**:同 Token 下以 `Session-Id` 标识顶层会话;存在 `Thread-Id` 时组合两者,隔离 Codex 主/子代理的 transcript、Response ID、turn lock;无 `Thread-Id` 回退原 `Session-Id` 契约。禁止改用请求体 `session_id`、`prompt_cache_key` 或每回合变化的 request/turn/window ID
- **默认限制**(新安装):下游连接全局 1024、单 Token 64;执行会话 1024;transcript payload 总预算 256 MiB。连接与会话默认按 ~500 并发客户端留量,限额只约束真实存在的资源、不预分配。所有 `responses_ws_*` 整数配置保存 `0` 用内建默认,负数非法;已有数据库记录不迁移
- **生命周期**:上游每 45s 发 Ping,连续 5 min 无帧/Pong 判失活;下游全断满 5 min 后由每分钟清理器关上游物理连接(实际约 5–6 min);稳定逻辑会话与已提交 transcript 在 `responses_ws_session_ttl_minutes`(默认 15,小内存机器可设 10)到期前不因容量/预算压力被逐出
- **超限语义**:达 `responses_ws_max_sessions` 只拒绝新会话身份;已提交 payload 超 `responses_ws_max_transcript_bytes` 后,所有新回合在触达上游前以 `429/rate_limit_error/rate_limit` 拒绝,已准入回合仍可提交,有限最坏超量 `max_sessions × max_body_bytes`
- **连接轮换**:达到 `upstream_connection_reuse_limit_seconds` 的空闲连接立即关闭,在途 turn 完成后再关;下一轮优先原渠道/Key/URL,按需重连并重放完整 transcript——Response ID 只在原物理 WebSocket 上有效
- **指标**:`/admin/runtime-metrics` 的 `transcript_bytes` 只统计有效 payload,不是 Go 堆占用;另有 `ttl_expired`/`capacity_rejected`/`budget_rejected`/`previous_response_misses` 进程累计计数

## 故障切换(`util/classifier.go` + `cooldown/detection.go`)

- Key 级(401/403)→ 冷却当前 Key,重试同渠道其他 Key;所有启用 Key 均冷却时自动升级渠道冷却
- 模型级(`model_cooldown`,上游 HTTP 400/499/5xx/520/524/429,597 服务类 SSE 错误,598/599 流故障,连接重置/HTTP2 流关闭/空响应/网络超时,404 模型不可用,410 明确模型退役)→ 写入 `(channel_id, 实际上游模型)` 冷却;直接切渠道,不再尝试同渠道其他 Key/URL,不影响其他模型;所有配置模型均冷却时自动升级渠道冷却
- 渠道级(DNS/连接拒绝/网络或路由不可达)→ 切渠道
- 原生协议能力不支持(响应未提交的 HTTP 400、非模型 404/405,或结构化 500 明确返回 `convert_request_failed` + `not implemented`)→ 写入带 `protocol capability fallback` 标记的代理尝试日志,开启 Debug 日志时同时保存请求/响应详情,但不冷却 Key/模型/渠道/URL;auto 模式可转换时同渠道/Key/URL 探测其他协议,不可转换时切 URL/渠道
- 客户端错误(406/413,404 非模型 `does not exist`)→ 直接返回,不重试
- 成本限额达到 → 跳过该渠道
- Key/模型/渠道共用指数退避:按错误类型取初始值(默认认证 5 min、服务端 2 min、超时/限流 1 min),翻倍并 30 min 封顶;上游或自定义规则给出精确 reset 截止时间时优先使用
- **冷却探测规则**(`cooldown/detection.go`):渠道 `cooldown_detection_rules` 为空时继承系统设置 `global_cooldown_detection_rules`;按 rules 数组顺序(提交后重编号 0..N-1)匹配 status+正则,命名捕获组可解析精确 reset 时间。网络故障故意不进匹配器(没有可信上游错误体);规则命中但不可执行时回退内置分类器,不猜冷却时长。`EvaluateCooldownDetectionRules` 无副作用,代理链路与 admin 规则测试端点共用
- **全冷却兜底**(`selector_cooldown.go`,`cooldown_fallback_enabled` 默认 true):所有渠道都冷却时不直接拒绝,挑「最早恢复」渠道打 `CooldownFallback` 标记继续正常流程,Key 也改选最早恢复的(`SelectCooldownFallbackKey`)。排查「明明全冷却了为什么还在发请求」先看这里;设 false 才直接拒绝
- **日志模型字段**(`model/log.go`+`logs` 表):`actual_model` 记实际发给上游的模型(空=未重定向),`response_model`(`LogEntry.ResponseModel`)记**上游成功响应声明的模型**,三库增量迁移齐全、前端日志页展示;它只用于记录与排查,成本归集、冷却与维度聚合仍以 `ActualModel` 为准
- **OAuth 凭证终态拒绝**(`proxy_forward.go:disableTerminalOAuthCredential`+`admin_oauth_cleanup.go:oauthRefreshTokenRejected`):刷新被上游明确拒绝(Token 端点 401,或 400/403 且错误码为 `invalid_grant`/`invalid_token`/`invalid_refresh_token`/`refresh_token_expired`/`refresh_token_revoked`/`expired_token`,或静态 PAT 本就不可刷新)时直接**禁用渠道**并清零其冷却,再按 `ActionRetryChannel` 切下一个渠道。禁用是凭证快照 CAS(`Store.DisableOAuthChannelIfCredentialMatches`:`WHERE id=? AND enabled=1 AND auth_type=? AND oauth_credential=?`),期间已重新授权的渠道快照不匹配,只打 INFO 跳过,绝不误禁;判定不成立的刷新失败仍走普通冷却
- **Responses WebSocket 特例**:
  - 首个语义输出前:非 WS→非 WS、原生 WS→非 WS/原生 WS 均可网关内部切换,WS→非 WS 用 execution session 完整 transcript;非 WS 故障且下一候选为原生 WS 时返回 `status=502` 的 `server_error/upstream_unavailable` 并以 close 1011 断开,让 Codex 客户端完整 replay
  - 已有语义输出后:禁止网关内部切换或重放;成功响应流在终结事件前中断 → `status=502` 的 `server_error/upstream_stream_interrupted` + close 1011,客户端重连并完整 replay 当前 turn;已完成的工具调用先提交 execution session,普通残缺文本不提交
  - 原生上游 WS 的 close 1006、心跳故障、嵌套网关返回的 `upstream_stream_interrupted`,统一按具体 WS 目标连续计数:首次不冷却,10 min 内第二个新物理连接仍失败才冷却 2 min,成功终结事件清零;不得升级为模型冷却

## 自定义状态码(改相关代码前先读语义)

- **499** 客户端取消:不计失败、不冷却;上游直接返回 499:模型级冷却
- **管理员手动中断**(`admin_active_requests.go`,`POST /admin/active-requests/:request_id/abort`):日志页中止在途请求时注入的 cancel cause(`errOperatorAbort`)**刻意写成 connection reset by peer 形态**——分类器只认错误文本,让手动中断按「上游断链」冒泡,走正常故障切换;改成 `context.Canceled` 或不含该文案会被误判成 499 客户端取消(不计失败、不冷却)。改这两处文案前先读 `proxy_forward.go` 与 `active_requests.go` 的集成测试
- **596** 1308 配额超限 → Key 级冷却,不计健康度
- **597** SSE error(HTTP 200+错误体)→ `classifySSEError` 按 error.type 动态判级
- **598** 首字节超时 → 模型级;**599** 流式中断 → 模型级
- **`fwResult.StreamDiagMsg` 是 599 的判定开关,不只是日志字段**:非空即被 `forwardAttempt` 判为流不完整,置 599 并走模型级冷却。所以只有真实上游故障才允许写入,客户端断开必须先过 `isClientDisconnectError`(`buildStreamDiagnostics` 与 Codex 非流式收集器 `codex_wire.go` 各有一处),漏一处就会把 499 误升成 599。`markIncompleteStreamForwardResult` 不覆盖已经是 598 的状态码——两者冷却初值不同
- **流终态有两套判据,判完整就必须给下游完整终止序列**(`proxy_sse_parser.go:parseEvent`+`proxy_forward.go`):除 `[DONE]`/`message_stop`/`response.completed` 外,OpenAI Chat 的非空 `finish_reason` 与 Gemini 的非空 `finishReason` 同样是终态——不少 OpenAI 兼容上游给完 `finish_reason` 就断流,只认 `[DONE]` 会把完整响应误记成 499/599。判据只有 `openAIStreamPayloadComplete`/`geminiStreamPayloadComplete` 一份,直通与跨协议两条路径共用,别再各写一份。而 `openai→{anthropic,codex,gemini}` 转换器的终止事件只挂在 `[DONE]` 上,所以上游省略它时由 `needsSynthesizedStreamTerminator` 在流结束后补喂一份合成 `data: [DONE]` 走同一个转换闭包收尾——不手搓终止帧,因为 open content block/stop_reason/usage 都在转换器内部状态里;同协议直通不补,避免改动透传字节。「上游没发 [DONE] 但客户端收到 message_stop」是预期行为
- **429** 统计页/健康时间线计入 ErrorCount 与成功率,`rate_limited` 是 ErrorCount 子集;健康度排序(`GetChannelSuccessRates`/effective priority)排除 429,真实渠道级限流交给冷却过滤。全局设置 `codex_map_429_to_503` 默认关闭;开启后只把所有候选耗尽时返回给官方 Codex 客户端的最终 429 改为 503,内部冷却/统计、其他 Responses 客户端和 ccLoad 自身限额仍保留真实状态
