package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

func TestProxyGemini_CodexModelsManifest(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	allowed, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "codex-manifest-allowed", URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.5"}, {Model: "gpt-6-sol"}, {Model: "gpt-4o"}, {Model: "claude-opus-5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateConfig(context.Background(), &model.Config{
		Name: "codex-manifest-denied", URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	server.authService = newTestAuthService(t)
	tokenHash := model.HashToken("codex-manifest-token")
	otherTokenHash := model.HashToken("other-codex-manifest-token")
	restriction := mustChannelRestriction(t, model.ChannelRestrictionModeAllow, allowed.ID)
	server.authService.authTokensMux.Lock()
	server.authService.authTokenModels[tokenHash] = []string{"gpt-5.5", "gpt-6-sol", "claude-opus-5", "gpt-secret"}
	server.authService.authTokenChannels[tokenHash] = restriction
	server.authService.authTokenModels[otherTokenHash] = []string{"gpt-4o"}
	server.authService.authTokensMux.Unlock()

	requestModels := func(hash, ifNoneMatch string) *httptest.ResponseRecorder {
		req := newRequest(http.MethodGet, "/v1/models?client_version=0.155.0", nil)
		req.Header.Set("User-Agent", "codex-cli/0.155.0")
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		client, response := newTestContext(t, req)
		client.Set("token_hash", hash)
		server.handleListOpenAIModels(client)
		return response
	}

	response := requestModels(tokenHash, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Models []struct {
			Slug                  string   `json:"slug"`
			DefaultReasoningLevel string   `json:"default_reasoning_level"`
			Visibility            string   `json:"visibility"`
			SupportedInAPI        bool     `json:"supported_in_api"`
			ContextWindow         int64    `json:"context_window"`
			MaxContextWindow      *int64   `json:"max_context_window"`
			MultiAgentVersion     string   `json:"multi_agent_version"`
			InputModalities       []string `json:"input_modalities"`
			ModelMessages         struct {
				InstructionsTemplate string `json:"instructions_template"`
			} `json:"model_messages"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
		Data json.RawMessage `json:"data"`
	}
	mustUnmarshalJSON(t, response.Body.Bytes(), &payload)
	if len(payload.Models) != 3 || len(payload.Data) != 0 {
		t.Fatalf("unexpected manifest: %+v", payload)
	}
	for index, expected := range []struct {
		slug          string
		contextWindow int64
		defaultEffort string
	}{
		{slug: "claude-opus-5", contextWindow: 1000000, defaultEffort: "none"},
		{slug: "gpt-5.5", contextWindow: 272000, defaultEffort: "medium"},
		{slug: "gpt-6-sol", contextWindow: 272000, defaultEffort: "medium"},
	} {
		entry := payload.Models[index]
		if entry.Slug != expected.slug || entry.Visibility != "list" || !entry.SupportedInAPI || entry.ContextWindow != expected.contextWindow || entry.MaxContextWindow != nil || entry.ModelMessages.InstructionsTemplate == "" || entry.MultiAgentVersion != "v2" || !slices.Contains(entry.InputModalities, "image") {
			t.Fatalf("incomplete Codex model: %+v", entry)
		}
		if entry.DefaultReasoningLevel != expected.defaultEffort || len(entry.SupportedReasoningLevels) == 0 {
			t.Fatalf("incorrect reasoning metadata: %+v", entry)
		}
	}

	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing manifest ETag")
	}
	if cached := requestModels(tokenHash, etag); cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
		t.Fatalf("conditional request: status=%d, body=%s", cached.Code, cached.Body.String())
	}
	if cached := requestModels(tokenHash, `"old", W/`+etag); cached.Code != http.StatusNotModified {
		t.Fatalf("weak conditional request: status=%d, body=%s", cached.Code, cached.Body.String())
	}
	if other := requestModels(otherTokenHash, etag); other.Code != http.StatusOK {
		t.Fatalf("different visible models reused ETag: status=%d, body=%s", other.Code, other.Body.String())
	} else {
		var visible struct {
			Models []struct {
				Slug            string   `json:"slug"`
				InputModalities []string `json:"input_modalities"`
			} `json:"models"`
		}
		mustUnmarshalJSON(t, other.Body.Bytes(), &visible)
		if len(visible.Models) != 1 || visible.Models[0].Slug != "gpt-4o" || !slices.Contains(visible.Models[0].InputModalities, "image") {
			t.Fatalf("other token models=%+v", visible.Models)
		}
	}

	client, ordinary := newTestContext(t, newRequest(http.MethodGet, "/v1/models?client_version=", nil))
	client.Set("token_hash", tokenHash)
	server.handleListOpenAIModels(client)
	var ordinaryList struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, ordinary.Body.Bytes(), &ordinaryList)
	if ordinary.Code != http.StatusOK || ordinaryList.Object != "list" || len(ordinaryList.Data) != 3 {
		t.Fatalf("empty client_version changed ordinary list: status=%d, body=%s", ordinary.Code, ordinary.Body.String())
	}
}

func TestProxyGemini_CodexModelsOnlyResponsesCapable(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	if err := util.InstallModelCatalog(&util.ModelCatalogSnapshot{
		Version: util.ModelCatalogSchemaVersion,
		Models: []util.ModelCatalogEntry{
			{ID: "gpt-5.5", Provider: "openai", OutputModalities: []string{"text"}},
			{ID: "text-embedding-3-large", Provider: "openai", Family: "text-embedding", OutputModalities: []string{"text"}},
		},
	}, "models.dev"); err != nil {
		t.Fatal(err)
	}

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.urlSelector = NewURLSelector()
	ctx := context.Background()
	compatible, err := store.CreateConfig(ctx, &model.Config{
		Name: "codex-responses-compatible", Enabled: true,
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		URLs:                  model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"anthropic"}}},
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-5.5"},
			{Model: "claude-opus-5"},
			{Model: "coding-alias", RedirectModel: "claude-opus-5"},
			{Model: "gpt-image-2.5"},
			{Model: "image-alias", RedirectModel: "gpt-image-2.5"},
			{Model: "text-embedding-3-large"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	incompatible, err := store.CreateConfig(ctx, &model.Config{
		Name: "codex-responses-incompatible", Enabled: true,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		URLs:                  model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"openai"}}},
		ModelEntries:          []model.ModelEntry{{Model: "gpt-unroutable"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := store.CreateConfig(ctx, &model.Config{
		Name: "codex-responses-disabled-url", Enabled: true,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		URLs: model.ChannelURLs{
			{URL: "https://codex.example.com", Protocols: []string{"codex"}},
			{URL: "https://openai.example.com", Protocols: []string{"openai"}},
		},
		ModelEntries: []model.ModelEntry{{Model: "gpt-disabled-route"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.urlSelector.DisableURL(disabled.ID, disabled.URLs[0].RuntimeURL())
	_, err = store.CreateConfig(ctx, &model.Config{
		Name: "codex-responses-denied", Enabled: true,
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		URLs:                  model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"anthropic"}}},
		ModelEntries:          []model.ModelEntry{{Model: "gpt-unroutable"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	server.authService = newTestAuthService(t)
	tokenHash := model.HashToken("codex-responses-model-token")
	server.authService.authTokensMux.Lock()
	server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeAllow, compatible.ID, incompatible.ID, disabled.ID)
	server.authService.authTokensMux.Unlock()

	req := newRequest(http.MethodGet, "/v1/models?client_version=0.155.0", nil)
	req.Header.Set("User-Agent", "codex-cli/0.155.0")
	client, response := newTestContext(t, req)
	client.Set("token_hash", tokenHash)
	server.handleListOpenAIModels(client)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	mustUnmarshalJSON(t, response.Body.Bytes(), &payload)
	slugs := make([]string, 0, len(payload.Models))
	for _, entry := range payload.Models {
		slugs = append(slugs, entry.Slug)
	}
	if want := []string{"claude-opus-5", "coding-alias", "gpt-5.5"}; !slices.Equal(slugs, want) {
		t.Fatalf("Codex Responses models = %v, want %v", slugs, want)
	}
}

func TestProxyGemini_ListModelsHandlers(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	createModelConfig := func(t testing.TB, name, _ string, _ []string, priority int, modelName string) {
		t.Helper()
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:     name,
			URLs:     model.ChannelURLs{{URL: "https://example.com"}},
			Priority: priority,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: modelName},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig %s failed: %v", name, err)
		}
	}

	assertOpenAIModelListed := func(t testing.TB, req *http.Request, wantID, failurePrefix string) {
		t.Helper()
		c, w := newTestContext(t, req)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		for _, item := range resp.Data {
			if item.ID == wantID {
				return
			}
		}
		t.Fatalf("%s, got %+v", failurePrefix, resp.Data)
	}

	assertGeminiModelListed := func(t testing.TB, wantName, failurePrefix string) {
		t.Helper()
		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1beta/models", nil))

		server.handleListGeminiModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		for _, item := range resp.Models {
			if item.Name == wantName {
				return
			}
		}
		t.Fatalf("%s, got %+v", failurePrefix, resp.Models)
	}

	_, err := store.CreateConfig(ctx, &model.Config{
		Name:     "g1",
		URLs:     model.ChannelURLs{{URL: "https://example.com"}},
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "gemini-2.5-flash-20250101"},
			{Model: "gemini-1.5-pro"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig gemini failed: %v", err)
	}
	_, err = store.CreateConfig(ctx, &model.Config{
		Name:     "o1",
		URLs:     model.ChannelURLs{{URL: "https://example.com"}},
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig openai failed: %v", err)
	}

	t.Run("handleListGeminiModels", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1beta/models", nil))

		server.handleListGeminiModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Models []struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"models"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if len(resp.Models) != 3 {
			t.Fatalf("models len=%d, want all 3 enabled models", len(resp.Models))
		}
		seen := make(map[string]bool, len(resp.Models))
		for _, m := range resp.Models {
			if m.Name == "" || m.DisplayName == "" {
				t.Fatalf("bad model entry: %+v", m)
			}
			if m.Name[:7] != "models/" {
				t.Fatalf("expected gemini name prefix models/, got %q", m.Name)
			}
			seen[m.Name] = true
		}
		for _, want := range []string{"models/gemini-1.5-pro", "models/gemini-2.5-flash-20250101", "models/gpt-4o"} {
			if !seen[want] {
				t.Fatalf("missing model %q: %+v", want, resp.Models)
			}
		}
	})

	t.Run("handleListOpenAIModels", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1/models", nil))

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.Object != "list" || len(resp.Data) != 3 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		seen := make(map[string]bool, len(resp.Data))
		for _, item := range resp.Data {
			seen[item.ID] = true
		}
		for _, want := range []string{"gemini-1.5-pro", "gemini-2.5-flash-20250101", "gpt-4o"} {
			if !seen[want] {
				t.Fatalf("missing model %q: %+v", want, resp)
			}
		}
	})

	t.Run("handleListOpenAIModels filters by token allowed models", func(t *testing.T) {
		server.authService = newTestAuthService(t)
		tokenHash := model.HashToken("restricted-openai-token")
		server.authService.authTokensMux.Lock()
		server.authService.authTokenModels[tokenHash] = []string{"gpt-4o"}
		server.authService.authTokensMux.Unlock()

		_, err := store.CreateConfig(ctx, &model.Config{
			Name:     "o2",
			URLs:     model.ChannelURLs{{URL: "https://example.com"}},
			Priority: 2,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-5"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig openai extra failed: %v", err)
		}

		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1/models", nil))
		c.Set("token_hash", tokenHash)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
			t.Fatalf("unexpected filtered resp: %+v", resp)
		}
	})

	t.Run("handleListOpenAIModels hides models only provided by denied channels", func(t *testing.T) {
		server.authService = newTestAuthService(t)

		_, err := store.CreateConfig(ctx, &model.Config{
			Name:     "allowed-model-list-channel",
			URLs:     model.ChannelURLs{{URL: "https://example.com"}},
			Priority: 3,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-allowed-channel"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig allowed channel failed: %v", err)
		}
		denied, err := store.CreateConfig(ctx, &model.Config{
			Name:     "disallowed-model-list-channel",
			URLs:     model.ChannelURLs{{URL: "https://example.com"}},
			Priority: 4,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-disallowed-channel"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig disallowed channel failed: %v", err)
		}

		tokenHash := model.HashToken("channel-restricted-openai-token")
		server.authService.authTokensMux.Lock()
		server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeDeny, denied.ID)
		server.authService.authTokensMux.Unlock()

		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1/models", nil))
		c.Set("token_hash", tokenHash)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		visible := make(map[string]bool, len(resp.Data))
		for _, item := range resp.Data {
			visible[item.ID] = true
		}
		if visible["gpt-disallowed-channel"] {
			t.Fatalf("model provided only by denied channel is visible: %+v", resp)
		}
		if !visible["gpt-allowed-channel"] {
			t.Fatalf("model provided by allowed channel is missing: %+v", resp)
		}
	})

	t.Run("handleListOpenAIModels includes transformed gemini channel", func(t *testing.T) {
		createModelConfig(t, "g2-oai", "gemini", []string{"openai"}, 3, "gemini-2.5-pro")
		assertOpenAIModelListed(t, newRequest(http.MethodGet, "/v1/models", nil), "gemini-2.5-pro", "expected transformed gemini model in openai model list")
	})

	t.Run("handleListOpenAIModels returns codex view for openai upstream with codex transform", func(t *testing.T) {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:     "o2-codex",
			URLs:     model.ChannelURLs{{URL: "https://example.com"}},
			Priority: 3,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-5-codex"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig openai->codex failed: %v", err)
		}

		req := newRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("User-Agent", "codex-cli/1.0")
		c, w := newTestContext(t, req)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID                string `json:"id"`
				MultiAgentVersion string `json:"multi_agent_version"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		found := false
		for _, item := range resp.Data {
			if item.ID == "gpt-5-codex" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected codex-exposed openai model in list, got %+v", resp.Data)
		}
		for _, item := range resp.Data {
			if item.ID == "gpt-5-codex" && item.MultiAgentVersion != "v2" {
				t.Fatalf("multi_agent_version = %q, want v2", item.MultiAgentVersion)
			}
		}
	})

	t.Run("handleListOpenAIModels returns openai view for codex upstream with openai transform", func(t *testing.T) {
		createModelConfig(t, "c2-openai", "codex", []string{"openai"}, 3, "gpt-4o")
		assertOpenAIModelListed(t, newRequest(http.MethodGet, "/v1/models", nil), "gpt-4o", "expected openai-exposed codex model in list")
	})

	t.Run("handleListGeminiModels exposes openai channels that declare gemini transform", func(t *testing.T) {
		createModelConfig(t, "o2-gemini", "openai", []string{"gemini"}, 4, "gpt-4.1")
		assertGeminiModelListed(t, "models/gpt-4.1", "expected transformed openai gemini model in list")
	})

	t.Run("handleListOpenAIModels returns anthropic style for anthropic view", func(t *testing.T) {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:     "g3-anthropic",
			URLs:     model.ChannelURLs{{URL: "https://example.com"}},
			Priority: 5,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "claude-3-5-sonnet"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig transformed anthropic failed: %v", err)
		}

		req := newRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("anthropic-version", "2023-06-01")
		c, w := newTestContext(t, req)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				Type        string `json:"type"`
				CreatedAt   string `json:"created_at"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.HasMore {
			t.Fatalf("expected has_more=false, got true")
		}
		if len(resp.Data) == 0 {
			t.Fatalf("expected anthropic models, got empty response")
		}
		found := false
		for _, item := range resp.Data {
			if item.ID == "claude-3-5-sonnet" {
				found = true
				if item.Type != "model" {
					t.Fatalf("expected anthropic type=model, got %q", item.Type)
				}
				if item.DisplayName == "" || item.CreatedAt == "" {
					t.Fatalf("expected anthropic display_name/created_at, got %+v", item)
				}
				break
			}
		}
		if !found {
			t.Fatalf("expected transformed anthropic model in anthropic view, got %+v", resp.Data)
		}
		if resp.FirstID == "" || resp.LastID == "" {
			t.Fatalf("expected anthropic pagination ids, got first=%q last=%q", resp.FirstID, resp.LastID)
		}
	})

	t.Run("handleListOpenAIModels keeps openai shape for codex view", func(t *testing.T) {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:     "c1",
			URLs:     model.ChannelURLs{{URL: "https://example.com"}},
			Priority: 6,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-5-codex"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig codex failed: %v", err)
		}

		req := newRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("User-Agent", "codex-cli/1.0")
		c, w := newTestContext(t, req)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.Object != "list" {
			t.Fatalf("expected openai-style list object for codex view, got %+v", resp)
		}
		found := false
		for _, item := range resp.Data {
			if item.ID == "gpt-5-codex" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected codex model in openai-shaped view, got %+v", resp.Data)
		}
	})

	t.Run("handleListGeminiModels filters by token allowed models", func(t *testing.T) {
		server.authService = newTestAuthService(t)
		tokenHash := model.HashToken("restricted-gemini-token")
		server.authService.authTokensMux.Lock()
		server.authService.authTokenModels[tokenHash] = []string{"gemini-1.5-pro"}
		server.authService.authTokensMux.Unlock()

		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1beta/models", nil))
		c.Set("token_hash", tokenHash)

		server.handleListGeminiModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if len(resp.Models) != 1 || resp.Models[0].Name != "models/gemini-1.5-pro" {
			t.Fatalf("unexpected filtered resp: %+v", resp)
		}
	})
}
