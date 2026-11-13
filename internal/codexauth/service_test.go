package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/**
 * @brief 验证授权 URL、授权码与刷新交换的官方请求契约
 * @param t 测试上下文
 */
func TestServiceAuthorizationAndTokenContracts(t *testing.T) {
	t.Parallel()

	type tokenRequest struct {
		contentType string
		originator  string
		userAgent   string
		grant       map[string]string
	}
	var grants []tokenRequest
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := tokenRequest{
			contentType: r.Header.Get("Content-Type"),
			originator:  r.Header.Get("Originator"),
			userAgent:   r.Header.Get("User-Agent"),
			grant:       map[string]string{},
		}
		if got.contentType == "application/json" {
			if err := json.NewDecoder(r.Body).Decode(&got.grant); err != nil {
				t.Errorf("decode JSON grant: %v", err)
			}
		} else {
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			for key := range r.PostForm {
				got.grant[key] = r.PostForm.Get(key)
			}
		}
		grants = append(grants, got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"rt-%d","id_token":%q,"expires_in":3600}`,
			len(grants), len(grants), testIDToken(t))
	}))
	defer tokenServer.Close()

	service := NewService(tokenServer.Client())
	service.AuthorizationURL = "https://auth.example.test/authorize"
	service.TokenURL = tokenServer.URL
	service.ClientID = "client-test"
	service.RedirectURI = "http://localhost:1455/auth/callback"

	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	state, err := GenerateState()
	if err != nil {
		t.Fatalf("GenerateState: %v", err)
	}
	link, err := service.AuthorizationLink(state, pkce)
	if err != nil {
		t.Fatalf("AuthorizationLink: %v", err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse link: %v", err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"client_id":             "client-test",
		"redirect_uri":          service.RedirectURI,
		"state":                 state,
		"code_challenge":        pkce.Challenge,
		"code_challenge_method": "S256",
		"scope":                 "openid email profile offline_access",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("authorization query %s = %q, want %q", key, got, want)
		}
	}

	credential, err := service.ExchangeCode(context.Background(), "code-test", pkce)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if credential.AccessToken != "at-1" || credential.RefreshToken != "rt-1" {
		t.Fatalf("exchange credential = %#v", credential)
	}
	if credential.Email != "user@example.com" || credential.ChatGPTUserID != "user-test" ||
		credential.AccountID != "acct-test" || credential.PlanType != "plus" {
		t.Fatalf("ID token metadata = %#v", credential)
	}

	refreshed, err := service.Refresh(context.Background(), credential.RefreshToken, &testRefreshJournal{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.AccessToken != "at-2" || refreshed.RefreshToken != "rt-2" {
		t.Fatalf("refresh credential = %#v", refreshed)
	}
	if len(grants) != 2 {
		t.Fatalf("token requests = %d, want 2", len(grants))
	}
	exchange := grants[0]
	if exchange.contentType != "application/x-www-form-urlencoded" ||
		exchange.grant["grant_type"] != "authorization_code" || exchange.grant["code_verifier"] != pkce.Verifier {
		t.Fatalf("exchange request = %#v", exchange)
	}
	// Native Codex refreshes with a JSON body without scope, carrying its client identity.
	refresh := grants[1]
	wantRefresh := map[string]string{"client_id": "client-test", "grant_type": "refresh_token", "refresh_token": "rt-1"}
	if refresh.contentType != "application/json" || !reflect.DeepEqual(refresh.grant, wantRefresh) {
		t.Fatalf("refresh request = %#v, want JSON grant %#v", refresh, wantRefresh)
	}
	for _, request := range grants {
		if request.originator != DefaultOriginator || request.userAgent != DefaultUserAgent {
			t.Fatalf("token request identity = %q / %q", request.originator, request.userAgent)
		}
	}
}

/**
 * @brief 验证缺少访问令牌的成功响应被归为不可重放的不确定结果
 * @param t 测试上下文
 */
func TestServiceRejectsEmptyAccessToken(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"refresh_token":"rt","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	service := NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL

	_, err := service.Refresh(context.Background(), "rt", &testRefreshJournal{})
	if !IsRefreshOutcomeUnknown(err) {
		t.Fatalf("Refresh error = %v, want unknown outcome", err)
	}
}

func TestServiceValidatesPersonalAccessTokenWhoAmIContract(t *testing.T) {
	t.Parallel()

	whoamiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer at-test-token" ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Originator") != DefaultOriginator ||
			r.Header.Get("User-Agent") != DefaultUserAgent {
			t.Fatalf("unexpected whoami request: method=%s headers=%v", r.Method, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"email":"user@example.com",
			"chatgpt_user_id":"user-123",
			"chatgpt_account_id":"acct-123",
			"chatgpt_plan_type":"plus",
			"chatgpt_account_is_fedramp":true
		}`))
	}))
	defer whoamiServer.Close()

	service := NewService(whoamiServer.Client())
	service.WhoAmIURL = whoamiServer.URL
	credential, err := service.ValidatePersonalAccessToken(context.Background(), " at-test-token ")
	if err != nil {
		t.Fatalf("ValidatePersonalAccessToken() error = %v", err)
	}
	if credential.AuthMode != AuthModePersonalAccessToken || credential.AccessToken != "at-test-token" ||
		credential.Email != "user@example.com" || credential.ChatGPTUserID != "user-123" ||
		credential.AccountID != "acct-123" || credential.PlanType != "plus" || !credential.AccountFedRAMP ||
		credential.RefreshToken != "" || credential.Expired != "" {
		t.Fatalf("PAT credential = %#v", credential)
	}
}

func TestServiceRejectsInvalidPersonalAccessTokenResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"invalid"}`},
		{name: "missing identity", status: http.StatusOK, body: `{"email":"user@example.com"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			service := NewService(server.Client())
			service.WhoAmIURL = server.URL
			if _, err := service.ValidatePersonalAccessToken(context.Background(), "at-test"); err == nil {
				t.Fatal("ValidatePersonalAccessToken() succeeded")
			}
		})
	}

	service := NewService(http.DefaultClient)
	if _, err := service.ValidatePersonalAccessToken(context.Background(), "eyJ.jwt"); err == nil ||
		!strings.Contains(err.Error(), "at-") {
		t.Fatalf("invalid prefix error = %v", err)
	}
}

func testIDToken(t *testing.T) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"user@example.com","https://api.openai.com/auth":{"chatgpt_user_id":"user-test","chatgpt_account_id":"acct-test","chatgpt_plan_type":"plus"}}`))
	return "header." + payload + ".signature"
}

/**
 * @brief 验证明确 grant 拒绝与客户端配置、临时响应的分类边界
 * @param t 测试上下文
 */
func TestServiceTerminalRefreshErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		terminal bool
	}{
		{"reused", 400, `{"error":{"type":"invalid_request_error","code":"refresh_token_reused"}}`, true},
		{"invalidated", 400, `{"error":"refresh_token_invalidated"}`, true},
		{"revoked", 403, `{"error":"token_revoked"}`, true},
		{"invalid grant", 400, `{"error":"invalid_grant"}`, true},
		{"unauthorized", 401, `unauthorized`, true},
		{"client configuration", 400, `{"error":"invalid_client"}`, false},
		{"unauthorized client configuration", 401, `{"error":"invalid_client"}`, false},
		{"server failure", 503, `{"error":"refresh_token_reused"}`, false},
		{"unknown", 400, `{"error":"invalid_request","error_description":"refresh_token_reused"}`, false},
		{"malformed", 400, `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			service := NewService(server.Client())
			service.TokenURL = server.URL
			_, err := service.Refresh(context.Background(), "secret-refresh-token", &testRefreshJournal{})
			if err == nil || IsTerminalRefreshError(err) != tc.terminal {
				t.Fatalf("refresh error=%v terminal=%v, want %v", err, IsTerminalRefreshError(err), tc.terminal)
			}
			if strings.Contains(err.Error(), tc.body) || strings.Contains(err.Error(), "secret-refresh-token") {
				t.Fatalf("refresh diagnostic exposes private data: %v", err)
			}
		})
	}
}

/**
 * @brief 提供可注入存储失败的刷新记录，用于认证服务边界测试
 */
type testRefreshJournal struct {
	mu          sync.Mutex        ///< 保护并发 claim 和回执
	entries     map[string][]byte ///< nil 回执表示未完成
	owners      map[string]string ///< grant 当前申领 owner
	claimErr    error             ///< claim 故障注入
	readErr     error             ///< 回执读取故障注入
	completeErr error             ///< 回执写入故障注入
	releaseErr  error             ///< claim 释放故障注入
	afterClaim  func()            ///< 成功申领后的取消或并发事件注入
	beforeRead  func()            ///< 读取前的并发释放事件注入
}

/**
 * @brief 原子取得测试 grant 的提交权
 * @param ctx 存储上下文
 * @param key grant 指纹
 * @param owner 本次申领独有标识
 * @return 首次 claim 时返回 true
 * @return 注入的存储错误
 */
func (j *testRefreshJournal) RefreshClaim(ctx context.Context, key, owner string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.claimErr != nil {
		return false, j.claimErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if j.entries == nil {
		j.entries = make(map[string][]byte)
		j.owners = make(map[string]string)
	}
	if _, exists := j.entries[key]; exists {
		return false, nil
	}
	j.entries[key] = nil
	j.owners[key] = owner
	if j.afterClaim != nil {
		j.afterClaim()
	}
	return true, nil
}

/**
 * @brief 释放属于当前 owner 的测试申领
 * @param ctx 存储上下文
 * @param key grant 指纹
 * @param owner 本次成功申领的独有标识
 * @return 故障注入、取消、owner 不匹配或已有回执时返回错误
 */
func (j *testRefreshJournal) RefreshRelease(ctx context.Context, key, owner string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.releaseErr != nil {
		return j.releaseErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.owners[key] != owner || j.entries[key] != nil {
		return errors.New("claim is not owned or already completed")
	}
	delete(j.entries, key)
	delete(j.owners, key)
	return nil
}

/**
 * @brief 读取测试中的持久化回执
 * @param ctx 存储上下文
 * @param key grant 指纹
 * @return 已保存的回执
 * @return grant 是否存在
 * @return 注入的存储错误
 */
func (j *testRefreshJournal) RefreshRead(ctx context.Context, key string) ([]byte, bool, error) {
	if j.beforeRead != nil {
		j.beforeRead()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.readErr != nil {
		return nil, false, j.readErr
	}
	value, exists := j.entries[key]
	return value, exists, ctx.Err()
}

/**
 * @brief 完成测试中的刷新回执
 * @param ctx 存储上下文
 * @param key grant 指纹
 * @param receipt 私密回执
 * @return 注入的错误或取消错误
 */
func (j *testRefreshJournal) RefreshComplete(ctx context.Context, key string, receipt []byte) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.completeErr != nil {
		return j.completeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	j.entries[key] = receipt
	return nil
}

/**
 * @brief 注入 OAuth HTTP 响应以验证认证服务契约
 */
type tokenRoundTripper func(*http.Request) (*http.Response, error)

/**
 * @brief 执行测试提供的 HTTP 响应函数
 * @param req 待发送的 OAuth 请求
 * @return 测试响应
 * @return 测试传输错误
 */
func (f tokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

/**
 * @brief 模拟已返回响应头但丢失响应正文
 */
type lostTokenBody struct{}

/**
 * @brief 模拟 token 响应正文读取失败
 * @param p 调用方提供的读取缓冲区
 * @return 始终返回零字节
 * @return 始终返回 io.ErrUnexpectedEOF
 */
func (lostTokenBody) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

/**
 * @brief 关闭测试正文
 * @return 始终返回 nil
 */
func (lostTokenBody) Close() error { return nil }

/**
 * @brief 验证构造、DNS、拨号和 TLS 失败释放提交权，恢复后同一 grant 可安全重试
 * @param t 测试上下文
 */
func TestServiceRefreshPreSendFailuresReleaseClaim(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"request_create", "dns", "dial", "tls"} {
		t.Run(stage, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"rotated","expires_in":3600}`)
			}))
			defer server.Close()
			journal := &testRefreshJournal{}
			service := NewService(server.Client())
			service.TokenURL = server.URL
			switch stage {
			case "request_create":
				service.TokenURL = ":invalid-url"
			case "dns", "dial":
				transport := server.Client().Transport.(*http.Transport).Clone()
				transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
					if stage == "dns" {
						return nil, &net.DNSError{Err: "private DNS error", Name: "private-host", IsNotFound: true}
					}
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
				}
				defer transport.CloseIdleConnections()
				service.Client = &http.Client{Transport: transport}
			case "tls":
				transport := http.DefaultTransport.(*http.Transport).Clone()
				transport.Proxy = nil
				defer transport.CloseIdleConnections()
				service.Client = &http.Client{Transport: transport}
			}
			ctx := context.Background()
			if _, err := service.Refresh(ctx, "presend-grant", journal); !IsRefreshPreSendError(err) || IsTerminalRefreshError(err) {
				t.Fatalf("pre-send failure = %v, want retryable error", err)
			} else if strings.Contains(err.Error(), "private") {
				t.Fatalf("private diagnostics leaked: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("pre-send failure reached endpoint %d times", calls.Load())
			}
			if _, exists, err := journal.RefreshRead(ctx, RefreshGrantKey(DefaultClientID, "presend-grant")); err != nil || exists {
				t.Fatalf("released claim exists=%v err=%v", exists, err)
			}
			if recovery, err := RefreshRecoveryRequired(ctx, journal, DefaultClientID, "presend-grant"); err != nil || recovery {
				t.Fatalf("pre-send failure requires reauthorization=%v err=%v", recovery, err)
			}
			recovered := NewService(server.Client())
			recovered.TokenURL = server.URL
			for range 2 {
				credential, err := recovered.Refresh(ctx, "presend-grant", journal)
				if err != nil || credential == nil || credential.RefreshToken != "rotated" || calls.Load() != 1 {
					t.Fatalf("retry credential=%v err=%v calls=%d", credential, err, calls.Load())
				}
			}
		})
	}
}

/**
 * @brief 验证申领后取消仍能安全释放且保留取消原因
 * @param t 测试上下文
 */
func TestServiceRefreshCancellationBeforeSendReleasesClaim(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	journal := &testRefreshJournal{afterClaim: cancel}
	client := &http.Client{Transport: tokenRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("cancelled grant reached transport")
		return nil, errors.New("unexpected send")
	})}
	_, err := NewService(client).Refresh(ctx, "cancelled-grant", journal)
	if !IsRefreshPreSendError(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	if _, exists, err := journal.RefreshRead(context.Background(), RefreshGrantKey(DefaultClientID, "cancelled-grant")); err != nil || exists {
		t.Fatalf("cancelled grant remained claimed: exists=%v err=%v", exists, err)
	}
}

/**
 * @brief 验证无法持久化释放时保留封锁状态，不将后续刷新视为安全重试
 * @param t 测试上下文
 */
func TestServiceRefreshReleaseFailurePreventsReplay(t *testing.T) {
	t.Parallel()
	journal := &testRefreshJournal{releaseErr: errors.New("storage unavailable")}
	calls := 0
	client := &http.Client{Transport: tokenRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, &net.DNSError{Err: "no such host", Name: "private-host", IsNotFound: true}
	})}
	for range 2 {
		_, err := NewService(client).Refresh(context.Background(), "release-failure", journal)
		if !IsRefreshOutcomeUnknown(err) || IsRefreshPreSendError(err) || IsTerminalRefreshError(err) || calls != 1 {
			t.Fatalf("release failure err=%v calls=%d", err, calls)
		}
	}
}

/**
 * @brief 验证并发 owner 在失败申领与读取之间释放后，等待者可重试且不误判为结果未知
 * @param t 测试上下文
 */
func TestServiceRefreshConcurrentReleaseRemainsRetryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := &testRefreshJournal{}
	key := RefreshGrantKey(DefaultClientID, "concurrent-release")
	if claimed, err := journal.RefreshClaim(ctx, key, "first-owner"); err != nil || !claimed {
		t.Fatalf("initial claim=%v err=%v", claimed, err)
	}
	journal.beforeRead = func() {
		journal.beforeRead = nil
		if err := journal.RefreshRelease(ctx, key, "first-owner"); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	client := &http.Client{Transport: tokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)), Request: req}, nil
	})}
	service := NewService(client)
	if _, err := service.Refresh(ctx, "concurrent-release", journal); !IsRefreshPreSendError(err) || calls != 0 {
		t.Fatalf("released claim error=%v calls=%d", err, calls)
	}
	credential, err := service.Refresh(ctx, "concurrent-release", journal)
	if err != nil || credential == nil || credential.RefreshToken != "rotated" || calls != 1 {
		t.Fatalf("retry credential=%v err=%v calls=%d", credential, err, calls)
	}
}

/**
 * @brief 验证网络、正文、响应与重定向的不确定结果均不能重放 grant
 * @param t 测试上下文
 */
func TestServiceRefreshUnknownOutcomesPreventReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"transport", 0, ""},
		{"connected dial failure", 0, ""},
		{"body read DNS failure", 0, ""},
		{"headers written DNS failure", 0, ""},
		{"read", 200, ""},
		{"decode", 200, "{"},
		{"access token", 200, `{"expires_in":3600}`},
		{"expiry", 200, `{"access_token":"new","expires_in":0}`},
		{"rate limited", 429, `{"error":"unavailable"}`},
		{"server failed", 503, `{"error":"unavailable"}`},
		{"redirect", 307, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := &testRefreshJournal{}
			calls := 0
			client := &http.Client{Transport: tokenRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if tc.status == 0 {
					trace := httptrace.ContextClientTrace(req.Context())
					switch tc.name {
					case "connected dial failure":
						trace.GetConn(req.URL.Host)
						trace.GotConn(httptrace.GotConnInfo{})
						return nil, &net.OpError{Op: "dial", Err: errors.New("private upstream diagnostics")}
					case "body read DNS failure":
						if _, err := io.Copy(io.Discard, req.Body); err != nil {
							t.Fatal(err)
						}
						return nil, &net.DNSError{Err: "private upstream diagnostics"}
					case "headers written DNS failure":
						trace.WroteHeaders()
						return nil, &net.DNSError{Err: "private upstream diagnostics"}
					}
					return nil, errors.New("private upstream diagnostics")
				}
				body := io.NopCloser(strings.NewReader(tc.body))
				if tc.name == "read" {
					body = lostTokenBody{}
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {DefaultTokenURL + "/redirected"}}, Body: body, Request: req}, nil
			})}
			for range 2 {
				_, err := NewService(client).Refresh(context.Background(), "unresolved-grant", journal)
				if !IsRefreshOutcomeUnknown(err) || IsTerminalRefreshError(err) || calls != 1 {
					t.Fatalf("refresh error=%v terminal=%v calls=%d", err, IsTerminalRefreshError(err), calls)
				}
				if strings.Contains(err.Error(), "private upstream diagnostics") || strings.Contains(err.Error(), "unresolved-grant") {
					t.Fatalf("private diagnostics leaked: %v", err)
				}
			}
		})
	}
}

/**
 * @brief 验证 claim 与回执写入失败均不能导致再次发送 grant
 * @param t 测试上下文
 */
func TestServiceRefreshStorageFailuresPreventReplay(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"claim", "complete"} {
		t.Run(stage, func(t *testing.T) {
			journal := &testRefreshJournal{}
			if stage == "claim" {
				journal.claimErr = errors.New("storage unavailable")
			} else {
				journal.completeErr = errors.New("storage unavailable")
			}
			calls := 0
			client := &http.Client{Transport: tokenRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)), Request: req}, nil
			})}
			if _, err := NewService(client).Refresh(context.Background(), "grant", journal); !IsRefreshOutcomeUnknown(err) {
				t.Fatalf("refresh error=%v", err)
			}
			wantCalls := 1
			if stage == "claim" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("network calls=%d, want %d", calls, wantCalls)
			}
			if stage == "complete" {
				journal.completeErr = nil
				if _, err := NewService(client).Refresh(context.Background(), "grant", journal); !IsRefreshOutcomeUnknown(err) || calls != 1 {
					t.Fatalf("lost receipt replayed grant: err=%v calls=%d", err, calls)
				}
			}
		})
	}
}

/**
 * @brief 验证回执读取失败不会授权重发，且管理状态查询会报告存储故障
 * @param t 测试上下文
 */
func TestServiceRefreshReadFailureDoesNotReplay(t *testing.T) {
	t.Parallel()
	journal := &testRefreshJournal{}
	ctx := context.Background()
	key := RefreshGrantKey(DefaultClientID, "claimed-grant")
	if _, err := journal.RefreshClaim(ctx, key, "test-owner"); err != nil {
		t.Fatal(err)
	}
	journal.readErr = errors.New("receipt storage unavailable")
	client := &http.Client{Transport: tokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		t.Error("receipt read failure submitted a claimed grant")
		return nil, errors.New("unexpected network request")
	})}
	if _, err := NewService(client).Refresh(ctx, "claimed-grant", journal); !IsRefreshOutcomeUnknown(err) {
		t.Fatalf("read failure error=%v", err)
	}
	if _, err := RefreshRecoveryRequired(ctx, journal, DefaultClientID, "claimed-grant"); err == nil {
		t.Fatal("recovery query hid storage failure")
	}
}

/**
 * @brief 验证响应返回后取消调用方不会丢弃已经轮换的成功回执
 * @param t 测试上下文
 */
func TestServiceRefreshSavesReceiptAfterCallerCancellation(t *testing.T) {
	t.Parallel()
	journal := &testRefreshJournal{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: tokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)), Request: req}, nil
	})}
	if _, err := NewService(client).Refresh(ctx, "original", journal); err != nil {
		t.Fatal(err)
	}
	credential, err := NewService(client).Refresh(context.Background(), "original", journal)
	if err != nil || credential == nil || credential.RefreshToken != "rotated" || calls != 1 {
		t.Fatalf("cancel discarded receipt: credential=%v err=%v calls=%d", credential, err, calls)
	}
}

/**
 * @brief 验证旧 grant 沿回执采用最新轮换，并阻止未完成的后续轮换
 * @param t 测试上下文
 */
func TestServiceRefreshReceiptFollowsRotation(t *testing.T) {
	t.Parallel()
	journal := &testRefreshJournal{}
	calls := 0
	client := &http.Client{Transport: tokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		var grant map[string]string
		if err := json.NewDecoder(req.Body).Decode(&grant); err != nil {
			t.Fatal(err)
		}
		next := "second"
		if grant["refresh_token"] == "second" {
			next = "third"
		}
		body := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":3600}`, "access-"+next, next)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	service := NewService(client)
	for _, grant := range []string{"first", "second", "first"} {
		credential, err := service.Refresh(context.Background(), grant, journal)
		if err != nil || credential == nil {
			t.Fatalf("refresh %s: credential=%v err=%v", grant, credential, err)
		}
		if calls == 2 && credential.RefreshToken != "third" {
			t.Fatalf("old grant returned stale rotation: %v", credential.RefreshToken)
		}
	}
	if calls != 2 {
		t.Fatalf("network calls=%d, want 2", calls)
	}
	if _, err := journal.RefreshClaim(context.Background(), RefreshGrantKey(DefaultClientID, "third"), "test-owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), "first", journal); !IsRefreshOutcomeUnknown(err) || calls != 2 {
		t.Fatalf("pending later rotation ignored: err=%v calls=%d", err, calls)
	}
}

/**
 * @brief 验证未轮换的 grant 只复用未过期回执，过期后必须恢复
 * @param t 测试上下文
 */
func TestServiceRefreshSameGrantReceiptExpires(t *testing.T) {
	t.Parallel()
	for _, rotated := range []bool{false, true} {
		t.Run(fmt.Sprintf("returned_token=%v", rotated), func(t *testing.T) {
			journal := &testRefreshJournal{}
			calls := 0
			client := &http.Client{Transport: tokenRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				body := `{"access_token":"new","expires_in":3600}`
				if rotated {
					body = `{"access_token":"new","refresh_token":"same","expires_in":3600}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			for range 2 {
				credential, err := NewService(client).Refresh(context.Background(), "same", journal)
				if err != nil || credential == nil || credential.RefreshToken != "same" || calls != 1 {
					t.Fatalf("same grant refresh=%v err=%v calls=%d", credential, err, calls)
				}
			}
			key := RefreshGrantKey(DefaultClientID, "same")
			var receipt refreshReceipt
			if err := json.Unmarshal(journal.entries[key], &receipt); err != nil {
				t.Fatal(err)
			}
			receipt.Credential.Expired = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			raw, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			journal.entries[key] = raw
			if _, err := NewService(client).Refresh(context.Background(), "same", journal); !IsRefreshOutcomeUnknown(err) || calls != 1 {
				t.Fatalf("expired receipt renewed grant: err=%v calls=%d", err, calls)
			}
		})
	}
}
