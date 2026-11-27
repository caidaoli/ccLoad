package cursorauth

import (
	"crypto/sha256"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

func TestParseRequestPromptIncludesToolTurns(t *testing.T) {
	t.Parallel()
	prompt := ParseRequest([]byte(`{
		"system":"sys",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"bash","input":{"cmd":"ls"}}]},
			{"role":"user","content":"follow up"}
		]
	}`)).Prompt
	if !strings.Contains(prompt, "sys") || !strings.Contains(prompt, "hello") ||
		!strings.Contains(prompt, "[tool_use id=1 name=bash]") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("prompt = %q", prompt)
	}
}

/**
 * @brief 验证模型目录与实际发送的上下文、速度和思考参数一致。
 * @param t 测试上下文。
 */
func TestSDKRunnerCatalogContextAndSlowVariants(t *testing.T) {
	options := func(id string, values ...string) *sdkv1.ModelParameterDefinition {
		param := &sdkv1.ModelParameterDefinition{Id: id}
		for _, value := range values {
			param.Values = append(param.Values, &sdkv1.ModelParameterDefinitionValue{Value: value})
		}
		return param
	}
	variant := func(window, fast string, isDefault bool) *sdkv1.ModelVariant {
		return &sdkv1.ModelVariant{IsDefault: isDefault, Params: []*sdkv1.ModelParameterValue{
			{Id: "context", Value: window}, {Id: "fast", Value: fast},
		}}
	}
	handler := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
	runner, _ := newCatalogTestSDKRunner(t, handler, []*sdkv1.SdkModel{
		{
			Id: "window",
			Parameters: []*sdkv1.ModelParameterDefinition{
				options("context", "300k", "1m", "256k"), options("fast", "true", "false"),
				options("reasoning_effort", "low", "high"),
			},
			Variants: []*sdkv1.ModelVariant{
				variant("300k", "false", false), variant("1m", "true", true),
				variant("256k", "false", false), variant("500k", "true", false),
			},
		},
		{Id: "window@256k"},
		{Id: "window@500k-fast"},
		{Id: "window-slow"},
		{Id: "window-high"},
	})
	credential := &Credential{APIKey: "key"}
	models, err := runner.ListModels(t.Context(), credential.APIKey)
	wantModels := []string{
		"window", "window-fast", "window@300k", "window@300k-slow", "window@500k",
		"window@256k", "window@500k-fast", "window-slow", "window-high",
	}
	if err != nil || !slices.Equal(models.Models, wantModels) {
		t.Fatalf("ListModels() = %v, %v, want %v", models, err, wantModels)
	}
	wantWindows := map[string]int{
		"window": 1000000, "window-fast": 1000000,
		"window@300k": 300000, "window@300k-slow": 300000, "window@500k": 500000,
		"window@1m": 1000000, "window@1m-fast": 1000000,
	}
	if !maps.Equal(models.ContextWindows, wantWindows) {
		t.Fatalf("context windows = %v, want %v", models.ContextWindows, wantWindows)
	}
	for _, tc := range []struct {
		model      string
		effort     string
		wantModel  string
		wantParams map[string]string
	}{
		{model: "window", wantModel: "window"},
		{model: "window-fast", wantModel: "window", wantParams: map[string]string{"fast": "true"}},
		{model: "window@300k", wantModel: "window", wantParams: map[string]string{"context": "300k"}},
		{model: "window@300k-slow", wantModel: "window", wantParams: map[string]string{"context": "300k", "fast": "false"}},
		{model: "window@300k-high-slow", effort: "low", wantModel: "window", wantParams: map[string]string{"context": "300k", "fast": "false", "reasoning_effort": "high"}},
		{model: "window@500k", effort: "low", wantModel: "window", wantParams: map[string]string{"context": "500k", "reasoning_effort": "low"}},
		{model: "window@300k-fast", wantModel: "window@300k-fast"},
		{model: "window@1m", wantModel: "window"},
		{model: "window@1m-fast", wantModel: "window", wantParams: map[string]string{"fast": "true"}},
		{model: "window@256k", wantModel: "window@256k"},
		{model: "window@256k-slow", wantModel: "window@256k-slow"},
		{model: "window@500k-fast", wantModel: "window@500k-fast"},
		{model: "window-slow", wantModel: "window-slow"},
		{model: "window-high-slow", wantModel: "window-high-slow"},
		{model: "window-high-fast", wantModel: "window-high", wantParams: map[string]string{"fast": "true"}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			request := Request{Model: tc.model, Prompt: "hi", Effort: tc.effort}
			drainRun(t, mustRun(t, runner, credential, request))
			handler.mu.Lock()
			sent := handler.create.GetOptions().GetModel()
			handler.mu.Unlock()
			params := make(map[string]string, len(sent.GetParams()))
			for _, param := range sent.GetParams() {
				params[param.GetId()] = param.GetValue()
			}
			if sent.GetId() != tc.wantModel || len(sent.GetParams()) != len(params) || !maps.Equal(params, tc.wantParams) {
				t.Fatalf("CreateAgent model = %v, want %q with %v", sent, tc.wantModel, tc.wantParams)
			}
		})
	}
}

/**
 * @brief 验证指定速度时只报告由目录确定的标称窗口。
 * @param t 测试上下文。
 */
func TestSDKRunnerReportsKnownContextWindows(t *testing.T) {
	variant := func(window, fast string, isDefault bool) *sdkv1.ModelVariant {
		return &sdkv1.ModelVariant{IsDefault: isDefault, Params: []*sdkv1.ModelParameterValue{
			{Id: "context", Value: window}, {Id: "fast", Value: fast},
		}}
	}
	runner, _ := newCatalogTestSDKRunner(t, &testAgentHandler{}, []*sdkv1.SdkModel{
		{Id: "asymmetric", Variants: []*sdkv1.ModelVariant{
			variant("1.5M", "false", true), variant("272.5k", "false", false), variant("272.5k", "true", false),
		}},
		{Id: "ambiguous", Variants: []*sdkv1.ModelVariant{
			variant("1m", "true", true), variant("1m", "false", false), variant("300k", "false", false),
		}},
		{Id: "absent", Variants: []*sdkv1.ModelVariant{
			variant("1m", "false", true), variant("", "true", false),
		}},
		{Id: "parameters-only", Parameters: []*sdkv1.ModelParameterDefinition{{
			Id: "context", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "200k"}},
		}}},
		{Id: "opaque@500k"},
		{Id: "invalid", Variants: []*sdkv1.ModelVariant{
			variant("auto", "false", true), variant("0k", "true", false), variant("9999999999999999999M", "false", false),
		}},
	})
	snapshot, err := runner.ListModels(t.Context(), "key")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	want := map[string]int{
		"asymmetric": 1500000, "asymmetric-slow": 1500000,
		"asymmetric-fast": 272500, "asymmetric@1.5M": 1500000, "asymmetric@1.5M-slow": 1500000,
		"asymmetric@272.5k": 272500, "asymmetric@272.5k-fast": 272500, "asymmetric@272.5k-slow": 272500,
		"ambiguous": 1000000, "ambiguous-fast": 1000000,
		"ambiguous@1m": 1000000, "ambiguous@1m-fast": 1000000,
		"ambiguous@300k": 300000, "ambiguous@300k-slow": 300000,
		"absent": 1000000, "absent-slow": 1000000, "absent@1m": 1000000, "absent@1m-slow": 1000000,
		"parameters-only": 200000, "parameters-only@200k": 200000,
	}
	if !maps.Equal(snapshot.ContextWindows, want) {
		t.Fatalf("context windows = %v, want %v", snapshot.ContextWindows, want)
	}
}

/**
 * @brief 验证目录发布各变体实际可组合的档位，与 SDK 发出的参数相同。
 * @param t 测试上下文。
 */
func TestSDKRunnerReportsVariantSpecificEffortLevels(t *testing.T) {
	variant := func(window, fast, effort string, isDefault bool) *sdkv1.ModelVariant {
		return &sdkv1.ModelVariant{IsDefault: isDefault, Params: []*sdkv1.ModelParameterValue{
			{Id: "context", Value: window}, {Id: "fast", Value: fast}, {Id: "reasoning", Value: effort},
		}}
	}
	agent := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
	runner, _ := newCatalogTestSDKRunner(t, agent, []*sdkv1.SdkModel{
		{Id: "default"},
		{Id: "toggle", Parameters: []*sdkv1.ModelParameterDefinition{{
			Id: "thinking", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "false"}, {Value: "true"}},
		}}},
		{Id: "sparse", Parameters: []*sdkv1.ModelParameterDefinition{{
			Id: "reasoning", Values: []*sdkv1.ModelParameterDefinitionValue{
				{Value: "off"}, {Value: "low"}, {Value: "medium"}, {Value: "extra-high"},
			},
		}}, Variants: []*sdkv1.ModelVariant{
			variant("1m", "false", "medium", true),
			variant("1m", "false", "off", false),
			variant("272k", "false", "low", false),
			variant("272k", "true", "extra-high", false),
		}},
		{Id: "sparse@272k-slow"},
	})
	snapshot, err := runner.ListModels(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"default": {}, "toggle": {"none", "high"},
		"sparse": {"none", "low", "medium", "xhigh"}, "sparse-fast": {"xhigh"},
		"sparse-slow": {"none", "low", "medium"}, "sparse@1m": {"none", "medium"},
		"sparse@1m-slow": {"none", "medium"}, "sparse@272k": {"low", "xhigh"},
		"sparse@272k-fast": {"xhigh"}, "sparse@272k-slow": {},
	}
	defaults := map[string]string{"sparse": "medium", "sparse-slow": "medium", "sparse@1m": "medium", "sparse@1m-slow": "medium"}
	if !maps.Equal(snapshot.DefaultEfforts, defaults) {
		t.Fatalf("discovered defaults = %v, want %v", snapshot.DefaultEfforts, defaults)
	}
	if !maps.EqualFunc(snapshot.EffortLevels, want, slices.Equal) {
		t.Fatalf("effort levels = %v, want %v", snapshot.EffortLevels, want)
	}
	for _, tc := range []struct {
		model      string
		effort     string
		wantParams map[string]string
	}{
		{model: "toggle", effort: "none", wantParams: map[string]string{"thinking": "false"}},
		{model: "sparse@1m", effort: "xhigh"},
		{model: "sparse@272k-fast", effort: "xhigh", wantParams: map[string]string{
			"context": "272k", "fast": "true", "reasoning": "extra-high",
		}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			request := Request{Model: tc.model, Prompt: "hi", Effort: tc.effort}
			drainRun(t, mustRun(t, runner, &Credential{APIKey: "key"}, request))
			agent.mu.Lock()
			sent := agent.create.GetOptions().GetModel().GetParams()
			agent.mu.Unlock()
			params := make(map[string]string, len(sent))
			for _, param := range sent {
				params[param.GetId()] = param.GetValue()
			}
			if !maps.Equal(params, tc.wantParams) {
				t.Fatalf("CreateAgent params = %v, want %v", params, tc.wantParams)
			}
		})
	}
}

/**
 * @brief 验证推理可在临时网络中断时使用同 Key 的近期目录，强制刷新和账号错误保持可见。
 * @param t 测试上下文。
 */
func TestSDKRunnerUsesRecentCatalogOnDiscoveryOutage(t *testing.T) {
	agent := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
	var failure atomic.Uint32
	catalog := &testCursorHandlerFunc{listModels: func(*sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
		if code := failure.Load(); code != 0 {
			return nil, connect.NewError(connect.Code(code), errors.New("discovery unavailable"))
		}
		return &sdkv1.ListModelsResponse{Items: []*sdkv1.SdkModel{{Id: "demo", Parameters: []*sdkv1.ModelParameterDefinition{{Id: "effort", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "high"}}}}}}}, nil
	}}
	mux := http.NewServeMux()
	mux.Handle(sdkv1connect.NewSdkAgentServiceHandler(agent))
	mux.Handle(sdkv1connect.NewSdkCursorServiceHandler(catalog))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	client.workdir = t.TempDir()
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{bridge: bridge, idleTimeout: time.Second}
	credential := &Credential{APIKey: "catalog-key"}
	request := Request{Model: "demo", Effort: "high", Prompt: "hello"}
	if _, err := runner.ListModels(t.Context(), credential.APIKey); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(credential.APIKey))
	expire := func(age time.Duration) {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		c := runner.catalogs[key]
		c.expiresAt = time.Now().Add(-age)
		runner.catalogs[key] = c
	}
	expire(time.Minute)
	failure.Store(uint32(connect.CodeUnavailable))
	drainRun(t, mustRun(t, runner, credential, request))
	agent.mu.Lock()
	sent := agent.create.GetOptions().GetModel().GetParams()
	agent.mu.Unlock()
	if len(sent) != 1 || sent[0].GetValue() != "high" {
		t.Fatalf("selected effort from recent catalog = %v", sent)
	}
	if _, err := runner.ListModels(t.Context(), credential.APIKey); err == nil {
		t.Error("explicit refresh reported a network failure as success")
	}
	if _, err := runner.Run(t.Context(), &Credential{APIKey: "other-key"}, request); err == nil {
		t.Error("reused a different key's catalog")
	}
	failure.Store(uint32(connect.CodeUnauthenticated))
	if _, err := runner.Run(t.Context(), credential, request); err == nil {
		t.Error("ignored rejected account")
	}
	failure.Store(uint32(connect.CodeUnavailable))
	expire(sdkModelCatalogTTL + time.Minute)
	if _, err := runner.Run(t.Context(), credential, request); err == nil {
		t.Error("used catalog after grace period")
	}
}
