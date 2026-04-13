# 渠道协议转换设计

日期：2026-04-12

## 结论

当前 `ccLoad` 的根问题不是缺少几个协议判断，而是把 `channel_type` 同时拿来表示“上游真实协议”和“客户端可见协议面”。这个语义污染已经蔓延到：

- `web/channels.html` 的渠道编辑
- `/v1/models` 与 `/v1beta/models` 的模型可见性
- 候选渠道筛选
- 实际转发时的请求头、路径、响应解析

本次设计必须拆语义：

- `channel_type` 继续只表示上游真实协议
- 新增 `protocol_transforms` 表示额外暴露给客户端的协议面
- 一个渠道最终支持的客户端协议集合为 `{channel_type} ∪ protocol_transforms`

不接受把 `channel_type` 继续做双重语义，也不接受只改前端或只改 `/v1/models` 的半套实现。

## 目标

- 在 `web/channels.html` 的渠道编辑弹窗中新增协议转换多选，支持 `anthropic`、`codex`、`openai`、`gemini`
- `/v1/models` 与 `/v1beta/models` 除原有按渠道类型过滤外，还能返回声明了相应协议转换的渠道模型
- 真正接入请求/响应协议转换链路，覆盖 `Anthropic`、`Codex`、`OpenAI`、`Gemini`
- 复用 `CLIProxyAPI` 的转换思路，但不直接引入整套 executor，不升级 `ccLoad` 的模块结构为外部网关
- 保留 `ccLoad` 现有的重试、冷却、多 URL、流式 usage 统计、SSE 错误分类和计费链路

## 非目标

- 不引入 `antigravity`、`gemini-cli`、`qwen` 等无关协议面
- 不重写现有路由、负载均衡或冷却管理
- 不把模型抓取逻辑改成按 transform 抓取
- 不在本次设计中处理所有 OpenAI 边缘端点，只覆盖 `ccLoad` 当前主要使用的文本生成和模型视图入口

## 方案选择

已评估三种方案：

1. 直接依赖 `CLIProxyAPI` 的 `sdk/translator`
2. 裁剪并内置 `CLIProxyAPI` 所需 translator 到 `ccLoad/internal/protocol`
3. 只做请求体改写，响应尽量透传

最终选择方案 2，原因：

- 方案 1 会把 `go.mod` 从 `go 1.25.0` 推到 `go 1.26`，并引入明显更大的依赖面
- 方案 3 是垃圾方案，流式、工具调用、usage、错误事件都会失真
- 方案 2 成本更高，但边界最清楚，可把依赖和维护面压在 `ccLoad` 自己的协议层里

## 数据模型

### 1. 渠道主表

`channels.channel_type` 保持不变，只表示真实上游协议：

- `anthropic`
- `codex`
- `openai`
- `gemini`

### 2. 新增关联表

新增表：`channel_protocol_transforms`

字段：

- `channel_id`
- `protocol`

约束：

- 主键：`(channel_id, protocol)`
- `protocol` 只允许 `anthropic/openai/gemini/codex`
- 不存与 `channel_type` 相同的值

选择独立表而不是逗号字符串的原因：

- 候选渠道筛选和模型视图本质是查询问题，不应退化成字符串解析
- 缓存层可以直接构建 `channelsByExposedProtocol` 索引
- CSV 仍可对外暴露逗号列，但内部结构保持干净

### 3. API 结构

`ChannelRequest` 新增字段：

- `ProtocolTransforms []string \`json:"protocol_transforms,omitempty"\``

`model.Config` 新增字段：

- `ProtocolTransforms []string \`json:"protocol_transforms,omitempty"\``

`ChannelWithCooldown`、渠道详情、列表接口都回传这个字段。

### 4. 语义规则

定义：

- `upstream_protocol = channel_type`
- `supported_protocols = {channel_type} ∪ protocol_transforms`

示例：

- `channel_type=gemini`
- `protocol_transforms=[openai, anthropic]`

则该渠道：

- 上游真实请求发往 Gemini
- `/v1beta/models` 可见
- OpenAI `/v1/models` 可见
- Anthropic 模型视图可见
- OpenAI / Anthropic 客户端请求命中后，需要进入协议转换链

## 路由与模型视图

### 1. 入站协议解析

新增独立概念：`ClientProtocol`

入站请求协议解析规则：

- `/v1/messages...` => `anthropic`
- `/v1/responses...` => `codex`
- `/v1beta/...` => `gemini`
- `/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`、`/v1/images/...` => `openai`
- `/v1/models` 特殊规则：
  - 有 `anthropic-version` 头 => `anthropic`
  - `User-Agent` 以 `claude-cli` 开头 => `anthropic`
  - `User-Agent` 含 `codex` => `codex`
  - 其他默认 `openai`

现有 `DetectChannelTypeFromPath` 继续服务路径匹配，但不再承担“上游协议”和“客户端协议”双重职责。

### 2. 模型视图

模型视图按暴露协议查，不按 `channel_type` 查。

- `/v1beta/models`：返回所有 `supported_protocols` 含 `gemini` 的模型
- `/v1/models`：
  - Anthropic 视图：返回所有 `supported_protocols` 含 `anthropic` 的模型
  - Codex 视图：返回所有 `supported_protocols` 含 `codex` 的模型
  - OpenAI 视图：返回所有 `supported_protocols` 含 `openai` 的模型

响应格式保持客户端期望格式：

- Anthropic 视图返回 Claude 风格模型列表
- Codex 和 OpenAI 视图返回 OpenAI 风格模型列表
- Gemini 视图返回 Gemini 风格模型列表

### 3. 候选渠道筛选

现有 `selectCandidatesByChannelType` / `selectCandidatesByModelAndType` 基于 `cfg.GetChannelType()` 过滤，这必须改掉。

正确顺序：

1. 根据 `ClientProtocol` 找出暴露了该协议的渠道
2. 再按模型精确匹配、模糊匹配筛选
3. 再做冷却、成本限额过滤
4. 选择 URL、Key、故障转移

这保证了：

- `gemini` 上游渠道只要声明了 `openai` transform，就能被 OpenAI 请求命中
- `channel_type` 不再误杀可转换渠道

## 协议转换架构

### 1. 新包结构

新增 `internal/protocol`，保持薄适配器结构：

- `types.go`
  - `ClientProtocol`
  - `UpstreamProtocol`
  - `TransformPlan`
- `registry.go`
  - 注册请求/响应转换器
- `request.go`
  - 请求体、路径转换
- `response.go`
  - 非流式与流式响应转换
- `builtin/`
  - 从 `CLIProxyAPI` 裁剪的 4 协议转换器

第一版目标 pair：

- `openai <-> gemini`
- `openai <-> anthropic`
- `openai <-> codex`
- `codex <-> gemini`
- `codex <-> anthropic`
- `anthropic <-> gemini`
- 必要的 no-op / normalize 适配

实现校准（2026-04-13，按当前代码而不是按目标）：

- 当前 runtime 实际已落地方向是：
  - `openai -> gemini`
  - `openai -> anthropic`
  - `openai -> codex`
  - `anthropic -> gemini`
  - `codex -> gemini`
  - `codex -> anthropic`
  - `codex -> openai`
  - same-protocol no-op
- 只有 `openai <-> codex` 已经严格双向；其余 pair 仍是单向实现。
- 根因不是少了几个 `Register`，而是当前实现仍以入站 normalize 能力为边界：`request_prompt.go` 没有 `normalizeGeminiConversation`，`types.go` 里的能力表也还是按“upstream + request family + source”白名单编码。
- 因此本文中的 `<->` 在设计语境里表示目标 pair，不等于当前 shipped 覆盖；实现验收必须用显式方向矩阵核对。

不纳入：

- `antigravity`
- `gemini-cli`
- `qwen`
- 任何 executor 或认证逻辑

### 2. TransformPlan

每次渠道尝试生成一个 `TransformPlan`，包含：

- `ClientProtocol`
- `UpstreamProtocol`
- `OriginalPath`
- `UpstreamPath`
- `OriginalBody`
- `TranslatedBody`
- `OriginalModel`
- `ActualModel`
- `Streaming`
- `NeedsTransform`

### 3. 请求链插入点

插入点必须在“选择候选渠道之后、真正构造上游请求之前”。

具体顺序：

1. 先保持现有候选渠道、Key、URL 选择逻辑
2. `prepareRequestBody` 完成模型重定向和请求体中的 model 替换
3. 根据 `ClientProtocol + channel_type` 生成 `TransformPlan`
4. 如果需要转换：
   - 改写请求体
   - 改写上游路径
5. `buildProxyRequest` 按 `channel_type` 注入认证头
6. 发往上游

要求：

- 认证头永远按真实上游协议注入
- 路径永远按真实上游协议构造
- `ClientProtocol` 只影响翻译，不影响上游认证

### 4. 响应链插入点

响应链不能破坏现有 usage 解析和错误判断。

原则：

- 内部判定继续看上游原始响应
- 客户端看到的是转换后的响应

非流式：

1. 读取完整上游 body
2. 用原始 body 做 usage/error/计费解析
3. 再将原始 body 转成客户端协议 body
4. 写回客户端

流式：

1. 每个上游 chunk / SSE event 先喂给上游 parser
2. 再送入响应 translator
3. translator 产出的 chunk 写回客户端
4. SSE 错误、1308、流完整性、usage 提取仍基于上游 parser

### 5. 保留现有稳定机制

以下能力必须继续按上游原始协议工作：

- `newSSEUsageParser(channelType)`
- `newJSONUsageParser(channelType)`
- SSE `error` 事件识别
- 1308 配额错误分类
- 首字节超时
- 流中断诊断
- 成本计算与 service tier 提取

也就是说，translator 只改变客户端可见字节流，不改变内部观测语义。

## 管理端与前端

### 1. 渠道编辑

`web/channels.html` 中：

- 现有 `channel_type` 单选改文案为“上游协议”
- 新增 `protocol_transforms` 多选，文案为“额外协议转换”
- 选项为 `Anthropic/Codex/OpenAI/Gemini`
- 不包含原生协议，不允许重复

交互规则：

- `fetchModelsFromAPI()` 仍只按 `channel_type` 抓取
- `addCommonModels()` 仍只按 `channel_type` 推荐预设模型
- 渠道测试、定时检测模型、URL 测试也只看 `channel_type`

### 2. 列表与详情

- 渠道卡片增加 transform 标签展示
- 编辑时正确回填多选值
- 保存时提交 `protocol_transforms`

### 3. 本地化

需要补充中英文文案：

- `channels.modal.upstreamProtocol`
- `channels.modal.protocolTransforms`
- `channels.modal.protocolTransformsHint`
- `channels.protocolTransformOpenAI`
- `channels.protocolTransformAnthropic`
- `channels.protocolTransformCodex`
- `channels.protocolTransformGemini`

## CSV 导入导出

### 1. 导出

新增列：

- `protocol_transforms`

格式：

- 逗号分隔，例如 `openai,anthropic`
- 空值表示无额外转换

### 2. 导入

规则：

- 允许列缺失，缺失时默认为空
- 非空时按逗号拆分
- trim、转小写、去重
- 仅允许 `anthropic/openai/gemini/codex`
- 与 `channel_type` 相同的值自动剔除

## 存储与缓存

### 1. 存储接口

新增方法：

- `GetEnabledChannelsByExposedProtocol(ctx, protocol string)`
- `GetEnabledChannelsByModelAndProtocol(ctx, modelName, protocol string)`

现有 `GetEnabledChannelsByType` 可以保留给少数上游真实协议场景，但不再承担入站协议筛选职责。

### 2. SQL 查询

新增对 `channel_protocol_transforms` 的 join / exists 查询：

- 直接 `channel_type == protocol` 的渠道算命中
- `channel_protocol_transforms.protocol == protocol` 的渠道也算命中

### 3. 缓存

现有 `channelsByType` 扩展为：

- `channelsByUpstreamType`
- `channelsByExposedProtocol`

缓存刷新时一次性构建两套索引，避免运行时反复拆解。

## 迁移

数据库迁移步骤：

1. 新增 `channel_protocol_transforms` 表
2. 不回填历史数据
3. 历史渠道默认 `protocol_transforms=[]`
4. 新代码发布后，旧渠道仅保持原生协议暴露

这个迁移是安全的，因为：

- 不改历史 `channel_type`
- 不改变现有渠道默认行为
- 只有用户显式勾选 transform 才扩展暴露协议面

## 测试计划

### 1. 前端

- 渠道新增时能提交 `protocol_transforms`
- 编辑时能正确回填
- 抓模按钮仍只按 `channel_type` 发请求
- 渠道卡片与模态框展示正确

### 2. 管理端

- 创建/更新渠道时校验 `protocol_transforms`
- 列表/详情回传正确
- CSV 导入导出 round-trip 正确

### 3. 模型视图

- `gemini` 上游 + `openai` transform 出现在 OpenAI `/v1/models`
- `gemini` 上游 + `anthropic` transform 出现在 Anthropic 模型视图
- 未声明 transform 的渠道不会错误暴露

### 4. 转发链路

至少覆盖这些组合的非流式与流式：

- OpenAI -> Gemini
- Anthropic -> Gemini
- Codex -> Gemini
- OpenAI -> Anthropic
- Codex -> Anthropic

同时验证：

- usage 统计仍正确
- SSE error 仍能触发冷却
- 首字节时间和流完整性诊断不回归
- 多 URL 和重试逻辑不被 translator 破坏

## 风险与约束

### 1. 最大风险

最大风险不是 translator 本身，而是“错误地把转换后的响应拿去做内部 usage/error 判定”。那会直接污染：

- 冷却策略
- 日志
- 计费
- 流完成检测

因此内部判定必须继续基于上游原始协议响应。

### 2. 裁剪风险

方案 2 需要从 `CLIProxyAPI` 裁剪转换器，存在两个风险：

- 裁剪不完整导致某些协议面工具调用或流式事件结构缺失
- 后续上游格式演化时，`ccLoad` 要自己维护

为控制范围，本次只裁剪与 `Anthropic/OpenAI/Codex/Gemini` 四协议直接相关的部分。

## 当前基线状态

在隔离工作树 `/Users/caidaoli/Share/Source/go/ccLoad/.worktrees/protocol-transforms` 上执行：

```bash
go mod download
go test -tags go_json ./internal/...
```

结果：

- 大部分包通过
- `internal/util` 当前已有失败，不是本次设计引入

失败明细：

- `TestAnthropicModelsFetcher`
- 错误：`获取失败: 上游配置错误 (HTTP 400): Missing anthropic-version header`

这说明当前基线不是全绿，后续实现阶段必须把该失败视为既有问题，单独判断是否需要顺手修正。

## 当前实现落地状态（2026-04-12）

本设计在当前工作树中已经落地到以下范围：

- `protocol_transforms` 已完成：
  - 模型层字段
  - 数据库存储与查询
  - 缓存索引
  - Admin API
  - CSV 导入导出
  - `web/channels.html` 编辑弹窗与保存载荷
  - `/v1/models` / `/v1beta/models` 暴露协议筛选

- 已打通的真实转换链路：
  - `OpenAI -> Gemini` 非流式/流式
  - `Anthropic -> Gemini` 非流式/流式
  - `Codex -> Gemini` 非流式/流式
  - `OpenAI -> Anthropic` 非流式/流式
  - `Codex -> Anthropic` 非流式/流式

- 已明确收口的风险：
  - Gemini 上游路径使用 `actualModel`，不回退到 `originalModel`
  - 流式翻译支持 `text/plain` SSE fallback
  - 未支持的结构化请求内容（image/tool/file/非 message item）会明确返回 400，而不是静默吞掉或伪装成 502

当前仍未实现的部分：

- 更完整的结构化内容支持（目前是“明确拒绝”，不是“真正转换”）
- 其余未覆盖的协议对
- 本地最终全量验证与提交

## 本地最终验收建议

当前沙箱存在环境限制：

- 不能创建 `.git/worktrees/.../index.lock`，因此无法在此环境中提交
- `httptest.NewServer` 在部分测试里会因为端口绑定权限失败；当前已确认 `go test -tags go_json ./internal/... -v` 在跑完大部分包后，会卡在 `internal/util/TestAnthropicModelsFetcher` 的 `listen tcp6 [::1]:0: bind: operation not permitted`
- Go 构建缓存默认落到 `~/Library/Caches/go-build`，当前沙箱无写权限；需要显式设置 `GOCACHE`
- `golangci-lint` 在未重定向缓存目录时会被同样的缓存权限问题误伤；将 `GOCACHE` 和 `GOLANGCI_LINT_CACHE` 指到工作树后，`golangci-lint run ./...` 已通过

因此，最终验收请在本地正常环境执行：

```bash
cd /Users/caidaoli/Share/Source/go/ccLoad/.worktrees/protocol-transforms

# 1. Go 全量
go test -tags go_json ./internal/... -v

# 2. 竞态
go test -tags go_json -race ./internal/...

# 3. 前端
make web-test

# 4. Lint
golangci-lint run ./...
```

当前沙箱内已确认的验证命令：

```bash
mkdir -p .cache/go-build .cache/golangci-lint
GOCACHE=$(pwd)/.cache/go-build go test -tags go_json ./internal/... -run '^$'
GOCACHE=$(pwd)/.cache/go-build go test -tags go_json ./internal/... -v
GOCACHE=$(pwd)/.cache/go-build go test -tags go_json ./internal/protocol/... ./internal/app -run 'ProtocolTransforms|ExposedProtocol|OpenAIToGemini|AnthropicToGemini|CodexToGemini|OpenAIToAnthropic|CodexToAnthropic|UnsupportedStructured|UsesResolvedActualModel|TextPlainSSE' -v
GOCACHE=$(pwd)/.cache/go-build go test -tags go_json -race ./internal/protocol ./internal/app -run 'ProtocolTransforms|ExposedProtocol|OpenAIToGemini|AnthropicToGemini|CodexToGemini|OpenAIToAnthropic|CodexToAnthropic|UnsupportedStructured|UsesResolvedActualModel|TextPlainSSE'
GOCACHE=$(pwd)/.cache/go-build go test -tags go_json -race ./internal/storage/... ./internal/util -run 'GetEnabledChannelsByExposedProtocol|Classify|ChannelType' -v
GOCACHE=$(pwd)/.cache/go-build GOLANGCI_LINT_CACHE=$(pwd)/.cache/golangci-lint golangci-lint run ./internal/protocol/... ./internal/app/... ./internal/storage/... ./internal/util/...
GOCACHE=$(pwd)/.cache/go-build GOLANGCI_LINT_CACHE=$(pwd)/.cache/golangci-lint golangci-lint run ./...
make web-test
```

建议额外人工烟测：

1. 新建 `gemini` 上游渠道，勾选 `openai` / `anthropic` / `codex`
2. 分别用 `/v1/chat/completions`、`/v1/messages`、`/v1/responses` 调用同一模型
3. 确认：
   - 模型视图可见
   - 请求命中正确上游
   - 非流式与流式都返回目标客户端协议格式
   - 未支持的结构化内容返回 400

## 实施原则

- 保持 `ccLoad` 现有代理、重试、冷却、URL 选择逻辑不变
- translator 只做协议适配，不接管执行器职责
- `channel_type` 永远只表示上游真实协议
- `protocol_transforms` 永远只表示额外暴露协议面
- 模型抓取永远按真实上游协议
- `/v1/models` 与候选渠道永远按暴露协议集合

这个边界如果被打破，代码会重新滑回现在这种语义污染状态。
