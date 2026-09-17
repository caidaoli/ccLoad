# 协议转换边界

修改 Registry、跨协议转换或同步 CLIProxyAPI 时读取。文中的 `protocol/` 指 `internal/protocol/`，`builtin/` 指其中的 `builtin/` 子目录。

- 同步/审查转换核心与 provider 纯请求/响应适配器必须使用仓库 Skill:Codex 调 `$sync-cliproxy-core`,Claude Code 调 `/sync-cliproxy-core`;一次操作固定同一上游 commit 并原子完成全部登记范围,唯一 Skill 源码在 `.agents/skills/`,`.claude/skills/` 只放发现链接
- `protocol/registry.go` 是唯一契约/调度边界:同协议原样透传;跨协议只走 `builtin/register.go` 注册的 12 个有向转换对
- `builtin/cliproxy_adapter.go` 只处理 ccLoad 通用边界(输入验证、JSON/SSE 规范化、流帧封装);`protocol/cliproxy/` 只允许放从 CLIProxyAPI 同步的纯转换核心和 allowlist provider adapter,实际已导入状态以 `protocol/cliproxy/UPSTREAM.md` 为准
- 不要把上游 auth/config/routing/cache service/plugin/executor/network 代码搬进来,也不要改成运行时 Go module 依赖;来源 commit、provider allowlist、许可证和同步步骤以 `protocol/cliproxy/UPSTREAM.md` 与仓库 Skill 为准
- `RequestTranslationError` 是客户端语义错误:代理返回 HTTP 400,不切渠道、不冷却;不要把无法表示的请求伪装成上游故障
- Registry 边界测试定义 ccLoad 线协议契约,上游同步测试守住转换行为;改协议后先跑命令区快照审计,再跑全量 `internal/...`
- Anthropic 转 Responses 的原生 JSON 与 SSE 都把 `max_tokens` 映射为 `incomplete`;流式 output item 在下一个内容块或 message stop 时确定最终状态,不得在得知截断原因前报告完成
- 转 Anthropic usage 时,未缓存输入量须扣除 cache read 和 cache creation;保留 ccLoad 的缓存写入字段及上游别名,避免缓存写入重复计量
- Gemini/Antigravity 的会话中途提醒不能拆散工具调用与结果配对;保留 model turn 与签名索引,Responses 入口含 functionResponse 的 user turn 不与普通提醒合并
- Responses 工具历史转换按原始 call_id 配对；孤立 output 转普通用户内容，Claude 缺失工具结果按中断错误补齐并保持结果先于文本。Responses → Chat 的工具声明、历史、tool_choice 和反向映射共用命名空间及长度/碰撞规则。
- OpenAI → Claude 接收 reasoning_content、reasoning、reasoning_details 的首个有效表示；工具流保留 length/content_filter，累计参数不是有效对象时不得标成正常 tool_use。单条 system 输入继续作为原始用户提示，不包中途提醒。
- Gemini 原生搜索仅查询静态模型能力；响应转换以实际发出的 googleSearch 请求判断搜索模式，不引入动态模型注册或探测。
