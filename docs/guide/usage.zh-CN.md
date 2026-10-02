# 使用说明

**[English](usage.md) | 简体中文** · [← 返回 README](../../README.zh-CN.md)

配置完成后即可通过兼容 API 调用：

## API 代理

**Claude API 代理（需授权）**：

先在 Web 界面配置 API 令牌，然后按 Claude API 兼容接口调用：

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-sonnet-4-6",
    "max_tokens": 1024,
    "messages": [
      {
        "role": "user",
        "content": "Hello, Claude!"
      }
    ]
  }'
```

**OpenAI 兼容 API 代理（Chat Completions）**：

OpenAI SDK 只需替换 `base_url` 即可接入，业务代码无需改动：

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {
        "role": "user",
        "content": "Hello!"
      }
    ]
  }'
```

**图像生成（Images API）**：

`POST /v1/images/generations` 兼容 OpenAI Images 接口，请求体上限独立由 `max_image_body_bytes` 控制。渠道模型是 `grok-4.6` 及之后的 xAI 对话模型时，ccLoad 会把请求自动桥接成 xAI Responses 的 `image_generation` 工具调用：非流请求聚合为标准 Images JSON，流式请求输出 `partial_image` / `completed` SSE 事件。

**Codex CLI 接入（推荐配置）**：

沿用官方内置的 `openai` provider，只改地址；不要为 ccLoad 自定义 `[model_providers.*]`：

```bash
# 以 API Key 方式登录，Key 填 ccLoad 的 API 令牌
printenv CCLOAD_API_TOKEN | codex login --with-api-key
```

```toml
# ~/.codex/config.toml
openai_base_url = "http://localhost:8080/v1"
```

内置 provider 会发送 `version` 头、启用 Responses WebSocket 与独立 web search；API Key 登录且设置了地址时，客户端只使用自带模型目录（每个模型的指令、工具形态和 responses-lite 开关都随客户端版本发布），不会拉取 ccLoad 合成的 `/models`。自定义 provider 默认不发 `version`、不启用 WebSocket，线上请求与官方客户端直连不一致。

**Codex Responses WebSocket**：

下游 WebSocket 和上游 WebSocket 是两个独立开关：认证后的客户端始终可以升级 `GET /v1/responses`，也可以使用 Codex 直连别名 `GET /v1/codex/responses` 或 `GET /backend-api/codex/responses`；渠道的 `websockets` 只决定 ccLoad 是否尝试连接原生 Codex 上游 WebSocket。未启用该字段的渠道仍可通过 HTTP/SSE 桥接参与候选和故障切换。

在 `/web/channels.html` 中选择包含 Codex 能力 URL 的渠道，勾选“原生 WebSocket”并点击“检测”即可启用。使用 Admin API 时对应的关键字段如下；URL 仍填写 `http://` 或 `https://` 地址，ccLoad 会在原生 WS 请求时转换为 `ws://` 或 `wss://`：

```json
{
  "urls": [{"url": "https://upstream.example.com", "protocols": ["codex"]}],
  "websockets": true
}
```

以 `websocat` 为例连接下游：

```bash
websocat \
  -H='Authorization: Bearer your-api-token' \
  -H='Session-Id: stable-conversation-id' \
  ws://localhost:8080/v1/responses
```

连接后发送文本帧。第一轮必须使用 `response.create` 并包含 `model`；后续轮次可使用 `response.append` 和上一轮返回的 `response.id`：

```json
{"type":"response.create","model":"your-model","input":[{"type":"message","role":"user","content":"Hello"}]}
```

```json
{"type":"response.append","previous_response_id":"resp_xxx","input":[{"type":"message","role":"user","content":"Continue"}]}
```

客户端持续读取 Responses 事件，直到收到 `response.completed`、`response.done`、`response.incomplete`、`response.failed` 或 `error`。只支持文本帧；二进制帧会返回 `unsupported_frame`。

故障切换只处理分类为可重试的 Key、模型或渠道级上游错误。客户端参数错误、无法转换的请求和消息过大不会切换。跨 Key、URL、渠道或传输的切换只发生在下游尚未提交任何可见的非心跳事件时；原生 WS 在同一上游上的一次内部重连使用下面单独说明的语义边界。

| 当前上游 | 后续动作 | ccLoad 行为 | 客户端行为 |
|---|---|---|---|
| HTTP/SSE 在提交可见事件前失败 | 切换到另一个 HTTP/SSE 候选 | 内部切换并重放完整 transcript | 无需处理 |
| 原生 WS 连接中断或 `previous_response_not_found`，且尚无语义事件 | 重连同一上游 | 内部重连一次并重放完整 transcript | 无需处理 |
| 原生 WS 在提交可见事件前失败 | 切换到 HTTP/SSE 或另一个原生 WS 候选 | 内部切换并重放完整 transcript | 无需处理 |
| 原生 WS 握手被拒绝或握手 EOF | 回退同渠道、同 Key、同 URL 的 HTTP/SSE | 内部回退并重放完整 transcript | 无需处理 |
| HTTP/SSE | 下一候选是原生 WS | 发送 `502/server_error/upstream_unavailable`，随后以 close code `1011` 关闭下游 | 使用相同会话标识重连，并发送不带 `previous_response_id` 的完整会话输入 |

原生 WS 的同一上游内部重连把 `response.created`、`response.queued` 和 `response.in_progress` 视为非语义事件，因此在这些事件之后仍可重连一次；其他事件都越过该重连边界。注意，这三个生命周期事件仍是已经提交给下游的可见事件，不能据此承诺继续跨候选切换。一旦文本、推理、工具调用或其他实际输出已经转发，ccLoad 不再切换或重放，避免重复输出、工具调用和费用。消息过大使用 close code `1009`，也不会故障切换。

所有渠道上游 HTTP Transport（包括 Antigravity 按凭证隔离的 HTTP/1.1 连接池）统一开启连接复用：每主机最多保留 20 条空闲连接，空闲 90 秒后关闭。启动时按持久化渠道总数为每个 Transport 设置总空闲连接容量，即渠道数的 2 倍，最少 2、最多 1024。渠道代理和 Antigravity 凭证仍拥有隔离的 Transport，因此 1024 是单个 Transport 的上限，不是整个进程的套接字硬上限。

重连时必须使用相同的 API 令牌和稳定的 execution 请求头。`Session-Id` 表示顶层 Codex 会话；存在 `Thread-Id` 时，ccLoad 组合两个请求头建立身份，使主代理和每个子代理线程分别拥有独立的 transcript、Response ID 和 turn lock。没有 `Thread-Id` 的客户端继续使用原 `Session-Id` 契约。`prompt_cache_key`、请求体 `session_id` 及其他缓存路由提示不代表 execution session，不会触发本地串行，也不会共享本地会话状态。execution session 是单进程内存状态：新安装默认最多保留 256 个会话，进程级 transcript 有效载荷总预算为 256 MiB；已有数据库记录不迁移。空闲 TTL 继续默认 15 分钟（小内存机器可设为 10 分钟）。下游全部断开 5 分钟后，每分钟运行的清理器会关闭上游物理连接，因此实际回收时间约为 5–6 分钟，但会话 transcript 会继续保留到 TTL。稳定会话及其已提交 transcript 在 TTL 到期前绝不会因会话容量或内存预算压力被逐出。会话数达到上限时只拒绝新的会话身份，已有稳定会话仍可继续。已提交载荷超预算后，包括已有会话在内的所有新回合都会在触达上游前被拒绝。两类限制都通过 WebSocket `429/rate_limit_error/rate_limit` 事件返回；客户端应等待 TTL 回收后重试，或修改设置并重启。重启会丢失内存会话，因此客户端随后必须发送不带 `previous_response_id` 的完整会话输入。

Transcript 预算是新工作准入阈值，不是严格分配上限：已经准入的回合允许完成并提交。除已配置预算外，有限的最坏超量为 `responses_ws_max_sessions × max_body_bytes`。进程重启不会恢复会话或累计会话指标。多实例部署必须使用粘性路由保证重连命中同一实例；否则客户端应发送不带 `previous_response_id` 的完整会话输入。会话数、TTL 和 transcript 预算可在系统设置中通过 `responses_ws_max_sessions`、`responses_ws_session_ttl_minutes`、`responses_ws_max_transcript_bytes` 调整。`GET /admin/runtime-metrics` 的 `transcript_bytes` 表示当前有效载荷字节数，不包含 Go 运行时、WebSocket 缓冲区和请求处理中临时对象的开销；同一响应还提供 WebSocket 拒绝、日志队列/落库失败，以及混合存储主库同步积压、失败、丢弃和最后成功时间。

**Codex Alpha Search（仅原生透传）**：

`POST /v1/alpha/search` 接收 Codex 原生搜索请求，`model` 字段可省略。该请求族没有本地转换路径：ccLoad 会在所有模型兼容渠道上尝试 Codex 原生端点，按 URL 缓存端点缺失结果，然后切换到下一个 URL 或渠道。

```bash
curl -X POST http://localhost:8080/v1/alpha/search \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -d '{
    "query": "golang channels"
  }'
```

普通渠道 URL 会自动追加 `/v1/alpha/search`。精确 URL 需要设置 `exact: true`，且 `url` 已指向完整端点，例如 `{"url":"https://upstream.example.com/v1/alpha/search","exact":true,"protocols":["codex"]}`。转发前会移除 Responses 专用字段 `prompt_cache_key` 和 `prompt_cache_retention`。

## 模型思考后缀

任意协议入口都支持在模型名尾部追加思考后缀，例如 `claude-sonnet-4-6(high)`、`gpt-5.2(xhigh)`、`gemini-3.1-pro(8192)`。ccLoad 先剥离后缀完成路由，再按实际转发的上游协议把等级写进请求体的思考参数（Anthropic `thinking`、OpenAI/Codex `reasoning.effort`、Gemini `thinkingBudget`）：

- **等级**：`minimal` / `low` / `medium` / `high` / `xhigh` / `max`；超出上游模型能力时收敛到最近可用档
- **关闭**：`(none)` 或 `(0)` 关闭思考
- **自动**：`(auto)` 交由上游默认思考策略
- **数字预算**：`(16384)` 等非负整数按 token 预算下发（Anthropic `budget_tokens`、Gemini `thinkingBudget`）

后缀不是模型身份：选路、鉴权、冷却、日志和发往上游的模型名一律使用基名，渠道模型列表无需登记带后缀的条目。HTTP 代理、Responses WebSocket 和管理后台的渠道测试都支持该后缀；渠道自定义请求规则晚于后缀生效，可覆盖它写入的字段。括号内容不是已知等级或非负整数的模型名（如上游真的叫 `foo(bar)`）原样透传。

## 多模态回退

系统设置 `model_multimodal_fallback` 以 JSON 对象 `{"文本模型":"回退模型"}` 为不支持视觉的模型配置回退模型（最多 64 条映射 / 8 KB；key 按小写基名归一，value 可带思考后缀）。请求含图片、文件等非文本内容时，ccLoad 在思考后缀处理与令牌、渠道、Key 过滤**之前**把模型整体改写为回退模型——选路、冷却与日志全部跟随回退模型。HTTP 入口检测客户端协议的请求体；Responses WebSocket 回合检测**完整 transcript**，因此历史里进入过的图片会让后续每一轮都稳定落在回退模型上。在设置页打开 **多模态回退模型** 即可编辑映射。与其他所有系统设置不同，只保存该映射时立即生效；单次提交触及其他设置仍会在约 2 秒后重启进程。

## 本地 Token 计数

发送请求前可用本地 Token 估算接口预估消耗，不调用上游 API：

```bash
curl -X POST http://localhost:8080/v1/messages/count_tokens \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [
      {"role": "user", "content": "Hello, how are you?"}
    ],
    "system": "You are a helpful assistant."
  }'

# 响应示例
# {
#   "input_tokens": 28
# }
```

**特点**：
- ✅ 符合 Anthropic 官方 API 规范
- ✅ 本地估算，不调用上游，不消耗 API 配额
- ✅ 支持系统提示词、工具定义、大规模工具场景
- ✅ 需授权令牌访问（在 Web 管理界面 `/web/tokens.html` 配置令牌）

## 渠道管理

渠道可通过 Web 界面或 Admin API 管理：

通过 Web 界面 `/web/channels.html` 或 API 管理渠道：

```bash
# 添加渠道，并逐 URL 声明协议能力
curl -X POST http://localhost:8080/admin/channels \
  -H "Authorization: Bearer your_admin_token" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Claude-API",
    "api_key": "sk-ant-api03-xxx",
    "urls": [
      {"url": "https://api.anthropic.com", "protocols": ["anthropic"]},
      {"url": "https://api2.anthropic.com"}
    ],
    "protocol_transform_mode": "auto",
    "priority": 10,
    "rpm_limit": 0,
    "max_concurrency": 0,
    "models": [{"model": "claude-sonnet-4-6"}, {"model": "claude-opus-4-6"}],
    "enabled": true
  }'
```

**OpenAI 兼容上游示例**：

```bash
# 添加使用 OpenAI 线协议的渠道
curl -X POST http://localhost:8080/admin/channels \
  -H "Authorization: Bearer your_admin_token" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "OpenAI-Compatible",
    "api_key": "sk-xxx",
    "urls": [
      {"url": "https://api.openai.com", "protocols": ["openai"]}
    ],
    "protocol_transform_mode": "auto",
    "priority": 10,
    "rpm_limit": 0,
    "max_concurrency": 0,
    "models": [{"model": "gpt-4o"}],
    "enabled": true
  }'
```

> 任何 OpenAI 兼容服务均可使用，只需把 `urls[].url` 改为它的 API 基础地址。不要包含 `/v1` 或具体端点路径，ccLoad 会按所选协议自动追加。`protocols: ["openai"]` 声明将该渠道作为 OpenAI 上游路由。

> **协议行为说明**：每个 `urls` 条目可通过 `protocols` 声明 `anthropic`、`codex`、`openai`、`gemini` 能力，非空列表是权威配置。`upstream`、`auto`、`local` 三种模式如何决定实际上游协议，见[协议路由](architecture.zh-CN.md#协议路由)。

> **多URL说明**：`urls` 是有序的 `{url, exact, protocols}` 对象数组。`exact: true` 表示该地址已经是完整上游请求 URL。系统按延迟加权选择 URL，并对故障 URL 独立冷却；local 模式会先把显式声明协议的 URL 稳定排到自动 URL 前面，各组内部顺序不变。

> **模型条目说明**：`models` 的每个元素是 `{model, redirect_model, disabled, pricing}`。同一渠道可多次填写同一个 `model`，用不同 `redirect_model` 作为轮转目标；每行可独立停用、定价。请求按组内启用且未冷却的行轮转，Key/URL/协议重试保持同一目标；全部冷却时选择最早恢复的行。精确重定向后允许再查找目标模型的首个启用行一次（`A→B→C` 会发送到 C），不会推进 B 组的轮转游标，也不会继续查找第三层。`disabled: true` 仅停用该行，组内全停用时模型才从对外列表消失。管理测试可用 `redirect_model` 指定目标，省略时按组轮转；定时检测只使用启用行。

> **Key 模型白名单**：`api_keys` 的每个元素可带 `allowed_models` 字符串数组，限定这个 Key 只服务哪些模型；省略、留空或写 `"*"` 都表示不限制，保持原有行为。列出的模型必须已存在于该渠道的 `models` 中（渠道声明通配模型时除外），否则保存被拒；保存时按渠道模型名归一大小写并去重，编码后不超过 2000 字节。匹配的是**渠道逻辑模型**：先模糊匹配、再比对白名单，`redirect_model` 重定向在这之后发生，所以白名单填渠道模型名而不是上游模型名。请求先按模型过滤 Key 再进入 Key 重试；某渠道所有 Key 都不服务该模型时直接跳过该渠道，不冷却也不记失败。Web 界面在 Key 行的 **模型范围** 中勾选，并可用 **检测此 Key** 探测上游实际支持的模型再自动匹配渠道模型。适合同一中转站下不同 Key 拥有不同模型权限的场景。

> **独立 Key 中转回退**：当同一中转站下的不同 Key 实际对应不同服务商时，可在渠道编辑器的 **高级设置 → 其他** 中启用 **渠道故障优先换 Key**。遇到可重试的模型级或渠道级上游故障（如 5xx、连接错误、首字节超时）时，ccLoad 会先冷却当前 Key 并尝试本渠道的其他 Key，全部 Key 都不可用后才切换其他渠道。该选项默认关闭，保持原有的模型/渠道冷却行为。

> **RPM限制说明**：`rpm_limit` 是渠道级请求数上限，按滚动 60 秒窗口统计；`0` 表示不限制。代理转发、手动测试、单 URL 测试和定时检测都会计入，达到上限后该渠道会被跳过；多 URL 故障重试按实际发出的上游 HTTP 请求计数。计数保存在当前进程内，服务重启会清空，多实例部署时各实例独立统计。

> **并发限制说明**：`max_concurrency` 是渠道级同时在飞请求上限；`0` 表示不限制。槽位从发起上游请求前占用，到响应体关闭后释放，流式请求会占用到流结束；达到上限后该渠道会被跳过，不触发冷却。计数保存在当前进程内，多实例部署时各实例独立统计。

### Z.ai Coding Plan（ZCode）

在渠道管理中选择 **Z.ai Coding Plan**，可完成浏览器授权，或直接导入已有的 Coding Plan API Key。提供商浏览器 OAuth 暂时不可用时，仍可通过 API Key 导入接入。

ccLoad 会在创建或刷新渠道时优先读取账号的 Coding Plan 模型目录，失败后回退到 models.dev，最后才使用内置列表。渠道卡片也可刷新并展示 Coding Plan 的额度窗口。

### Cursor

在渠道管理中选择 **Cursor** 并导入 Cursor User API Key。ccLoad 会用它换取控制面会话；不提供无法用于推理的浏览器登录和 `accessToken` 导入。身份和额度刷新走 `api2.cursor.sh`；模型目录直接读取 SDK Bridge 的 `ListModels`，保存 Cursor 返回的模型 ID，并补充 SDK 支持的 `-fast` 形式，不生成思考等级变体。启动时只要存在 Cursor 渠道，ccLoad 就会在后台查找并探活已有 `cursor-sdk-bridge`；没有可用 Bridge 时下载官方锁定版本、校验内置 SHA-256，并原子安装到 `cursor-sdk/bin/<version>`：设置 `SQLITE_PATH` 时位于数据库旁，否则使用操作系统用户缓存，最后才回退系统临时目录。该过程不阻塞 HTTP 服务启动，不需要安装 Cursor CLI；手动或离线安装可从 [Cursor SDK Bridge 官方发布页](https://github.com/cursor/sdk-bridge/releases)下载。

SDK Agent 只开放 Cursor 的 `mcp` capability group：SDK custom tools 通过 Cursor 合成的 `custom-user-tools` MCP server 暴露，网关本机的 shell、文件等其他内建工具仍被禁用。客户端函数通过 `LocalAgentOptions.custom_tools` 注册；ccLoad 在经过鉴权的 loopback 地址提供 `SdkCustomToolCallbackService`，把原生回调转换为 Anthropic `tool_use` 或 OpenAI `tool_calls`，并挂起对应 Agent，直到客户端下一轮交回匹配结果。同一 Cursor 渠道的并发请求按 `agent_id` 隔离会话、按 `call_id` 路由回调。

渠道卡片可刷新包含额度 / API / Auto 三个花费窗口（`DashboardService/GetCurrentPeriodUsage`）。

### Zed

在渠道管理中选择 **Zed** 并完成原生登录。这不是 OAuth code/PKCE 流程：每次登录在随机 loopback 端口生成临时 RSA-2048 密钥，把 PKCS#1 DER 公钥以 base64url 传给 `zed.dev/native_app_signin`，再用 RSA-OAEP/SHA-256 把回调的 `access_token` 解密成长期 native credential；临时私钥绝不持久化。`system_id` 是可选的 Zed 安装标识，主要用于试用权限绑定真实安装（表单值 → `CCLOAD_ZED_SYSTEM_ID` → 本机 Zed `db/0-global/db.sqlite`）；没有该值时仍可发起登录并尝试换取令牌，请求会省略 `x-zed-system-id`，由上游决定账号是否具备试用权限；禁止生成随机值或复制其他机器的固定值，同账号重授权保留已存值。

数据请求先用 native credential 经 `/client/llm_tokens` 换短期 JWT（提前 60 秒单飞刷新并 CAS 持久化），再以 `Authorization: Bearer` 调 `/completions`。渠道固定 exact `/completions`、codex 协议 + local 转换、禁用 WebSocket；ccLoad 动态暴露 `/models` 中能跨 OpenAI/Anthropic/Google 提供商完成 wire 转换的模型。请求会包进 Zed `thread_id/prompt_id/intent/provider/model/provider_request` envelope；`plan` 403 只冷却当前模型并切换渠道，其他 401/403 才刷新凭证。

### 管理账户（API Key 渠道）

API Key 渠道可以选择绑定上游管理账户，用于余额查询和每日签到。在渠道编辑器中打开 **高级设置 → 管理账户**，选择 profile 并填入上游凭据：

| Profile | 余额 | 签到 | 说明 |
|---------|------|------|------|
| **New API** | ✅ | ✅ | 可选 `user_id`，适配多租户站点 |
| **Sub2API** | ✅ | ❌ | 仅查询余额 |
| **Sub2API Pro** | ✅ | ✅ | 基于订阅的余额，含日/周/月额度窗口 |

**每日自动签到**：在管理账户面板中启用并设置时间（HH:MM，服务器本地时间）。ccLoad 每分钟扫描一次；错过的时间窗口会在下次扫描时补偿。签到结果以 `checkin` 日志来源记录审计日志，可在日志页面查看。不支持签到的 profile（Sub2API）会被静默跳过。

**手动操作**：渠道卡片提供 **刷新余额** 和 **签到** 按钮，也可通过 Admin API 调用：
```bash
# 刷新余额
curl -X POST http://localhost:8080/admin/channels/:id/management-account/balance \
  -H "Authorization: Bearer your_admin_token"

# 手动签到
curl -X POST http://localhost:8080/admin/channels/:id/management-account/checkin \
  -H "Authorization: Bearer your_admin_token"
```

> **CSV 往返**：导出包含 `management_daily_checkin_enabled` 和 `management_daily_checkin_time` 列；导入可以只更新签到设置而不影响凭据。`oauth_credential` 列同时承载 OAuth 凭据和管理封套，可用于跨实例迁移。

## 自定义请求规则（高级）

渠道编辑弹窗底部「高级」按钮可打开二级模态，按渠道粒度改写转发给上游的 **HTTP 请求头** 与 **JSON 请求体**，常用于 `User-Agent` 覆写、强制版本头、微调 `thinking` / `max_tokens` 等字段。规则按配置顺序生效，保存后对该渠道后续所有请求立即生效。

**动作矩阵**:

| 对象 | `remove` | `override` | `append` |
|---|---|---|---|
| HTTP Header | 删除指定 header（支持对多值头按 token 精确剔除，如 `Anthropic-Beta`） | `Header.Set` 替换所有值 | `Header.Add` 追加一个值（多值头语义） |
| JSON Body | 按点分路径删除 key / 数组元素 | 按路径设置值，不存在则创建中间节点 | 不支持（JSON 语义模糊） |

**JSON 路径语法**:
- 点分路径 + 数字数组下标：`thinking.budget_tokens`、`messages.0.role`、`generation_config.temperature`
- 值支持任意 JSON 字面量：数字 `0.7`、布尔 `true`、字符串 `"claude-opus-4-6"`、对象 `{"type":"adaptive"}`、数组 `["a","b"]`

**安全约束**（硬保护，前端校验被绕过也由后端兜底）:
- **认证头黑名单**：`Authorization`、`x-api-key`、`x-goog-api-key`（大小写不敏感）任何规则一律忽略并写 `slog.Warn`
- **CRLF 注入防御**：header 名称/值禁止包含 `\r\n`
- **非 JSON body 静默跳过**：`Content-Type` 不含 `application/json`、body 为空、或反序列化失败时原样透传，不阻断请求
- **容量上限**：单渠道 header 规则 ≤ 32 条、body 规则 ≤ 32 条、单条 value ≤ 8 KB；违反返回 400

**典型示例**:
```jsonc
{
  "custom_request_rules": {
    "headers": [
      { "action": "override", "name": "User-Agent", "value": "claude-cli/1.0 (custom)" },
      { "action": "remove",   "name": "Anthropic-Beta", "value": "context-1m-2025-08-07" },
      { "action": "append",   "name": "Accept", "value": "application/json" }
    ],
    "body": [
      { "action": "override", "path": "thinking", "value": {"type":"adaptive"} },
      { "action": "override", "path": "max_tokens", "value": 4096 },
      { "action": "remove",   "path": "stop_sequences" }
    ]
  }
}
```

> **与内置逻辑的关系**：自定义规则在 anyrouter 的 `anthropic-beta` 注入和 anyrouter adaptive thinking 兜底之后生效，可覆盖或移除这些字段。项目生成的 Anthropic 请求用 `thinking.type=adaptive` + `output_config.effort` 控制思考深度；anyrouter `/v1/messages` 额外补齐缺失 thinking 并归一旧的 `thinking.type=enabled`。认证头无论何时都不可改写。

## 批量数据管理

渠道数量较多时，可用 CSV 导入导出批量维护配置：

**导出配置**:
```bash
# Web界面: 访问 /web/channels.html，点击"导出CSV"按钮
# API调用:
curl -H "Authorization: Bearer your_token" \
  http://localhost:8080/admin/channels/export > channels.csv
```

**导入配置**:
```bash
# Web界面: 访问 /web/channels.html，点击"导入CSV"按钮
# API调用:
curl -X POST -H "Authorization: Bearer your_token" \
  -F "file=@channels.csv" \
  http://localhost:8080/admin/channels/import
```

**CSV格式示例**:
```csv
name,api_key,urls,priority,model_entries_json,enabled
Claude-API-1,sk-ant-xxx,"[{""url"":""https://api.anthropic.com"",""protocols"":[""anthropic""]}]",10,"[{""model"":""auto"",""redirect_model"":""claude-sonnet-4-6""},{""model"":""auto"",""redirect_model"":""claude-opus-4-6""}]",true
```

**特性**:
- 支持中英文列名自动映射
- 智能数据验证和错误提示
- 增量导入和覆盖更新
- UTF-8编码，Excel兼容
- `oauth_credential` 同时包含 OAuth 凭据和 API Key 渠道管理账号封套，可用于跨实例迁移；CSV 属于敏感文件，使用后应立即妥善删除

升级说明：新导出的 CSV 只使用 `model_entries_json` 保存有序模型行及停用、价格配置。旧 `models`/`model_redirects`/`model_pricing` 列仍可单独导入；新旧模型列混用会被拒绝。原有的两步链式重定向 `A→B→C` 可与 A 的多目标轮转同时使用；不会继续跟随第三层。

## 监控与管理后台

管理后台提供请求、日志、Token 和渠道状态的实时视图：

![ccLoad管理界面](../../images/ccload-dashboard.jpeg)
![ccLoad日志界面](../../images/ccload-logs.jpg)
*实时监控大屏：Claude Code、Codex、OpenAI、Gemini四大平台数据一目了然*

**核心功能**：
- 📈 **24小时趋势图** - 请求量一目了然，高峰低谷清清楚楚
- 🔴 **实时错误日志** - 渠道异常可秒级发现；可直接从日志页中断进行中的请求——中断按上游断链分类并触发故障切换，不会被当作客户端取消
- 📊 **渠道调用统计** - 用数据判断渠道负载和可用性
- 💬 **模型测试工作台** - 支持按渠道、按模型和对话式模型测试：
  - 对话模式支持图片上传与粘贴，直接验证多模态请求
  - 可切换思考等级、模型内置搜索与流式输出，观察协议转换后的真实响应
  - 对话记录可导出为 Markdown / HTML，方便留档和复盘
  - 独立的图片生成标签页，可选 Images API 或 Chat Completions 生成图片，提示词在页面刷新后保留
- ⚡ **性能指标** - 延迟、成功率，性能瓶颈无处藏
- 💰 **Token用量统计** - 钱花哪了心里有数：
  - 自定义时间范围，想看哪段看哪段
  - 按API令牌分类，多租户也能分账
  - 支持Gemini/OpenAI缓存Token展示
- 🎛️ **日志列显隐自定义** - 点击齿轮图标按需显示/隐藏列，设置自动保存到浏览器
