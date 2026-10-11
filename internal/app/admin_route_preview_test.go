package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func previewRoute(t *testing.T, server *Server, payload map[string]any) routePreviewResult {
	t.Helper()
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/route-preview", payload))
	server.HandleRoutePreview(c)
	if w.Code != http.StatusOK {
		t.Fatalf("route-preview status=%d body=%s", w.Code, w.Body.String())
	}
	var response APIResponse[routePreviewResult]
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)
	if !response.Success {
		t.Fatalf("preview failed: %s", w.Body.String())
	}
	return response.Data
}

func addPreviewChannel(t *testing.T, server *Server, cfg *model.Config, keys ...*model.APIKey) *model.Config {
	t.Helper()
	cfg.Enabled = true
	if cfg.URLs == nil {
		cfg.URLs = model.ChannelURLs{{URL: "https://example.test"}}
	}
	if cfg.ModelEntries == nil {
		cfg.ModelEntries = []model.ModelEntry{{Model: "test-model"}}
	}
	created, err := server.store.CreateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		key.ChannelID = created.ID
		key.KeyIndex = i
		if key.APIKey == "" {
			key.APIKey = "sk-test"
		}
	}
	if len(keys) > 0 {
		if err = server.store.CreateAPIKeysBatch(context.Background(), keys); err != nil {
			t.Fatal(err)
		}
	}
	server.InvalidateChannelListCache()
	server.InvalidateAPIKeysCache(created.ID)
	return created
}

func TestRoutePreview_Validation(t *testing.T) {
	server := newInMemoryServer(t)
	for _, payload := range []map[string]any{
		{"client_protocol": "invalid", "model": "test-model"},
		{"client_protocol": "openai", "model": ""},
		{"client_protocol": "openai", "model": "*"},
		{"client_protocol": "openai", "model": "test-model", "auth_token_id": 0},
	} {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/route-preview", payload))
		server.HandleRoutePreview(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("payload=%v status=%d", payload, w.Code)
		}
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/route-preview", map[string]any{"client_protocol": "openai", "model": "test-model", "auth_token_id": 999}))
	server.HandleRoutePreview(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown token status=%d", w.Code)
	}
}

func TestRoutePreview_ReadOnlyAndCandidateGroups(t *testing.T) {
	for _, health := range []bool{false, true} {
		t.Run(map[bool]string{false: "health-off", true: "health-on"}[health], func(t *testing.T) {
			server := newInMemoryServer(t)
			if health {
				server.healthCache = &HealthCache{config: model.HealthScoreConfig{Enabled: true}}
			}
			var upstreamRequests atomic.Int64
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			first := addPreviewChannel(t, server, &model.Config{Name: "one", Priority: 50, URLs: model.ChannelURLs{{URL: upstream.URL}}, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: "target-a"}, {Model: "test-model", RedirectModel: "target-b"}}}, &model.APIKey{}, &model.APIKey{})
			addPreviewChannel(t, server, &model.Config{Name: "two", Priority: 50}, &model.APIKey{})
			keys, err := server.store.GetAPIKeys(context.Background(), first.ID)
			if err != nil {
				t.Fatal(err)
			}
			control := &Server{store: server.store, channelBalancer: NewSmoothWeightedRR(), healthCache: server.healthCache}
			controlKeys := NewKeySelector()
			// Prime all three rotations, then compare subsequent observable choices.
			for _, target := range []*Server{server, control} {
				if _, err = target.selectCandidatesByModelAndClientProtocol(context.Background(), "test-model", "openai"); err != nil {
					t.Fatal(err)
				}
			}
			for _, selector := range []*KeySelector{server.keySelector, controlKeys} {
				if _, _, err = selector.SelectAvailableKey(first.ID, keys, nil); err != nil {
					t.Fatal(err)
				}
				selector.SelectModelRow(first.ID, "test-model", []bool{true, true})
			}
			configsBefore, err := server.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model"})
				if len(result.Candidates) != 2 || len(result.Excluded) != 0 || len(result.Limitations) == 0 {
					t.Fatalf("preview=%+v", result)
				}
				for _, candidate := range result.Candidates {
					if !candidate.SameTierRotation || candidate.EffectivePriority != 50 || candidate.CooldownFallback {
						t.Fatalf("candidate=%+v", candidate)
					}
				}
			}
			for range 4 {
				actual, err := server.selectCandidatesByModelAndClientProtocol(context.Background(), "test-model", "openai")
				if err != nil {
					t.Fatal(err)
				}
				expected, err := control.selectCandidatesByModelAndClientProtocol(context.Background(), "test-model", "openai")
				if err != nil {
					t.Fatal(err)
				}
				if actual[0].ID != expected[0].ID {
					t.Fatalf("preview advanced channel rotation: %d != %d", actual[0].ID, expected[0].ID)
				}
				actualKey, _, err := server.keySelector.SelectAvailableKey(first.ID, keys, nil)
				if err != nil {
					t.Fatal(err)
				}
				expectedKey, _, err := controlKeys.SelectAvailableKey(first.ID, keys, nil)
				if err != nil {
					t.Fatal(err)
				}
				if actualKey != expectedKey {
					t.Fatalf("preview advanced Key rotation: %d != %d", actualKey, expectedKey)
				}
				actualRow, _ := server.keySelector.SelectModelRow(first.ID, "test-model", []bool{true, true})
				expectedRow, _ := controlKeys.SelectModelRow(first.ID, "test-model", []bool{true, true})
				if actualRow != expectedRow {
					t.Fatalf("preview advanced model row: %d != %d", actualRow, expectedRow)
				}
			}
			configsAfter, err := server.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			keysAfter, err := server.store.GetAPIKeys(context.Background(), first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(configsBefore, configsAfter) || !reflect.DeepEqual(keys, keysAfter) || upstreamRequests.Load() != 0 {
				t.Fatal("preview modified persisted state or called upstream")
			}
		})
	}
}

func TestRoutePreview_ActualExclusionReasons(t *testing.T) {
	server := newInMemoryServer(t)
	healthy := addPreviewChannel(t, server, &model.Config{Name: "healthy"}, &model.APIKey{})
	scoped := addPreviewChannel(t, server, &model.Config{Name: "scoped"}, &model.APIKey{})
	costly := addPreviewChannel(t, server, &model.Config{Name: "cost", DailyCostLimit: 1}, &model.APIKey{})
	server.costCache.Load(map[int64]float64{costly.ID: 1})
	now := time.Now()
	outside := addPreviewChannel(t, server, &model.Config{Name: "outside", AvailableTimeStart: now.Add(2 * time.Hour).Format("15:04"), AvailableTimeEnd: now.Add(3 * time.Hour).Format("15:04")}, &model.APIKey{})
	channelCold := addPreviewChannel(t, server, &model.Config{Name: "channel-cold"}, &model.APIKey{})
	modelCold := addPreviewChannel(t, server, &model.Config{Name: "model-cold", ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: "actual-model"}}}, &model.APIKey{})
	keysCold := addPreviewChannel(t, server, &model.Config{Name: "keys-cold"}, &model.APIKey{AllowedModels: []string{"test-model"}}, &model.APIKey{Disabled: true})
	unauthorized := addPreviewChannel(t, server, &model.Config{Name: "unauthorized"}, &model.APIKey{AllowedModels: []string{"other-model"}})
	keysDisabled := addPreviewChannel(t, server, &model.Config{Name: "keys-disabled"}, &model.APIKey{Disabled: true})
	for _, set := range []func() error{
		func() error {
			return server.store.SetChannelCooldown(context.Background(), channelCold.ID, now.Add(time.Minute))
		},
		func() error {
			return server.store.SetModelCooldown(context.Background(), modelCold.ID, "actual-model", now.Add(2*time.Minute))
		},
		func() error {
			return server.store.SetKeyCooldown(context.Background(), keysCold.ID, 0, now.Add(3*time.Minute))
		},
	} {
		if err := set(); err != nil {
			t.Fatal(err)
		}
	}
	server.InvalidateChannelListCache()
	server.InvalidateAPIKeysCache(keysCold.ID)
	token := &model.AuthToken{Token: model.HashToken("preview-scope"), IsActive: true, AllowedChannelIDs: []int64{scoped.ID}, ChannelRestrictionMode: model.ChannelRestrictionModeDeny}
	if err := server.store.CreateAuthToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := server.authService.ReloadAuthTokens(); err != nil {
		t.Fatal(err)
	}
	result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model", "auth_token_id": token.ID})
	if len(result.Candidates) != 1 || result.Candidates[0].ID != healthy.ID {
		t.Fatalf("candidates=%+v", result.Candidates)
	}
	want := map[int64]string{scoped.ID: "token_channel_scope", costly.ID: "cost_limit", outside.ID: "time_window", channelCold.ID: "channel_cooldown", modelCold.ID: "model_cooldown", keysCold.ID: "all_keys_cooling", unauthorized.ID: "key_model_scope", keysDisabled.ID: "keys_disabled"}
	if len(result.Excluded) != len(want) {
		t.Fatalf("excluded=%+v", result.Excluded)
	}
	for _, excluded := range result.Excluded {
		if excluded.Reason != want[excluded.ID] {
			t.Fatalf("excluded=%+v expected=%s", excluded, want[excluded.ID])
		}
		if slices.Contains([]string{"channel_cooldown", "model_cooldown", "all_keys_cooling"}, excluded.Reason) && (excluded.Until == nil || !excluded.Until.After(now)) {
			t.Fatalf("cooldown missing until: %+v", excluded)
		}
	}
}

func TestRoutePreview_AllCoolingFallbackAndDeduplication(t *testing.T) {
	server := newInMemoryServer(t)
	first := addPreviewChannel(t, server, &model.Config{Name: "first"}, &model.APIKey{})
	second := addPreviewChannel(t, server, &model.Config{Name: "second", Priority: 7}, &model.APIKey{})
	now := time.Now()
	if err := server.store.SetKeyCooldown(context.Background(), first.ID, 0, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := server.store.SetModelCooldown(context.Background(), second.ID, "test-model", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	server.InvalidateChannelListCache()
	server.InvalidateAPIKeysCache(first.ID)
	result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model"})
	if len(result.Candidates) != 1 || result.Candidates[0].ID != first.ID || !result.Candidates[0].CooldownFallback || result.Candidates[0].AvailableKeyCount != 0 {
		t.Fatalf("candidates=%+v", result.Candidates)
	}
	if len(result.Excluded) != 1 || result.Excluded[0].ID != second.ID || result.Excluded[0].Priority != 7 || result.Excluded[0].Reason != "model_cooldown" {
		t.Fatalf("duplicate or fallback exclusion: %+v", result.Excluded)
	}
}

func TestRoutePreview_ModelScopeAfterMultimodalFallback(t *testing.T) {
	server := newInMemoryServer(t)
	addPreviewChannel(t, server, &model.Config{Name: "image", ModelEntries: []model.ModelEntry{{Model: "image-model"}}}, &model.APIKey{})
	server.setMultimodalFallbackModels(map[string]string{"text-model": "image-model(max)"})
	token := &model.AuthToken{Token: model.HashToken("preview-models"), IsActive: true, AllowedModels: []string{"image-model"}}
	if err := server.store.CreateAuthToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := server.authService.ReloadAuthTokens(); err != nil {
		t.Fatal(err)
	}
	denied := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "text-model", "auth_token_id": token.ID})
	if !denied.ModelDenied || len(denied.Candidates) != 0 {
		t.Fatalf("denied=%+v", denied)
	}
	allowed := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "text-model", "multimodal": true, "auth_token_id": token.ID})
	if allowed.ModelDenied || allowed.Model != "image-model" || len(allowed.Candidates) != 1 {
		t.Fatalf("allowed=%+v", allowed)
	}
}

func TestRoutePreview_AvailableKeysRespectEnabledRowsAndScopes(t *testing.T) {
	server := newInMemoryServer(t)
	addPreviewChannel(t, server, &model.Config{Name: "key-scopes", ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: "active-target"}, {Model: "test-model", RedirectModel: "disabled-target", Disabled: true}}},
		&model.APIKey{}, &model.APIKey{Disabled: true}, &model.APIKey{AllowedModels: []string{"other-model"}}, &model.APIKey{DetectedModels: []string{"disabled-target"}}, &model.APIKey{DetectedModels: []string{"active-target"}},
	)
	addPreviewChannel(t, server, &model.Config{Name: "oauth", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: `{}`})
	result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model"})
	if len(result.Candidates) != 2 {
		t.Fatalf("candidates=%+v", result.Candidates)
	}
	for _, candidate := range result.Candidates {
		want := 2
		if candidate.Name == "oauth" {
			want = 1
		}
		if candidate.AvailableKeyCount != want {
			t.Fatalf("candidate=%+v want=%d", candidate, want)
		}
	}
}

func TestRoutePreview_HealthScoresUseFinalCandidateMedian(t *testing.T) {
	server := newInMemoryServer(t)
	fast := addPreviewChannel(t, server, &model.Config{Name: "fast", Priority: 50}, &model.APIKey{})
	slow := addPreviewChannel(t, server, &model.Config{Name: "slow", Priority: 50}, &model.APIKey{})
	excluded := addPreviewChannel(t, server, &model.Config{Name: "excluded", Priority: 50}, &model.APIKey{})
	if err := server.store.SetChannelCooldown(context.Background(), excluded.ID, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	server.InvalidateChannelListCache()
	server.healthCache = &HealthCache{config: model.HealthScoreConfig{Enabled: true, EnableTTFBScore: true, TTFBPenaltyWeight: 8, TTFBMaxSlowRatio: 2}}
	stats := map[int64]model.ChannelHealthStats{
		fast.ID:     {SuccessRate: 1, AvgFirstByteSeconds: 1, FirstByteSampleCount: 10},
		slow.ID:     {SuccessRate: 1, AvgFirstByteSeconds: 3, FirstByteSampleCount: 10},
		excluded.ID: {SuccessRate: 1, AvgFirstByteSeconds: 100, FirstByteSampleCount: 10},
	}
	server.healthCache.healthStats.Store(&stats)
	result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model"})
	if len(result.Candidates) != 2 || result.Candidates[0].ID != fast.ID || result.Candidates[0].EffectivePriority != 50 || result.Candidates[1].ID != slow.ID || result.Candidates[1].EffectivePriority != 46 {
		t.Fatalf("scores=%+v", result.Candidates)
	}
	for _, candidate := range result.Candidates {
		if candidate.SameTierRotation {
			t.Fatalf("different health tiers marked same: %+v", candidate)
		}
	}
}

func TestRoutePreview_MixedCooldownReportsRestoringBottleneck(t *testing.T) {
	server := newInMemoryServerWithSettings(t, map[string]string{"cooldown_fallback_enabled": "false"})
	keyLater := addPreviewChannel(t, server, &model.Config{Name: "key-later"}, &model.APIKey{})
	modelLater := addPreviewChannel(t, server, &model.Config{Name: "model-later"}, &model.APIKey{})
	now := time.Now()
	for _, cfg := range []*model.Config{keyLater, modelLater} {
		keyUntil, modelUntil := now.Add(3*time.Minute), now.Add(time.Minute)
		if cfg.ID == modelLater.ID {
			keyUntil, modelUntil = modelUntil, keyUntil
		}
		if err := server.store.SetKeyCooldown(context.Background(), cfg.ID, 0, keyUntil); err != nil {
			t.Fatal(err)
		}
		if err := server.store.SetModelCooldown(context.Background(), cfg.ID, "test-model", modelUntil); err != nil {
			t.Fatal(err)
		}
		server.InvalidateAPIKeysCache(cfg.ID)
	}
	server.InvalidateChannelListCache()
	result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model"})
	if len(result.Candidates) != 0 || len(result.Excluded) != 2 {
		t.Fatalf("preview=%+v", result)
	}
	for _, excluded := range result.Excluded {
		want := "model_cooldown"
		if excluded.ID == keyLater.ID {
			want = "all_keys_cooling"
		}
		if excluded.Reason != want || excluded.Until == nil || excluded.Until.Before(now.Add(2*time.Minute)) {
			t.Fatalf("excluded=%+v want=%s", excluded, want)
		}
	}
}

func TestRoutePreview_OAuthCredentialCooldownFallbackHasNoAvailableKey(t *testing.T) {
	server := newInMemoryServer(t)
	cfg := addPreviewChannel(t, server, &model.Config{Name: "oauth-cooling", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: `{}`})
	if err := server.store.SetChannelCooldown(context.Background(), cfg.ID, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	server.InvalidateChannelListCache()
	result := previewRoute(t, server, map[string]any{"client_protocol": "codex", "model": "test-model"})
	if len(result.Candidates) != 1 || !result.Candidates[0].CooldownFallback || result.Candidates[0].AvailableKeyCount != 0 || len(result.Excluded) != 0 {
		t.Fatalf("preview=%+v", result)
	}
}

func TestRoutePreview_PriorityGroupMatchesHealthRotationTier(t *testing.T) {
	server := newInMemoryServer(t)
	first := addPreviewChannel(t, server, &model.Config{Name: "9.01", Priority: 10}, &model.APIKey{})
	second := addPreviewChannel(t, server, &model.Config{Name: "9.09", Priority: 10}, &model.APIKey{})
	server.healthCache = &HealthCache{config: model.HealthScoreConfig{Enabled: true, SuccessRatePenaltyWeight: 10, MinConfidentSample: 1}}
	stats := map[int64]model.ChannelHealthStats{first.ID: {SuccessRate: 0.901, SampleCount: 1}, second.ID: {SuccessRate: 0.909, SampleCount: 1}}
	server.healthCache.healthStats.Store(&stats)
	result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model"})
	if len(result.Candidates) != 2 {
		t.Fatalf("candidates=%+v", result.Candidates)
	}
	for _, candidate := range result.Candidates {
		if candidate.PriorityGroup != 9 || !candidate.SameTierRotation || candidate.EffectivePriority <= 9 || candidate.EffectivePriority >= 9.1 {
			t.Fatalf("candidate=%+v", candidate)
		}
	}
}

func TestRoutePreview_OrdinaryGenerationMatchesRealProxyCandidates(t *testing.T) {
	for _, client := range []string{"anthropic", "openai", "codex", "gemini"} {
		t.Run(client, func(t *testing.T) {
			var upstreamRequests atomic.Int64
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				var body string
				switch client {
				case "anthropic":
					body = `{"id":"msg-test","type":"message","role":"assistant","model":"vision-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				case "openai":
					body = `{"id":"chat-test","object":"chat.completion","model":"vision-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
				case "codex":
					body = `{"id":"resp-test","object":"response","model":"vision-model","status":"completed","output":[{"id":"msg-test","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
				case "gemini":
					body = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
				}
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{
				{name: "first", upstreamProtocol: client, models: "vision-model", priority: 50},
				{name: "second", upstreamProtocol: client, models: "vision-model", priority: 50},
			}, map[int]string{0: upstream.URL, 1: upstream.URL})
			env.server.setMultimodalFallbackModels(map[string]string{"text-model": "vision-model"})
			result := previewRoute(t, env.server, map[string]any{"client_protocol": client, "model": "text-model", "multimodal": true})
			if len(result.Candidates) != 2 || result.Model != "vision-model" || upstreamRequests.Load() != 0 {
				t.Fatalf("preview=%+v calls=%d", result, upstreamRequests.Load())
			}
			var path string
			var payload map[string]any
			switch client {
			case "anthropic":
				path = "/v1/messages"
				payload = map[string]any{"model": "text-model", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "QUJD"}}}}}}
			case "openai":
				path = "/v1/chat/completions"
				payload = map[string]any{"model": "text-model", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}}}}}}
			case "codex":
				path = "/v1/responses"
				payload = map[string]any{"model": "text-model", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "https://example.test/image.png"}}}}}
			case "gemini":
				path = "/v1beta/models/text-model:generateContent"
				payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "QUJD"}}}}}}
			}
			// The proxy module boundary receives the rewritten model, before Credits,
			// channel-header and session filters. Compare complete candidate groups.
			routeContext, _ := newTestContext(t, newJSONRequest(t, http.MethodPost, path, payload))
			real, err := env.server.selectRouteCandidates(context.Background(), routeContext, result.Model, client)
			if err != nil || len(real) != len(result.Candidates) {
				t.Fatalf("real candidates=%v err=%v", real, err)
			}
			for _, cfg := range real {
				i := slices.IndexFunc(result.Candidates, func(candidate routePreviewCandidate) bool { return candidate.ID == cfg.ID })
				if i < 0 || result.Candidates[i].PriorityGroup != float64(cfg.Priority) {
					t.Fatalf("candidate group mismatch cfg=%+v preview=%+v", cfg, result.Candidates)
				}
			}
			response := doProxyRequest(t, env.engine, path, payload, nil)
			if response.Code != http.StatusOK || upstreamRequests.Load() != 1 {
				t.Fatalf("real proxy status=%d body=%s calls=%d", response.Code, response.Body.String(), upstreamRequests.Load())
			}
		})
	}
}

type previewKeyFailureStore struct {
	storage.Store
	failChannelID int64
}

func (store *previewKeyFailureStore) GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	if channelID == store.failChannelID {
		return nil, errors.New("test Key snapshot unavailable")
	}
	return store.Store.GetAPIKeys(ctx, channelID)
}

func TestRoutePreview_KeyLoadFailureIsExcludedOnce(t *testing.T) {
	var wrapped *previewKeyFailureStore
	server := newInMemoryServerWithCustomStore(t, func(store storage.Store) storage.Store {
		wrapped = &previewKeyFailureStore{Store: store}
		return wrapped
	})
	cfg := addPreviewChannel(t, server, &model.Config{Name: "keys-unavailable"}, &model.APIKey{})
	wrapped.failChannelID = cfg.ID
	result := previewRoute(t, server, map[string]any{"client_protocol": "openai", "model": "test-model"})
	if len(result.Candidates) != 0 || len(result.Excluded) != 1 || result.Excluded[0].ID != cfg.ID || result.Excluded[0].Reason != "key_load_failed" {
		t.Fatalf("preview=%+v", result)
	}
}
