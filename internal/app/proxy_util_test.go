package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/model"
)

func TestWriteResponseWithHeaders_PreservesContentType(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	hdr := http.Header{}
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Connection", "keep-alive") // hop-by-hop should be stripped

	writeResponseWithHeaders(w, http.StatusBadGateway, hdr, []byte("oops"))

	if got := w.Code; got != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d", http.StatusBadGateway, got)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected Content-Type preserved, got %q", got)
	}
	if got := w.Header().Get("Connection"); got != "" {
		t.Fatalf("expected hop-by-hop header stripped, got %q", got)
	}
	if got := w.Body.String(); got != "oops" {
		t.Fatalf("expected body preserved, got %q", got)
	}
}

func TestWriteResponseWithHeaders_DefaultsToJSONContentTypeWhenBodyLooksJSON(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeResponseWithHeaders(w, http.StatusBadGateway, nil, []byte(`{"error":"x"}`))

	if got := w.Code; got != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d", http.StatusBadGateway, got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("expected Content-Type json, got %q", got)
	}
}

func TestBuildLogEntry_StreamDiagMsg(t *testing.T) {
	channelID := int64(1)

	t.Run("正常成功响应", func(t *testing.T) {
		res := &fwResult{
			Status:       200,
			InputTokens:  10,
			OutputTokens: 20,
		}
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
		})
		if entry.Message != "ok" {
			t.Errorf("expected Message='ok', got %q", entry.Message)
		}
	})

	t.Run("流传输中断诊断", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流传输中断: 错误=unexpected EOF | 已读取=1024字节(分5次)",
		}
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
		})
		if entry.Message != res.StreamDiagMsg {
			t.Errorf("expected Message=%q, got %q", res.StreamDiagMsg, entry.Message)
		}
	})

	t.Run("流响应不完整诊断", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流响应不完整: 正常EOF但无usage | 已读取=512字节(分3次)",
		}
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
		})
		if entry.Message != res.StreamDiagMsg {
			t.Errorf("expected Message=%q, got %q", res.StreamDiagMsg, entry.Message)
		}
	})

	t.Run("errMsg优先于StreamDiagMsg", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流传输中断",
		}
		errMsg := "network error"
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
			ErrMsg:       errMsg,
		})
		if entry.Message != errMsg {
			t.Errorf("expected Message=%q, got %q", errMsg, entry.Message)
		}
	})
}

func TestCopyRequestHeaders_StripsHopByHopAndAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	src := http.Header{}
	src.Set("Connection", "Upgrade, X-Hop")
	src.Set("Upgrade", "websocket")
	src.Set("X-Hop", "1")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("TE", "trailers")
	src.Set("Trailer", "X-Trailer")
	src.Set("Proxy-Authorization", "secret")
	src.Set("Authorization", "Bearer client-token")
	src.Set("X-API-Key", "client-token2")
	src.Set("x-goog-api-key", "client-goog")
	src.Set("Accept-Encoding", "br")
	src.Set("X-Pass", "ok")

	copyRequestHeaders(req, src)

	if got := req.Header.Get("X-Pass"); got != "ok" {
		t.Fatalf("expected X-Pass=ok, got %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("expected default Accept=application/json, got %q", got)
	}

	for _, k := range []string{
		"Connection",
		"Upgrade",
		"X-Hop",
		"Keep-Alive",
		"TE",
		"Trailer",
		"Proxy-Authorization",
		"Authorization",
		"X-API-Key",
		"x-goog-api-key",
		"Accept-Encoding",
	} {
		if v := req.Header.Get(k); v != "" {
			t.Fatalf("expected header %q stripped, got %q", k, v)
		}
	}
}

func TestFilterAndWriteResponseHeaders_StripsHopByHop(t *testing.T) {
	w := httptest.NewRecorder()

	hdr := http.Header{}
	hdr.Set("Connection", "Upgrade, X-Hop")
	hdr.Set("Upgrade", "websocket")
	hdr.Set("X-Hop", "1")
	hdr.Set("Transfer-Encoding", "chunked")
	hdr.Set("Trailer", "X-Trailer")
	hdr.Set("Content-Length", "123")
	hdr.Set("Content-Encoding", "br")
	hdr.Set("X-Pass", "ok")

	filterAndWriteResponseHeaders(w, hdr)

	if got := w.Header().Get("X-Pass"); got != "ok" {
		t.Fatalf("expected X-Pass=ok, got %q", got)
	}
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("expected Content-Encoding=br, got %q", got)
	}

	for _, k := range []string{
		"Connection",
		"Upgrade",
		"X-Hop",
		"Transfer-Encoding",
		"Trailer",
		"Content-Length",
	} {
		if v := w.Header().Get(k); v != "" {
			t.Fatalf("expected header %q stripped, got %q", k, v)
		}
	}
}

// TestPrepareRequestBody_FuzzyMatch 测试模糊匹配模型名替换
// 确保 model_fuzzy_match 启用时，请求体中的模型名会被替换为匹配到的实际模型名
func TestPrepareRequestBody_FuzzyMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		modelFuzzyMatch bool
		configModels    []model.ModelEntry
		originalModel   string
		requestBody     string
		wantModel       string
		wantBodyModel   string // 期望请求体中的模型名
	}{
		{
			name:            "精确匹配_不修改模型名",
			modelFuzzyMatch: true,
			configModels:    []model.ModelEntry{{Model: "gpt-4"}},
			originalModel:   "gpt-4",
			requestBody:     `{"model":"gpt-4","messages":[]}`,
			wantModel:       "gpt-4",
			wantBodyModel:   "gpt-4",
		},
		{
			name:            "模糊匹配_替换为实际模型名",
			modelFuzzyMatch: true,
			configModels:    []model.ModelEntry{{Model: "gemini-2.5-flash"}},
			originalModel:   "flash", // 用户请求的模糊名称
			requestBody:     `{"model":"flash","messages":[]}`,
			wantModel:       "gemini-2.5-flash",
			wantBodyModel:   "gemini-2.5-flash",
		},
		{
			name:            "模糊匹配关闭_不替换模型名",
			modelFuzzyMatch: false,
			configModels:    []model.ModelEntry{{Model: "gemini-2.5-flash"}},
			originalModel:   "flash",
			requestBody:     `{"model":"flash","messages":[]}`,
			wantModel:       "flash", // 不替换
			wantBodyModel:   "flash",
		},
		{
			name:            "模糊匹配_多个候选选最新版本",
			modelFuzzyMatch: true,
			configModels: []model.ModelEntry{
				{Model: "claude-sonnet-4-5-20250514"},
				{Model: "claude-sonnet-4-5-20250929"},
			},
			originalModel: "sonnet",
			requestBody:   `{"model":"sonnet","messages":[]}`,
			wantModel:     "claude-sonnet-4-5-20250929", // 最新版本
			wantBodyModel: "claude-sonnet-4-5-20250929",
		},
		{
			name:            "重定向优先于模糊匹配",
			modelFuzzyMatch: true,
			configModels: []model.ModelEntry{
				{Model: "gpt-4", RedirectModel: "gpt-4-turbo"},
				{Model: "gpt-4-turbo"},
			},
			originalModel: "gpt-4",
			requestBody:   `{"model":"gpt-4","messages":[]}`,
			wantModel:     "gpt-4-turbo", // 重定向优先
			wantBodyModel: "gpt-4-turbo",
		},
		{
			name:            "模糊匹配_无匹配时保持原样",
			modelFuzzyMatch: true,
			configModels:    []model.ModelEntry{{Model: "gpt-4"}},
			originalModel:   "claude",
			requestBody:     `{"model":"claude","messages":[]}`,
			wantModel:       "claude", // 无匹配，保持原样
			wantBodyModel:   "claude",
		},
		{
			// 注意：gemini-3-flash 不包含于 gemini-2.5-flash，因此不会匹配
			// 模糊匹配是子串包含，不是相似度匹配
			name:            "模糊匹配_不同版本号不匹配",
			modelFuzzyMatch: true,
			configModels:    []model.ModelEntry{{Model: "gemini-2.5-flash"}},
			originalModel:   "gemini-3-flash", // 不存在的模型
			requestBody:     `{"model":"gemini-3-flash","messages":[]}`,
			wantModel:       "gemini-3-flash", // 不匹配，保持原样
			wantBodyModel:   "gemini-3-flash",
		},
		{
			// 子串匹配：flash 包含于 gemini-2.5-flash
			name:            "模糊匹配_子串匹配成功",
			modelFuzzyMatch: true,
			configModels:    []model.ModelEntry{{Model: "gemini-2.5-flash"}},
			originalModel:   "2.5-flash", // 子串
			requestBody:     `{"model":"2.5-flash","messages":[]}`,
			wantModel:       "gemini-2.5-flash",
			wantBodyModel:   "gemini-2.5-flash",
		},
		{
			// 核心场景：gemini-3-flash → gemini-3-flash-preview
			// gemini-3-flash 是 gemini-3-flash-preview 的子串
			name:            "模糊匹配_gemini-3-flash到preview版本",
			modelFuzzyMatch: true,
			configModels:    []model.ModelEntry{{Model: "gemini-3-flash-preview"}},
			originalModel:   "gemini-3-flash",
			requestBody:     `{"model":"gemini-3-flash","messages":[]}`,
			wantModel:       "gemini-3-flash-preview",
			wantBodyModel:   "gemini-3-flash-preview",
		},
		{
			// [FIX] 2026-01: 链式解析场景
			// gemini-3-flash → 模糊匹配 gemini-3-flash-preview → 重定向 gemini-3-flash-preview-0719
			name:            "链式解析_模糊匹配后再重定向",
			modelFuzzyMatch: true,
			configModels: []model.ModelEntry{
				{Model: "gemini-3-flash-preview", RedirectModel: "gemini-3-flash-preview-0719"},
				{Model: "gemini-3-flash-preview-0719"},
			},
			originalModel: "gemini-3-flash",
			requestBody:   `{"model":"gemini-3-flash","messages":[]}`,
			wantModel:     "gemini-3-flash-preview-0719", // 模糊匹配后再重定向
			wantBodyModel: "gemini-3-flash-preview-0719",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// 构造 Server（只设置 modelFuzzyMatch）
			s := &Server{
				modelFuzzyMatch: tt.modelFuzzyMatch,
			}

			// 构造 Config
			cfg := &model.Config{
				ModelEntries: tt.configModels,
			}

			// 构造请求上下文
			reqCtx := &proxyRequestContext{
				originalModel: tt.originalModel,
				body:          []byte(tt.requestBody),
			}

			// 调用被测函数
			actualModel, bodyToSend := s.prepareRequestBody(cfg, reqCtx)

			// 验证返回的模型名
			if actualModel != tt.wantModel {
				t.Errorf("actualModel = %q, want %q", actualModel, tt.wantModel)
			}

			// 验证请求体中的模型名
			var reqData map[string]any
			if err := json.Unmarshal(bodyToSend, &reqData); err != nil {
				t.Fatalf("failed to unmarshal body: %v", err)
			}
			if gotModel, _ := reqData["model"].(string); gotModel != tt.wantBodyModel {
				t.Errorf("body model = %q, want %q", gotModel, tt.wantBodyModel)
			}
		})
	}
}

// TestReplaceModelInPath_GeminiAPI 测试 Gemini API URL 路径中模型名替换
// [FIX] 2026-01: 验证模糊匹配后 URL 路径中的模型名也被正确替换
func TestReplaceModelInPath_GeminiAPI(t *testing.T) {
	tests := []struct {
		name          string
		originalPath  string
		originalModel string
		actualModel   string
		wantPath      string
	}{
		{
			name:          "Gemini streamGenerateContent 模型名替换",
			originalPath:  "/v1beta/models/gemini-3-flash:streamGenerateContent",
			originalModel: "gemini-3-flash",
			actualModel:   "gemini-3-flash-preview",
			wantPath:      "/v1beta/models/gemini-3-flash-preview:streamGenerateContent",
		},
		{
			name:          "Gemini generateContent 模型名替换",
			originalPath:  "/v1beta/models/gemini-pro:generateContent",
			originalModel: "gemini-pro",
			actualModel:   "gemini-1.5-pro",
			wantPath:      "/v1beta/models/gemini-1.5-pro:generateContent",
		},
		{
			name:          "模型名未变更不替换",
			originalPath:  "/v1beta/models/gemini-2.0-flash:streamGenerateContent",
			originalModel: "gemini-2.0-flash",
			actualModel:   "gemini-2.0-flash",
			wantPath:      "/v1beta/models/gemini-2.0-flash:streamGenerateContent",
		},
		{
			name:          "OpenAI 路径无模型名",
			originalPath:  "/v1/chat/completions",
			originalModel: "gpt-4",
			actualModel:   "gpt-4-turbo",
			wantPath:      "/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestPath := replaceModelInPath(tt.originalPath, tt.originalModel, tt.actualModel)

			if requestPath != tt.wantPath {
				t.Errorf("requestPath = %q, want %q", requestPath, tt.wantPath)
			}
		})
	}
}
