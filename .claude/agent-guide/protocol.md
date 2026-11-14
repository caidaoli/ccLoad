# 协议转换边界

修改 Registry、跨协议转换或同步 CLIProxyAPI 时读取。文中的 `protocol/` 指 `internal/protocol/`，`builtin/` 指其中的 `builtin/` 子目录。

- 同步/审查转换核心与 provider 纯请求/响应适配器必须使用仓库 Skill:Codex 调 `$sync-cliproxy-core`,Claude Code 调 `/sync-cliproxy-core`;一次操作固定同一上游 commit 并原子完成全部登记范围,唯一 Skill 源码在 `.agents/skills/`,`.claude/skills/` 只放发现链接
- `protocol/registry.go` 是唯一契约/调度边界:同协议原样透传;跨协议只走 `builtin/register.go` 注册的 12 个有向转换对
- `system_one`(`RequestFamilySystemOne`,`/v1/systemone`)是第五种客户端协议,但只能同协议透传到 System One 渠道:不加入 `AllProtocols`/`IsValid`、转换对或 URL 协议声明
- `builtin/cliproxy_adapter.go` 只处理 ccLoad 通用边界(输入验证、JSON/SSE 规范化、流帧封装);`protocol/cliproxy/` 只允许放从 CLIProxyAPI 同步的纯转换核心和 allowlist provider adapter,实际已导入状态以 `protocol/cliproxy/UPSTREAM.md` 为准
- 不要把上游 auth/config/routing/cache service/plugin/executor/network 代码搬进来,也不要改成运行时 Go module 依赖;来源 commit、provider allowlist、许可证和同步步骤以 `protocol/cliproxy/UPSTREAM.md` 与仓库 Skill 为准
- `RequestTranslationError` 表示本地转换无法表示该请求:按协议能力缺失处理,不冷却,继续探测其他协议或渠道,没有候选能表示时以 HTTP 400 结束;不要把它伪装成上游故障
- **Responses 跨协议压缩**由 app 的 `responses_compaction.go` 与转发层执行，不属于同步转换核心：`/responses/compact` 或 input 中的 `compaction_trigger` 转 OpenAI Chat/Anthropic/Gemini 时，先用完整历史生成非流摘要，再返回单个 `compaction` 条目；HTTP 流与 WebSocket 使用标准 Responses 事件。摘要请求删除工具声明、强制工具选择与输出格式约束，工具历史转文本，不能丢弃未知压缩密文；缺少完整历史、摘要为空、截断或没有源协议成功终态时不得报告成功。原生 Responses 压缩仍透传。
- ccLoad 自有压缩条目的 `encrypted_content` 使用 `ccload-compact-v1:` 加 base64url JSON 摘要，仅是可跨重启恢复的传输封装，**不是加密或认证**。发送前只解包此格式为普通 user 上下文，包括切换到原生 Responses 渠道；不提升为 system/developer 指令。代理跨协议转发遇到其他来源的 `compaction`/`compaction_summary` 按能力不足处理，继续尝试原生渠道，不沿用底层 Chat 转换器静默丢弃密文的行为。来源参考 CLIProxyAPI `f0f13cfda531da02a077748512f78a07679efd08` 的摘要执行流程及 sub2api `42bc7f6cffe24bcb471608e48e66b4a0afa1f882` 的原生压缩入口区分；本次不更新转换核心快照。
- 转换核心丢弃无法发送的内容块后,若某个 user turn 不再有可发送内容(`common.UserTurnDrops` → `UnsupportedPartError`),必须返回错误并由 Registry 包装为 `RequestTranslationError`,不得静默发出空回合;同回合仍有实际文本或附件时只丢弃该块。ccLoad 保留远程 URL、`file_id`、`file_url` 等可透传引用,只有既无字节也无引用的块才算无法发送
- Registry 边界测试定义 ccLoad 线协议契约,上游同步测试守住转换行为;改协议后先跑命令区快照审计,再跑全量 `internal/...`
- Anthropic 转 Responses 的原生 JSON 与 SSE 都把 `max_tokens` 映射为 `incomplete`;流式 output item 在下一个内容块或 message stop 时确定最终状态,不得在得知截断原因前报告完成
- OpenAI Chat JSON/SSE 的 DeepSeek `prompt_cache_hit_tokens` 在自有 `builtin/cliproxy_adapter.go` 边界归一化到 `prompt_tokens_details.cached_tokens`，标准数字字段含 0 优先；不修改 CLIProxyAPI 快照，同协议原文继续透传，app 计量独立识别同一字段。
- 转 Anthropic usage 时,未缓存输入量须扣除 cache read 和 cache creation;保留 ccLoad 的缓存写入字段及上游别名,避免缓存写入重复计量
- Gemini/Antigravity 的会话中途提醒不能拆散工具调用与结果配对;保留 model turn 与签名索引,Responses 入口含 functionResponse 的 user turn 不与普通提醒合并
- Gemini/Antigravity 转 Chat 时，同一 SSE 帧中的多个正文和思考 Part 分别按源顺序累计；空文本签名载体不清空之前的内容。partialArgs 未完成时的正文中断校验仍先于聚合。
- Gemini/Antigravity 转 Claude 时，`MALFORMED_FUNCTION_CALL` 只有没有可见正文/有效工具时才输出错误，已有部分输出保留；空工具占位和签名载体不算有效输出。Antigravity 转 Chat 不输出无内容的空 delta，也不凭仅签名和零输出计量在 `[DONE]` 合成 stop。app 在转换前处理零输出异常，转换器保留自己的纯协议错误语义。
- Gemini/Antigravity 的 `MAX_TOKENS` 转 Chat 时使用标准 `finish_reason=length`，`native_finish_reason` 保留 `max_tokens`，工具调用仍优先 `tool_calls`。
- Gemini 转 Chat 的工具签名放在 `tool_calls[].extra_content.google.thought_signature`，延迟签名仅绑定当前候选最近的工具，正文、附件和不支持的 Part 隔断绑定。Gemini 经 Claude 回放的签名载体按标记恢复原 Part；不跨消息或语义边界，不降低提供商签名兼容性检查。
- Chat 转 Responses 的 JSON/SSE 支持 typed content 数组及结构化 reasoning，思考和正文分别输出，未知 typed Part 不序列化成正文。同一消息/流帧的顶层 reasoning 表示非空时优先于内容数组的 thinking，避免重复输出；顶层表示为空时仍使用 typed thinking，普通正文始终保留。流式交错正文/思考保持独立 item 与顺序，已关闭的消息不因后续截断改为 incomplete；纯思考流有明确 finish_reason 才允许终结，缺失源协议终态不能伪造成功。
- Responses 转 Claude 时，支持中途系统消息的模型将会话开始后的纯文本 system/developer 项放在下一个输出的 user／完整工具结果组之后，保留缓存前缀、内容块和 cache_control；初始 instructions、旧模型、不支持的内容及无可放置 user 回合的项保持顶层路径。模型判定由 cliproxy/util 与 app 共用；API Key Haiku 仅实际携带中途 system 时补对应 beta，OpenCode、Z.ai 与 Zed Anthropic wire 在转换前保持原顶层路径，Zed 的 messages 仅接受 user/assistant。不导入上游 executor 上下文。
- Responses 并行工具结果之间的 developer/system 提醒不能拆散同一批结果；按 call_id 恢复调用顺序并保留图片附件，提醒放到结果组之后；不得跨越真实 user/assistant 回合、新工具调用或 reasoning 边界收集结果，也不得把孤立结果跨提醒搬移。
- Gemini 工具参数转换为 JSON Schema 时，将 minLength/maxLength、minItems/maxItems、minProperties/maxProperties 的规范非负整数字符串转为 JSON 数字；仅遍历 Schema 子结构，不改 default、enum、examples 或扩展元数据中的普通字符串。普通 Gemini 同协议请求仍遵循原样透传契约。
- Antigravity Gemini 历史含多余工具结果时，优先按调用 ID 选择当前组结果，再按名称及顺序补齐；当前待配对组已全部完成后的多余结果丢弃，不得带入后续 model turn。
- Gemini/Antigravity 请求清理 `systemInstruction.parts` / `system_instruction.parts` 中的空对象 `{}`，保留其他 Part 及顺序；全部为空时删除该系统指令字段。清理不扩大 Vertex 思考签名的既有端点边界；未知 Gemini 中继的原生请求继续透传，Antigravity 的身份提示仍按原规则注入。
- Claude 请求转 Antigravity Claude 模型时，工具结果须独立成 user turn 并紧跟 model 工具调用；夹在中间的文本/提醒后移，同轮并行结果合并。Gemini 模型保持原有合并规则。
- Claude `tool_result.is_error` 仅布尔 `true` 在 Gemini/Antigravity 请求中映射为 `functionResponse.response.error`，其他值仍用 `result`；保留结果 JSON 类型、图片及工具调用配对。
- Claude 工具结果中的图片在 Gemini/Antigravity 请求中嵌入所属 `functionResponse.parts[].inlineData`，不作为外层独立 Part；并行工具各自保留图片、调用 ID 与错误结果。
- Gemini 跨协议入口接受 `functionCall/functionResponse` 及蛇形别名；缺少 role 且包含工具调用时按 assistant 处理，普通缺省 role 始终为 user，不按上一回合交替推断。纯工具结果转 Chat 不额外生成空 user 消息。
- Gemini 转 Claude 的工具结果始终进入 user 回合；混合调用/结果按顺序拆分角色，合并后的结果排在其他 user 内容之前并保留调用 ID。Claude 转 Gemini 的普通/隐藏思考签名只输出兼容 Gemini 且可解码为标准 Base64 的值，补齐原生签名 padding；不输出 bypass 占位串或外来签名，保留普通思考文本及结束事件。流式、SSE 聚合和原生 JSON 响应遵守相同签名约束。
- Chat/Claude/Gemini 转 Responses 保留输出 token 上限为 `max_output_tokens`；Chat 优先 `max_completion_tokens` 再 `max_tokens`，Claude 使用 `max_tokens`，Gemini 接受 generationConfig 及蛇形字段。普通同协议请求仍原样透传。
- Responses 转 Claude 回放时，同一 assistant 回合存在空搜索结果则移除其文本的 `web_search_result_location` 引用，保留正文、其他引用和完整结果对应回合的引用；不引入运行时搜索缓存。
- Antigravity 转 Claude 将安全/过滤终止原因映射为 `refusal`，工具调用仍优先 `tool_use`；流尾部空 `finishReason` 不覆盖先前非空原因，但仍触发待发搜索内容的排空。`MALFORMED_FUNCTION_CALL` 与 Gemini 转 Claude 一致不映射为 `refusal`（Claude Code 会将其报为违规），非流仍由应用层转为错误。
- Claude 会话中 system 消息的 `tool_addition` 转 Responses 为 `additional_tools`，转 Chat 合并工具声明；内联工具参与长名称缩短及响应反向映射。工具声明与文本提醒不能拆散待完成的调用/结果配对；不因此启用 Claude `tool_search`。
- OpenAI Chat 转 Codex 时，`service_tier` 的 `fast`/`priority` 规范为 `priority`，保留 `ultrafast`，忽略其他值；匹配时忽略大小写与首尾空白。
- Responses 工具历史转换按原始 call_id 配对；孤立 output 转普通用户内容，Claude 缺失工具结果按中断错误补齐并保持结果先于文本。Responses → Chat 的工具声明、历史、tool_choice 和反向映射共用命名空间及长度/碰撞规则。原生 Codex OAuth 的 HTTP/WS 共用 normalizer 也处理缺少调用引用的 `function_call_output/custom_tool_call_output`：能配对则补 call_id，否则保留文本与附件为 user 内容；显式 continuation 引用和 `codex_app` 委托输出不降级，不把 item id 当成未知远端调用引用。参照 CLIProxyAPI `05e668cf`。
- Responses → Chat 保留 developer 角色、消息内容和相对位置；不降级为 user，无法表示附件的拒绝判定仍仅针对真实 user 回合。Claude → Responses 的长 MCP gateway 工具名保留可读的工具后缀并消除碰撞，声明、历史、tool_choice 与响应原名恢复使用一致映射。
- 跨协议 `custom apply_patch` 转函数工具时，保留补丁语法并使用唯一字符串 `input` 参数；反向转换按原始获胜声明识别工具，普通同名 function 不升级为 custom。OpenAI Chat 嵌套 `custom` 声明与 tool_choice 转 Codex 时拍平为 Responses 形状；这类工具的调用以原生 `type:"custom"` tool_calls 和原始 input 返回，扁平声明保持函数包装。补丁正文的空白、转义和 Unicode 必须在历史回放、增量、done 与最终 output 中一致。
- 上游补丁参数非法、身份冲突或流缺少源协议终态时，转换边界传播工具输入错误，不得伪造成功；已开始的 Responses 流最多发送一次 `response.failed`。EOF 时上游已有语义终态（如 OpenAI `finish_reason`）则先补终止事件，再检查转换状态；启用补丁转换且缺少语义终态的流不得用合成终态掩盖截断。传输错误、取消与上游错误事件保留原错误，不改判为工具参数错误。普通工具与同协议透传保持原契约。
- OpenAI → Claude 接收 reasoning_content、reasoning、reasoning_details 的首个有效表示；JSON/SSE 的 content 数组按顺序转换 text、thinking 与 reasoning 块，工具参数期间的内容沿用串行块缓冲，不把数组输出为 JSON 文本或丢弃思考。工具流保留 length/content_filter，累计参数不是有效对象时不得标成正常 tool_use。单条 system 输入继续作为原始用户提示，不包中途提醒。流式 `finish_reason` 块不发 `message_delta`/`message_stop`，等尾部 usage 块或 `[DONE]`（含网关补喂）再用权威 usage 收尾。
- OpenAI 的纯空白 reasoning 不创建 Claude thinking 块，已有 thinking 内的空白仍保留；Claude 转 Chat 时缺省 thinking 的独立 output_config.effort 仍映射到 reasoning_effort，显式预算与 disabled 等现有优先级不变。
- Gemini 原生搜索仅查询静态模型能力；响应转换以实际发出的 googleSearch 请求判断搜索模式，不引入动态模型注册或探测。
- Claude→Codex 的 `defer_loading` 工具不提前放入顶层 `tools`，由工具结果中的 `tool_reference`、独立引用块或 `tool_search_tool_result` 在发现位置引入 `additional_tools`；跨轮去重并保留混合结果中的引用。显式 `tool_choice` 强制选择尚未发现的工具时，保留其顶层声明。常驻工具优先分配短名，新增延迟工具不得改变已有名称或缓存前缀。工具结果中的历史引用即使缺少当前声明也作为文本保留；无法解析且不能表示的独立引用返回 `RequestTranslationError`。已有 `tool_addition` 契约保留。这与反方向的 Responses `tool_search` 开关无关。
- Codex 客户端 `tool_search_call`/`tool_search_output` 只由 OpenAI Chat 与 Gemini 桥接往返；转 Chat 时 `tool_choice.type=tool_search` 映射到保留的 `tool_search` function。普通工具占用该保留名称仍明确拒绝，hosted search 与非法参数校验保持不变。Anthropic 桥接仍拒绝（Claude 开关保持关闭）。Registry 层的 `compaction` 条目只有 OpenAI 桥接接受。
- Gemini 转 Anthropic 遇到无工具调用的 `MALFORMED_FUNCTION_CALL` 时，流式发 `error` 事件，非流返回转换错误；不得把错误体当 2xx 成功响应输出。

- Codex commentary 转 Claude 为携带内部 `cpa:commentary:v1` 标记的 thinking 块，最终答案按独立消息输出；回到 Codex 恢复 commentary phase，其他目标恢复普通 assistant 文本。该标记不是提供商签名，不得发送上游；原生 Anthropic 最终化与 OpenCode vendor-reasoning 特殊入口同样恢复。
- Vertex `functionCall.partialArgs` 在 Claude/Chat/Responses 三方向按调用身份组装 JSONPath 与字符串续片；无效/未完成参数通过既有 ToolInputError/FinalizeToolInput 报错，Responses 至多一个 response.failed，不伪造成功工具终态。未完成调用中插入非空 text/thought 时统一报错，不再打开普通内容块；调用 ID 与续片匹配按 id/call_id/callId 优先级识别。参数清理前的临时 JSON 同样限制为 4 MiB，包含稀疏数组 null 空洞与转义后的字节数。
- Gemini/Antigravity 转 Responses 的 tool_search item 使用 tsc_；Codex HTTP/WS 出站同时规范旧 ts_ 和无前缀 ID，处理碰撞并保留 call_id。Claude thinking 用量细分接受 output_tokens_details.thinking_tokens，保留总输出用量与旧别名。

- Claude 会话中途 `role=system` 消息的最后非空字符串 `output_config.effort` 是当前轮等级，优先于顶层缓存占位（参考 CLIProxyAPI `fb30a9e8b`）。Chat、Responses、Gemini 及 Antigravity 请求在 ccLoad 转换边界归一为当前等级；`thinking.type=disabled/between_tools` 不采纳该指令。原生 Anthropic 仍保留顶层与消息级字段；日志读取最新指令。显式模型后缀与动态思考决策覆盖消息级等级，保留消息正文及其他配置字段。
