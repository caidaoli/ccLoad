# API Token 渠道可见性设计

## 背景与根因

现有 API Token Web Dashboard 已经通过 `WebIdentity.AuthTokenID` 和
`ApplyWebIdentityScope` 在服务端强制绑定登录令牌。真正的问题不是数据隔离缺失，
而是响应层又额外删除了渠道维度：统计按模型重新聚合、趋势清空渠道数据、筛选接口
省略渠道选项、日志投影删除渠道与来源字段。响应层恢复这些字段后，Token 页面样式仍把
调用统计的渠道列整体隐藏；渠道名称和该列内的健康时间线因此一起不可见。

这些额外裁剪把“只能查询自己的 Token”错误实现成了“不能看到渠道信息”。本设计修订
`2026-07-12-api-token-web-dashboard-design.md` 中禁止 Token 用户查看渠道元数据的条款。

## 目标

- API Token 用户继续只能查询当前 Web 会话绑定的 `auth_token_id`。
- 保留现有页面、筛选条件、排序、字段和交互结构，不创建第二套 Token 专用界面。
- Token 用户在调用统计、请求趋势和日志中可以看到自己请求对应的渠道类型、渠道名、
  渠道 ID、模型和日志来源。
- Token 顶部导航只显示概览、调用统计、请求趋势和日志，不显示渠道管理与模型测试。
- 调用统计显示渠道名称以及由同一个 Token 作用域计算的渠道健康时间线。
- 不暴露上游 API Key、完整上游 URL、客户端 IP、调试日志正文等敏感数据。
- 管理员现有行为保持不变。

## 非目标

- 不新增权限配置系统。
- 不允许 Token 用户选择或伪造其他 `auth_token_id`。
- 不开放任何 `/admin/*` 写接口。
- 不复制现有统计、日志或渠道页面形成独立实现。
- 不删除或修改 `channels.html`、`model-test.html` 的直接 URL 行为与既有后端路由；
  本次产品边界只调整 Token 顶部导航入口。
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
前端不得再通过 `body.web-role-api-token` 隐藏统计表的渠道列；渠道名称与健康指示器沿用
管理员统计页相同的模板和渲染逻辑。

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

## Token 导航行为

Token 顶部导航白名单固定为：

- `index`
- `stats`
- `trend`
- `logs`

`channels`、`model-test`、`tokens` 和 `settings` 不进入 Token 顶部导航。管理员导航保持完整。
这只是导航展示规则，不新增直接 URL 重定向，也不删除既有页面或路由。

## 前端数据流

1. 页面通过 `/dashboard/session` 获取 `role`，不信任本地存储中的角色值。
2. 顶部导航根据服务端会话角色过滤；不得依据提交的 API Token 文本决定角色。
3. 管理员继续使用现有管理接口和完整操作。
4. API Token 用户使用 `/dashboard` 只读查询；页面不发送可变的 Token 选择条件。
5. 服务端从 Gin context 获取 `WebIdentity`，覆盖任何请求中的 `auth_token_id`。
6. 现有筛选、分页和排序参数原样生效。

### 筛选控件可见性

通用页面启动器不得按控件 ID 隐藏渠道类型、渠道 ID 或渠道名。它只负责会话校验、
页面访问边界、翻译和顶部导航，不拥有各页面筛选器的产品策略。

筛选器由各自组件管理无效状态：

- Token 选择器识别 API Token 会话后隐藏自身，因为会话作用域已经固定 Token ID。
- 日志来源筛选器在 API Token 会话中隐藏自身并固定为代理请求日志，不读取管理员设置。
- 渠道类型、渠道名和模型筛选器保持显示，选项来自 Token 作用域内的
  `/dashboard/logs/bootstrap` 与 `/dashboard/models` 响应。

这条边界避免再次把服务端数据权限错误实现为前端字段裁剪，也避免通用启动器随着页面
控件增加而积累硬编码黑名单。

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

- Token 顶部导航不包含渠道管理、模型测试、Token 管理和设置。
- 管理员顶部导航结构保持不变。
- Token 模式不显示或提交可变的 Token 选择器。

渠道类型、渠道名和模型筛选器属于展示行为，不新增针对 `hidden`、CSS 或具体 DOM 结构的
脆弱断言；通过 Token 作用域的 bootstrap/models API 契约测试证明选项数据可用，并在真实
页面验收中确认三个控件可见。

统计页的渠道名称与健康时间线属于展示行为，按项目测试政策不新增 CSS class、`hidden` 或
HTML 结构断言；使用后端公开响应测试证明字段存在，并在真实页面验收中确认该列可见。

按 `CLAUDE.md` 运行后端测试、Web 验证、lint 和构建。
