package app

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func channelPrice(input, output float64) *util.CustomModelPrice {
	return &util.CustomModelPrice{InputPrice: &input, OutputPrice: &output}
}

func TestProxy_ChannelModelPricingBillsLogsAndTokenStats(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":2000,"total_tokens":3000}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()
	urls := channelURLsForTest(upstream.URL)
	for i := range urls {
		urls[i].Protocols = []string{util.ProtocolOpenAI}
	}
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "channel-model-pricing", AuthType: model.AuthTypeAPIKey, URLs: urls,
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Priority: 100, Enabled: true,
		ModelEntries: []model.ModelEntry{
			{Model: "relay-gpt", RedirectModel: "gpt-4o", Pricing: channelPrice(1, 2)},
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-priced", CostMultiplier: 0.5},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}
	tokenHash := model.HashToken("test-api-key")
	if err := srv.store.CreateAuthToken(ctx, &model.AuthToken{Token: tokenHash, IsActive: true}); err != nil {
		t.Fatalf("CreateAuthToken: %v", err)
	}
	stored, err := srv.store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetAuthTokenByValue: %v", err)
	}
	injectAPIToken(srv.authService, "test-api-key", 0, stored.ID)
	engine := gin.New()
	srv.SetupRoutes(engine)

	for _, requestModel := range []string{"relay-gpt", "gpt-4o"} {
		response := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
			"model": requestModel, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", requestModel, response.Code, response.Body.String())
		}
	}
	time.Sleep(srv.logService.batchTimeout + 250*time.Millisecond)

	// relay-gpt 按渠道价格计费（倍率照旧叠加）；同渠道未配置价格的 gpt-4o 仍走目录价格。
	wantCosts := map[string]float64{
		"relay-gpt": 1000*1.0/1e6 + 2000*2.0/1e6,
		"gpt-4o":    util.CalculateCostDetailed("gpt-4o", 1000, 2000, 0, 0, 0),
	}
	logs, err := srv.store.ListLogs(ctx, time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{ChannelID: &created.ID})
	if err != nil || len(logs) != 2 {
		t.Fatalf("ListLogs = (%d logs, %v), want 2", len(logs), err)
	}
	prices := srv.logModelPrices(ctx, logs)
	totalCost := 0.0
	for _, entry := range logs {
		want := wantCosts[entry.Model]
		if math.Abs(entry.Cost-want) > 1e-12 || entry.CostMultiplier != 0.5 {
			t.Fatalf("log %s cost=%v multiplier=%v, want %v and 0.5", entry.Model, entry.Cost, entry.CostMultiplier, want)
		}
		if breakdown := buildLogCostBreakdown(entry, prices); breakdown == nil || math.Abs(breakdown.Total-want) > 1e-12 {
			t.Fatalf("log %s breakdown=%#v, want total %v", entry.Model, breakdown, want)
		}
		totalCost += want
	}

	// 令牌统计异步落库，成本须与日志同源。
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		token, err := srv.store.GetAuthTokenByValue(ctx, tokenHash)
		if err == nil && token.SuccessCount == 2 {
			if math.Abs(token.TotalCostUSD-totalCost) > 1e-9 || math.Abs(token.EffectiveCostUSD-totalCost*0.5) > 1e-9 {
				t.Fatalf("token cost=(%v, %v), want (%v, %v)", token.TotalCostUSD, token.EffectiveCostUSD, totalCost, totalCost*0.5)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("token stats not updated: token=%+v err=%v", token, err)
		}
	}
}

func TestProxy_ModelVariantPriceMatchesSelectedUpstreamTarget(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":1000,"total_tokens":2000}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{name: "priced-variants", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{
		{Model: "auto", RedirectModel: "priced-a", Pricing: channelPrice(1, 2)},
		{Model: "auto", RedirectModel: "priced-b", Pricing: channelPrice(3, 4)},
	}}}, map[int]string{0: upstream.URL})
	for range 2 {
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	time.Sleep(env.server.logService.batchTimeout + 250*time.Millisecond)
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("configs=%v err=%v", configs, err)
	}
	logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{ChannelID: &configs[0].ID})
	if err != nil || len(logs) != 2 {
		t.Fatalf("logs=%v err=%v", logs, err)
	}
	want := map[string]float64{"priced-a": 0.003, "priced-b": 0.007}
	prices := env.server.logModelPrices(context.Background(), logs)
	for _, entry := range logs {
		expected, ok := want[entry.ActualModel]
		if !ok || math.Abs(entry.Cost-expected) > 1e-12 {
			t.Fatalf("actual=%s cost=%v, want=%v", entry.ActualModel, entry.Cost, expected)
		}
		if breakdown := buildLogCostBreakdown(entry, prices); breakdown == nil || math.Abs(breakdown.Total-expected) > 1e-12 {
			t.Fatalf("actual=%s breakdown=%+v, want=%v", entry.ActualModel, breakdown, expected)
		}
		delete(want, entry.ActualModel)
	}
	if len(want) != 0 {
		t.Fatalf("missing targets: %v", want)
	}
}

func TestProxy_ModelVariantLogOmitsAmbiguousPriceBreakdown(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":1000,"total_tokens":2000}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{name: "ambiguous-priced-variants", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{
		{Model: "auto", RedirectModel: "middle", Pricing: channelPrice(1, 2)},
		{Model: "auto", RedirectModel: "final", Pricing: channelPrice(3, 4)},
		{Model: "middle", RedirectModel: "final"},
	}}}, map[int]string{0: upstream.URL})
	for range 2 {
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	time.Sleep(env.server.logService.batchTimeout + 250*time.Millisecond)
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("configs=%v err=%v", configs, err)
	}
	logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{ChannelID: &configs[0].ID})
	if err != nil || len(logs) != 2 {
		t.Fatalf("logs=%v err=%v", logs, err)
	}
	wantCosts := map[float64]bool{0.003: false, 0.007: false}
	for _, entry := range logs {
		if entry.ActualModel != "final" {
			t.Fatalf("actual model=%q, want final", entry.ActualModel)
		}
		matched := false
		for want := range wantCosts {
			if math.Abs(entry.Cost-want) < 1e-12 {
				wantCosts[want] = true
				matched = true
			}
		}
		if !matched {
			t.Fatalf("unexpected persisted cost=%v", entry.Cost)
		}
	}
	for cost, seen := range wantCosts {
		if !seen {
			t.Fatalf("missing persisted cost=%v", cost)
		}
	}
	for _, projected := range projectDashboardLogs(logs, env.server.logModelPrices(context.Background(), logs)) {
		if projected.CostBreakdown != nil {
			t.Fatalf("ambiguous log cost=%v exposed misleading breakdown=%+v", projected.Cost, projected.CostBreakdown)
		}
	}
}

// 管理端非流式测试、流式对话与真实代理请求必须按同一口径计费：同一份渠道价格、同一个计费模型。
// 重定向目标没有目录价格时计费模型回退到请求模型 gpt-5.4，其高上下文阈值是 272K；
// 若按重定向目标计费会落到默认 200K 阈值，250K 输入就被错算成高上下文价。
func TestChannelTestsBillLikeProxyWithRedirectAndHighContextPrice(t *testing.T) {
	t.Parallel()
	const usage = `{"prompt_tokens":250000,"completion_tokens":1000,"total_tokens":251000}`
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"chat-1","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"id":"chat-1","choices":[],"usage":`+usage+`}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":`+usage+`}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()
	urls := channelURLsForTest(upstream.URL)
	for i := range urls {
		urls[i].Protocols = []string{util.ProtocolOpenAI}
	}
	price := channelPrice(1, 2)
	inputHigh, outputHigh := 10.0, 20.0
	price.InputPriceHigh, price.OutputPriceHigh = &inputHigh, &outputHigh
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "channel-test-billing", AuthType: model.AuthTypeAPIKey, URLs: urls,
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Priority: 100, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.4", RedirectModel: "vendor-x-alpha", Pricing: price}},
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-priced"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}
	injectAPIToken(srv.authService, "test-api-key", 0, 0)
	engine := gin.New()
	srv.SetupRoutes(engine)
	want := 250_000*1.0/1e6 + 1000*2.0/1e6

	started := time.Now()
	response := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-5.4", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
	}
	time.Sleep(srv.logService.batchTimeout + 250*time.Millisecond)
	proxyLogs, err := srv.store.ListLogs(ctx, started.Add(-time.Second), 10, 0, &model.LogFilter{ChannelID: &created.ID})
	if err != nil || len(proxyLogs) != 1 || math.Abs(proxyLogs[0].Cost-want) > 1e-12 {
		t.Fatalf("proxy logs=%+v err=%v, want one log costing %v", proxyLogs, err, want)
	}

	cfg, err := srv.store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	result := srv.testChannelAPI(ctx, cfg, "sk-priced", &testutil.TestChannelRequest{Model: "gpt-5.4", ClientProtocol: "openai", Content: "hi"})
	if cost, _ := result["cost_usd"].(float64); math.Abs(cost-want) > 1e-12 {
		t.Fatalf("non-stream test cost_usd=%v, want %v; result=%+v", result["cost_usd"], want, result)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model": "gpt-5.4", "client_protocol": "openai", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelChat(c)
	summary := ""
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok && gjson.Get(data, "summary").Exists() {
			summary = data
		}
	}
	if cost := gjson.Get(summary, "summary.cost_usd").Float(); math.Abs(cost-want) > 1e-12 {
		t.Fatalf("stream summary=%s, want cost_usd %v; body:\n%s", summary, want, w.Body.String())
	}
	chatLogs, err := srv.store.ListLogsRange(ctx, started.Add(-time.Second), time.Now().Add(time.Second), 10, 0,
		&model.LogFilter{ChannelID: &created.ID, LogSource: model.LogSourceDetection})
	if err != nil || len(chatLogs) != 1 || math.Abs(chatLogs[0].Cost-want) > 1e-12 {
		t.Fatalf("chat detection logs=%+v err=%v, want one log costing %v", chatLogs, err, want)
	}
}

func TestReplaceModelEntriesCarriesChannelPricing(t *testing.T) {
	t.Parallel()
	price := channelPrice(1, 2)
	cfg := &model.Config{ModelEntries: []model.ModelEntry{{Model: "openai/GPT-5", Pricing: price}}}
	replaceModelEntries(cfg, []model.ModelEntry{{Model: "gpt-5"}, {Model: "added"}},
		modelNormalizationOptions{lowercaseModels: true, stripModelSourcePrefix: true})
	if cfg.ModelEntries[0].Pricing != price || cfg.ModelEntries[1].Pricing != nil {
		t.Fatalf("replaced models=%+v, want gpt-5 to keep its channel price", cfg.ModelEntries)
	}
}

func TestCSVModelPricingRoundTripAndCarry(t *testing.T) {
	t.Parallel()
	price := channelPrice(1.5, 6)
	exported, err := exportChannelModelPricing([]model.ModelEntry{{Model: "model-a", Pricing: price}, {Model: "model-b"}})
	if err != nil || exported != `{"model-a":{"input_price":1.5,"output_price":6}}` {
		t.Fatalf("exported model_pricing = (%s, %v)", exported, err)
	}

	columns := map[string]int{"name": 0, "api_key": 1, "urls": 2, "models": 3, "model_pricing": 4}
	parse := func(pricing string, hasColumn bool, existing map[string][]model.ModelEntry) (*model.ChannelWithKeys, string) {
		channel, message, _ := (&Server{}).parseChannelImportRow(
			[]string{"priced", "sk-imported", `[{"url":"https://api.example.com"}]`, "model-a,model-b", pricing},
			columns, 2, false, false, false, false, false, false, false, false, false, hasColumn,
			nil, nil, nil, nil, nil, nil, nil, existing,
		)
		return channel, message
	}

	channel, message := parse(exported, true, nil)
	if message != "" || !channel.Config.ModelEntries[0].Pricing.Equal(price) || channel.Config.ModelEntries[1].Pricing != nil {
		t.Fatalf("imported pricing: channel=%#v message=%q", channel, message)
	}
	for _, invalid := range []string{
		`{"unknown-model":{"input_price":1,"output_price":2}}`,
		`{"model-a":{"input_price":-1,"output_price":2}}`,
		// 只填缓存价会让输入/输出按 0 计费；后端与编辑器同样要求必填。
		`{"model-a":{"cache_read_price":0.1}}`,
	} {
		if _, message := parse(invalid, true, nil); !strings.Contains(message, "model_pricing") {
			t.Fatalf("model_pricing %s accepted: %q", invalid, message)
		}
	}

	// 旧版 CSV 没有 model_pricing 列：沿用已有同名渠道的模型价格。
	channel, message = parse("", false, map[string][]model.ModelEntry{"priced": {{Model: "model-b", Pricing: price}}})
	if message != "" || channel.Config.ModelEntries[0].Pricing != nil || !channel.Config.ModelEntries[1].Pricing.Equal(price) {
		t.Fatalf("carried pricing: channel=%#v message=%q", channel, message)
	}
}
