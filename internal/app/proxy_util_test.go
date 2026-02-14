package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestWriteResponseWithHeaders_PreservesContentType(t *testing.T) {
	t.Parallel()

	w := newRecorder()
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

	w := newRecorder()
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
	w := newRecorder()

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

func TestSafeBodyToString(t *testing.T) {
	t.Parallel()

	if got := safeBodyToString(nil); got != "" {
		t.Fatalf("expected empty string for nil, got %q", got)
	}
	if got := safeBodyToString([]byte("hello\nworld")); got != "hello\nworld" {
		t.Fatalf("expected plain string passthrough, got %q", got)
	}

	bin := make([]byte, 200) // 全0：显然不是文本
	if got := safeBodyToString(bin); got != "[binary/compressed response]" {
		t.Fatalf("expected binary placeholder, got %q", got)
	}
}

func TestIsLikelyText(t *testing.T) {
	t.Parallel()

	if !isLikelyText([]byte("abc\tdef\n")) {
		t.Fatal("expected ascii text to be likely text")
	}

	// 高字节（UTF-8/非ASCII）不应被当作“不可打印字符”
	if !isLikelyText([]byte{0xe4, 0xbd, 0xa0, 0xe5, 0xa5, 0xbd}) { // "你好" 的 UTF-8
		t.Fatal("expected utf-8 bytes to be likely text")
	}

	notText := make([]byte, 100)
	for i := range notText {
		notText[i] = 0x00
	}
	if isLikelyText(notText) {
		t.Fatal("expected binary data to be not likely text")
	}
}

func TestFormatModelDisplayName(t *testing.T) {
	t.Parallel()

	if got := formatModelDisplayName("gemini-2.5-flash-20250101"); got != "Gemini 2.5 Flash" {
		t.Fatalf("unexpected display name: %q", got)
	}
}

func TestParseTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		query  map[string][]string
		header http.Header
		want   time.Duration
	}{
		{
			name:   "query_timeout_ms",
			query:  map[string][]string{"timeout_ms": {"500"}},
			header: nil,
			want:   500 * time.Millisecond,
		},
		{
			name:   "query_timeout_s",
			query:  map[string][]string{"timeout_s": {"10"}},
			header: nil,
			want:   10 * time.Second,
		},
		{
			name:   "query_timeout_ms_priority",
			query:  map[string][]string{"timeout_ms": {"1000"}, "timeout_s": {"5"}},
			header: nil,
			want:   1 * time.Second, // timeout_ms 优先
		},
		{
			name:  "header_timeout_ms",
			query: nil,
			header: http.Header{
				"X-Timeout-Ms": []string{"2000"},
			},
			want: 2 * time.Second,
		},
		{
			name:  "header_timeout_s",
			query: nil,
			header: http.Header{
				"X-Timeout-S": []string{"30"},
			},
			want: 30 * time.Second,
		},
		{
			name:   "query_priority_over_header",
			query:  map[string][]string{"timeout_ms": {"100"}},
			header: http.Header{"X-Timeout-Ms": []string{"9999"}},
			want:   100 * time.Millisecond, // query 优先
		},
		{
			name:   "invalid_value_returns_zero",
			query:  map[string][]string{"timeout_ms": {"invalid"}},
			header: nil,
			want:   0,
		},
		{
			name:   "negative_value_returns_zero",
			query:  map[string][]string{"timeout_ms": {"-100"}},
			header: nil,
			want:   0,
		},
		{
			name:   "empty_returns_zero",
			query:  nil,
			header: nil,
			want:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTimeout(tc.query, tc.header)
			if got != tc.want {
				t.Errorf("parseTimeout()=%v, want %v", got, tc.want)
			}
		})
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

func TestPrepareRequestBody_PreservesLargeIntegersOnModelRewrite(t *testing.T) {
	t.Parallel()

	s := &Server{
		modelFuzzyMatch: true,
	}
	cfg := &model.Config{
		ModelEntries: []model.ModelEntry{
			{Model: "gemini-3-flash-preview"},
		},
	}
	reqCtx := &proxyRequestContext{
		originalModel: "gemini-3-flash",
		body:          []byte(`{"model":"gemini-3-flash","id":9223372036854775807,"messages":[]}`),
	}

	actualModel, bodyToSend := s.prepareRequestBody(cfg, reqCtx)
	if actualModel != "gemini-3-flash-preview" {
		t.Fatalf("actualModel = %q, want %q", actualModel, "gemini-3-flash-preview")
	}
	if !bytes.Contains(bodyToSend, []byte(`"id":9223372036854775807`)) {
		t.Fatalf("expected large integer preserved, got %s", bodyToSend)
	}

	var reqData map[string]any
	if err := json.Unmarshal(bodyToSend, &reqData); err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}
	if gotModel, _ := reqData["model"].(string); gotModel != "gemini-3-flash-preview" {
		t.Fatalf("body model = %q, want %q", gotModel, "gemini-3-flash-preview")
	}
}

func TestStripAnthropicBillingHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantHasSystem bool
		wantSystem    any
		extraAssert   func(t *testing.T, result []byte)
	}{
		{
			name:          "无system字段_不修改",
			input:         `{"model":"claude-3","messages":[]}`,
			wantHasSystem: false,
		},
		{
			name:          "system为字符串_不修改",
			input:         `{"model":"claude-3","system":"you are helpful","messages":[]}`,
			wantHasSystem: true,
			wantSystem:    "you are helpful",
		},
		{
			name:          "过滤billing_header条目",
			input:         `{"model":"claude-3","system":[{"type":"text","text":"you are helpful"},{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.42.603; cc_entrypoint=cli; cch=00000;"}],"messages":[]}`,
			wantHasSystem: true,
			wantSystem: []any{
				map[string]any{"type": "text", "text": "you are helpful"},
			},
		},
		{
			name:          "全部为billing_header_移除system",
			input:         `{"model":"claude-3","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.42.603; cc_entrypoint=cli; cch=00000;"}],"messages":[]}`,
			wantHasSystem: false, // system 被完全移除
		},
		{
			name:          "无billing_header_不修改",
			input:         `{"model":"claude-3","system":[{"type":"text","text":"prompt1"},{"type":"text","text":"prompt2"}],"messages":[]}`,
			wantHasSystem: true,
			wantSystem: []any{
				map[string]any{"type": "text", "text": "prompt1"},
				map[string]any{"type": "text", "text": "prompt2"},
			},
		},
		{
			name:          "混合多条_只过滤billing",
			input:         `{"model":"claude-3","system":[{"type":"text","text":"system prompt"},{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.42.603; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"another prompt"}],"messages":[]}`,
			wantHasSystem: true,
			wantSystem: []any{
				map[string]any{"type": "text", "text": "system prompt"},
				map[string]any{"type": "text", "text": "another prompt"},
			},
		},
		{
			name:          "包含子串但非注入格式_不删除",
			input:         `{"model":"claude-3","system":[{"type":"text","text":"请解释 x-anthropic-billing-header 的含义"}],"messages":[]}`,
			wantHasSystem: true,
			wantSystem: []any{
				map[string]any{"type": "text", "text": "请解释 x-anthropic-billing-header 的含义"},
			},
		},
		{
			name:          "billing前缀但无键值对_不删除",
			input:         `{"model":"claude-3","system":[{"type":"text","text":"x-anthropic-billing-header: this is plain text"}],"messages":[]}`,
			wantHasSystem: true,
			wantSystem: []any{
				map[string]any{"type": "text", "text": "x-anthropic-billing-header: this is plain text"},
			},
		},
		{
			name:          "过滤时保持大整数精度",
			input:         `{"model":"claude-3","id":9223372036854775807,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.42.603; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"keep me"}],"messages":[]}`,
			wantHasSystem: true,
			wantSystem: []any{
				map[string]any{"type": "text", "text": "keep me"},
			},
			extraAssert: func(t *testing.T, result []byte) {
				t.Helper()
				if !bytes.Contains(result, []byte(`"id":9223372036854775807`)) {
					t.Fatalf("expected large integer preserved, got %s", result)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := stripAnthropicBillingHeaders([]byte(tt.input))
			if tt.extraAssert != nil {
				tt.extraAssert(t, result)
			}

			var got map[string]any
			if err := json.Unmarshal(result, &got); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}

			systemVal, hasSystem := got["system"]
			if hasSystem != tt.wantHasSystem {
				t.Fatalf("has system = %v, want %v (value=%v)", hasSystem, tt.wantHasSystem, systemVal)
			}
			if !tt.wantHasSystem {
				return
			}

			// 验证 system 内容
			wantJSON, _ := json.Marshal(tt.wantSystem)
			gotJSON, _ := json.Marshal(systemVal)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("system = %s, want %s", gotJSON, wantJSON)
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
