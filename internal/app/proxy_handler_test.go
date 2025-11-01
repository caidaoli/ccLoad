package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandleProxyRequest_UnknownPathReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{
		concurrencySem: make(chan struct{}, 1),
	}

	body := bytes.NewBufferString(`{"model":"gpt-4"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/unknown", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	srv.HandleProxyRequest(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("预期状态码404，实际%d", w.Code)
	}

	if body := w.Body.String(); !bytes.Contains([]byte(body), []byte("unsupported path")) {
		t.Fatalf("响应内容缺少错误信息，实际: %s", body)
	}
}

// ============================================================================
// 增加proxy_handler测试覆盖率
// ============================================================================

// TestParseIncomingRequest_ValidJSON 测试有效JSON解析
func TestParseIncomingRequest_ValidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		body         string
		path         string
		expectModel  string
		expectStream bool
		expectError  bool
	}{
		{
			name:         "有效JSON-claude模型",
			body:         `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hello"}]}`,
			path:         "/v1/messages",
			expectModel:  "claude-3-5-sonnet-20241022",
			expectStream: false,
			expectError:  false,
		},
		{
			name:         "流式请求-stream=true",
			body:         `{"model":"gpt-4","stream":true,"messages":[]}`,
			path:         "/v1/chat/completions",
			expectModel:  "gpt-4",
			expectStream: true,
			expectError:  false,
		},
		{
			name:         "空模型名-从路径提取",
			body:         `{"messages":[{"role":"user","content":"test"}]}`,
			path:         "/v1/models/gpt-4/completions",
			expectModel:  "gpt-4",
			expectStream: false,
			expectError:  false,
		},
		{
			name:         "GET请求-无模型使用通配符",
			body:         "",
			path:         "/v1/models",
			expectModel:  "*",
			expectStream: false,
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bytes.NewBufferString(tt.body)
			req := httptest.NewRequest(http.MethodPost, tt.path, body)
			if tt.body == "" {
				req.Method = http.MethodGet
			}
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			model, _, isStreaming, err := parseIncomingRequest(c)

			if tt.expectError && err == nil {
				t.Errorf("期望错误但未发生")
			}
			if !tt.expectError && err != nil {
				t.Errorf("不期望错误但发生: %v", err)
			}
			if model != tt.expectModel {
				t.Errorf("模型名错误: 期望%s, 实际%s", tt.expectModel, model)
			}
			if isStreaming != tt.expectStream {
				t.Errorf("流式标志错误: 期望%v, 实际%v", tt.expectStream, isStreaming)
			}
		})
	}
}

// TestParseIncomingRequest_BodyTooLarge 测试请求体过大
func TestParseIncomingRequest_BodyTooLarge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 创建超大请求体（>2MB）
	largeBody := make([]byte, 3*1024*1024) // 3MB
	for i := range largeBody {
		largeBody[i] = 'a'
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(largeBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	_, _, _, err := parseIncomingRequest(c)

	if err != errBodyTooLarge {
		t.Errorf("期望errBodyTooLarge错误, 实际: %v", err)
	}
}

// TestAcquireConcurrencySlot 测试并发槽位获取
func TestAcquireConcurrencySlot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{
		concurrencySem: make(chan struct{}, 2), // 最大并发数=2
		maxConcurrency: 2,
	}

	// 创建有效的gin.Context
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	// 第一次获取应该成功
	release1, acquired1 := srv.acquireConcurrencySlot(c)
	if !acquired1 {
		t.Fatal("第一次获取应该成功")
	}

	// 第二次获取应该成功
	release2, acquired2 := srv.acquireConcurrencySlot(c)
	if !acquired2 {
		t.Fatal("第二次获取应该成功")
	}

	// 释放一个槽位
	release1()

	// 现在应该可以再次获取
	release3, acquired3 := srv.acquireConcurrencySlot(c)
	if !acquired3 {
		t.Fatal("释放后再次获取应该成功")
	}

	// 清理
	release2()
	release3()

	t.Log("✅ 并发控制测试通过：2个槽位正确管理")
}
