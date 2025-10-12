package main

import (
	"ccLoad/internal/model"
	"testing"

	"github.com/bytedance/sonic"
)

// TestPrepareRequestBody_WithRedirect 测试模型重定向功能
func TestPrepareRequestBody_WithRedirect(t *testing.T) {
	// 准备测试数据
	cfg := &model.Config{
		ModelRedirects: map[string]string{
			"gpt-5":                  "gpt-5-2025-08-07",
			"claude-3-opus":          "claude-3-5-sonnet-20241022",
			"gemini-pro":             "gemini-2.0-flash-exp",
		},
	}

	tests := []struct {
		name              string
		originalModel     string
		expectedModel     string
		shouldModifyBody  bool
		description       string
	}{
		{
			name:              "重定向gpt-5到gpt-5-2025-08-07",
			originalModel:     "gpt-5",
			expectedModel:     "gpt-5-2025-08-07",
			shouldModifyBody:  true,
			description:       "测试OpenAI模型重定向",
		},
		{
			name:              "重定向claude-3-opus",
			originalModel:     "claude-3-opus",
			expectedModel:     "claude-3-5-sonnet-20241022",
			shouldModifyBody:  true,
			description:       "测试Claude模型重定向",
		},
		{
			name:              "重定向gemini-pro",
			originalModel:     "gemini-pro",
			expectedModel:     "gemini-2.0-flash-exp",
			shouldModifyBody:  true,
			description:       "测试Gemini模型重定向",
		},
		{
			name:              "无重定向映射的模型保持不变",
			originalModel:     "gpt-4o",
			expectedModel:     "gpt-4o",
			shouldModifyBody:  false,
			description:       "测试未配置重定向的模型",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 构建测试请求体
			originalBody := map[string]any{
				"model": tt.originalModel,
				"messages": []map[string]string{
					{"role": "user", "content": "test"},
				},
			}
			bodyBytes, _ := sonic.Marshal(originalBody)

			reqCtx := &proxyRequestContext{
				originalModel: tt.originalModel,
				body:          bodyBytes,
			}

			// 执行测试
			actualModel, bodyToSend := prepareRequestBody(cfg, reqCtx)

			// 验证返回的模型名称
			if actualModel != tt.expectedModel {
				t.Errorf("%s\n期望模型: %s, 实际模型: %s",
					tt.description, tt.expectedModel, actualModel)
			}

			// 验证请求体修改
			var modifiedBody map[string]any
			if err := sonic.Unmarshal(bodyToSend, &modifiedBody); err != nil {
				t.Fatalf("解析修改后的请求体失败: %v", err)
			}

			modifiedModel, ok := modifiedBody["model"].(string)
			if !ok {
				t.Fatal("请求体中缺少model字段")
			}

			if modifiedModel != tt.expectedModel {
				t.Errorf("请求体中的模型名称错误\n期望: %s, 实际: %s",
					tt.expectedModel, modifiedModel)
			}

			// 验证是否应该修改请求体
			if tt.shouldModifyBody {
				if string(bodyToSend) == string(bodyBytes) {
					t.Error("预期请求体应该被修改，但实际未修改")
				}
			}
		})
	}
}

// TestPrepareRequestBody_NoRedirectConfig 测试无重定向配置的场景
func TestPrepareRequestBody_NoRedirectConfig(t *testing.T) {
	tests := []struct {
		name          string
		cfg           *model.Config
		originalModel string
		description   string
	}{
		{
			name: "空重定向映射",
			cfg: &model.Config{
				ModelRedirects: map[string]string{},
			},
			originalModel: "gpt-4o",
			description:   "测试空的ModelRedirects配置",
		},
		{
			name: "nil重定向映射",
			cfg: &model.Config{
				ModelRedirects: nil,
			},
			originalModel: "claude-3-5-sonnet",
			description:   "测试nil的ModelRedirects配置",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 构建测试请求体
			originalBody := map[string]any{
				"model": tt.originalModel,
				"messages": []map[string]string{
					{"role": "user", "content": "test"},
				},
			}
			bodyBytes, _ := sonic.Marshal(originalBody)

			reqCtx := &proxyRequestContext{
				originalModel: tt.originalModel,
				body:          bodyBytes,
			}

			// 执行测试
			actualModel, bodyToSend := prepareRequestBody(tt.cfg, reqCtx)

			// 验证模型名称不变
			if actualModel != tt.originalModel {
				t.Errorf("%s\n模型名称应保持不变\n期望: %s, 实际: %s",
					tt.description, tt.originalModel, actualModel)
			}

			// 验证请求体未修改
			if string(bodyToSend) != string(bodyBytes) {
				t.Error("请求体不应该被修改")
			}
		})
	}
}

// TestPrepareRequestBody_EmptyRedirect 测试重定向到空字符串的场景
func TestPrepareRequestBody_EmptyRedirect(t *testing.T) {
	cfg := &model.Config{
		ModelRedirects: map[string]string{
			"test-model": "", // 空字符串重定向
		},
	}

	reqCtx := &proxyRequestContext{
		originalModel: "test-model",
		body:          []byte(`{"model":"test-model","messages":[]}`),
	}

	actualModel, _ := prepareRequestBody(cfg, reqCtx)

	// 空字符串重定向应被忽略，保持原模型名称
	if actualModel != "test-model" {
		t.Errorf("空字符串重定向应被忽略\n期望: test-model, 实际: %s", actualModel)
	}
}

// TestPrepareRequestBody_ComplexJSON 测试复杂JSON结构的请求体修改
func TestPrepareRequestBody_ComplexJSON(t *testing.T) {
	cfg := &model.Config{
		ModelRedirects: map[string]string{
			"gpt-4": "gpt-4-turbo",
		},
	}

	// 复杂的请求体结构
	complexBody := map[string]any{
		"model": "gpt-4",
		"messages": []map[string]any{
			{
				"role": "system",
				"content": "You are a helpful assistant",
			},
			{
				"role": "user",
				"content": []map[string]string{
					{"type": "text", "text": "Hello"},
				},
			},
		},
		"temperature": 0.7,
		"max_tokens":  1000,
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "Get weather info",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]string{"type": "string"},
						},
					},
				},
			},
		},
	}

	bodyBytes, _ := sonic.Marshal(complexBody)

	reqCtx := &proxyRequestContext{
		originalModel: "gpt-4",
		body:          bodyBytes,
	}

	// 执行测试
	actualModel, bodyToSend := prepareRequestBody(cfg, reqCtx)

	// 验证模型重定向
	if actualModel != "gpt-4-turbo" {
		t.Errorf("期望模型: gpt-4-turbo, 实际模型: %s", actualModel)
	}

	// 验证复杂JSON结构完整性
	var modifiedBody map[string]any
	if err := sonic.Unmarshal(bodyToSend, &modifiedBody); err != nil {
		t.Fatalf("解析修改后的请求体失败: %v", err)
	}

	// 验证model字段修改
	if modifiedBody["model"] != "gpt-4-turbo" {
		t.Error("model字段未正确修改")
	}

	// 验证其他字段保持不变
	if modifiedBody["temperature"] != 0.7 {
		t.Error("temperature字段被意外修改")
	}

	if modifiedBody["max_tokens"] != float64(1000) { // JSON数字解析为float64
		t.Error("max_tokens字段被意外修改")
	}

	// 验证messages数组结构
	messages, ok := modifiedBody["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Error("messages字段结构被破坏")
	}

	// 验证tools数组结构
	tools, ok := modifiedBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Error("tools字段结构被破坏")
	}
}

// TestPrepareRequestBody_InvalidJSON 测试无效JSON的容错处理
func TestPrepareRequestBody_InvalidJSON(t *testing.T) {
	cfg := &model.Config{
		ModelRedirects: map[string]string{
			"gpt-4": "gpt-4-turbo",
		},
	}

	// 无效的JSON
	invalidBody := []byte(`{"model":"gpt-4","messages":invalid}`)

	reqCtx := &proxyRequestContext{
		originalModel: "gpt-4",
		body:          invalidBody,
	}

	// 执行测试
	actualModel, bodyToSend := prepareRequestBody(cfg, reqCtx)

	// 验证模型重定向仍然生效
	if actualModel != "gpt-4-turbo" {
		t.Errorf("即使JSON无效，模型重定向仍应生效\n期望: gpt-4-turbo, 实际: %s", actualModel)
	}

	// 验证无效JSON不会导致panic，原始body应保持不变
	if string(bodyToSend) != string(invalidBody) {
		t.Error("无效JSON时请求体应保持不变")
	}
}

// TestPrepareRequestBody_ModelFieldMissing 测试缺少model字段的请求体
func TestPrepareRequestBody_ModelFieldMissing(t *testing.T) {
	cfg := &model.Config{
		ModelRedirects: map[string]string{
			"default-model": "upgraded-model",
		},
	}

	// 请求体中缺少model字段
	bodyWithoutModel := map[string]any{
		"messages": []map[string]string{
			{"role": "user", "content": "test"},
		},
		"temperature": 0.5,
	}
	bodyBytes, _ := sonic.Marshal(bodyWithoutModel)

	reqCtx := &proxyRequestContext{
		originalModel: "default-model",
		body:          bodyBytes,
	}

	// 执行测试
	actualModel, bodyToSend := prepareRequestBody(cfg, reqCtx)

	// 验证模型重定向
	if actualModel != "upgraded-model" {
		t.Errorf("期望模型: upgraded-model, 实际模型: %s", actualModel)
	}

	// 验证请求体添加了model字段
	var modifiedBody map[string]any
	if err := sonic.Unmarshal(bodyToSend, &modifiedBody); err != nil {
		t.Fatalf("解析修改后的请求体失败: %v", err)
	}

	if modifiedBody["model"] != "upgraded-model" {
		t.Error("应该添加model字段到请求体")
	}
}

// BenchmarkPrepareRequestBody 性能基准测试
func BenchmarkPrepareRequestBody(b *testing.B) {
	cfg := &model.Config{
		ModelRedirects: map[string]string{
			"gpt-4": "gpt-4-turbo",
		},
	}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"test"}]}`)
	reqCtx := &proxyRequestContext{
		originalModel: "gpt-4",
		body:          body,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = prepareRequestBody(cfg, reqCtx)
	}
}

// BenchmarkPrepareRequestBody_NoRedirect 无重定向场景性能测试
func BenchmarkPrepareRequestBody_NoRedirect(b *testing.B) {
	cfg := &model.Config{
		ModelRedirects: map[string]string{
			"gpt-4": "gpt-4-turbo",
		},
	}

	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"test"}]}`)
	reqCtx := &proxyRequestContext{
		originalModel: "claude-3",
		body:          body,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = prepareRequestBody(cfg, reqCtx)
	}
}
