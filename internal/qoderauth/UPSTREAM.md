# Qoder 协议来源与实现边界

协议事实参考 [Agent2API/agent2api](https://github.com/Agent2API/agent2api) 固定提交 `072e3c71d253cd6dc265f3656d8cb481dd7c9f84` 的 Qoder 接口记录，仅用于核对 HTTP 端点、字段与状态语义。本包由 ccLoad 按接口契约独立实现，未复制或翻译其代码，也未引入其运行时、脚本或受限分发组件。该项目 LICENSE 是 MIT 条款加附加使用限制，不能视作纯 MIT 源码快照。

2026-11-07 增量核对 [aimod-cc/Agent2API](https://github.com/aimod-cc/Agent2API) 固定提交 `e8bed7397aaf9d8962489074557c2f1f1a8d4f97` 的插件迁移记录，实际 Qoder 协议参照 [ProvidersPlugin/Agent2Providers](https://github.com/ProvidersPlugin/Agent2Providers) 固定提交 `3055f416841c1393ec04294a702603fd41e840aa`。后者使用 AGPL-3.0；同样仅核对协议事实，不复制或翻译其实现。

2026-11-12 参考 [magpie-community/plugins 的 Qoder 插件 0.2.65](https://github.com/magpie-community/plugins/tree/52e31c4a1fdb857d673c6e92c90ef4a6c1294c68/packages/qoder)，固定提交 `52e31c4a1fdb857d673c6e92c90ef4a6c1294c68`，调整请求、目录、工具响应与凭据恢复。来源为 MIT，许可随本目录 `LICENSE-MAGPIE`、`LICENSE-CLIProxyAPI` 保留。未引入其 JavaScript 运行时。

2026-11-25 对照 Agent2Providers `b35b163d` 的[原生 Qoder 实现](https://github.com/ProvidersPlugin/Agent2Providers/tree/b35b163d01fc3effb30ae40a012fd657fdc0a54d/crates/providers/src/providers/qoder)与[国际版 WASM 开发包](https://github.com/ProvidersPlugin/Agent2Providers/tree/b35b163d01fc3effb30ae40a012fd657fdc0a54d/wasm/qoder-intl/src)，在 ccLoad 独立实现每日积分活动筛选和响应错误分类。

| 协议分支 | ccLoad 行为 |
| --- | --- |
| 国内 / 国际 | 门户 `qoder.cn` / `qoder.com`，使用各地区 client_id；控制面仍为 `openapi.qoder.com.cn` / `openapi.qoder.sh`，推理与目录分别为 `gateway.qoder.com.cn` / `api3.qoder.sh`，不再按 `jt-` 切 api2 |
| 网页 / PAT | 网页使用 device-token 单对，PAT 换 job token。凭据的 `expires_at` 以 Unix 秒保存。优先读上游绝对到期时间，包括数字字符串和 RFC3339。仅有 `expires_in` 时，job 按毫秒、设备登录按秒。设备刷新兼容两种单位：数值不超过 2,592,000 时沿用秒，更大且不超过 2,592,000,000 时按毫秒，超出这一范围则保留未知。两个上限分别对应 30 天 |
| 设备登录轮询 | 在原有 5 分钟窗口内，授权等待或轮询暂时断网时每 2 秒继续查询。兼容 `accessToken`、`access_token`、`refreshToken` 和 `uid` 等字段。取得令牌后调用 `/api/v1/userinfo` 核对账号，数字用户 ID 按原文比较 |
| device / job 刷新 | 按刷新令牌的 `jrt-` 前缀选端点。刷新回包兼容 `accessToken`，并按原文核对字符串或数字用户 ID。账号不匹配时拒绝新令牌。job refresh 返回 401/403 且有 PAT 时重新兑换并核对身份。凭据发布使用共享刷新和 CAS |
| 模型目录 | App 场景覆盖同 key 的 Chat 条目，再补 Chat 独有模型；只公开 `enable=true`，排除 auto/default；旧公开名沿用显示名去空白小写，同名后续条目以原生 key 区分；可按 key 精确调用 |
| 目录恢复 | 缓存绑定访问令牌；缓存缺模型时重取。仅免费模型可用且额度未确认耗尽时，尝试刷新并重取，五分钟内不重复；无刷新材料或可选刷新暂时失败时保留已成功读取的可用模型，真实认证失败仍返回错误；显式禁用模型且额度确认耗尽返回 429 |
| system / developer | 合并在前置 Qoder 默认身份提示词之后；原有可配置零宽处理先作用于调用方 system/developer，再构造信封，默认身份不被改写 |
| 会话 / 工具续轮 | 稳定会话按显式 session、prompt_cache_key、user 或开场内容派生；同一用户轮的工具循环共用 request_set_id/business.id/begin_at，business.model_key 始终写原生模型。账号隔离的任务缓存最多512项，空闲2小时到期；user blocks 写 contents，最后 user 文本/图片块标 ephemeral |
| 思考 / 输出 | 优先目录默认档与最近支持档；明确 none/off 在可关闭模型上关闭，强制思考模型取最低档；未指定思考且输出预算≤1024也采用该策略。输出上限取目录 max_output_tokens，QoDuck缺失目录上限时为131072，其余不再统一截断32768；明确的上限拒绝仅同渠道降额重试一次，不建立跨请求学习缓存 |
| 上下文 | 保留目录对象/数组档位、CJK粗估与15%余量，另计输出预算；选档同时写 context_length、model_config.max_input_tokens、ideModelConfigOverride |
| 工具历史 | 补稳定调用ID，按次序配对缺失或无效结果ID，孤立结果转user，工具图片另成user；保留或归一reasoning历史；补oneOf对象schema类型 |
| 工具响应 | 保留标准tool_calls，迟到名称前暂存参数，同index不同ID分开；已声明工具的JSON/XML文本可转换；代码示例、未知工具保留正文，不完整调用报错；纯无名调用返回502，已有输出则丢弃无名调用 |
| 排队 | 业务码 `10605`、`isQueued: true`，或同一对象内同时出现 `serviceAvailable: false` 和非空 `queueType` 时，返回 503 并交公共路由。HTTP、JSON 和首帧 SSE 错误均保留等待提示。`Retry-After` 优先取 `retryAfterSeconds`，其次取 `waitTime`。两者均接受数字和数字字符串，正小数向上取整，无有效值时用 30 秒 |
| 故障 | 对话返回 HTTP 200 JSON 错误时也按业务码分类。403 携带令牌过期信号时归一为 401，明确额度不足归一为 429，其余权限拒绝保留 403，交公共冷却策略处理。供应商内部失败归一为 502。输出之后禁止自动重放 |
| SSE | 支持多行 data、对象或字符串 body、顶层 choices/usage 和多个 choice。优先识别外层及 body 中的排队或错误，跳过空心跳和未排队通知。整次回复只有空成功终态时返回 502。明确的 `length` 与 `content_filter` 保留原义。同一 choice 在结束后继续输出或给出相互冲突的结束原因时按流错误处理。已有输出的流收到真实 `[DONE]` 可补结束原因，每个 choice 都已结束时可接受 EOF |

请求按 App 身份发送，COSY `1.1.69` / ClientType `10`；`model_config` 只带客户端定义字段。推理不发送伪造的 MachineToken/MachineType，不在推理路径安装或运行 UMID。

额度继续使用 `/api/v2/quota/usage` 的 `userQuota`、`addOnQuota`、`orgResourcePackage`，套餐来自可选 `/api/v2/user/plan`。不同单位不合计，credits 不转美元；本次没有切换 Magpie 的 Sash 额度接口。

签到从 campaigns 清单选择当期的每日通用 Credits，手动、自动签到和不确定结果回查都按同一活动判断。限时模型福利的已领取状态不会更新每日签到时间。签到窗口从 UTC+8 每日 10 点开始。回执按选中活动时所在的窗口记账，较晚到达的旧回执不会覆盖新窗口的记录。国际版先读 Sash UID 再获取官方 UMID 身份；国内版不要求组件。国际桌面和CLI组件均传环境 `3`；发现支持官方CLI目录环境变量及平台/架构缓存文件。保留官方npm下载、SHA-512校验和原子安装。

本轮没有增加企业VPC/专属域、Lingma、QwenWork、双令牌存储或静态目录回退；这些是独立账号/产品能力，不属于当前公网Qoder协议兼容调整。
