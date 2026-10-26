# 配置、管理账户、更新与存储

修改对应运行机制时读取相关章节。渠道级限流见 [routing.md](routing.md)，Responses WebSocket 生命周期见 [proxy.md](proxy.md)；原「关键机制」中的引导期配置说明位于本文件「系统设置与连接生命周期」章节。

## 系统设置与连接生命周期

- **系统设置无热重载,唯一例外是多模态回退映射**(`config_service.go`+`admin_settings.go`):`LoadDefaults` 启动读一次进内存,运行期只读;唯一写入口 `POST /admin/settings/batch` 写库后 `go s.triggerRestart()`,2 秒后重启进程生效。重启回调属于 `Server` 实例并由锁保护,禁止恢复为包级可变全局。例外由 `settingsRequireRestart` 判定:仅当一次提交的**唯一修改键**是 `model_multimodal_fallback` 时跳过重启,`commitSettingUpdates` 把持久化与运行态发布绑定成同一有序操作(含该映射的提交整体串行化,防并发提交让数据库终值与运行态快照错序),代理热路径只做一次原子快照读取。除该键外别在 `AdminBatchUpdateSettings` 里加"顺手刷新缓存"——重启才是生效机制
- **引导期配置只能是环境变量**:`ConfigService` 依赖已建好的 `storage.Store`,建库阶段消费的配置不可能迁进系统设置(要读设置得先开库,要开库得先知道设置)。`SQLITE_PATH`/`SQLITE_JOURNAL_MODE`(拼 DSN,`factory.go:buildSQLiteDSN`)、`CCLOAD_MYSQL`/`CCLOAD_POSTGRES`/`CCLOAD_ENABLE_SQLITE_REPLICA`/`CCLOAD_SQLITE_LOG_DAYS`(`factory.go:NewStore`)全属这一类,保持环境变量;运行期策略才进系统设置
- **全局限额与冷却时长**(`server.go:loadServerRuntimeConfig`):均为系统设置,启动读一次,改后重启生效。`max_concurrency`(全局并发信号量;三层同名警告见渠道级限流条)、`max_body_bytes`/`max_image_body_bytes`(Images 路径独立上限,同时约束 Responses WS 帧与 transcript,注入见 `newRequestBodyLimits`)、`cooldown_{auth,server,timeout,rate_limit,min,max}_seconds`(`loadCooldownSettings` 读出 `util.CooldownSettings`,经 `Store.ConfigureCooldown` 注入;下限>上限时整对回退默认)。旧 `CCLOAD_MAX_CONCURRENCY`/`CCLOAD_MAX_BODY_BYTES`/`CCLOAD_COOLDOWN_*` 已废弃,仍设置时启动打 WARN
- **下游请求读取超时**(`config/defaults.go`+`server.go:loadHTTPReadTimeout`+`main.go`):系统设置 `http_read_timeout_seconds`(秒,0=内建默认 120 秒,负数回退默认),启动读一次注入 `http.Server.ReadTimeout`,改后重启生效。它覆盖**请求头+请求体的整段读取**,和 `max_body_bytes` 是两件事:体积超限立即 413(`errBodyTooLarge`),读取超时是 408(`errBodyReadTimeout`),两条错误文案分别点名对应设置,别再互相误判——注意调大体积上限反而会让原本快速 413 的请求改为等到读取超时才失败
- **入站压缩请求体**(`proxy_handler.go:parseIncomingRequest`+`content_encoding.go`):所有 HTTP 代理入口按 `Content-Encoding` 解码 gzip/deflate/br/zstd(可叠加),`max_body_bytes` 约束**解码后**字节(解压炸弹同样 413);未知编码 415,压缩数据损坏 400,解码中读取超时仍 408。zstd 窗口限 8MB(RFC 9659)且单并发。解码后删除 `Content-Encoding`/`Content-Length`,上游只收明文,出站压缩(Codex OAuth zstd、Anthropic 第一方 gzip)照常按自身条件判断。WebSocket 不经此路径
- **上游超时**(`server.go:loadProtocolTimeouts`):`upstream_first_byte_timeout`(0=禁用,仅流式)、`stream_timeout`(0=禁用,流式总时长)、`stream_idle_timeout`(0=禁用,流式上游连续无数据)、`non_stream_timeout`(600s),首字节与非流式超时可按实际上游协议 `{protocol}_*` 覆盖;流空闲超时目前只有 `anthropic_stream_idle_timeout`(默认 180s,0=用全局值)一项协议覆盖。写回前调 `disableResponseWriteTimeout` 防 `WriteTimeout` 截断响应体
- **上调过的默认值**(`storage/migrate.go`):`max_body_bytes` 10MB→32MB、`non_stream_timeout` 120s→600s,迁移只改写仍等于旧默认值的记录,用户改过的值不动
- **上游连接最长复用时间**(`upstream_connection_age.go`+`codex_upstream_websocket.go`):`upstream_connection_reuse_limit_seconds`(默认 0=不限制)统一约束直连及渠道代理池中的 HTTP/1.1、HTTP/2、WebSocket 物理连接;达到时限后不再接收新请求,空闲连接立即关闭,在途请求/turn 完成后关闭,新请求自动建连。原生 WS 重连语义见「Responses WebSocket 会话与资源」;计划轮换不记失败、不触发冷却
- **上游 HTTP 连接池**(`server.go:buildHTTPTransport`+`upstream_connection_age.go`):所有渠道统一开启连接复用,每主机最多保留 20 条空闲连接,空闲超时 90 秒;Antigravity 只保留标准 HTTP/1.1 与按 refresh token 隔离 Transport 的协议/身份边界,不再覆盖连接池参数。Anthropic OAuth 按渠道账号身份与代理地址隔离 Transport（账号 UUID 优先,邮箱次之,缺失时用令牌/凭证摘要）,同一账号的令牌轮换保持连接池,不同账号不会复用物理连接或 uTLS 会话缓存。两类凭证共用有上限的 LRU 客户端缓存,淘汰与关闭服务时清理空闲连接。启动时以持久化渠道总数计算每个 Transport 的 `MaxIdleConns=min(max(渠道数,1)×2,1024)`;渠道代理和凭证各有隔离 Transport,所以该值不是进程级套接字硬上限,运行期增删渠道不会改写已在用的 Transport
- **上游重定向**(`upstream_connection_age.go:refuseCrossOriginRedirect`):直连、渠道代理与 OAuth 凭证池的 Client 只跟随同源(scheme+host+端口，默认端口归一)重定向，最多 10 跳。net/http 跨域只剥 `Authorization`/Cookie，`x-api-key`、`x-goog-api-key` 与渠道自定义凭证头会原样带走，所以跨源一跳直接失败为 `util.ErrUpstreamInvalidResponse`(502、渠道级、切换下一个渠道),不把 3xx 透传给客户端。需要读 `Location` 的专用请求自行设 `CheckRedirect`

- **上游 HTTP/2 健康探测**(`server.go:buildHTTPTransport`+`codex_utls_transport.go:newCodexUTLSH2Transport`):普通 HTTPS 与专用 uTLS H2 连接共用 30 秒无入站帧后发 PING、15 秒无应答关闭连接的策略。只作用于实际使用 H2 的连接,不改变 H1/代理协议选择;这是连接级探测,不替代首字节/流式总超时、下游 SSE 心跳或 WebSocket 心跳,也不恢复已中断的生成。

## 动态思考等级

- 系统设置 `reasoning_auto` 使用 JSON：`enabled`、`decision_channel_id`、`decision_model`、`business_models`、`timeout_ms`。默认关闭，空业务模型名单不启用任何模型；名单按去除思考后缀后的逻辑模型精确匹配，不支持通配符。超时范围 100–10000 ms，默认 2000 ms；保存后重启生效。
- 判断服务沿用渠道管理，判断渠道只能是 System One 渠道。System One 是认证类型 `auth_type=system_one`（由“添加 → 决策渠道”入口新建，中文界面统称“决策渠道”，之后不可改），URL 不声明协议，凭证与存储口径同 API Key（`model.UsesAPIKeyCredentials`），不可带 OAuth 凭证或管理账户。它是独立上游决策能力，不增加聊天协议转换，下游只经 `POST /v1/systemone` 透传使用；模型发现走 `GET /v1/models`（取 `models[].name`），定时聊天检测跳过它；手动测试发送原生 `POST /v1/systemone`，测试请求不带 `client_protocol`（带了返回 400），普通渠道仍必填。
- 日志可空 `reasoning_decision` JSON 是持久化和管理 API 契约，决策调用日志以请求协议 `system_one` 区分（无独立来源）；开启 Debug 日志时，判断调用与手动测试同代理尝试一样保存实际发送的 System One 请求（凭证脱敏）及上游原始响应，未形成响应时记录传输错误；三种数据库及混合存储读写均需保留。旧日志元数据为空时行为不变。TypeSafe 旧错误分析设置与 `jev` 日志来源继续退役。

## 已下线设置

- TypeSafe（Jev）错误分析已移除。`TypeSafe_enabled`/`TypeSafe_api_key` 旧行保留在 `system_settings` 中不删，迁移不再播种；`admin_settings.go:retiredSettingKeys` 让列表/单项查询不展示、批量写入按未知设置拒绝。历史 `log_source=jev` 日志行同样不删，但前后端不再识别该来源：读出按 proxy 归一，仅在“全部日志”中可见，不再有 Jev 专属标记、消息解析和 Debug 入口。
- 设置页将 API Token 登录和程序更新并入高级，按全局冷却规则、Token 登录/渠道显示、更新间隔/渠道/检查按钮排序；容器说明紧邻只读更新控件。

## API Token 网页登录

- **趋势查询边界**：`/admin/metrics`、`/dashboard/metrics` 的 `bucket_min` 缺省为 5，显式传入必须为正整数且可表示为 Go 时长。按存储相同的对齐方式计算含首尾桶的数量，超过 10,000 桶返回 HTTP 400，调用方需缩短范围或加大粒度；不能让空数据补桶绕过资源上限。499 的已计费用量/成本保留在趋势中，次数与耗时仍排除取消请求。

- 系统设置 `api_token_login_enabled`、`api_token_show_channels` 均默认关闭，保存后重启生效。登录页始终保留 Token 入口；关闭时 `/login` 的 `api_token` 模式返回 403 和「未开启API Token登陆，请联系管理员」。启动时清除持久化 Token 网页会话，重新开启必须重新登录；管理员密码登录与普通 API 认证不受影响。
- 渠道显示只作用于 API Token 网页身份，管理员与未登录公开摘要保持原行为。隐藏时后端清空渠道筛选，裁剪渠道名称/ID、渠道选项及趋势中的渠道序列，前端隐藏相应列、筛选和图表。统计保留原有行和计算口径，不合并不同渠道的同名模型；投影不能修改缓存中的原始统计。开启显示也仅返回 Token 自身调用中的渠道身份，不返回渠道 URL、Key 或管理配置。

## 渠道管理与定时检测

- **渠道与 Key 写入原子性**：管理端创建渠道及其 Key、编辑渠道时替换完整 Key 集合，均在同一存储事务提交配置/模型与 Key；失败不遗留新渠道，也不删除旧 Key。模型刷新在同一事务校验渠道模型快照及完整 Key 行身份、凭据、禁用和范围策略后提交模型与 Key 范围，冲突重新读取；不得用旧探测结果覆盖提交窗口内的手工禁用、范围编辑或 Key 重排。

- **批量模型刷新**：`merge` 保留渠道已有模型行，`replace` 移除未返回的模型（仍保留失败 Key 所需模型及已配置的多目标组）。只有 Key 范围变化也返回 `updated` 并失效缓存。并发：前端每渠道单独请求、最多 4 个渠道同时在途；后端单渠道内逐 Key 探测最多 4 个并发（`modelFetchKeyConcurrency`），结果仍按 Key 顺序聚合，与串行一致；30 秒超时由该渠道全部 Key 共享。
  - 重算范围：`replace` 按每个成功探测的 Key 重算 `allowed_models`、`detected_models` 和空范围状态；`merge` 只重算 `model_scope_auto=true` 的 Key，不受限 Key 与手工白名单是策略而非探测产物，保持不变。重算以本次探测为准，不与旧范围取并集。失败或未探测 Key 保留原范围，模型名归一化时仍按原能力边界重映射。
  - 结果归属：探测结果按探测时的行 ID 与凭据重新绑定到写库前读取的 Key 行；同一凭据可配置多行，各行探测状态独立。探测期间被删除、重写或改值的 Key 视为未探测，不按 `key_index` 或凭据套用其他行的结果。
  - 匹配规则：按实际路由的最终上游模型匹配，包含自定义 body `model` 规则，Gemini 路径保持自身模型语义；编辑器探测（`editorFinalUpstreamModels`）使用同一规则。手工编辑渠道模型表保留已有探测范围；新模型不在该范围内时，需重新探测并应用 Key 范围。
  - 来源标记 `model_scope_auto`（持久化/API 字段）：`detected_models` 也保存归一化产生的手工目标边界，不能用非空判断来源。旧记录默认 false，不推断历史来源。覆盖刷新、编辑器明确应用探测结果（含范围不变仅转为自动）置 true；手工修改 Key 范围置 false；普通编辑、归一化和裁剪保留来源。API 省略该字段时仅在范围未变时保留原值；CSV 列 `api_key_model_scope_auto` 按 Key 顺序保存布尔数组，缺列同样保守保留。
- **渠道模型多行存储**：`channel_models` 仍以 `(channel_id, model)` 为主键；`model_variants` 保存同名模型的有序 JSON 行组，非空时为权威数据。旧 `redirect_model`/`disabled`/`pricing` 列只投影首个启用行（全停用则首行），以便旧查询读取代表值；旧库无 JSON 时按单行读取。损坏的 JSON 必须报读取错误，不得静默退回代表列。保存、批量导入和刷新都按请求模型+实际目标的行身份处理价格与停用状态。
- **渠道管理账户**(`channel_management_service.go`+`admin_channel_management.go`+`model/channel_management.go`):仅限 `auth_type=api_key` 且不含官方 OpenCode URL 的渠道(`supportsChannelManagement`),在 `oauth_credential` 字段存放版本化私有封套(`ChannelManagementEnvelope`,kind=`channel_management`,version=1)。三种 profile:`new_api`(New API,含 `user_id`、支持签到+余额)、`sub2api`(Sub2API,仅余额)、`sub2api_pro`(Sub2API Pro,签到+余额)。所有写入走 `CompareAndSwapChannelManagement` CAS,并发安全;`acquireChannel` 渠道级互斥保证同一渠道的余额刷新/签到/设置修改序列化。每日自动签到由 `channel_management_scheduler.go` 驱动:启动立即补偿扫描+每分钟定时扫描,按服务器本地时间 `HH:MM` 判到期,`LastScheduledDay` CAS claim 保证幂等;4 worker 并发执行,签到结果写 `log_source=checkin` 审计日志。手动签到/余额刷新走 Admin API `POST /admin/channels/:id/management-account/{checkin,balance}`。请求体写出后拒绝 uTLS 重放(`errManagementRequestAlreadySent`),POST 结果不确定时回读状态判定(`uncertain`)。CSV 导入导出支持 `management_daily_checkin_enabled`/`management_daily_checkin_time` 列,`oauth_credential` 列同时承载 OAuth 凭证和管理封套。编辑器端点 `GET /admin/channels/:id/editor` 回填凭据(`channelManagementEditorView`)供前端渠道编辑弹窗的管理账户区显示
- **每日定时检测**(`channel_check_scheduler.go`):渠道独立配置 `scheduled_check_enabled`、`scheduled_check_interval_minutes`（1–1440 整数分钟，默认 300）、`scheduled_check_start_time`（服务端本地时间 `HH:MM`，默认 `00:00`）与 `scheduled_check_model`。每天从开始时间按间隔执行至当天结束，次日重新开始；按整分钟调度，停机/忙碌错过的时间点不补跑，渠道之间独立执行，同渠道禁止重叠。保存后从下一个未来计划时间点生效，继续遵守渠道开关与可用时段。OAuth 渠道复用管理测试的凭据准备流程，按需刷新过期凭据，不依赖 API Key 表；普通 Key 渠道按模型白名单选择未禁用、未冷却的 Key，全冷却时跳过。CSV 导入导出支持全部四项，旧 CSV 缺列时更新保留原值、新建使用默认值。启动迁移一次性将旧全局小时间隔换算为分钟并删除旧设置；旧全局为 0 时保持渠道检测关闭。
- **CodeBuddy OAuth 签到与余额**：复用 `managementCheckinLoop` 的服务器本地时间扫描，在每日 09:00、21:00 两个时点调用 CodeBuddy 计费接口签到并查询有效套餐余额；签到业务错误（例如今日已签到）不阻断余额查询。管理端可通过 `POST /admin/channels/:id/codebuddy-checkin` 手动签到，通过 `POST /admin/channels/:id/oauth-usage` 与批量用量接口手动刷新余额。余额快照通过 OAuth 凭证 CAS 写入 `oauth_usage`，渠道列表和 OAuth 用量卡片只展示安全的剩余积分值。

- **定时检测协议**：优先检测已声明协议的 URL，并按该 URL 的 `Protocols` 顺序发送原生协议请求；未声明时按 OpenAI → Anthropic → Codex → Gemini 探测，仅协议端点不支持时继续下一协议。切换 URL 后重新选择协议，不受渠道 `protocol_transform_mode` 影响；手动检测仍使用用户指定的客户端协议与渠道转换策略。

## 发布与更新

- 发布必须使用仓库 Skill:Codex 调 `$ccload-release`,Claude Code 调 `/ccload-release`;唯一源码在 `.agents/skills/ccload-release/`,`.claude/skills/ccload-release` 只是软链接
- 无参数默认 Beta;只有显式 `stable` 才发稳定版。Tag 只允许 `vX.Y.Z-beta.N` / `vX.Y.Z`
- `.github/workflows/test.yml` 是提交级唯一发布门禁:`master` 的完整 SHA 必须通过后端测试、Web 验证、构建、五平台交叉编译、lint 和 PostgreSQL 集成测试,发布脚本才允许打 Tag;交叉编译同时为 Release 预热按平台区分的 Go 构建缓存(Tag ref 的缓存互不可见,Release 只恢复不保存),GOCACHE 仅 master push 保存且保存前剔除本 Job 未用条目,防止滚动缓存无限膨胀。`.github/workflows/release.yml` 只校验 Tag、构建多平台产物并生成 Release 和 GHCR 镜像;Beta=`prerelease=true` 且不改 GitHub latest,镜像发布精确版本 Tag+`beta`;稳定版更新 GitHub latest,镜像发布精确版本 Tag+`latest`,且该稳定版为 SemVer 最高版本时同步把 `beta` 别名推进到它(存在更高 Beta Tag 时不动,禁止降级)——`beta` 别名语义=全渠道 SemVer 最高版本,与 `preview` 更新渠道一致
- GitHub Release 仅上传 ccLoad 各平台二进制和 `checksums.txt`,不再分发 `cursor-sdk-bridge-*` 旧更新器兼容附件;使用 `v4.7.3-beta.1` 旧更新器的用户需手动升级。
- Homebrew 使用本仓库 `Formula/ccload.rb` 作为 tap,安装四种 macOS/Linux Release 二进制并校验 SHA-256;稳定版 Release 成功后工作流更新 Formula 并普通推送 `master`,Beta 不更新。更新脚本拒绝版本回退。安装入口设置 `CCLOAD_CONTAINER=1` 复用现有外部更新托管模式,统一通过 `brew upgrade` 升级;服务工作目录为 Homebrew `var/ccload`,配置和数据保留在 Cellar 外。使用与失败恢复见 `docs/guide/deployment.zh-CN.md#homebrew`。
- 官方容器直接打包同一 Release 的 ccLoad Linux 二进制;Cursor SDK Bridge 在镜像构建时从 Cursor 官方下载 `bridge.lock` 锁定版本并校验 SHA-256。`CCLOAD_CONTAINER=1` 时不启动版本检查或进程内更新,`auto_update_*` 设置只读;稳定版/测试版分别通过 `latest`/`beta` 镜像标签切换
- 非容器部署的单一更新管理器同时负责前端版本提示和可选自动应用;默认 `auto_update_channel=stable`,`preview` 同时考虑稳定版/测试版并按 SemVer 取最高版本;`auto_update_interval_hours=0` 只关闭定时检查——设置页「检测更新」按钮走 `POST /admin/update/check`(`HandleManualUpdate` → `UpdateManager.CheckNow`,互斥单飞),在任何间隔值下都执行完整检查/校验/替换流程;容器部署不注册该入口

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
- **Claude 重置并发控制**：按组织在当前进程内拒绝并发兑换，兑换前重新查询资格，每次操作最多发送一次兑换 POST。结果为 `unknown` 时不自动重试，管理员必须重新查询券状态后再决定是否兑换；不保存租约、组织封锁或确认结果，不提供 `Idempotency-Key` 结果重放。
