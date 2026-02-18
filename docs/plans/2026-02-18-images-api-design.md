# Images API 代理支持设计

## 目标

支持 `/v1/images/generations` 和 `/v1/images/edits` 两个 OpenAI Images API 端点的透明代理，复用现有的渠道选择、故障切换、计费架构。

## 需求

- 路由：images 路径自动匹配 OpenAI 渠道类型
- 模型提取：从 JSON body（generations）和 multipart/form-data（edits）中提取 model 字段
- 计费：优先从响应 usage 字段计费，无 usage 时按请求次数记录
- 重试：复用现有 Key/渠道级故障切换
- Body 大小：images 路径使用 20MB 上限（支持图片上传），其他保持 2MB

## 方案选择

**方案 A（采用）：最小化路径扩展**
改动 2 个文件约 30-40 行代码，完全复用现有架构。

淘汰方案：
- B（独立 Handler）：重复转发/重试/计费逻辑，违反 DRY
- C（中间件抽象）：过度工程，只有 images 需要 multipart 支持

## 具体改动

### 1. 路由匹配 — `internal/util/channel_types.go`

OpenAI 的 PathPatterns 增加 `"/v1/images/"`，使用现有的 `MatchTypePrefix` 匹配。

### 2. Multipart Model 提取 — `internal/app/proxy_handler.go`

`parseIncomingRequest` 增加分支：当 JSON unmarshal 未提取到 model 且 Content-Type 为 `multipart/form-data` 时，解析 multipart boundary 从 form field "model" 提取。body 原始字节不做修改，原样转发上游。

### 3. Body 大小按路径区分 — `internal/app/proxy_handler.go`

`parseIncomingRequest` 中根据请求路径判断：`/v1/images/` 前缀使用 20MB 上限，其他保持 2MB 默认值。

## 无需改动

| 模块 | 原因 |
|------|------|
| 转发逻辑 | body 原始字节透传，Content-Type header 原样复制 |
| 流式检测 | multipart/JSON body 均无 stream 字段，正确返回 false |
| Usage 提取 | jsonUsageParser 已支持 input_tokens/output_tokens |
| 故障切换 | 现有 Key/渠道级重试逻辑通用 |
| 成本计算 | cost_calculator 已有 gpt-image-1 等定价 |
