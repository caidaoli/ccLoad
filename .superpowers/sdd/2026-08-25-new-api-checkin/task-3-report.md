# Task 3 报告：New API 管理余额与签到

## 摘要

实现 New API 管理余额读取、严格响应 envelope/data 校验、原始额度快照、签到有限状态机，以及 service 级单次状态 CAS 持久化。整个网络操作、CAS 重试和渠道 gate 共用 Task 2 的单个 30 秒 operation context；POST 请求不可重放，CAS 冲突只重读本地配置并合并状态。

## 实现提交

- Commit: `ff05597d2de4495b185869d5231e83fabc663843`
- Message: `feat: support New API management checkin`
- Base: `89ce1d010eb8da7d01e81eac19c596537b6490db`

## 文件

- `internal/app/channel_management_new_api.go`：New API DTO、余额解析、签到状态机、安全分类及请求头。
- `internal/app/channel_management_new_api_test.go`：余额、失败边界、状态机请求序列、CAS 冲突和 Sub2API dispatch 契约测试。
- `internal/app/channel_management_service.go`：仅 dispatch `new_api`；合并签到状态、时间和余额后执行一次逻辑 CAS 更新。

## RED / GREEN 证据

### 余额

- RED：`go test ./internal/app -run 'NewAPI.*Balance'`
- 真实失败：`service.refreshNewAPIBalance undefined`
- GREEN：同一命令通过，覆盖 Bearer、显式/缺失 `New-API-User`、原始 quota、缺失 used、负数、加法溢出、business false、缺/null data、非 JSON、超限 body、401/403 和秘密不泄漏。

### 签到状态机

- RED：`go test ./internal/app -run 'NewAPI.*Checkin'`
- 真实失败：`service.checkInNewAPI undefined`
- 首次 GREEN 尝试暴露测试 transport 对 nil GET body 的错误读取并 panic；修正测试夹具后，同一命令通过。
- 请求脚本逐次校验 method、完整 URL、`month=2026-08`、Bearer、`New-API-User`、POST `{}`、`Content-Type: application/json`、`WroteRequest` 和总请求数。

### Service / CAS

- RED：`go test ./internal/app -run 'ChannelManagementServiceNewAPI'`
- 真实失败：`RefreshBalance` 和 `CheckIn` 均返回 `channel_management_provider_unavailable`。
- GREEN：同一命令通过；强制首个 CAS 冲突时只产生四次既定上游请求和一次 POST，第二次 CAS 候选仍同时包含 checkin status/time/balance，并保留并发写入的 `LastScheduledDay`。

### 最终验证

- `go test -count=1 ./internal/app -run 'NewAPI|ChannelManagementService'`
- 结果：`ok ccLoad/internal/app 1.364s`
- 提交钩子：`golangci-lint ./internal/app/...`，结果 `0 issues`。

## 状态矩阵

| 场景 | 请求序列 | POST 次数 | 结果 | 刷新余额 |
|---|---|---:|---|---:|
| 全局关闭 | status | 0 | `skipped_disabled` | 否 |
| 当日已签到 | status → monthly | 0 | `already_checked` | 是 |
| POST 成功 | status → monthly → POST | 1 | `success` | 是 |
| POST business false，读回已签到 | status → monthly → POST → monthly | 1 | `already_checked` | 是 |
| Turnstile，读回未签到 | status → monthly → POST → monthly | 1 | `manual_required` | 否 |
| 404/405 | 在失败端点停止；POST 失败时再读回一次 | 0 或 1 | `unsupported` | 否 |
| 401/403 | 在失败端点停止；POST 失败时再读回一次 | 0 或 1 | `credential_invalid` | 否 |
| POST 已写出且读回失败 | status → monthly → POST → monthly | 1 | `uncertain` | 否 |
| POST 未写出 | status → monthly → POST | 1 次调用、0 次写出 | 安全上游错误 | 否 |

POST 失败最多执行一次只读 monthly readback；CAS 冲突不进入状态机，因此不会产生第二次 POST 或任何额外上游请求。读回已签到优先于 Turnstile、unsupported 和 credential 分类。

## 数据与安全

- 持久化仅保留 `remaining_raw`、可选 `used_raw`/`total_raw`、`divisor: 500000` 和 `sampled_at`；USD 与百分比只在 View 构造时换算。
- `used_quota` 缺失时不伪造 used、total 或 available percent。
- 所有上游错误只返回固定安全错误；token、Authorization、原始 body 和业务 message 均不进入错误或日志。
- 仅配置显式 `user_id` 时发送 `New-API-User`。

## 遗留问题

- 无 Task 3 阻塞问题。
- Sub2API / Sub2API Pro 仍明确返回 `channel_management_provider_unavailable`，按计划留给 Task 4。
- 签到已成功或确认已签到后，余额刷新失败不会否定签到结果；状态仍会持久化，旧余额保持不变。
