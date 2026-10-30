package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"
	"ccLoad/internal/vertexauth"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	vertexTestProject   = "vertex-proj"
	vertexTestEmail     = "svc@vertex-proj.iam.gserviceaccount.com"
	vertexTestModel     = "gemini-2.5-pro"
	vertexTestRegionURL = "/v1/projects/" + vertexTestProject + "/locations/us-central1/publishers/google"
	vertexGeminiReply   = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
)

var vertexTestKeys = sync.OnceValue(func() [2]string {
	var keys [2]string
	for i := range keys {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			panic(err)
		}
		keys[i] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	return keys
})

// vertexTestServiceAccount returns a Google service account key file whose
// token_uri points at tokenURI. keyIndex selects one of two RSA keys.
func vertexTestServiceAccount(t testing.TB, tokenURI string, keyIndex int) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "service_account", "project_id": vertexTestProject, "private_key_id": fmt.Sprintf("kid-%d", keyIndex),
		"private_key": vertexTestKeys()[keyIndex], "client_email": vertexTestEmail, "token_uri": tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func vertexTestCredential(t testing.TB, tokenURI string) string {
	t.Helper()
	credential, err := vertexauth.ParseCredential(vertexTestServiceAccount(t, tokenURI, 0))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

type vertexTestUpstream struct {
	server     *testHTTPServer
	tokenCalls atomic.Int32
	mu         sync.Mutex
	requests   []*http.Request
	bodies     [][]byte
	// respond answers a publisher request; nil replies with vertexGeminiReply.
	respond func(w http.ResponseWriter, r *http.Request)
}

// newVertexTestUpstream serves the OAuth token endpoint at /token, minting
// tok-1, tok-2, ..., and records every other request as a publisher call.
func newVertexTestUpstream(t testing.TB) *vertexTestUpstream {
	t.Helper()
	upstream := &vertexTestUpstream{}
	upstream.server = newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			call := upstream.tokenCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":3600,"token_type":"Bearer"}`, call)
			return
		}
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.requests = append(upstream.requests, r.Clone(context.Background()))
		upstream.bodies = append(upstream.bodies, body)
		upstream.mu.Unlock()
		// Vertex, unlike the Gemini API, rejects a content without a valid role.
		for _, content := range gjson.GetBytes(body, "contents").Array() {
			if role := content.Get("role").String(); role != "user" && role != "model" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Please use a valid role: user, model.","status":"INVALID_ARGUMENT"}}`)
				return
			}
		}
		if upstream.respond != nil {
			upstream.respond(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+vertexGeminiReply+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, vertexGeminiReply)
	}))
	return upstream
}

func (u *vertexTestUpstream) tokenURI() string { return u.server.URL + "/token" }

func (u *vertexTestUpstream) calls() ([]*http.Request, [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request(nil), u.requests...), append([][]byte(nil), u.bodies...)
}

func setupVertexProxyTestEnv(t testing.TB, upstream *vertexTestUpstream) *proxyTestEnv {
	t.Helper()
	return setupProxyTestEnv(t, []testChannel{{
		name: "vertex", upstreamProtocol: util.ProtocolGemini, models: vertexTestModel,
		authType: model.AuthTypeVertexOAuth, oauthCredential: vertexTestCredential(t, upstream.tokenURI()),
	}}, map[int]string{0: upstream.server.URL + vertexTestRegionURL})
}

func TestProxy_VertexOAuthMapsGeminiPassthroughToPublisherEndpoint(t *testing.T) {
	t.Parallel()
	upstream := newVertexTestUpstream(t)
	env := setupVertexProxyTestEnv(t, upstream)
	// Role-less turns are valid Gemini API input; Vertex needs them filled.
	geminiBody := map[string]any{"contents": []any{
		map[string]any{"parts": []any{map[string]any{"text": "hi"}}},
		map[string]any{"parts": []any{map[string]any{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{}}}}},
		map[string]any{"parts": []any{map[string]any{"text": "done"}}},
	}}

	for _, path := range []string{
		"/v1beta/models/" + vertexTestModel + ":generateContent",
		"/v1beta/models/" + vertexTestModel + ":streamGenerateContent?alt=sse&key=test-api-key",
		"/v1beta/models/" + vertexTestModel + ":streamGenerateContent",
	} {
		response := doProxyRequest(t, env.engine, path, geminiBody, map[string]string{"x-goog-api-key": "test-api-key"})
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}

	requests, bodies := upstream.calls()
	want := []struct{ path, query string }{
		{vertexTestRegionURL + "/models/" + vertexTestModel + ":generateContent", ""},
		{vertexTestRegionURL + "/models/" + vertexTestModel + ":streamGenerateContent", "alt=sse"},
		// A passthrough keeps the caller's query; ccLoad does not switch it to SSE.
		{vertexTestRegionURL + "/models/" + vertexTestModel + ":streamGenerateContent", ""},
	}
	if len(requests) != len(want) {
		t.Fatalf("publisher calls=%d, want %d", len(requests), len(want))
	}
	for i, request := range requests {
		if request.URL.Path != want[i].path || request.URL.RawQuery != want[i].query {
			t.Fatalf("call %d URL=%s?%s, want %s?%s", i, request.URL.Path, request.URL.RawQuery, want[i].path, want[i].query)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Fatalf("call %d Authorization=%q", i, got)
		}
		if got := request.Header.Get("x-goog-api-key"); got != "" {
			t.Fatalf("call %d leaked x-goog-api-key=%q", i, got)
		}
		// A functionResponse turn is user; other role-less turns alternate.
		if got := gjson.GetBytes(bodies[i], "contents.#.role").String(); got != `["user","user","model"]` {
			t.Fatalf("call %d roles=%s", i, got)
		}
	}
	if calls := upstream.tokenCalls.Load(); calls != 1 {
		t.Fatalf("token mints=%d, want one cached token", calls)
	}
}

func TestProxy_VertexPreservesBareTinkThoughtSignatures(t *testing.T) {
	t.Parallel()
	// Vertex emits bare Tink output with this observed key ID, without the
	// protobuf envelope used by other Gemini endpoints.
	decoded := append([]byte{1, 0x8f, 0x3d, 0x6b, 0x5f}, bytes.Repeat([]byte{0x42}, 400)...)
	signature := base64.StdEncoding.EncodeToString(decoded)
	for _, authType := range []string{model.AuthTypeVertexOAuth, model.AuthTypeAPIKey} {
		for _, exact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exact=%v", authType, exact), func(t *testing.T) {
				t.Parallel()
				upstream := newVertexTestUpstream(t)
				baseURL := upstream.server.URL + vertexTestRegionURL
				if exact {
					baseURL += "/models/" + vertexTestModel + ":generateContent" + model.ExactUpstreamURLMarker
				}
				channel := testChannel{name: "vertex", upstreamProtocol: util.ProtocolGemini, models: vertexTestModel, authType: authType}
				if authType == model.AuthTypeVertexOAuth {
					channel.oauthCredential = vertexTestCredential(t, upstream.tokenURI())
				}
				env := setupProxyTestEnv(t, []testChannel{channel}, map[int]string{0: baseURL})
				response := doProxyRequest(t, env.engine, "/v1beta/models/"+vertexTestModel+":generateContent", map[string]any{
					"contents": []any{
						map[string]any{"role": "model", "parts": []any{
							map[string]any{"text": "thinking", "thought": true, "thoughtSignature": signature},
							map[string]any{"functionCall": map[string]any{"name": "lookup", "args": map[string]any{}}, "thoughtSignature": signature},
						}},
						map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"ok": true}}}}},
					},
				}, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				_, bodies := upstream.calls()
				if len(bodies) != 1 {
					t.Fatalf("publisher calls=%d, want 1", len(bodies))
				}
				parts := gjson.GetBytes(bodies[0], "contents.0.parts").Array()
				if len(parts) != 2 {
					t.Fatalf("Vertex signed history changed: %s", bodies[0])
				}
				for _, part := range parts {
					if got := part.Get("thoughtSignature").String(); got != signature {
						t.Fatalf("Vertex signature changed: %q", got)
					}
				}
			})
		}
	}
}

func TestProxy_VertexAPIKeyCompleteURLRequiresExactMarker(t *testing.T) {
	t.Parallel()
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprintf("exact=%v", exact), func(t *testing.T) {
			t.Parallel()
			upstream := newVertexTestUpstream(t)
			endpoint := vertexTestRegionURL + "/models/" + vertexTestModel + ":generateContent"
			requestPath := "/v1beta/models/" + vertexTestModel + ":generateContent"
			baseURL := upstream.server.URL + endpoint
			wantPath := endpoint + requestPath
			if exact {
				baseURL += model.ExactUpstreamURLMarker
				wantPath = endpoint
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "vertex", upstreamProtocol: util.ProtocolGemini, models: vertexTestModel,
			}}, map[int]string{0: baseURL})
			response := doProxyRequest(t, env.engine, requestPath, map[string]any{
				"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			requests, _ := upstream.calls()
			if len(requests) != 1 || requests[0].URL.Path != wantPath {
				t.Fatalf("complete URL exact=%v: calls=%v, want path %s", exact, requests, wantPath)
			}
		})
	}
}

func TestProxy_VertexOAuthExactURLNormalizesRoles(t *testing.T) {
	t.Parallel()
	upstream := newVertexTestUpstream(t)
	endpoint := "/custom:generateContent?route=exact"
	env := setupProxyTestEnv(t, []testChannel{{
		name: "vertex", upstreamProtocol: util.ProtocolGemini, models: vertexTestModel,
		authType: model.AuthTypeVertexOAuth, oauthCredential: vertexTestCredential(t, upstream.tokenURI()),
	}}, map[int]string{0: upstream.server.URL + endpoint + model.ExactUpstreamURLMarker})
	response := doProxyRequest(t, env.engine, "/v1beta/models/"+vertexTestModel+":generateContent", map[string]any{
		"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "hi"}}}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requests, bodies := upstream.calls()
	if len(requests) != 1 {
		t.Fatalf("publisher calls=%d", len(requests))
	}
	if requests[0].URL.RequestURI() != endpoint {
		t.Fatalf("exact URL changed: %s", requests[0].URL)
	}
	if got := gjson.GetBytes(bodies[0], "contents.0.role").String(); got != "user" {
		t.Fatalf("role=%q, want user", got)
	}
}

func TestProxy_VertexOAuthTranslatedResponsesStreamUsesSSEAndDropsCallIDs(t *testing.T) {
	t.Parallel()
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprintf("exact=%v", exact), func(t *testing.T) {
			t.Parallel()
			upstream := newVertexTestUpstream(t)
			baseURL := upstream.server.URL + vertexTestRegionURL
			wantPath := vertexTestRegionURL + "/models/" + vertexTestModel + ":streamGenerateContent"
			wantQuery := "alt=sse"
			if exact {
				wantPath = "/custom:streamGenerateContent"
				wantQuery = "alt=sse&route=exact"
				baseURL = upstream.server.URL + wantPath + "?" + wantQuery + model.ExactUpstreamURLMarker
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "vertex", upstreamProtocol: util.ProtocolGemini, models: vertexTestModel,
				authType: model.AuthTypeVertexOAuth, oauthCredential: vertexTestCredential(t, upstream.tokenURI()),
			}}, map[int]string{0: baseURL})

			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": vertexTestModel, "stream": true,
				"input": []any{
					map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "weather?"}}},
					map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`},
					map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
				},
				"tools": []any{map[string]any{
					"type": "function", "name": "lookup",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
				}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}

			requests, bodies := upstream.calls()
			if len(requests) != 1 {
				t.Fatalf("publisher calls=%d", len(requests))
			}
			if requests[0].URL.Path != wantPath || requests[0].URL.RawQuery != wantQuery {
				t.Fatalf("URL=%s, want %s?%s", requests[0].URL, wantPath, wantQuery)
			}
			var functionCalls, functionResponses int
			gjson.GetBytes(bodies[0], "contents").ForEach(func(_, content gjson.Result) bool {
				content.Get("parts").ForEach(func(_, part gjson.Result) bool {
					if part.Get("functionCall").Exists() {
						functionCalls++
					}
					if part.Get("functionResponse").Exists() {
						functionResponses++
					}
					if part.Get("functionCall.id").Exists() || part.Get("functionResponse.id").Exists() {
						t.Errorf("Vertex body kept a Responses call id: %s", part.Raw)
					}
					return true
				})
				return true
			})
			if functionCalls != 1 || functionResponses != 1 {
				t.Fatalf("functionCall=%d functionResponse=%d body=%s", functionCalls, functionResponses, bodies[0])
			}
		})
	}
}

func TestProxy_VertexOAuthRemintsTokenAfterUnauthorized(t *testing.T) {
	t.Parallel()
	upstream := newVertexTestUpstream(t)
	upstream.respond = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, vertexGeminiReply)
	}
	env := setupVertexProxyTestEnv(t, upstream)

	response := doProxyRequest(t, env.engine, "/v1beta/models/"+vertexTestModel+":generateContent", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requests, _ := upstream.calls()
	if len(requests) != 2 || requests[1].Header.Get("Authorization") != "Bearer tok-2" {
		t.Fatalf("publisher calls=%d, want a retry with the re-minted token", len(requests))
	}
	if calls := upstream.tokenCalls.Load(); calls != 2 {
		t.Fatalf("token mints=%d, want 2", calls)
	}
}

func TestVertexCredentialsConcurrentRefresh(t *testing.T) {
	t.Parallel()
	upstream := newVertexTestUpstream(t)
	manager := newVertexCredentialManager(func(*model.Config) *http.Client { return upstream.server.Client() })
	now := time.Now()
	manager.now = func() time.Time { return now }
	cfg := &model.Config{ID: 1, AuthType: model.AuthTypeVertexOAuth, OAuthCredential: vertexTestCredential(t, upstream.tokenURI())}

	checkBatch := func(rejected, wantToken string, wantMints int32) {
		t.Helper()
		const concurrency = 32
		start := make(chan struct{})
		errors := make(chan error, concurrency)
		var workers sync.WaitGroup
		for range concurrency {
			workers.Go(func() {
				<-start
				token, err := manager.accessToken(context.Background(), cfg, rejected)
				if err != nil {
					errors <- err
				} else if token != wantToken {
					errors <- fmt.Errorf("token=%q, want %q", token, wantToken)
				}
			})
		}
		close(start)
		workers.Wait()
		close(errors)
		for err := range errors {
			t.Error(err)
		}
		if calls := upstream.tokenCalls.Load(); calls != wantMints {
			t.Fatalf("token mints=%d, want %d", calls, wantMints)
		}
	}
	checkBatch("", "tok-1", 1)
	checkBatch("tok-1", "tok-2", 2)
	checkBatch("tok-1", "tok-2", 2)
	// A replacement for a rejected token remains usable inside the refresh lead;
	// ordinary requests still refresh five minutes before expiry.
	now = now.Add(56 * time.Minute)
	checkBatch("tok-1", "tok-2", 2)
	checkBatch("", "tok-3", 3)

	rotated, err := vertexauth.ParseCredential(vertexTestServiceAccount(t, upstream.tokenURI(), 1))
	if err != nil {
		t.Fatal(err)
	}
	cfg.OAuthCredential, err = rotated.JSON()
	if err != nil {
		t.Fatal(err)
	}
	checkBatch("", "tok-4", 4)
}

func TestVertexCredentialsRefreshFailureFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		elapsed   time.Duration
		rejected  string
		wantToken string
	}{
		{name: "near_expiry", elapsed: 56 * time.Minute, wantToken: "tok-1"},
		{name: "expired", elapsed: 61 * time.Minute},
		{name: "rejected", rejected: "tok-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) > 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"tok-1","expires_in":3600,"token_type":"Bearer"}`)
			}))
			manager := newVertexCredentialManager(func(*model.Config) *http.Client { return upstream.Client() })
			now := time.Now()
			manager.now = func() time.Time { return now }
			cfg := &model.Config{ID: 1, AuthType: model.AuthTypeVertexOAuth, OAuthCredential: vertexTestCredential(t, upstream.URL+"/token")}
			if token, err := manager.accessToken(context.Background(), cfg, ""); err != nil || token != "tok-1" {
				t.Fatalf("initial token=%q err=%v", token, err)
			}
			now = now.Add(tc.elapsed)
			token, err := manager.accessToken(context.Background(), cfg, tc.rejected)
			if err == nil || token != tc.wantToken {
				t.Fatalf("refresh token=%q err=%v, want %q with error", token, err, tc.wantToken)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("token mints=%d, want 2", got)
			}
		})
	}
}

func TestVertexCredentialsConcurrentMixedRejections(t *testing.T) {
	t.Parallel()
	for _, leaderRejected := range []string{"", "tok-1"} {
		t.Run("leader_rejected_"+leaderRejected, func(t *testing.T) {
			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseFirst) })
			defer release()
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				call := calls.Add(1)
				if call == 1 {
					close(firstStarted)
					<-releaseFirst
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":3600,"token_type":"Bearer"}`, call+1)
			}))
			manager := newVertexCredentialManager(func(*model.Config) *http.Client { return upstream.Client() })
			cfg := &model.Config{ID: 1, AuthType: model.AuthTypeVertexOAuth, OAuthCredential: vertexTestCredential(t, upstream.URL+"/token")}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type tokenResult struct {
				token string
				err   error
			}
			leader := make(chan tokenResult, 1)
			leaderDone := make(chan struct{})
			go func() {
				defer close(leaderDone)
				token, err := manager.accessToken(ctx, cfg, leaderRejected)
				leader <- tokenResult{token, err}
			}()
			defer func() {
				release()
				<-leaderDone
			}()
			select {
			case <-firstStarted:
			case <-ctx.Done():
				t.Fatal("first mint did not start")
			}
			// This caller already knows tok-2 is rejected, even though the cache
			// is empty. It must not share the first caller's pending tok-2 mint.
			token, err := manager.accessToken(ctx, cfg, "tok-2")
			if err != nil || token != "tok-3" {
				t.Fatalf("mixed-rejection token=%q err=%v, want tok-3", token, err)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("token mints=%d, want 2", got)
			}
			release()
			select {
			case result := <-leader:
				if result.err != nil || result.token != "tok-2" {
					t.Errorf("leader token=%q err=%v, want tok-2", result.token, result.err)
				}
			case <-ctx.Done():
				t.Fatal("first mint did not complete")
			}
		})
	}
}

func TestVertexCredentialsRemintAcceptsRepeatedToken(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"same-token","expires_in":3600,"token_type":"Bearer"}`)
	}))
	manager := newVertexCredentialManager(func(*model.Config) *http.Client { return upstream.Client() })
	cfg := &model.Config{ID: 1, AuthType: model.AuthTypeVertexOAuth, OAuthCredential: vertexTestCredential(t, upstream.URL+"/token")}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, rejected := range []string{"", "same-token"} {
		if token, err := manager.accessToken(ctx, cfg, rejected); err != nil || token != "same-token" {
			t.Fatalf("rejected=%q: token=%q err=%v", rejected, token, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("token mints=%d, want 2", got)
	}
}

func TestProxy_VertexPublisherURLWithAPIKeyKeepsGoogleAPIKey(t *testing.T) {
	t.Parallel()
	upstream := newVertexTestUpstream(t)
	env := setupProxyTestEnv(t, []testChannel{{
		name: "vertex-express", upstreamProtocol: util.ProtocolGemini, models: vertexTestModel, apiKey: "vertex-express-key",
	}}, map[int]string{0: upstream.server.URL + "/v1/publishers/google"})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": vertexTestModel, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requests, _ := upstream.calls()
	if len(requests) != 1 {
		t.Fatalf("publisher calls=%d", len(requests))
	}
	if want := "/v1/publishers/google/models/" + vertexTestModel + ":generateContent"; requests[0].URL.Path != want {
		t.Fatalf("path=%s, want %s", requests[0].URL.Path, want)
	}
	if got := requests[0].Header.Get("x-goog-api-key"); got != "vertex-express-key" {
		t.Fatalf("x-goog-api-key=%q", got)
	}
	if got := requests[0].Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization=%q, want none for API key mode", got)
	}
	if calls := upstream.tokenCalls.Load(); calls != 0 {
		t.Fatalf("API key mode minted %d tokens", calls)
	}
}

// Vertex publisher bases carry /v1 in their path, which ordinary channels
// reject. Saving an imported Vertex channel from the editor or CSV must stay possible.
func TestValidateChannelBaseURLAcceptsVertexPublisherBase(t *testing.T) {
	t.Parallel()
	regional := "https://us-central1-aiplatform.googleapis.com" + vertexTestRegionURL
	for _, tc := range []struct {
		url, authType string
		wantErr       bool
	}{
		{regional, model.AuthTypeVertexOAuth, false},
		{"https://aiplatform.googleapis.com/v1/publishers/google", model.AuthTypeAPIKey, false},
		{"https://us-central1-aiplatform.googleapis.com", model.AuthTypeVertexOAuth, true},
		{"https://us-central1-aiplatform.googleapis.com/v1/projects/p", model.AuthTypeVertexOAuth, true},
		{"https://aiplatform.googleapis.com/v1", model.AuthTypeAPIKey, true},
	} {
		normalized, err := validateChannelBaseURL(tc.url+"/", tc.authType)
		if tc.wantErr {
			if err == nil {
				t.Errorf("validateChannelBaseURL(%q, %s) accepted", tc.url, tc.authType)
			}
			continue
		}
		if err != nil || normalized != tc.url {
			t.Errorf("validateChannelBaseURL(%q, %s) = %q, %v", tc.url, tc.authType, normalized, err)
		}
	}
}

func TestAdminTestVertexOAuthUsesPublisherEndpointAndMintedToken(t *testing.T) {
	t.Parallel()
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprintf("exact=%v", exact), func(t *testing.T) {
			t.Parallel()
			upstream := newVertexTestUpstream(t)
			srv := newInMemoryServer(t)
			baseURL := upstream.server.URL + vertexTestRegionURL
			if exact {
				baseURL = upstream.server.URL + "/custom:streamGenerateContent?alt=sse&route=exact"
			}
			cfg, err := srv.store.CreateConfig(context.Background(), &model.Config{
				Name: "vertex-admin-test", AuthType: model.AuthTypeVertexOAuth,
				OAuthCredential:       vertexTestCredential(t, upstream.tokenURI()),
				URLs:                  model.ChannelURLs{{URL: baseURL, Exact: exact, Protocols: []string{util.ProtocolGemini}}},
				ProtocolTransformMode: model.ProtocolTransformModeLocal,
				ModelEntries:          []model.ModelEntry{{Model: vertexTestModel}}, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}

			runtimeCfg, selection, handled, err := srv.prepareOAuthChannelTestAuth(context.Background(), cfg, oauthCredentialRefreshIfNeeded)
			if !handled || err != nil {
				t.Fatalf("prepare Vertex test auth handled=%v err=%v", handled, err)
			}
			for _, stream := range []bool{false, true} {
				result := srv.executeChannelTestWithCooldown(context.Background(), runtimeCfg, selection.keyIndex, selection.requestCredential, &testutil.TestChannelRequest{
					Model: vertexTestModel, ClientProtocol: util.ProtocolGemini, Content: "hi", Stream: stream,
				}, true)
				if success, _ := result["success"].(bool); !success {
					t.Fatalf("stream=%v result=%+v", stream, result)
				}
			}
			requests, _ := upstream.calls()
			if len(requests) != 2 {
				t.Fatalf("publisher calls=%d", len(requests))
			}
			base := vertexTestRegionURL + "/models/" + vertexTestModel
			if !exact && (requests[0].URL.Path != base+":generateContent" ||
				requests[1].URL.Path != base+":streamGenerateContent" || requests[1].URL.Query().Get("alt") != "sse") {
				t.Fatalf("URLs=%s, %s", requests[0].URL, requests[1].URL)
			}
			for _, request := range requests {
				if exact && (request.URL.Path != "/custom:streamGenerateContent" || request.URL.RawQuery != "alt=sse&route=exact") {
					t.Fatalf("exact URL changed: %s", request.URL)
				}
				if request.Header.Get("Authorization") != "Bearer tok-1" || request.Header.Get("x-goog-api-key") != "" {
					t.Fatalf("headers=%v", request.Header)
				}
			}
		})
	}
}

func TestFetchModelsForVertexOAuthListsPublisherGeminiModels(t *testing.T) {
	t.Parallel()
	pages := map[string]string{
		"": `{"publisherModels":[{"name":"publishers/google/models/gemini-2.5-pro"},{"name":"publishers/google/models/gemma-3-27b-it"},` +
			`{"name":"publishers/google/models/gemini-"},{"name":"publishers/meta/models/gemini-x"}],"nextPageToken":"p2"}`,
		"p2": `{"publisherModels":[{"name":"publishers/google/models/gemini-2.5-flash"},{"name":"publishers/google/models/gemini-2.5-pro"}]}`,
	}
	for _, tc := range []struct {
		name       string
		listStatus int
		wantSource string
		wantModels []string
	}{
		{name: "live", listStatus: http.StatusOK, wantSource: "api", wantModels: []string{"gemini-2.5-flash", "gemini-2.5-pro"}},
		{name: "catalog fallback", listStatus: http.StatusForbidden, wantSource: "predefined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstream := newVertexTestUpstream(t)
			upstream.respond = func(w http.ResponseWriter, r *http.Request) {
				if tc.listStatus != http.StatusOK {
					w.WriteHeader(tc.listStatus)
					return
				}
				_, _ = io.WriteString(w, pages[r.URL.Query().Get("pageToken")])
			}
			srv := newInMemoryServer(t)
			cfg, err := srv.store.CreateConfig(context.Background(), &model.Config{
				Name: "vertex-models-" + tc.name, AuthType: model.AuthTypeVertexOAuth,
				OAuthCredential:       vertexTestCredential(t, upstream.tokenURI()),
				URLs:                  model.ChannelURLs{{URL: upstream.server.URL + vertexTestRegionURL, Protocols: []string{util.ProtocolGemini}}},
				ProtocolTransformMode: model.ProtocolTransformModeLocal,
				ModelEntries:          []model.ModelEntry{{Model: vertexTestModel}}, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}

			response, err := srv.fetchModelsForChannel(context.Background(), cfg, "", modelFetchFirstAvailableKey)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(response.Models))
			for i, entry := range response.Models {
				got[i] = entry.Model
				if strings.HasPrefix(entry.Model, "imagen-") {
					t.Fatalf("Vertex lists Imagen model %q", entry.Model)
				}
			}
			if response.Source != tc.wantSource || response.Protocol != util.ProtocolGemini || len(got) == 0 ||
				(tc.wantModels != nil && strings.Join(got, ",") != strings.Join(tc.wantModels, ",")) {
				t.Fatalf("source=%s protocol=%s models=%v", response.Source, response.Protocol, got)
			}
			requests, _ := upstream.calls()
			if len(requests) == 0 {
				t.Fatal("model list was not requested")
			}
			for _, request := range requests {
				if request.URL.Path != "/v1beta1/publishers/google/models" || request.URL.Query().Get("pageSize") != "300" ||
					request.Header.Get("Authorization") != "Bearer tok-1" {
					t.Fatalf("list request=%s headers=%v", request.URL, request.Header)
				}
			}
		})
	}
}

func importVertexCredential(t *testing.T, srv *Server, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	c, w := newTestContext(t, newRequest(http.MethodPost, target, bytes.NewReader(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	srv.HandleImportVertexCredential(c)
	return w
}

func TestHandleImportVertexCredentialCreatesThenRotatesChannel(t *testing.T) {
	t.Parallel()
	upstream := newVertexTestUpstream(t)
	srv := newInMemoryServer(t)
	ctx := context.Background()

	w := importVertexCredential(t, srv, "/admin/vertex/credentials/import?location=us-central1",
		vertexTestServiceAccount(t, upstream.tokenURI(), 0))
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	created := mustParseAPIResponse[vertexCredentialImportResponse](t, w.Body.Bytes()).Data
	if !created.Created || created.Email != vertexTestEmail || created.ProjectID != vertexTestProject {
		t.Fatalf("create response=%+v", created)
	}
	cfg, err := srv.store.GetConfig(ctx, created.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "https://us-central1-aiplatform.googleapis.com" + vertexTestRegionURL
	if !cfg.UsesVertexOAuth() || len(cfg.URLs) != 1 || cfg.URLs[0].URL != wantURL || !cfg.SupportsModel(vertexTestModel) {
		t.Fatalf("channel=%+v", cfg)
	}
	for _, entry := range cfg.ModelEntries {
		if strings.HasPrefix(entry.Model, "imagen-") {
			t.Fatalf("Vertex channel lists Imagen model %q", entry.Model)
		}
	}
	if _, err := srv.store.UpdateChannelEnabled(ctx, cfg.ID, false); err != nil {
		t.Fatal(err)
	}

	// The rotated key for the same account replaces the credential in place;
	// the channel keeps its URL although the new file defaults to global.
	w = importVertexCredential(t, srv, "/admin/vertex/credentials/import", vertexTestServiceAccount(t, upstream.tokenURI(), 1))
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", w.Code, w.Body.String())
	}
	rotated := mustParseAPIResponse[vertexCredentialImportResponse](t, w.Body.Bytes()).Data
	if rotated.Created || rotated.ChannelID != created.ChannelID {
		t.Fatalf("rotate response=%+v", rotated)
	}
	cfg, err = srv.store.GetConfig(ctx, created.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := vertexauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.URLs[0].URL != wantURL || stored.ServiceAccount["private_key_id"] != "kid-1" {
		t.Fatalf("rotated channel enabled=%v url=%s key=%v", cfg.Enabled, cfg.URLs[0].URL, stored.ServiceAccount["private_key_id"])
	}
	if calls := upstream.tokenCalls.Load(); calls != 2 {
		t.Fatalf("token mints=%d, want one validation per import", calls)
	}
}

func TestHandleImportVertexCredentialRejectsUnusableKey(t *testing.T) {
	t.Parallel()
	tokenServer := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`)
	}))
	srv := newInMemoryServer(t)

	w := importVertexCredential(t, srv, "/admin/vertex/credentials/import", vertexTestServiceAccount(t, tokenServer.URL+"/token", 0))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	w = importVertexCredential(t, srv, "/admin/vertex/credentials/import?location=us-central1/../x",
		vertexTestServiceAccount(t, tokenServer.URL+"/token", 0))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid location status=%d body=%s", w.Code, w.Body.String())
	}
	configs, err := srv.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("channels=%d err=%v, want none", len(configs), err)
	}
}

func TestHandleImportOAuthCredentialsDetectsVertexServiceAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newVertexTestUpstream(t)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store, client: newTestHTTPClient()}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "vertex-proj.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(vertexTestServiceAccount(t, upstream.tokenURI(), 0)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("provider", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := importOAuthCredentialsViaJob(t, server, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	summary := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes()).Data
	if summary.Created != 1 || summary.Failed != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	if strings.Contains(response.Body.String(), "PRIVATE KEY") {
		t.Fatalf("response leaked the private key: %s", response.Body.String())
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 1 || !channels[0].UsesVertexOAuth() || !channels[0].SupportsModel(vertexTestModel) {
		t.Fatalf("channels=%+v err=%v", channels, err)
	}
	if calls := upstream.tokenCalls.Load(); calls != 1 {
		t.Fatalf("token mints=%d, want the key validated once", calls)
	}
}

func TestHandleImportVertexCredentialEgressesThroughProxy(t *testing.T) {
	t.Parallel()
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "vertex-token.invalid" || r.URL.Path != "/token" {
			t.Errorf("proxy forwarded %s%s", r.Host, r.URL.Path)
		}
		proxyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok-proxy","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(proxy.Close)
	// A real transport: an injected test RoundTripper would bypass the channel proxy.
	srv := &Server{store: newCodexAuthTestStore(t), client: &http.Client{Transport: &http.Transport{}}}
	key := vertexTestServiceAccount(t, "http://vertex-token.invalid/token", 0)

	w := importVertexCredential(t, srv, "/admin/vertex/credentials/import?proxy_url=ftp%3A%2F%2Fproxy.invalid", key)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid proxy status=%d body=%s", w.Code, w.Body.String())
	}
	w = importVertexCredential(t, srv, "/admin/vertex/credentials/import?location=us-central1&proxy_url="+proxy.URL, key)
	if w.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
	}
	created := mustParseAPIResponse[vertexCredentialImportResponse](t, w.Body.Bytes()).Data
	cfg, err := srv.store.GetConfig(context.Background(), created.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || cfg.ProxyURL != proxy.URL || proxyHits.Load() != 1 {
		t.Fatalf("created=%v proxy=%q proxy hits=%d", created.Created, cfg.ProxyURL, proxyHits.Load())
	}
}
