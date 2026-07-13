# API Token 渠道可见性设计

## 背景与根因

现有 API Token Web Dashboard 已经通过 `WebIdentity.AuthTokenID` 和
`ApplyWebIdentityScope` 在服务端强制绑定登录令牌。真正的问题不是数据隔离缺失，
而是响应层又额外删除了渠道维度：统计按模型重新聚合、趋势清空渠道数据、筛选接口
省略渠道选项、日志投影删除渠道与来源字段。

这些额外裁剪把“只能查询自己的 Token”错误实现成了“不能看到渠道信息”。本设计修订
`2026-07-12-api-token-web-dashboard-design.md` 中禁止 Token 用户查看渠道元数据的条款。

## 目标

- API Token 用户继续只能查询当前 Web 会话绑定的 `auth_token_id`。
- 保留现有页面、筛选条件、排序、字段和交互结构，不创建第二套 Token 专用界面。
- Token 用户可以看到自己请求对应的渠道类型、渠道名、渠道 ID、模型和日志来源。
- `channels.html` 对 Token 用户只读；不开放新增、编辑、删除、测试或批量管理。
- 不暴露上游 API Key、完整上游 URL、客户端 IP、调试日志正文等敏感数据。
- 管理员现有行为保持不变。

## 非目标

- 不新增权限配置系统。
- 不允许 Token 用户选择或伪造其他 `auth_token_id`。
- 不开放任何 `/admin/*` 写接口。
- 不复制现有统计、日志或渠道页面形成独立实现。
- 不改变代理请求的模型、渠道、费用、并发或允许渠道限制。

## 权限与作用域

`ApplyWebIdentityScope` 是唯一的数据作用域入口：

- 管理员请求保留显式查询条件。
- API Token 请求无条件把 `LogFilter.AuthTokenID` 覆盖为登录会话绑定的 Token ID。
- 客户端传入其他 `auth_token_id` 时忽略该值。
- 缺失或失效的 Web 身份继续返回 `401`；Token 用户访问管理写接口继续返回 `403`。

前端隐藏 Token 选择器只是界面约束，服务端覆盖才是安全边界。

## 现有查询响应修订

### 统计

`GET /dashboard/stats` 对 Token 用户不再调用 `aggregateTokenStats`。查询本身已经带有
登录 Token 作用域，因此直接返回原有渠道维度：

- `channel_id`
- `channel_name`
- `channel_type`
- `model`
- 原有成功率、延迟、Token、成本、RPM 和最后请求字段

健康时间线继续使用同一个已绑定 Token 的 `LogFilter` 计算，不读取其他 Token 数据。

### 趋势

`GET /dashboard/metrics` 不再清空 `MetricPoint.Channels`。返回结构保持管理员页面现有格式，
但底层日志查询仍被固定到登录 Token。

### 模型和筛选选项

`GET /dashboard/models` 与 `GET /dashboard/stats/filter-options` 不再因为 API Token 身份跳过
渠道查询。渠道名和模型选项来自同一个已绑定 Token 的日志范围，不枚举其他 Token 的数据。

### 日志

API Token 日志继续使用安全投影，但投影允许以下原有展示字段：

- `channel_id`
- `channel_name`
- `channel_type`（由已有渠道元数据映射补全）
- `model` 与 `actual_model`
- `log_source`
- 时间、状态、耗时、Token 用量、成本和已清洗错误摘要

投影继续禁止 API Key、API Key 哈希、完整上游 URL、客户端 IP 和调试日志内容。

## `channels.html` 只读行为

Token 会话使用现有渠道页面的筛选、排序和渲染逻辑。页面读取 `/dashboard/*` 只读接口，
所有日志/统计条件由服务端固定到登录 Token。

页面保留渠道类型、渠道名、模型、来源和统计展示。Token 模式隐藏新增、编辑、删除、测试、
批量操作、Key、URL 和设置入口；即使直接请求相应 `/admin/*` 接口也会收到 `403`。

不得采用“浏览器先下载全量渠道再过滤”的实现。Token 可见渠道必须由服务端基于已绑定
Token 的查询结果确定，防止其他 Token 的渠道数据进入浏览器。

## 前端数据流

1. 页面通过 `/dashboard/session` 获取 `role`，不信任本地存储中的角色值。
2. 管理员继续使用现有管理接口和完整操作。
3. API Token 用户使用 `/dashboard` 只读接口；页面不发送可变的 Token 选择条件。
4. 服务端从 Gin context 获取 `WebIdentity`，覆盖任何请求中的 `auth_token_id`。
5. 现有筛选、分页和排序参数原样生效。

## 错误处理

- 登录 Token 被禁用、删除或过期：`401`，现有 Web 会话失效。
- Token 用户调用管理写接口：`403`。
- 请求伪造其他 `auth_token_id`：忽略并覆盖，不返回错误，也不泄漏目标 Token 是否存在。
- 作用域内无数据：返回现有空数组/空统计结构，页面显示现有空状态。

## 验证

后端公共行为测试覆盖：

- Token 请求伪造其他 `auth_token_id` 时仍只返回登录 Token 的数据。
- Stats 保留渠道 ID、渠道名、渠道类型和模型。
- Metrics 保留登录 Token 对应的渠道序列。
- Models 和 filter-options 只返回登录 Token 产生过的数据对应的渠道与模型。
- Logs 返回渠道与来源字段，同时不返回 API Key、URL、客户端 IP 和调试内容。
- Token 会话对所有管理写接口继续得到 `403`。
- 管理员响应结构和行为不变。

前端测试覆盖：

- Token 模式保留现有渠道筛选与展示。
- Token 模式隐藏全部写操作和敏感入口。
- Token 模式不显示或提交可变的 Token 选择器。
- 管理员模式不受影响。

按 `CLAUDE.md` 运行后端测试、Web 验证、lint 和构建。
