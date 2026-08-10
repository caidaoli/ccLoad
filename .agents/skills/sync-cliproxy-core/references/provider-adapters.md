# Provider adapter allowlist

本文件定义可随四协议 core 原子同步的 provider-specific 纯转换语义。逐文件映射、显式排除、生产接线和契约测试的机器可读唯一 allowlist 是 [provider-adapters.manifest](provider-adapters.manifest)；版本、commit、实际已同步状态只记录在 `internal/protocol/cliproxy/UPSTREAM.md`。不要在脚本中维护第二份 provider/file 清单。

## Antigravity

- 上游源目录：
  - `internal/translator/antigravity/claude`
  - `internal/translator/antigravity/gemini`
  - `internal/translator/antigravity/openai/chat-completions`
  - `internal/translator/antigravity/openai/responses`
- 本地目标：`internal/protocol/cliproxy/providers/antigravity/...`
- 同步 request、stream response、non-stream response、直接依赖的纯 helper 及对应行为测试。
- 排除每个目录的 `init.go`、`noop_optimization_test.go`、benchmark，以及 `internal/translator/antigravity/interactions`。Claude 上游的 request/response 大测试直接操纵签名缓存、动态 Registry 和 runtime logger，也明确排除；其纯 wire 契约由本地 provider 与 `internal/app` 集成测试覆盖，不能为了照搬测试把运行时副作用重新引入转换层。
- 保留 web-search grounding、tool ID、thinking/tool signature、usage/cache、错误 envelope 和 SSE 状态语义。
- 去除 `internal/cache`、runtime logger 等副作用依赖；使用本地纯 `common`、`signature`、`util`，或把必要逻辑收敛为 provider 包内纯函数。不要同步签名缓存服务。

## 依赖边界

允许：

- 纯数据标准库：`bytes`、`context`、`encoding/*`、`errors`、`fmt`、`hash/*`、`io`、`math/*`、`path/*`、`reflect`、`regexp`、`sort`、`strconv`、`strings`、`sync/*`、`time`、`unicode/*`、`cmp`、`maps`、`slices`；测试另可使用 `runtime`、`testing`；
- `gjson`、`sjson`；测试可使用 ccLoad 已有的 `protowire` 解析 fixture；
- `ccLoad/internal/protocol/cliproxy/{claude,codex,common,gemini,misc,openai,registry,signature,thinking,util}` 中的纯 core 包；
- 同一 provider adapter 下的纯包。

禁止：

- OAuth 登录、token refresh、credential store；
- CLIProxyAPI `internal/runtime`、`internal/config`、`sdk`、动态 Registry、executor；
- ccLoad `internal/app`、存储层和任意 auth service；
- 网络请求、后台任务、全局可变缓存或路由选择。

## 生产接线

- provider adapter 是通用协议转换前后的覆盖层，不是第五种公共协议。
- 由 ccLoad 请求上下文根据实际 provider/AuthType 精确选择；普通 Gemini、OpenAI、Anthropic、Codex 渠道不得误入。
- request、non-stream response、stream response 三条路径必须成对接入。
- 无法转换的客户端输入返回 `RequestTranslationError`；provider 上游错误继续由转发层处理。

## 新增 provider

发现新的 provider dialect 时，在同一次原子同步中完成：

1. 证明它存在独立 wire 语义，不能仅因为使用 OAuth 就登记；
2. 在本文件登记边界，并在 manifest 登记 provider、逐文件映射、生产接线和契约测试；
3. 保持 `scripts/verify.sh` 从 manifest 读取，不在脚本硬编码第二份 allowlist；
4. 同步生产源码、匹配测试并接入生产路径；
5. 在 `UPSTREAM.md` 记录实际快照；
6. 与 core 一起通过全部验证后，才更新共享 commit/date。

## 最低契约测试

每个 provider × 支持的客户端协议都覆盖：

- request、non-stream response、stream response；
- 多工具 ID 往返及逆序 tool result；
- thinking/tool signature 往返；
- usage、cache read/write 和 total-token fallback；
- SSE 起止事件、跨 chunk 参数和终止原因；
- provider error envelope 不被成功响应转换吞掉；
- provider 选择隔离，普通渠道线协议保持不变。
