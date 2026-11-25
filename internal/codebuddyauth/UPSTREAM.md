# CodeBuddy 协议来源

参考 [lovingfish/workbuddy-cliproxy](https://github.com/lovingfish/workbuddy-cliproxy)，固定提交 `7efb280563b8cf2bf62e4295340708bac4ec3d6a`。上游 MIT 许可见 [LICENSE](LICENSE)。

本包按其 CodeBuddy CLI 协议实现登录、轮询、账号信息与 Token 刷新。渠道管理、凭证 CAS 持久化、超时、SSE 读取、协议转换与用量解析由 ccLoad 实现；未引入上游 HTTP 服务或独立转换核心。

## Chat 请求兼容参考

2026-11-11 对照 [magpie-community/plugins 的 WorkBuddy 0.2.27](https://github.com/magpie-community/plugins/blob/b4916d414321bff0ffdf0f43af7d14e37b540789/packages/workbuddy/index.mjs)，固定提交 `b4916d414321bff0ffdf0f43af7d14e37b540789`；其 MIT 许可见 [LICENSE.magpie](LICENSE.magpie)。只在 ccLoad 的 `internal/app/codebuddy_wire.go` 最终化现有出站请求，不导入插件运行时或改动 CLIProxyAPI 快照。

| 上游分支 | ccLoad 处理 |
| --- | --- |
| `systemOn`：全部 developer 转 system，首条无 system 时注入默认提示 | 采用角色归一；保留 ccLoad 官方 CLI 默认提示。ccLoad 在角色转换前判断：首条为 developer 时也前置默认 system，再将 developer 转为 system |
| `max_completion_tokens` 存在且无 `max_tokens` 时迁移；已有 max_tokens 时保留；删除前者 | 采用，位于渠道 BodyRules 后；CodeBuddy 跳过通用 OpenAI 上限归一，避免国际版 gpt-5.5 等模型提前丢失 max_tokens |
| `asReasoning`：已有非空 reasoning 优先，否则取 reasoning_content，再取 reasoning_details 文本；删除别名 | 采用；details 每项取非空 text，否则取 summary，按顺序直接拼接；不动其他角色或工具调用 |
| `FLAGGED`：billing header、Claude 身份与主分支、Codex 身份、DSH 身份、Web GUI 说明 | 保留 ccLoad 原有两条 Claude 精确替换；增加 DSH 身份句的逗号与 Web GUI 句的局部改写，仅作用 system/assistant。其余规则不导入 |
| `hide_dsh`：默认隐藏身份、checkout、Web GUI 名称以及普通文本中的品牌；保护路径和工具结果 | 不导入通用名称隐藏或配置开关，避免改动未验证内容 |
| `commaPaused`：11128 对未知身份句式再改写并重试，成功记忆 | 采用 OPENERS 身份句匹配及一次同渠道重试，仅改 system/assistant 文本，保留其他内容块；已有逗号不再改写。触发限定为结构化 code=11128 的 HTTP 400 或未提交 SSE 错误，不采用上游宽泛文案匹配 |
| `commaSystems`：以 system 文本为键，最多保留 256 项成功策略 | 采用有界内存记忆；ccLoad 要求重试完整成功且 system 有改动，失败、截断和 assistant-only 不学习。以渠道 ID、上游 origin、wire model、完整 system 消息的 SHA-256 隔离，256 项 FIFO；缓存命中在隐私遮蔽后应用同一改写，不持久化 |
| `systemAsUser`：拒绝后将系统指令迁到用户消息并缓存 | 不导入；保留指令角色层级，未知错误仍走现有错误处理 |
| `prepared` 的工具修复、原生 Messages 签名与其他 provider 流程 | 不同步，仍由 ccLoad 现有协议与 wire 层负责 |

本轮真实渠道重放另外确认：完整 `Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root` 片段在 system 中返回 HTTP 400/11128；单独各词、两句拆分及逐词删除均成功。早期验证在该完整片段的 workspace 首字符后插入 U+034F 连续两次成功；按用户要求，最终规则改为在片段开头 Tokens 的 T 后插入 U+034F，保留 workspace 原样。最终位置已用 deepseek-v4.1-flash 真实短请求对照验证：原文仍返回 400/11128，改写后两次均为 200、stop、[DONE]，其中一次以 developer 开头并由最终化入口前置默认 system。因此把它作为窄范围固定文本规则，与已有可配置敏感词规则分开；不将 workspace 加入共享默认敏感词，不改写 user、工具结果、工具参数或非文本内容块。

## 认证、目录与计费

新建渠道的初始模型列表来自 `lovingfish/workbuddy-cliproxy` 的 `7efb2805`，不代表账号实际可用模型。

2026-11-25 对照 Agent2Providers 固定提交 `b35b163d` 的 [WorkBuddy 凭据时间](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/workbuddy/src/util.rs#L112-L130)、[国内错误分类](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/workbuddy/src/errors.rs)及[国际错误分类](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/workbuddy-intl/src/errors.rs)。ccLoad 按字段及业务码独立实现以下行为。

- 导入 WorkBuddy 文件或读取既有渠道时，绝对到期时间兼容 Unix 秒和毫秒，保存时统一为秒。令牌接口的相对期限 `expiresIn` 仍按秒计算。
- 推理的 HTTP JSON 和 SSE 遇到 `12153` 均按凭据过期处理。首段输出前可刷新一次并重放，已经输出的流直接报告错误。
- `6003` 返回 429，`14003` 返回 503，只冷却本次请求的模型。`14001/14012/14013/14014/14018/14019` 表示账号额度耗尽，在 ccLoad 返回 402 并冷却该 OAuth 渠道。
- 默认冷却期限优先取上游 `resetAt` 或 `reset_at` 的 Unix 毫秒时间，其次取有效的 `Retry-After`，均缺失或无效时沿用现有退避策略。
- 令牌刷新和余额快照共用同一份凭据。刷新期间如果只有额度或昵称变化，将新令牌与最新元数据一起保存。若账号或认证材料已变更，以重新授权后的凭据为准。[WorkBuddy 刷新落盘](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/crates/providers/src/providers/workbuddy/refresh.rs#L111-L145)
- 个人余额在现有 `get-user-resource` 接口按每页 100 条读取。有 `Response.Data.TotalCount` 时收齐并核对数量，无总数时以短页确认结束。明确的空 `Accounts` 数组可表示零余额；列表缺失、余额字段缺失或分页未完成时返回错误，保留上次快照。[WorkBuddy 分页完整性校验](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/workbuddy/src/billing/resources.rs#L93-L132)
- 只有 `usage` 而省略 `choices` 的流式尾帧在本地补为 `choices:[]`，使 Responses 客户端收到完成事件的用量。DeepSeek 请求通过 `reasoning_effort` 或 `reasoning.effort` 显式选择 `none` 或 `off` 时，保留调用方的设置。[WorkBuddy 流式事件](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/workbuddy/src/chat.rs#L101-L122) · [思考档位](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/workbuddy/src/reasoning.rs#L245-L288)
- 指定函数时首发保留官方 CLI 的标准 `tool_choice` 对象。仅在输出前收到 HTTP 400、业务码 11101，且上游明确指出顶层 `tool_choice` 对象无法解码为字符串时，代理以 `"required"` 和唯一被选中的工具声明重试一次，保留该工具的完整参数定义。[WorkBuddy-CLIProxy 的指定函数回归测试](https://github.com/libukai/workbuddy-cliproxy/blob/v0.2.0/main_test.go)
- 2026-11-25 在 `www.codebuddy.ai/v2/chat/completions` 用 `deepseek-v4.1-flash`、两个无副作用的工具验证。指定第二个工具时，对象形态返回 400/11101。裸函数名返回 200，却选了另一个工具。`"required"` 加上单个工具声明返回 200，实际调用了指定函数。国内站的本地凭据已过期，本轮只确认了匿名请求返回 401。[Agent2Providers 裸函数名实现](https://github.com/ProvidersPlugin/Agent2Providers/blob/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/workbuddy/src/normalize.rs#L153-L226)

“获取模型”使用官方 `@tencent-ai/codebuddy-code@2.148.0` 的 `CloudProductProvider` / `CloudProductManagerImpl` 契约：携带账号凭证 GET `{产品端点}/v3/config`，读取 `data.models[].id`。CodeBuddy 国内版产品端点是 `https://copilot.tencent.com`，国际版产品端点是 `https://www.codebuddy.ai`（历史保存的 `www.workbuddy.ai` 仍识别为国际版）；两版 CLI OAuth 都使用 `/v2/plugin/auth/state?platform=CLI`、`/v2/plugin/auth/token` 和 `/v2/plugin/login/account`，对话端点分别为 `{产品端点}/v2/chat/completions`。官方安装包版本通过 npm registry 核实，协议从本机同版本官方发布包读取。该入口按实时响应返回模型，不使用初始列表过滤或错误回退；额度查询使用对应产品端点的独立计费接口（历史凭据未标记端点时默认国内版）。

`data.models` 是模型定义总目录，不等于 CLI 可调用清单。模型范围以 `data.agents` 中 `name="cli"` 的 `models` 为准，与模型定义按完整 ID 匹配，移除 `disabled:true`，并对非空 `availableModels` 取交集、去重。不维护固定模型排除名单，也不根据别名、标签或 `disabledMultimodal` 扩展或排除模型。CLI 列表缺失、为空或筛选后无模型时返回错误，不回退到完整目录或内置列表。上游 `11102 / service info not found` 表示模型无服务配置；目录筛选不能保证账号余额、权限或服务实时健康。对话接口要求 `messages[0].role` 为 `system`；标准 OpenAI 请求缺少该消息时，ccLoad 在 CodeBuddy 出站最终化阶段补入官方 CLI 默认系统提示词 `You are CodeBuddy Code.You are an interactive CLI tool that helps users with software engineering tasks.`。出站强制 `stream:true` 并补 `stream_options.include_usage`。DeepSeek 系模型默认开启思考，未指定档位时取 `high`。调用方显式关闭思考时跳过这两个默认值。Chat 请求使用官方 CLI 会话头族（`X-CodeBuddy-Request`、`X-Agent-Intent`、`X-IDE-*`、`X-Conversation-*`），且**不**携带 `X-Refresh-Token`。国际版新增 OAuth/导入默认使用 `https://www.codebuddy.ai`；目录、刷新和计费按凭证的国际版身份选择该端点。对话严格使用渠道配置的 URL，不按 JWT `iss` 或凭证 `base_url` 自动改写；`Origin`、`Referer` 和 `X-Domain` 跟随实际请求地址。历史 WorkBuddy 渠道如需改用 CodeBuddy，由管理员手动修改渠道 URL。2026-11-01 按项目要求撤回 2026-09-21 PR #164 的 WorkBuddy 端点切换及错误链接改写，保留后续目录与对话请求头一致性修复。

签到和余额查询沿用 `workbuddy2api` 的计费接口：国内版向 `https://www.codebuddy.cn/v2/billing/meter/daily-checkin` POST 空 JSON；国际版不支持签到，ccLoad 不会对国际版发起该请求。两版都向各自产品端点的 `/v2/billing/meter/get-user-resource` POST 当前有效套餐筛选条件查询余额。余额按套餐的周期剩余量聚合，周期字段缺失时回退到总剩余量，并将负值截为零。ccLoad 仅在每日 09:00 和 21:00（服务器本地时间）为国内 CodeBuddy OAuth 渠道执行签到并把安全余额快照写入凭证；管理端的 CodeBuddy 手动签到按钮同样仅对国内版显示，OAuth 用量接口仍可为两版手动刷新余额。

2026-09-12 用三个真实渠道的 OAuth Bearer 直连上游实测签到响应：该接口把所有非成功结果都编码为 `HTTP 400` + `code 10001`，只有 `msg` 区分语义，因此判定必须同时看 `code` 和 `msg`，不能只看 `code`。三种实测文案：国内个人版当天已签到 `今天已签到，请明天再来`（幂等成功，ccLoad 记为 `already_checked`）；国际版 `签到活动未开启或已过期`；企业版 `企业账号不支持该操作`。后两者表示签到并未发生，仍按失败返回。国际版换用不带 `/v2` 的 `/billing/meter/daily-checkin` 结果相同，同一凭证的 `get-user-resource` 两个路径均返回 200，可确认国际版是确实没有签到活动而非"已签到"。幂等判定依赖上游中文文案（见 `IsAlreadyCheckedIn`），上游若改变措辞会回退为报错而非静默假成功。

企业渠道（`enterprise_id` 非空）使用 `POST /v2/billing/meter/get-enterprise-user-usage`，空 JSON 请求体及现有企业身份头。企业账号不执行个人 `daily-checkin`（上游返回 10001/400“企业账号不支持该操作”），手动签到操作直接刷新企业积分。2026-09-11 对照[官方用量页面](https://www.codebuddy.cn/profile/usage)及其公开 `config-D_y8qqW1.js`、`index-Da0UxXq9.js` 核实：官网使用不带 `/v2` 的同名接口，现有 OAuth Bearer 实测两个路径均可读取。`credit` 是周期已用积分、`limitNum` 是成员周期总额度，剩余为 `max(0, limitNum-credit)`；`limitNum=-1` 表示无限额度。企业积分保留小数，不能按个人资源包的空 `Accounts` 当作零余额。安全快照包含 `remain/used/total/unlimited`；个人包按同一周期口径聚合总额，缺总额时不生成进度条。
