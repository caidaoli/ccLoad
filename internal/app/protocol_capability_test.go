package app

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/gin-gonic/gin"
)

func TestProtocolCapabilityCacheClearChannelsKeepsOtherChannels(t *testing.T) {
	t.Parallel()

	cache := &protocolCapabilityCache{}
	keyFor := func(channelID int64, upstreamModel string) protocolCapabilityKey {
		return protocolCapabilityKey{
			channelID: channelID, baseURL: "https://api.example.com",
			clientProtocol: protocol.OpenAI, requestFamily: protocol.RequestFamilyChatCompletions,
			upstreamModel: upstreamModel,
		}
	}
	cache.set(keyFor(1, "model-a"), protocol.Anthropic)
	cache.set(keyFor(1, "model-b"), protocolUnsupported)
	cache.set(keyFor(2, "model-a"), protocol.Codex)

	cache.clearChannels(1)

	if _, known := cache.get(keyFor(1, "model-a")); known {
		t.Fatal("channel 1 model-a capability survived clearChannels(1)")
	}
	if _, known := cache.get(keyFor(1, "model-b")); known {
		t.Fatal("channel 1 unsupported sentinel survived clearChannels(1)")
	}
	if got, known := cache.get(keyFor(2, "model-a")); !known || got != protocol.Codex {
		t.Fatalf("channel 2 capability=%q known=%v, want codex", got, known)
	}
}

func TestProtocolCapabilityModelUsesRedirectTarget(t *testing.T) {
	t.Parallel()

	server := &Server{}
	cfg := &model.Config{ModelEntries: []model.ModelEntry{
		{Model: "alias-a", RedirectModel: "GPT-5.5-Codex"},
		{Model: "plain-b"},
	}}
	tests := []struct {
		name   string
		model  string
		family protocol.RequestFamily
		want   string
	}{
		{name: "redirect target", model: "alias-a", family: protocol.RequestFamilyChatCompletions, want: "gpt-5.5-codex"},
		{name: "plain model", model: "plain-b", family: protocol.RequestFamilyChatCompletions, want: "plain-b"},
		{name: "alpha search stays endpoint scoped", model: "alias-a", family: protocol.RequestFamilyAlphaSearch, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reqCtx := &proxyRequestContext{originalModel: tt.model}
			if got := server.protocolCapabilityModel(cfg, reqCtx, tt.family); got != tt.want {
				t.Fatalf("protocolCapabilityModel(%q)=%q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestInvalidateChannelListCacheKeepsLearnedProtocols(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	key := protocolCapabilityKey{
		channelID: 1, baseURL: "https://api.example.com",
		clientProtocol: protocol.OpenAI, requestFamily: protocol.RequestFamilyChatCompletions,
		upstreamModel: "model-a",
	}
	server.protocolCapabilities.set(key, protocol.Anthropic)

	// OAuth 刷新、额度元数据等运行时写库都会走这里，不能让所有渠道重新探测。
	server.InvalidateChannelListCache()

	if got, known := server.protocolCapabilities.get(key); !known || got != protocol.Anthropic {
		t.Fatalf("capability=%q known=%v after InvalidateChannelListCache, want anthropic", got, known)
	}
}

func TestHandleUpdateChannelClearsOnlyUpdatedChannelProtocolCapabilities(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	createChannel := func(name string) *model.Config {
		t.Helper()
		created, err := store.CreateConfig(ctx, &model.Config{
			Name:         name,
			URLs:         model.ChannelURLs{{URL: "https://" + name + ".example.com"}},
			Priority:     10,
			Enabled:      true,
			ModelEntries: []model.ModelEntry{{Model: "model-a"}},
		})
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
			ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-" + name, KeyStrategy: model.KeyStrategySequential,
		}}); err != nil {
			t.Fatalf("创建测试 API Key 失败: %v", err)
		}
		return created
	}
	updated := createChannel("capability-updated")
	untouched := createChannel("capability-untouched")
	keyFor := func(cfg *model.Config) protocolCapabilityKey {
		return protocolCapabilityKey{
			channelID: cfg.ID, baseURL: cfg.URLs[0].URL,
			clientProtocol: protocol.OpenAI, requestFamily: protocol.RequestFamilyChatCompletions,
			upstreamModel: "model-a",
		}
	}
	server.protocolCapabilities.set(keyFor(updated), protocol.Anthropic)
	server.protocolCapabilities.set(keyFor(untouched), protocol.Codex)

	payload := map[string]any{
		"name":                    updated.Name,
		"api_key":                 "sk-" + updated.Name,
		"urls":                    []map[string]any{{"url": updated.URLs[0].URL}},
		"priority":                10,
		"protocol_transform_mode": model.ProtocolTransformModeLocal,
		"models":                  []map[string]any{{"model": "model-a"}},
		"enabled":                 true,
	}
	id := strconv.FormatInt(updated.ID, 10)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+id, payload))
	c.Params = gin.Params{{Key: "id", Value: id}}
	server.handleUpdateChannel(c, updated.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	if _, known := server.protocolCapabilities.get(keyFor(updated)); known {
		t.Fatal("updated channel kept its learned protocol after config change")
	}
	if got, known := server.protocolCapabilities.get(keyFor(untouched)); !known || got != protocol.Codex {
		t.Fatalf("untouched channel capability=%q known=%v, want codex", got, known)
	}
}
