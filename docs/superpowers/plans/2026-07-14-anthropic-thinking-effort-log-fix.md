# Anthropic Thinking Effort Log Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Anthropic 请求的旧式 `thinking.budget_tokens` 归一成日志思考等级，避免把 `thinking.type=enabled` 显示为等级。

**Architecture:** 修复现有请求元数据提取入口，不改代理协议或前端。显式 effort/level 继续优先；缺失显式等级时复用现有 `anthropicBudgetToEffort`，并彻底移除 `thinking.type` 的等级语义。

**Tech Stack:** Go 1.24、Gin、Sonic JSON、现有内存存储代理集成测试。

## Global Constraints

- 所有 Go 命令必须带 `-tags sonic`。
- 测试必须覆盖“代理请求 → 持久化日志”的公开边界，不得直接测试私有提取函数。
- 复用现有 `internal/app/proxy_integration_test.go`，不新增测试文件。
- 不修改请求转发、协议转换、数据库 Schema 或前端展示代码。
- `budget_tokens` 映射必须复用现有阈值：`1..4095 → low`、`4096..16383 → medium`、`>=16384 → high`。

---

### Task 1: 正确记录旧式 Anthropic 请求的思考等级

**Files:**
- Modify: `internal/app/proxy_integration_test.go:400`
- Modify: `internal/app/proxy_util.go:778-782`

**Interfaces:**
- Consumes: 代理入口 `POST /v1/messages`、现有 `anthropicBudgetToEffort(budget int) string`、持久化日志字段 `model.LogEntry.ThinkingEffort string`。
- Produces: `extractThinkingEffortFromPayload(payload map[string]any) string` 对 `thinking.budget_tokens` 返回统一等级，不再返回 `thinking.type`。

- [ ] **Step 1: 写入失败的代理日志回归测试**

在 `internal/app/proxy_integration_test.go` 的现有思考等级日志测试旁加入：

```go
func TestProxy_LogsAnthropicBudgetAsThinkingEffort(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"mimo-v2.5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "fufu-thinking", models: "mimo-v2.5", apiKey: "sk-fufu-thinking", channelType: util.ChannelTypeAnthropic},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/messages", map[string]any{
		"model":      "mimo-v2.5",
		"max_tokens": 32000,
		"thinking": map[string]any{
			"type":          "enabled",
			"budget_tokens": 31999,
			"display":       "summarized",
		},
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]string{{
				"type": "text",
				"text": "hi",
			}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "mimo-v2.5")
	if entry.ThinkingEffort != "high" {
		t.Fatalf("ThinkingEffort=%q, want high", entry.ThinkingEffort)
	}
}
```

- [ ] **Step 2: 运行定向测试并确认按预期失败**

Run:

```bash
go test -tags sonic ./internal/app -run '^TestProxy_LogsAnthropicBudgetAsThinkingEffort$' -count=1
```

Expected: FAIL，错误包含 `ThinkingEffort="enabled", want high`，证明用例复现了 `docs/1.txt` 中的缺陷。

- [ ] **Step 3: 实施最小根因修复**

将 `internal/app/proxy_util.go` 的 Anthropic `thinking` 分支改为：

```go
if thinking, ok := payload["thinking"].(map[string]any); ok {
	if effort := firstStringMapValue(thinking, "effort", "level", "thinkingLevel", "thinking_level"); effort != "" {
		return normalizeThinkingEffort(effort)
	}
	if budget, ok := thinking["budget_tokens"].(float64); ok && budget > 0 {
		return anthropicBudgetToEffort(int(budget))
	}
}
```

- [ ] **Step 4: 运行定向与相关回归测试并确认通过**

Run:

```bash
go test -tags sonic ./internal/app -run 'TestProxy_Logs.*ThinkingEffort|TestExtractThinkingEffort' -count=1
```

Expected: PASS；fufu 请求记录 `high`，现有显式 `output_config.effort`、Codex `reasoning.effort` 和响应覆盖行为保持通过。

- [ ] **Step 5: 运行项目级验证**

Run:

```bash
go test -tags sonic ./internal/...
golangci-lint run ./...
```

Expected: 两条命令均退出码 0，测试无失败，lint 无警告。

- [ ] **Step 6: 提交修复**

```bash
git add internal/app/proxy_util.go internal/app/proxy_integration_test.go
git commit -m "fix(logs): derive Anthropic thinking effort from budget"
```
