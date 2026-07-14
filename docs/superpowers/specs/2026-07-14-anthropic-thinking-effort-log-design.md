# Anthropic 思考等级日志归一化修复设计

## 根因

日志提取函数把 Anthropic `thinking.type` 作为思考等级的兜底字段。`enabled`、`adaptive` 和 `disabled` 描述的是思考模式，不是 `low`、`medium`、`high` 等强度等级。因此，请求包含 `thinking.type=enabled` 时，错误值 `enabled` 会进入活动请求和持久化日志，最终被前端原样显示为“思考等级”。

原始复现请求同时包含 `thinking.budget_tokens=31999`。项目已有旧 Anthropic 预算到等级的统一映射，但日志提取链路没有使用它。

## 目标

- 显式等级字段保持现有优先级，包括 `reasoning_effort`、`output_config.effort` 和各协议的 `thinkingLevel`。
- Anthropic `thinking` 没有显式等级但包含正数 `budget_tokens` 时，复用现有预算映射：`1..4095 → low`、`4096..16383 → medium`、`>=16384 → high`。
- `thinking.type` 不再作为等级写入日志；仅有 `enabled`、`adaptive` 或 `disabled` 时返回空等级。
- 原始复现请求的日志等级为 `high`，不再是 `enabled`。

## 非目标

- 不修改代理转发请求或协议转换行为。
- 不增加数据库字段，不持久化原始 `budget_tokens`。
- 不为无法还原预算的历史日志猜测等级。
- 不在前端硬编码 `enabled → high`。

## 设计

只修改 `extractThinkingEffortFromPayload` 的 Anthropic `thinking` 分支：

1. 先读取 `effort`、`level`、`thinkingLevel`、`thinking_level`。
2. 若没有显式等级，再读取数值型 `budget_tokens`，调用现有 `anthropicBudgetToEffort`。
3. 删除 `type` 兜底。模式字段不具备等级语义，不应进入 `thinking_effort`。

提取优先级的其他部分保持不变。修复发生在请求归一化入口，因此活动请求、普通日志和检测日志继续共享同一结果，不需要前端补丁或存储迁移。

## 测试与验证

复用 `internal/app/proxy_integration_test.go`，从代理请求到持久化日志的公开边界增加回归用例：`thinking.type=enabled` 且 `budget_tokens=31999` 时，日志中的 `thinking_effort` 必须为 `high`。测试不直接调用私有提取函数。

先运行定向用例确认修改前失败，再实施最小修复并运行：

```bash
go test -tags sonic ./internal/app -run '^TestProxy_LogsAnthropicBudgetAsThinkingEffort$'
go test -tags sonic ./internal/...
golangci-lint run ./...
```
