# model-test 协议转换测试设计

## 背景

`web/model-test.html` 当前把 `testChannelType` 同时用于两件不同的事：

1. 作为按模型测试模式的渠道筛选条件。
2. 作为 `/admin/channels/:id/test` 请求里的 `channel_type`，决定测试请求协议。

这两个职责混在一起，导致页面语义是错的。协议转换功能接入后，如果继续复用这个字段，会出现：

1. 用户以为自己在选“显示哪些渠道”，实际却改了“发什么协议的请求”。
2. 按模型测试模式无法表达“只看支持某个客户端协议的渠道”。
3. 后端测试接口无法区分“渠道上游协议”和“本次测试的客户端协议”。

根因不是缺一个选项，而是缺少明确的协议边界。

## 目标

1. 移除 model-test 页面中的“类型”选择。
2. 增加“协议转换”单选控件。
3. 协议转换默认值始终等于当前上下文中的渠道上游协议。
4. 点击“开始测试”时，根据协议转换选择，执行真实的协议转换测试。
5. 按模型测试模式改为按“客户端协议可见性”过滤渠道，而不是按上游类型过滤。

## 非目标

1. 不修改渠道编辑页的协议转换配置方式。
2. 不新增多选协议测试。
3. 不改现有代理主链路的协议转换规则。
4. 不引入新的协议类型。

## 方案结论

采用单一协议源模型：

1. 前端显式维护一个 `selectedProtocol`，语义是“本次测试对客户端暴露的协议”。
2. 渠道自身的 `channel_type` 继续表示上游协议。
3. 后端测试接口新增 `protocol_transform` 字段，语义同样是“客户端协议”。
4. 如果 `protocol_transform == channel_type`，走原生测试。
5. 如果 `protocol_transform != channel_type`，复用现有 `protocol.Registry` 做请求和响应转换。

这样每个字段只负责一件事，边界清楚，不再让 UI 文案和请求行为互相撒谎。

## 前端设计

### 页面结构

`web/model-test.html`

把现有“类型”下拉替换成“协议转换”单选容器。单选项固定使用四种协议：

1. `anthropic`
2. `codex`
3. `openai`
4. `gemini`

原因：

1. 渠道配置页已经把协议集合标准化为这四种。
2. model-test 只需要选择一个客户端协议，不需要额外抽象。

### 状态模型

`web/assets/js/model-test.js`

新增状态：

1. `selectedProtocol`

删除旧语义：

1. `typeSelect` 不再承担筛选或请求协议职责。

保留两个模式，但重定义它们的数据来源：

1. 按渠道测试：数据源是当前选中的单个渠道。
2. 按模型测试：数据源是 `channelsList` 中 `SupportsProtocol(selectedProtocol)` 的渠道集合。

### 默认值规则

按渠道测试：

1. 选中渠道后，`selectedProtocol` 默认重置为该渠道的 `channel_type`。
2. 用户可改成该渠道 `protocol_transforms` 中任意一个协议。
3. 如果当前选择的协议不再被该渠道支持，立即回退到该渠道的 `channel_type`。

按模型测试：

1. 初始默认值取首个渠道的 `channel_type`。
2. 切换模式时保留当前 `selectedProtocol`。
3. 如果当前协议在仓库中没有任何渠道支持，则页面保留选择，但展示空状态。

### 渲染规则

新增帮助函数：

1. `getSupportedProtocols(channel)`：返回 `[channel_type, ...protocol_transforms]` 去重结果。
2. `channelSupportsProtocol(channel, protocol)`：判断渠道是否暴露该客户端协议。
3. `getAllModelsForProtocol(protocol)`：汇总所有支持该协议的渠道模型。
4. `getChannelsSupportingModel(protocol, model)`：从支持该协议的渠道里筛选模型。

按模型测试模式的新语义：

1. 模型列表来自“支持当前协议的全部渠道”的模型并集。
2. 渠道列表来自“支持当前协议且支持该模型”的渠道。

### 发起测试

开始测试时，前端请求体改为发送：

```json
{
  "model": "...",
  "stream": true,
  "content": "...",
  "protocol_transform": "openai"
}
```

不再发送旧的 `channel_type` 作为测试协议。

## 后端设计

### 请求结构

`internal/testutil/types.go`

给 `TestChannelRequest` 新增字段：

1. `ProtocolTransform string 'json:"protocol_transform,omitempty"'`

保留 `ChannelType` 仅用于兼容旧调用方，但 model-test 新实现不再依赖它。

### 测试语义

`internal/app/admin_testing.go`

新增规则：

1. 默认客户端协议为渠道自己的 `cfg.GetChannelType()`。
2. 如果请求携带 `protocol_transform`，则把它当作客户端协议。
3. 若所选协议不是渠道原生协议，也不在 `cfg.ProtocolTransforms` 中，直接返回错误。
4. 若客户端协议与上游协议不同，构建真实 transform plan。

### 执行路径

对测试链路做最小侵入改造：

1. 原生协议测试仍沿用现有 `ChannelTester.Build/Parse`。
2. 协议转换测试不再试图让 `ChannelTester` 假装支持所有协议。
3. 使用“客户端协议测试器 + 协议注册表 + 上游协议”三段式流程：
   - 客户端协议测试器负责构造客户端请求体和解析客户端响应体。
   - `protocol.Registry` 负责把客户端请求转换成上游请求，以及把上游响应转换回客户端响应。
   - 上游 HTTP 请求头和路径由真实上游协议决定。

请求侧：

1. 用客户端协议测试器生成“客户端请求体”和客户端路径。
2. 用 `protocol.BuildTransformPlan` 基于 `clientProtocol -> upstreamProtocol` 生成计划。
3. 复用 `protocolRegistry.TranslateRequest` 转成上游请求体。
4. 按上游协议修正请求路径和认证头。

响应侧：

1. 非流式响应通过 `TranslateResponseNonStream` 转回客户端协议，再交给客户端协议测试器解析。
2. 流式响应复用现有 SSE 聚合逻辑，把上游事件流转译后重新拼成客户端协议流，再做测试结果抽取。

这样测试链路与正式代理链路共用同一套协议转换规则，避免“测试能过，真实请求却不通”的假阳性。

## 兼容性

1. 现有直接调用 `/admin/channels/:id/test` 并只传 `channel_type` 的代码保持可用。
2. 新页面优先使用 `protocol_transform`。
3. 当旧调用方未传 `protocol_transform` 时，行为与当前实现一致。

## 测试计划

前端：

1. HTML 测试验证“类型”控件消失，“协议转换”控件存在。
2. JS 测试验证按模型测试模式改为按协议过滤渠道和模型。
3. JS 测试验证开始测试时发送 `protocol_transform` 而不是 `channel_type`。
4. JS 测试验证切换渠道时默认协议回落到渠道上游协议。

后端：

1. 新增接口测试验证 `protocol_transform` 默认值为渠道上游协议。
2. 新增接口测试验证非法协议会被拒绝。
3. 新增接口测试验证仅当渠道支持该协议时允许测试。
4. 新增测试验证 `openai -> anthropic` 这类转换请求会真正经过 `protocolRegistry`。

## 风险

1. 流式测试转换如果重新实现一套解析器，容易和代理主链路漂移。
2. 旧代码里 `channel_type` 到处被当成“请求协议”使用，改动时必须限制在 model-test 测试链路，不扩散。
3. 按模型测试模式的空状态文案会改变，需要补测试，避免手机端布局回归。

## 验收标准

1. 页面上不再出现“类型”选择。
2. 页面上出现“协议转换”单选。
3. 渠道模式默认选中渠道原生协议，用户可改选该渠道支持的额外协议。
4. 模型模式只展示支持当前协议的渠道和模型。
5. 开始测试时，协议转换请求能真实命中后端协议转换逻辑。
6. 未配置该协议的渠道不会被错误地展示或测试。
