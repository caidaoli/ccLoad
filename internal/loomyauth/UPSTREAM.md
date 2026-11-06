# Loomy 协议依据

本包使用 Go 标准库实现 Loomy HTTP 协议。短信签名、公开客户端标识、积分与任务协议参考 `magpie-plugin-loomy-auth` 0.1.22 的 `src/api.mjs`、`src/pure.mjs`、`src/constants.mjs`，其 MIT 许可保留于 LICENSE。

另核对 Agent2Providers 提交 `a5697f1ca9ac7188964914c21349dc8e62fd2940` 的 `crates/providers/src/providers/loomy/`；未复制其实现。

session 没有已知续期接口，不采用请求 expire 推导本地强制到期时间。首期只支持由短信校验直接取得身份，不接受未经上游身份验证的 session 导入。每日签到后处理八个已知任务，完成后持久化跳过；不兑换邀请码，不执行真实 PPT 或远程配置活动。

每日积分总额只采用签到响应的 `dailyQuota`，随业务日期保存。参考客户端 `src/usage.mjs` 的“余额加当天消费流水”推导未采纳：两个接口没有共同采样点，并发消费会抬高总额。缺少当天签到总额、余额或读取跨越北京业务日时，总额与已用量保持未知；不读取旧额度快照中的推导总额。永久池无周期、无总额，仅展示余额。

这些是参考客户端契约，非官方保证。mock 测试不能证明当前平台奖励或真实账号可用性。DeepSeek 专用图片格式转换不在本期实现范围内。
