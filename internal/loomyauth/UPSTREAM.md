# Loomy 协议依据

本包使用 Go 标准库实现 Loomy HTTP 协议。短信签名、公开客户端标识、积分与任务协议参考 `magpie-plugin-loomy-auth` 0.1.22 的 `src/api.mjs`、`src/pure.mjs`、`src/constants.mjs`，其 MIT 许可保留于 LICENSE。

另核对 Agent2Providers 提交 `a5697f1ca9ac7188964914c21349dc8e62fd2940` 的 `crates/providers/src/providers/loomy/`；未复制其实现。

session 没有已知续期接口，不采用请求 expire 推导本地强制到期时间。首期只支持由短信校验直接取得身份，不接受未经上游身份验证的 session 导入。每日签到后处理八个已知任务，完成后持久化跳过；不兑换邀请码，不执行真实 PPT 或远程配置活动。

每日积分额度按同版本 `src/usage.mjs` 获取：签到响应的 `dailyQuota` 随业务日期保存；缺少当天值时从 V1 `points/records` 完整分页汇总当天 `consumeSource=daily` 的 debit，与每日余额相加推导总额。每页 100 条、最多 6 页，分页不完整、跨北京业务日或查询失败均保留未知，禁止硬编码额度。推导结果随余额快照缓存，当日后续读取不重复翻页。永久池无周期、无总额，仅展示余额。

这些是参考客户端契约，非官方保证。mock 测试不能证明当前平台奖励或真实账号可用性。DeepSeek 专用图片格式转换不在本期实现范围内。
