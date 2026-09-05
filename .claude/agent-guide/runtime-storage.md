# 配置、管理账户、更新与存储

修改对应运行机制时读取相关章节。渠道级限流见 [routing.md](routing.md)，Responses WebSocket 生命周期见 [proxy.md](proxy.md)；原「关键机制」中的引导期配置说明位于本文件「系统设置与连接生命周期」章节。

## 系统设置与连接生命周期

- **系统设置无热重载,唯一例外是多模态回退映射**(`config_service.go`+`admin_settings.go`):`LoadDefaults` 启动读一次进内存,运行期只读;单改/重置/批量三个写入口都是写库后 `go s.triggerRestart()`,2 秒后重启进程生效。重启回调属于 `Server` 实例并由锁保护,禁止恢复为包级可变全局。例外由 `settingsRequireRestart` 判定:仅当一次提交的**唯一修改键**是 `model_multimodal_fallback` 时跳过重启,`commitSettingUpdates` 把持久化与运行态发布绑定成同一有序操作(含该映射的提交整体串行化,防并发提交让数据库终值与运行态快照错序),代理热路径只做一次原子快照读取。除该键外别在 `AdminUpdateSetting` 里加"顺手刷新缓存"——重启才是生效机制
- **引导期配置只能是环境变量**:`ConfigService` 依赖已建好的 `storage.Store`,建库阶段消费的配置不可能迁进系统设置(要读设置得先开库,要开库得先知道设置)。`SQLITE_PATH`/`SQLITE_JOURNAL_MODE`(拼 DSN,`factory.go:buildSQLiteDSN`)、`CCLOAD_MYSQL`/`CCLOAD_POSTGRES`/`CCLOAD_ENABLE_SQLITE_REPLICA`/`CCLOAD_SQLITE_LOG_DAYS`(`factory.go:NewStore`)全属这一类,保持环境变量;运行期策略才进系统设置
- **全局限额与冷却时长**(`server.go:loadServerRuntimeConfig`):均为系统设置,启动读一次,改后重启生效。`max_concurrency`(全局并发信号量;三层同名警告见渠道级限流条)、`max_body_bytes`/`max_image_body_bytes`(Images 路径独立上限,同时约束 Responses WS 帧与 transcript,注入见 `newRequestBodyLimits`)、`cooldown_{auth,server,timeout,rate_limit,min,max}_seconds`(`loadCooldownSettings` 读出 `util.CooldownSettings`,经 `Store.ConfigureCooldown` 注入;下限>上限时整对回退默认)。旧 `CCLOAD_MAX_CONCURRENCY`/`CCLOAD_MAX_BODY_BYTES`/`CCLOAD_COOLDOWN_*` 已废弃,仍设置时启动打 WARN
- **下游请求读取超时**(`config/defaults.go`+`server.go:loadHTTPReadTimeout`+`main.go`):系统设置 `http_read_timeout_seconds`(秒,0=内建默认 120 秒,负数回退默认),启动读一次注入 `http.Server.ReadTimeout`,改后重启生效。它覆盖**请求头+请求体的整段读取**,和 `max_body_bytes` 是两件事:体积超限立即 413(`errBodyTooLarge`),读取超时是 408(`errBodyReadTimeout`),两条错误文案分别点名对应设置,别再互相误判——注意调大体积上限反而会让原本快速 413 的请求改为等到读取超时才失败
- **上游超时**(`server.go:loadProtocolTimeouts`):`upstream_first_byte_timeout`(0=禁用,仅流式)、`stream_timeout`(0=禁用,流式总时长)、`non_stream_timeout`(120s),首字节与非流式超时可按实际上游协议 `{protocol}_*` 覆盖;写回前调 `disableResponseWriteTimeout` 防 `WriteTimeout` 截断响应体
- **上游连接最长复用时间**(`upstream_connection_age.go`+`codex_upstream_websocket.go`):`upstream_connection_reuse_limit_seconds`(默认 0=不限制)统一约束直连及渠道代理池中的 HTTP/1.1、HTTP/2、WebSocket 物理连接;达到时限后不再接收新请求,空闲连接立即关闭,在途请求/turn 完成后关闭,新请求自动建连。原生 WS 重连语义见「Responses WebSocket 会话与资源」;计划轮换不记失败、不触发冷却

## 渠道管理与定时检测

- **渠道管理账户**(`channel_management_service.go`+`admin_channel_management.go`+`model/channel_management.go`):仅限 `auth_type=api_key` 渠道,在 `oauth_credential` 字段存放版本化私有封套(`ChannelManagementEnvelope`,kind=`channel_management`,version=1)。三种 profile:`new_api`(New API,含 `user_id`、支持签到+余额)、`sub2api`(Sub2API,仅余额)、`sub2api_pro`(Sub2API Pro,签到+余额)。所有写入走 `CompareAndSwapChannelManagement` CAS,并发安全;`acquireChannel` 渠道级互斥保证同一渠道的余额刷新/签到/设置修改序列化。每日自动签到由 `channel_management_scheduler.go` 驱动:启动立即补偿扫描+每分钟定时扫描,按服务器本地时间 `HH:MM` 判到期,`LastScheduledDay` CAS claim 保证幂等;4 worker 并发执行,签到结果写 `log_source=checkin` 审计日志。手动签到/余额刷新走 Admin API `POST /admin/channels/:id/management-account/{checkin,balance}`。请求体写出后拒绝 uTLS 重放(`errManagementRequestAlreadySent`),POST 结果不确定时回读状态判定(`uncertain`)。CSV 导入导出支持 `management_daily_checkin_enabled`/`management_daily_checkin_time` 列,`oauth_credential` 列同时承载 OAuth 凭证和管理封套。编辑器端点 `GET /admin/channels/:id/editor` 回填凭据(`channelManagementEditorView`)供前端渠道编辑弹窗的管理账户区显示
- **每日定时检测**(`channel_check_scheduler.go`):渠道独立配置 `scheduled_check_enabled`、`scheduled_check_interval_minutes`（1–1440 整数分钟，默认 300）、`scheduled_check_start_time`（服务端本地时间 `HH:MM`，默认 `00:00`）与 `scheduled_check_model`。每天从开始时间按间隔执行至当天结束，次日重新开始；按整分钟调度，停机/忙碌错过的时间点不补跑，渠道之间独立执行，同渠道禁止重叠。保存后从下一个未来计划时间点生效，继续遵守渠道开关与可用时段。CSV 导入导出支持全部四项，旧 CSV 缺列时更新保留原值、新建使用默认值。启动迁移一次性将旧全局小时间隔换算为分钟并删除旧设置；旧全局为 0 时保持渠道检测关闭。

## 发布与更新

- 发布必须使用仓库 Skill:Codex 调 `$ccload-release`,Claude Code 调 `/ccload-release`;唯一源码在 `.agents/skills/ccload-release/`,`.claude/skills/ccload-release` 只是软链接
- 无参数默认 Beta;只有显式 `stable` 才发稳定版。Tag 只允许 `vX.Y.Z-beta.N` / `vX.Y.Z`
- `.github/workflows/test.yml` 是提交级唯一发布门禁:`master` 的完整 SHA 必须通过后端测试、Web 验证、构建、lint 和 PostgreSQL 集成测试,发布脚本才允许打 Tag。`.github/workflows/release.yml` 只校验 Tag、构建多平台产物并生成 Release 和 GHCR 镜像;Beta=`prerelease=true` 且不改 GitHub latest,镜像发布精确版本 Tag+`beta`;稳定版更新 GitHub latest,镜像发布精确版本 Tag+`latest`,且该稳定版为 SemVer 最高版本时同步把 `beta` 别名推进到它(存在更高 Beta Tag 时不动,禁止降级)——`beta` 别名语义=全渠道 SemVer 最高版本,与 `preview` 更新渠道一致
- 官方容器直接打包同一 Release 的 Linux 二进制;`CCLOAD_CONTAINER=1` 时不启动版本检查或进程内更新,`auto_update_*` 设置只读;稳定版/测试版分别通过 `latest`/`beta` 镜像标签切换
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
