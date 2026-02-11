package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestAuthToken_MaskToken(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		expected string
	}{
		{
			name:     "Long token",
			token:    "sk-ant-1234567890abcdefghijklmnop",
			expected: "sk-a****mnop",
		},
		{
			name:     "Short token",
			token:    "short",
			expected: "****",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			masked := model.MaskToken(tt.token)
			if masked != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, masked)
			}
		})
	}
}

func TestAdminAPI_CreateAuthToken_Basic(t *testing.T) {
	server := newInMemoryServer(t)

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/auth-tokens", map[string]any{
		"description": "Test Token",
	}))

	server.HandleCreateAuthToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			ID    int64  `json:"id"`
			Token string `json:"token"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)

	if !response.Success || len(response.Data.Token) == 0 {
		t.Error("Token creation failed")
	}

	ctx := context.Background()
	stored, err := server.store.GetAuthToken(ctx, response.Data.ID)
	if err != nil {
		t.Fatalf("DB error: %v", err)
	}

	expectedHash := model.HashToken(response.Data.Token)
	if stored.Token != expectedHash {
		t.Error("Hash mismatch")
	}
}

func TestAdminAPI_ListAuthTokens_ResponseShape(t *testing.T) {
	server := newInMemoryServer(t)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/auth-tokens", nil))

	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	type listResp struct {
		Tokens  []*model.AuthToken `json:"tokens"`
		IsToday bool               `json:"is_today"`
	}
	resp := mustParseAPIResponse[listResp](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data.Tokens == nil {
		t.Fatalf("tokens is null, want []")
	}
}

// --- HandleListAuthTokens 补充测试 ---

// authTokenListResponse 用于反序列化 HandleListAuthTokens 响应
type authTokenListResponse struct {
	Tokens          []*model.AuthToken `json:"tokens"`
	DurationSeconds float64            `json:"duration_seconds"`
	RPMStats        *model.RPMStats    `json:"rpm_stats"`
	IsToday         bool               `json:"is_today"`
}

// createTestToken 通过 store 直接创建测试 token 并返回
func createTestToken(t testing.TB, srv *Server, desc string) *model.AuthToken {
	t.Helper()
	ctx := context.Background()
	token := &model.AuthToken{
		Token:       model.HashToken("test-token-" + desc),
		Description: desc,
		IsActive:    true,
	}
	if err := srv.store.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}
	return token
}

func TestHandleListAuthTokens_EmptyResult(t *testing.T) {
	server := newInMemoryServer(t)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/auth-tokens", nil))
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if len(resp.Data.Tokens) != 0 {
		t.Errorf("Expected 0 tokens, got %d", len(resp.Data.Tokens))
	}
	// 无 range 参数时 IsToday 应为 false
	if resp.Data.IsToday {
		t.Error("Expected IsToday=false when no range param")
	}
}

func TestHandleListAuthTokens_WithTokens(t *testing.T) {
	server := newInMemoryServer(t)
	createTestToken(t, server, "token-a")
	createTestToken(t, server, "token-b")

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/auth-tokens", nil))
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if len(resp.Data.Tokens) != 2 {
		t.Errorf("Expected 2 tokens, got %d", len(resp.Data.Tokens))
	}
}

func TestHandleListAuthTokens_RangeToday(t *testing.T) {
	server := newInMemoryServer(t)
	token := createTestToken(t, server, "range-token")

	// 创建一条日志记录，使统计聚合有数据
	ctx := context.Background()
	now := time.Now()
	logEntry := &model.LogEntry{
		Time:         model.JSONTime{Time: now},
		Model:        "test-model",
		ChannelID:    1,
		StatusCode:   200,
		Duration:     0.5,
		AuthTokenID:  token.ID,
		InputTokens:  100,
		OutputTokens: 50,
		Cost:         0.01,
	}
	if err := server.store.AddLog(ctx, logEntry); err != nil {
		t.Fatalf("AddLog failed: %v", err)
	}

	req := newRequest(http.MethodGet, "/admin/auth-tokens?range=today", nil)
	c, w := newTestContext(t, req)
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d, body=%s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if !resp.Data.IsToday {
		t.Error("Expected IsToday=true for range=today")
	}
	if resp.Data.DurationSeconds < 1 {
		t.Errorf("Expected DurationSeconds >= 1, got %f", resp.Data.DurationSeconds)
	}
}

func TestHandleListAuthTokens_RangeWeek(t *testing.T) {
	server := newInMemoryServer(t)
	createTestToken(t, server, "week-token")

	req := newRequest(http.MethodGet, "/admin/auth-tokens?range=this_week", nil)
	c, w := newTestContext(t, req)
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	// this_week 不是 today，所以 IsToday 应为 false
	if resp.Data.IsToday {
		t.Error("Expected IsToday=false for range=this_week")
	}
	if resp.Data.DurationSeconds < 1 {
		t.Errorf("Expected DurationSeconds >= 1, got %f", resp.Data.DurationSeconds)
	}
}

func TestHandleListAuthTokens_RangeMonth(t *testing.T) {
	server := newInMemoryServer(t)
	createTestToken(t, server, "month-token")

	req := newRequest(http.MethodGet, "/admin/auth-tokens?range=this_month", nil)
	c, w := newTestContext(t, req)
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data.IsToday {
		t.Error("Expected IsToday=false for range=this_month")
	}
}

func TestHandleListAuthTokens_RangeAll_SkipsStats(t *testing.T) {
	server := newInMemoryServer(t)
	createTestToken(t, server, "all-token")

	// range=all 应跳过统计聚合
	req := newRequest(http.MethodGet, "/admin/auth-tokens?range=all", nil)
	c, w := newTestContext(t, req)
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	// range=all 时不执行统计分支
	if resp.Data.DurationSeconds != 0 {
		t.Errorf("Expected DurationSeconds=0 for range=all, got %f", resp.Data.DurationSeconds)
	}
	if resp.Data.IsToday {
		t.Error("Expected IsToday=false for range=all")
	}
}

func TestHandleListAuthTokens_StatsAggregation(t *testing.T) {
	server := newInMemoryServer(t)
	tokenA := createTestToken(t, server, "stats-a")
	tokenB := createTestToken(t, server, "stats-b")

	ctx := context.Background()
	now := time.Now()

	// 创建渠道供日志引用
	cfg := &model.Config{
		Name:         "test-ch",
		URL:          "https://test.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model"}},
		Enabled:      true,
	}
	created, err := server.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	// tokenA: 2 条成功日志
	for i := 0; i < 2; i++ {
		entry := &model.LogEntry{
			Time:         model.JSONTime{Time: now.Add(-time.Duration(i) * time.Minute)},
			Model:        "test-model",
			ChannelID:    created.ID,
			StatusCode:   200,
			Duration:     0.3,
			AuthTokenID:  tokenA.ID,
			InputTokens:  100,
			OutputTokens: 50,
			Cost:         0.005,
		}
		if err := server.store.AddLog(ctx, entry); err != nil {
			t.Fatalf("AddLog failed: %v", err)
		}
	}

	// tokenB: 1 条成功 + 1 条失败
	entryOK := &model.LogEntry{
		Time:         model.JSONTime{Time: now.Add(-30 * time.Second)},
		Model:        "test-model",
		ChannelID:    created.ID,
		StatusCode:   200,
		Duration:     0.5,
		AuthTokenID:  tokenB.ID,
		InputTokens:  200,
		OutputTokens: 100,
		Cost:         0.01,
	}
	if err := server.store.AddLog(ctx, entryOK); err != nil {
		t.Fatalf("AddLog failed: %v", err)
	}
	entryFail := &model.LogEntry{
		Time:        model.JSONTime{Time: now.Add(-20 * time.Second)},
		Model:       "test-model",
		ChannelID:   created.ID,
		StatusCode:  500,
		Duration:    0.1,
		AuthTokenID: tokenB.ID,
	}
	if err := server.store.AddLog(ctx, entryFail); err != nil {
		t.Fatalf("AddLog failed: %v", err)
	}

	req := newRequest(http.MethodGet, "/admin/auth-tokens?range=today", nil)
	c, w := newTestContext(t, req)
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d, body=%s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}

	// 验证统计数据已叠加到 token 上
	tokenMap := make(map[int64]*model.AuthToken)
	for _, t := range resp.Data.Tokens {
		tokenMap[t.ID] = t
	}

	if ta, ok := tokenMap[tokenA.ID]; ok {
		if ta.SuccessCount != 2 {
			t.Errorf("tokenA SuccessCount: expected 2, got %d", ta.SuccessCount)
		}
	} else {
		t.Error("tokenA not found in response")
	}

	if tb, ok := tokenMap[tokenB.ID]; ok {
		if tb.SuccessCount != 1 {
			t.Errorf("tokenB SuccessCount: expected 1, got %d", tb.SuccessCount)
		}
		if tb.FailureCount != 1 {
			t.Errorf("tokenB FailureCount: expected 1, got %d", tb.FailureCount)
		}
	} else {
		t.Error("tokenB not found in response")
	}
}

func TestHandleListAuthTokens_StatsZeroForNoData(t *testing.T) {
	server := newInMemoryServer(t)
	token := createTestToken(t, server, "zero-stats")

	// 有 range 参数但该 token 无日志，统计应清零
	req := newRequest(http.MethodGet, "/admin/auth-tokens?range=today", nil)
	c, w := newTestContext(t, req)
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[authTokenListResponse](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}

	for _, tk := range resp.Data.Tokens {
		if tk.ID == token.ID {
			if tk.SuccessCount != 0 || tk.FailureCount != 0 {
				t.Errorf("Expected zero stats for token with no data, got success=%d failure=%d", tk.SuccessCount, tk.FailureCount)
			}
			if tk.PromptTokensTotal != 0 || tk.CompletionTokensTotal != 0 {
				t.Errorf("Expected zero token stats, got prompt=%d completion=%d", tk.PromptTokensTotal, tk.CompletionTokensTotal)
			}
			return
		}
	}
	t.Errorf("token ID=%d not found in response", token.ID)
}

func TestHandleListAuthTokens_RPMStats(t *testing.T) {
	server := newInMemoryServer(t)
	createTestToken(t, server, "rpm-token")

	// 创建渠道和多条日志来生成 RPM 统计
	ctx := context.Background()
	now := time.Now()
	cfg := &model.Config{
		Name:         "rpm-ch",
		URL:          "https://rpm.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "m"}},
		Enabled:      true,
	}
	created, err := server.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	for i := 0; i < 5; i++ {
		entry := &model.LogEntry{
			Time:        model.JSONTime{Time: now.Add(-time.Duration(i) * time.Second)},
			Model:       "m",
			ChannelID:   created.ID,
			StatusCode:  200,
			Duration:    0.1,
			AuthTokenID: 1,
		}
		if err := server.store.AddLog(ctx, entry); err != nil {
			t.Fatalf("AddLog failed: %v", err)
		}
	}

	req := newRequest(http.MethodGet, "/admin/auth-tokens?range=today", nil)
	c, w := newTestContext(t, req)
	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	// 解析原始 JSON 验证 rpm_stats 字段存在
	var raw map[string]json.RawMessage
	mustUnmarshalJSON(t, w.Body.Bytes(), &raw)
	var dataField map[string]json.RawMessage
	mustUnmarshalJSON(t, raw["data"], &dataField)

	// rpm_stats 可以是 null 或对象，但字段应存在
	if _, ok := dataField["rpm_stats"]; !ok {
		t.Error("Expected rpm_stats field in response")
	}
}
