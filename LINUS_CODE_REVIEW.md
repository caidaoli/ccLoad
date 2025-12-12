# ccLoad 代码审查问题清单（Linus 风格）

本文档基于对项目全量代码的审读与 `go test -tags go_json ./...`、`go vet` 结果整理。目标是直接指出会"咬人"的设计与实现问题，并给出可落地的整改方向。优先级从 P0（必须立刻修）到 P3（可排期优化）。

---

## P0 立刻修复（架构/一致性级）

### 1. ~~SQLite 并发写导致冷却更新不可靠~~ ✅ 已修复 (2025-12-12)
- **位置**
  - `internal/storage/factory.go`：`CreateSQLiteStore` 连接池配置
- **修复方案**
  - 强制单连接 `SetMaxOpenConns(1)`，由 database/sql 串行化所有事务
  - 热读已被缓存层吸收（Channel/APIKey/Cooldown cache），性能影响有限
  - 保留重试逻辑作为跨进程场景兜底
- **验证**
  - `TestConcurrentKeyCooldown` 现在稳定通过

---

## P1 高优先级（正确性/未来扩展风险）

### 2. ~~Round‑Robin Key 选择对 KeyIndex 连续性的隐式假设~~ ✅ 已修复 (2025-12-12)
- **位置**
  - `internal/app/key_selector.go`：`selectRoundRobin`
- **修复方案**
  - 删除 `findKeyByIndex` 函数（O(n²) → O(n)）
  - RR 按 slice 索引轮询，返回真实 `apiKey.KeyIndex`
  - 单Key场景用真实 KeyIndex 检查排除集合
- **新增测试**
  - `TestSelectAvailableKey_RoundRobin_NonContiguousKeyIndex` - 非连续 KeyIndex 轮询
  - `TestSelectAvailableKey_SingleKey_NonZeroKeyIndex` - 单Key非零KeyIndex排除

### 3. ~~1308 重置时间解析过于脆弱~~ ✅ 已修复 (2025-12-12)
- **位置**
  - `internal/util/classifier.go`：`ParseResetTimeFrom1308Error`
- **修复方案**
  - 使用正则 `\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}` 匹配时间
  - 不再依赖中文文案（"将在"/"重置"）
- **验证**
  - 现有测试全部通过，兼容多语言场景

---

## P2 中优先级（可维护性/风格一致性）

### 4. ~~注释与实现不一致~~ ✅ 附带修复 (2025-12-12)
- **位置**
  - `internal/util/classifier.go`：`ParseResetTimeFrom1308Error`
- **状态**
  - 重写函数时已更新注释，移除"使用 sonic 解析"的误导性描述

### 5. ~~`NewServer` 过载（配置加载/校验/初始化混杂）~~ ✅ 已修复 (2025-12-12)
- **位置**
  - `internal/app/server.go`：`NewServer`
  - `internal/app/config_service.go`：新增带约束的配置API
- **修复方案**
  - 新增 `GetIntMin`、`GetDurationNonNegative`、`GetDurationPositive` 等带约束的配置读取方法
  - 配置验证逻辑收敛到 `ConfigService`（SRP）
  - `NewServer` 直接调用带约束的API，不再做inline校验

---

## P3 低优先级（安全/体验）

### 6. ~~Web 管理台大量 `innerHTML` 拼接~~ ✅ 已修复 (2025-12-12)
- **位置**
  - `web/assets/js/ui.js`：`ChannelTypeManager` 模块
  - `web/assets/js/channels-test.js`：`displayTestResult` 函数
  - `web/assets/js/logs.js`：测试结果显示
- **修复方案**
  - 在 `ChannelTypeManager` 内添加 `escapeHtml` 函数
  - 所有动态拼接HTML的位置统一使用 `escapeHtml` 转义
  - 修复 `renderChannelTypeRadios`、`renderChannelTypeSelect`、`renderChannelTypeFilter`、`renderChannelTypeTabs` 等函数

---

## 总体评价
- **优点**
  - 核心分层清晰（HTTP/app、cooldown、storage、util），缓存与错误分类是正确方向。
  - `request_context.go` 使用 `context.AfterFunc` + 必 `defer cancel`，无 goroutine 泄漏。
  - 关闭链路 `main.go` + `Server.Shutdown` 逻辑清楚，资源回收完整。
- **所有问题已修复** ✅
  - P0/P1/P2/P3 全部问题已于 2025-12-12 修复完成

---

## 整改进度

| 优先级 | 问题 | 状态 | 日期 |
|--------|------|------|------|
| P0 | SQLite 并发写 | ✅ 已修复 | 2025-12-12 |
| P1 | RR KeyIndex 假设 | ✅ 已修复 | 2025-12-12 |
| P1 | 1308 时间解析 | ✅ 已修复 | 2025-12-12 |
| P2 | 注释不一致 | ✅ 附带修复 | 2025-12-12 |
| P2 | NewServer 过载 | ✅ 已修复 | 2025-12-12 |
| P3 | innerHTML XSS | ✅ 已修复 | 2025-12-12 |

