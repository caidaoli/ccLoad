package app

import (
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
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
	for _, invalid := range []string{`{"unknown-model":{"input_price":1}}`, `{"model-a":{"input_price":-1}}`} {
		if _, message := parse(invalid, true, nil); !strings.Contains(message, "model_pricing") {
			t.Fatalf("model_pricing %s accepted: %q", invalid, message)
		}
	}

	// 旧版 CSV 没有 model_pricing 列：沿用已有同名渠道的模型价格。
	channel, message = parse("", false, map[string][]model.ModelEntry{"priced": {{Model: "model-b", Pricing: price}}})
	if message != "" || channel.Config.ModelEntries[0].Pricing != nil || channel.Config.ModelEntries[1].Pricing != price {
		t.Fatalf("carried pricing: channel=%#v message=%q", channel, message)
	}
}
