# 模型目录自动同步设计

## 根因

ccLoad 把三种不同职责混在静态源码里：

- `internal/util/cost_calculator.go` 同时保存模型价格、模型名解析规则和计费规则。
- `web/assets/js/channels-modals.js` 维护另一份“常用模型”静态列表。
- 新模型或官方调价必须修改源码、重新构建并发布，即使计费算法本身没有变化。

真正的问题不是缺少定时任务，而是缺少独立、可热替换的模型目录。models.dev 的价格按 `provider + model` 组织，ccLoad 当前按模型名使用官方基准价，再由渠道 `cost_multiplier` 修正实际成本。同步层必须完成 provider 归一化，不能把远端 JSON 直接塞进请求链路。

## 目标

- 使用 `https://models.dev/api.json` 自动同步可明确映射的官方厂商模型和基础价格。
- 启动时立即检查，此后默认每 6 小时检查一次；`model_catalog_sync_interval_hours=0` 关闭网络同步。
- 同步成功后无锁、原子地替换运行时目录，不重启 ccLoad。
- 网络、格式或校验失败时继续使用最后成功快照；没有快照时使用内置价格。
- 新增普通 token 计费模型或官方基础调价不再要求发布 ccLoad。
- 管理页“常用模型”从运行时目录获取协议原生厂商的最新模型，失败时使用内置列表。

## 非目标

- 不让请求计费路径直接访问 models.dev。
- 不把 models.dev 当成有 SLA 的权威服务。
- 不自动解释新的计费语义。service tier、Anthropic fast mode、缓存 token 口径、图像工具计费和按次计费仍由本地规则控制。
- 不根据第三方聚合商价格替换官方基准价；第三方差价继续使用渠道 `cost_multiplier`。
- 不增加独立价格微服务、分布式锁或数据库快照表。

## 数据源与厂商映射

同步端点固定为 `https://models.dev/api.json`，请求携带上次成功响应的 `ETag`。HTTP `304` 表示目录未变化，不重建快照。

首版使用以下有序官方 provider allowlist；顺序同时是模型 ID 冲突时的确定性优先级：

- `openai`
- `anthropic`
- `google`
- `xai`
- `deepseek`
- `alibaba`
- `zai`
- `minimax`
- `moonshotai`
- `xiaomi`
- `mistral`

区域版、Coding Plan、Token Plan、聚合商、云平台代理和社区 provider 一律忽略。一个模型 ID 若在多个 allowlisted provider 下重名，使用上述显式优先级；不依赖 JSON map 遍历顺序。

管理页常用模型只使用协议原生厂商：Anthropic 渠道取 `anthropic`，OpenAI/Codex 渠道取 `openai`，Gemini 渠道取 `google`。其他官方厂商只参与计价目录；它们的渠道模型列表继续通过现有 `/v1/models` 获取。

## 运行时目录

新增独立的不可变目录快照。请求热路径只读取 `atomic.Pointer` 指向的完整快照，不加锁、不解析 JSON。

目录合并顺序：

1. models.dev 已校验的官方价格。
2. 内置 `basePricing` 兜底价格。
3. 本地别名解析。
4. 基于合并后模型 ID 动态生成的最长前缀匹配索引。

远端价格只覆盖它能够表达的数值字段：基础 input/output、显式 cache read 和 context tier。现有本地条目中的 `CacheReadCountsTowardTier`、固定按次费用及其他行为标记必须保留。远端不存在的特殊字段不得清零。

对于远端新增模型，目录创建普通 token 计费条目。无法表达的计费结构跳过并记录警告，不能猜价格。

静态 `fuzzyPrefixes` 改为从“内置目录 + 远端目录”生成，按前缀长度降序并按首字符分桶。这样日期后缀或供应商后缀仍能匹配，新一代模型不需要手工追加前缀。

## models.dev 适配器

适配器只负责将不稳定上游转换为 ccLoad 内部结构：

- HTTP 超时 15 秒。
- 响应体硬限制 16 MiB。
- 只接受 HTTP 200 或 304。
- 拒绝空 provider、空 model ID、NaN/Inf、负价格、缺失 input/output 的计费条目。
- context tier 必须有正阈值和完整 input/output 价格，按阈值排序后转换。
- 任一 allowlisted provider 缺失或没有有效 token 计费模型时拒绝整次更新。
- 单个模型错误时跳过该模型并记录数量；不能因为一个坏条目丢弃全部有效目录。

上游 schema 变化只能导致同步失败和继续使用旧快照，不能影响代理请求。

## 持久化与失败回退

最后成功的“归一化快照”写入 `data/model-catalog.json`，不保存 3 MiB 原始响应。路径可通过 `CCLOAD_MODEL_CATALOG_CACHE` 覆盖，主要用于测试和非标准部署。

写入流程使用同目录临时文件、`fsync`、原子 rename，文件权限 `0600`。持久化失败不撤销已经通过校验的内存更新，但必须记录错误；下次重启会退回更旧的持久化快照或内置价格。

启动顺序：

1. 安装内置目录。
2. 加载并校验本地最后成功快照，存在则原子安装。
3. 若同步间隔大于零，启动受 Server 生命周期管理的同步协程并立即请求一次。

`0` 只关闭网络同步，不禁止加载已有快照。

## 调度与生命周期

新增同步协程挂在 `Server` 上，复用 `baseCtx`、`shutdownCh` 和 `wg`。同步使用 `atomic.Bool` 防重入；上一轮未结束时跳过本轮。

配置项：

- `model_catalog_sync_interval_hours`
- 类型：`float`
- 默认：`6`
- 允许：`>= 0`
- `0`：关闭网络同步
- 修改后沿用现有设置机制，重启生效

同步请求使用 `context.WithTimeout(s.baseCtx, 15*time.Second)`，`Shutdown()` 取消后必须及时退出。

## 管理 API 与前端

新增鉴权 Admin API：

`GET /admin/model-catalog/common?channel_type=<type>`

返回：

- `models`: 最多 6 个模型 ID。
- `source`: `models.dev`、`cache` 或 `embedded`。
- `fetched_at`: 可为空的 RFC3339 时间。

筛选规则固定且可解释：

- 只取协议原生官方 provider。
- 排除 `deprecated`。
- 只取具有 input/output token 价格且输出支持 text 的模型。
- 同时存在无日期别名和日期版本时优先无日期别名。
- 按 `last_updated`、`release_date`、model ID 稳定排序，取前 6 个。

`addCommonModels()` 改为异步调用该接口，继续复用现有大小写去重和表单 dirty 逻辑。接口失败、返回空列表或页面尚未加载完成时使用当前内置 `COMMON_MODELS`，因此管理操作不依赖外网同步成功。

## 计费边界

`CalculateCostDetailed`、usage 归一化、缓存 token 拆分、`OpenAIServiceTierMultiplier`、`CalculateFastModeCost`、图像工具计费和渠道 `cost_multiplier` 的职责保持不变。

允许的最小调整是让 token tier 携带显式 cache-read 价格，以正确表达 models.dev 的 context tier。旧内置 tier 未设置该字段时继续使用现有缓存规则。

新的特殊模式若 models.dev 只通过 `experimental` 字段表达，首版忽略。普通价格自动更新和新计费语义自动执行是两回事；后者必须有协议解析与回归测试后才能启用。

## 测试与验证

只测试有价值的公开行为和模块边界：

- models.dev JSON 归一化：provider allowlist、价格、context tier、非法值和确定性冲突优先级。
- `CalculateCostDetailed` 使用远端新模型、远端调价、tier cache-read，并在清除远端快照后回退内置价格。
- 同步 HTTP：200、304、超时、超大响应、无效 JSON、失败保留旧快照。
- 持久化快照跨重启加载，损坏文件回退内置目录。
- 调度间隔为 0 时不联网；Shutdown 后协程退出；同步不重入。
- Admin API 按 channel type 返回稳定的 6 个模型，并在无远端目录时返回内置来源。
- 前端验证接口结果会进入最终提交的模型数组，接口失败走内置列表；不断言 CSS 或 DOM 包装结构。

最终运行：

```bash
go test -tags sonic ./internal/...
make verify-web
golangci-lint run ./...
make build
```

## 可观测性

每次同步只记录一条结果日志：`updated`、`not_modified` 或 `failed`。成功日志包含模型数、provider 数、耗时和 ETag；失败日志包含阶段和错误，不输出完整响应体。未知模型继续沿用现有返回成本 `0` 的行为。
