# Qoder 协议来源与实现边界

协议事实参考 [Agent2API/agent2api](https://github.com/Agent2API/agent2api) 固定提交 `072e3c71d253cd6dc265f3656d8cb481dd7c9f84` 的 Qoder 接口记录，仅用于核对 HTTP 端点、字段与状态语义。本包由 ccLoad 按接口契约独立实现，未复制或翻译其代码，也未引入其运行时、脚本或受限分发组件。该项目 LICENSE 是 MIT 条款加附加使用限制，不能视作纯 MIT 源码快照。

2026-11-07 增量核对 [aimod-cc/Agent2API](https://github.com/aimod-cc/Agent2API) 固定提交 `e8bed7397aaf9d8962489074557c2f1f1a8d4f97` 的插件迁移记录，实际 Qoder 协议参照 [ProvidersPlugin/Agent2Providers](https://github.com/ProvidersPlugin/Agent2Providers) 固定提交 `3055f416841c1393ec04294a702603fd41e840aa`。后者使用 AGPL-3.0；同样仅核对协议事实，不复制或翻译其实现。

凭证使用 ccLoad canonical JSON：`type=qoder`，显式 `region=cn|global`；`expires_at` 以 Unix 秒存储，导入时接受毫秒整数并归一化。令牌、PAT、机器身份和 PKCE verifier 均留在后端。

两地区的门户分别为 `qoder.com.cn` / `qoder.com`，控制面分别为 `openapi.qoder.com.cn` / `openapi.qoder.sh`。设备 PKCE 使用 `/device/selectAccounts` 及 `/api/v1/deviceToken/poll`，`nonce` 必须是带横线的标准 UUID，轮询响应支持 `token` / `device_token`。PAT 使用 `/api/v1/jobToken/exchange`。刷新仅按刷新令牌前缀分流：`jrt-` 使用 `/api/v1/jobToken/refresh`，其余使用 `/api/v1/deviceToken/refresh`；只发送 JSON `refresh_token`，不携带旧 Bearer。国际作业令牌 `jt-` 推理使用 `api2.qoder.sh`，设备令牌使用 `api3.qoder.sh`；国内版统一使用 `gateway.qoder.com.cn`。

模型目录声明的思考档位优先于常见别名；例如目录包含 `high` 时直接发送 `high`，否则可映射成 `xhigh`。长上下文读取目录 `context_config` 的对象或数组档位，以当前 `max_input_tokens` 或默认档为下限，按消息和工具定义的粗略 token 估算选择更大的可用档；升级时同时写入 `parameters.context_length`、`chat_context.extra.ideModelConfigOverride.max_input_tokens`、`model_config.max_input_tokens`，之后再编码和 COSY 签名。排队信号包括业务码 `10605`、`isQueued=true` 与 `serviceAvailable=false`，统一映射 503。

额度来自 `/api/v2/quota/usage` 的 `userQuota`、`addOnQuota`、`orgResourcePackage`；组织包 `available=false` 不计入，组织总额读取 `cap`（没有时读取 `total`）。各桶单位必须一致。套餐标签来自可选的 `/api/v2/user/plan`，查询失败不掩盖已成功取得的额度。

签到只使用 `/sash/api/v1/me/campaigns?forceRefresh=true` 与 `/{campaignId}/claim`，Bearer 和 `Cosy-ClientType: 10`。只领取 `actionType=CLAIM_BENEFIT` 且 `claimStatus=CLAIMABLE` 的活动。明确 `ALREADY_CLAIMED` 或回查同一活动为 `CLAIMED` 才算幂等成功；普通 409 不视为成功。奖励仅来自响应的 `benefit.amount`，缺失不补造固定积分。

国际版签到依赖官方 UMID/runtime-info 产生的 `Cosy-MachineToken`、`Cosy-MachineCode`、`Cosy-MachineType`。本包通过 `Service.RiskHeaders` 接受该身份，先取 sash UID 再获取风控头；缺少提供者、组件异常或字段不完整时返回 `missing_component`。本包不会下载、内置、执行未知二进制，也不会伪造风控身份。
