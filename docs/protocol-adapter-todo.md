# 协议适配审计与 TODO

日期：2026-04-13

## 结论

按严格定义审计：`A <-> B` 指 **A 能作为 client 打到 B upstream，且 B 也能作为 client 打到 A upstream**。

当前 `ccLoad` 不是“六组 pair 全双向”，而是一个**单向矩阵**：

- `openai -> gemini`
- `openai -> anthropic`
- `openai -> codex`
- `anthropic -> gemini`
- `codex -> gemini`
- `codex -> anthropic`
- `codex -> openai`
- same-protocol no-op

所以用户关心的 6 组 pair，当前只有 **`openai <-> codex` 真正双向**；其余 5 组都只是单向实现。

根因不是少写几个 `Register`，而是当前抽象从一开始就按“现有入站请求族”收口：

- `internal/protocol/types.go` 的 `supportedTransformSourcesByUpstreamAndFamily` 只声明了上面那组单向矩阵。
- `internal/protocol/builtin/request_prompt.go` 只存在 `normalizeOpenAIConversation`、`normalizeAnthropicConversation`、`normalizeCodexConversation`，**没有 `normalizeGeminiConversation`**。
- 这意味着 Gemini 现在根本不是一个完整的入站 client surface；没有入站归一化，就不可能把 `gemini -> *` 真正打通。

## 当前覆盖判断

### 已实现

- `openai <-> codex`
  - `openai -> codex`：`internal/protocol/builtin/register.go`
  - `codex -> openai`：`internal/protocol/builtin/register.go`
  - 对应实现集中在 `internal/protocol/builtin/openai_codex.go`

### 部分实现（只有一个方向）

- `openai <-> gemini`
  - 已有：`openai -> gemini`
  - 缺少：`gemini -> openai`
- `openai <-> anthropic`
  - 已有：`openai -> anthropic`
  - 缺少：`anthropic -> openai`
- `codex <-> gemini`
  - 已有：`codex -> gemini`
  - 缺少：`gemini -> codex`
- `codex <-> anthropic`
  - 已有：`codex -> anthropic`
  - 缺少：`anthropic -> codex`
- `anthropic <-> gemini`
  - 已有：`anthropic -> gemini`
  - 缺少：`gemini -> anthropic`

### no-op / normalize

- same-protocol no-op：**已实现**
  - `BuildTransformPlan` 在 `client == upstream` 时直接返回 `NeedsTransform=false`
  - `Registry.TranslateRequest/TranslateResponse*` 在 `from == to` 时直接透传原始字节
- normalize：**部分实现**
  - 已有：OpenAI / Anthropic / Codex 入站归一化到共享会话 IR
  - 缺少：Gemini 入站归一化
- 回归覆盖：**不完整**
  - 现有测试覆盖了已实现方向的 request/response 翻译
  - 没看到 same-protocol no-op 的专门回归测试

## 执行 lanes（2026-04-13）

- `worker-1 / runtime`
  - 先处理 P0/P1：能力矩阵、Gemini 入站 normalize、缺失的反向 adapter。
  - 共享文件优先级：`internal/protocol/types.go`、`internal/protocol/builtin/request_prompt.go`、`internal/protocol/builtin/register.go` 及新增 adapter 文件。
- `worker-2 / tests`
  - 先补 P2：same-protocol no-op、normalize 覆盖完整性、新增协议路径的 request/response 回归。
  - 验证基线：`internal/protocol/registry_test.go`、`internal/protocol/builtin/request_prompt_test.go`，必要时补更细的 family/matrix 断言。
- `worker-3 / docs + audit`
  - 对齐 `docs/protocol-adapter-todo.md` 与 `docs/superpowers/specs/2026-04-12-channel-protocol-transforms-design.md`，把“设计目标”和“当前已落地覆盖”拆开写。
  - 固化文件级代码质量风险，避免后续继续把 `<->` 误读成“已经严格双向”。

## 是否参考 CLIProxyAPI

是，但不是直接依赖，也不是原样照抄。

### 明确证据

- 设计文档 `docs/superpowers/specs/2026-04-12-channel-protocol-transforms-design.md` 已写明：
  - 复用 `CLIProxyAPI` 的转换思路
  - 最终选择“裁剪并内置到 `ccLoad/internal/protocol`”
  - 不直接依赖 `CLIProxyAPI` SDK
- 当前 `go.mod` 没有 `github.com/router-for-me/CLIProxyAPI` 依赖，说明这里是**内置裁剪/改写**，不是直接 import。

### 哪些方向明显能参考 CLIProxyAPI

这些方向在 `CLIProxyAPI` 里有直接对位的 translator 注册：

- `gemini -> openai`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/openai/gemini/init.go`
- `anthropic -> openai`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/openai/claude/init.go`
- `openai -> gemini`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/gemini/openai/chat-completions/init.go`
- `openai -> anthropic`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/claude/openai/chat-completions/init.go`
- `gemini -> anthropic`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/claude/gemini/init.go`
- `anthropic -> gemini`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/gemini/claude/init.go`
- `gemini -> codex`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/codex/gemini/init.go`
- `anthropic -> codex`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/codex/claude/init.go`
- `openai -> codex`
  - `~/Share/Source/go/CLIProxyAPI/internal/translator/codex/openai/chat-completions/init.go`

### 哪些地方是 ccLoad 自己重做的

- `internal/protocol/builtin/request_prompt.go`
  - 这是一个共享会话 IR + normalize/encode 层，不是 CLIProxyAPI 那种 pair-by-pair 直接拼接的注册布局。
- `internal/protocol/builtin/openai_codex.go` 的双向桥接
  - 当前仓库把 `openai <-> codex` 做成了真正双向。
  - CLIProxyAPI 能直接参考的是 `openai -> codex`；当前这份 `codex -> openai` 不是简单照搬它的目录布局就能得到的。

## 文件级审计备注

- `internal/protocol/types.go`
  - `supportedTransformSourcesByUpstreamAndFamily` 同时承担“能力声明”和“请求族门禁”，方向矩阵被拆散在多层 map 里，审计时必须靠推导，不是单一事实源。
  - `SupportsTransform` / `SupportsTransformFamily` 只回答“当前白名单里有没有”，回答不了“目标应该支持但尚未实现哪些方向”。
- `internal/protocol/builtin/register.go`
  - 注册表是手工枚举；只要漏掉一个 request/stream/non-stream leg，文档里的 `<->` 就会和运行时实际能力脱钩。
  - 这里缺少“成对约束”测试：没有任何机制保证某个设计上声称双向的 pair 真的同时注册了两个方向。
- `internal/protocol/builtin/request_prompt.go`
  - 共享会话 IR 思路是对的，但当前只有 OpenAI / Anthropic / Codex 三个入站 normalize，Gemini 缺席导致所有 `gemini -> *` 先天不可能成立。
  - 单文件体量过大，normalize / encode / 辅助提取都堆在一起，回归面很宽；后续继续补协议时要优先防止把“支持某协议输出”误写成“支持该协议完整入站”。
- `internal/protocol/registry_test.go`
  - 已覆盖已实现方向的主路径，但还没有把 same-protocol no-op、缺失反向 pair、请求族边界当成显式契约写死。
  - 当前测试更像“已注册功能可用”，还不是“矩阵完整性可审计”。
- `docs/superpowers/specs/2026-04-12-channel-protocol-transforms-design.md`
  - 设计文档先前把 pair 目标和 shipped 覆盖混写，且遗漏了已经落地的 `openai <-> codex`，这就是误读来源之一。

## TODO

### P0：先修根因，不然只会继续堆单向特判

- [ ] `[worker-1]` 把支持矩阵从“当前入站请求族白名单”提升为“协议对 + 请求族”的显式能力表。
- [ ] `[worker-1]` 给 Gemini 增加入站 normalize（等价于 `normalizeGeminiConversation`）。
- [ ] `[worker-1]` 明确 OpenAI 在本仓库里到底只代表 `chat/completions`，还是要继续承担更宽的 surface；别让 `Protocol` 和 `RequestFamily` 的职责继续糊在一起。

### P1：补齐严格双向缺口

- [ ] `[worker-1]` `gemini -> openai`
  - 参考：`~/Share/Source/go/CLIProxyAPI/internal/translator/openai/gemini/init.go`
  - 需要：request transform + stream response + non-stream response + family/matrix 接入
- [ ] `[worker-1]` `anthropic -> openai`
  - 参考：`~/Share/Source/go/CLIProxyAPI/internal/translator/openai/claude/init.go`
  - 需要：request transform + stream response + non-stream response + family/matrix 接入
- [ ] `[worker-1]` `gemini -> anthropic`
  - 参考：`~/Share/Source/go/CLIProxyAPI/internal/translator/claude/gemini/init.go`
  - 需要：request transform + stream response + non-stream response + family/matrix 接入
- [ ] `[worker-1]` `gemini -> codex`
  - 参考：`~/Share/Source/go/CLIProxyAPI/internal/translator/codex/gemini/init.go`
  - 需要：request transform + stream response + non-stream response + family/matrix 接入
- [ ] `[worker-1]` `anthropic -> codex`
  - 参考：`~/Share/Source/go/CLIProxyAPI/internal/translator/codex/claude/init.go`
  - 需要：request transform + stream response + non-stream response + family/matrix 接入

### P2：补齐 no-op / normalize 的证据链

- [ ] `[worker-2]` 给 same-protocol no-op 补回归测试：
  - `BuildTransformPlan(client == upstream)`
  - `Registry.TranslateRequest(from == to)`
  - `Registry.TranslateResponseStream(from == to)`
  - `Registry.TranslateResponseNonStream(from == to)`
- [ ] `[worker-2]` 给 normalize 层补“入站协议覆盖完整性”测试，至少显式证明为什么 Gemini 现在不支持，避免以后继续误读成“已经双向”。

### P3：文档收口

- [x] `[worker-3]` 更新 `docs/superpowers/specs/2026-04-12-channel-protocol-transforms-design.md` 的第一版覆盖描述；那份文档现在已经落后于代码，尤其是 `openai <-> codex` 已经实现，但其余 pair 仍不是严格双向。
- [x] `[worker-3]` 把执行项显式拆成 worker lanes，并补文件级审计备注，避免继续用模糊的 `<->` 让实现和审计各讲各话。

## 审计依据

- `internal/protocol/types.go`
- `internal/protocol/registry.go`
- `internal/protocol/builtin/register.go`
- `internal/protocol/builtin/request_prompt.go`
- `internal/protocol/registry_test.go`
- `docs/superpowers/specs/2026-04-12-channel-protocol-transforms-design.md`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/**/init.go`
