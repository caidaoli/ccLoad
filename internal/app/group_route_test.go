package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
)

func TestSelectGroupRouteCandidates_ModelCooldownDoesNotBlockSiblingModel(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	channel := createGroupRouteTestChannel(t, srv, "group-channel", "https://api.example.com", []string{"gpt-5.5", "gpt-5.4"})
	group, err := srv.store.CreateGroup(ctx, &model.Group{
		Name: "gpt-5",
		Mode: model.GroupModeFailover,
		Items: []model.GroupItem{
			{ChannelID: channel.ID, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
			{ChannelID: channel.ID, ModelName: "gpt-5.4", Priority: 2, Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	if err := srv.store.SetChannelCooldown(ctx, channel.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetChannelCooldown failed: %v", err)
	}
	if _, err := srv.store.BumpModelCooldown(ctx, channel.ID, "gpt-5.5", time.Now(), http.StatusServiceUnavailable); err != nil {
		t.Fatalf("BumpModelCooldown failed: %v", err)
	}

	candidates, err := srv.selectGroupRouteCandidates(ctx, group, "openai")
	if err != nil {
		t.Fatalf("selectGroupRouteCandidates failed: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Item.ModelName != "gpt-5.4" {
		t.Fatalf("expected surviving candidate gpt-5.4, got %+v", candidates[0].Item)
	}
}

func TestSelectGroupRouteCandidates_PrefersStickyChannel(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := withGroupRouteSession(context.Background(), "sticky-token", "gpt-5")

	channelA := createGroupRouteTestChannel(t, srv, "sticky-a", "https://sticky-a.example.com", []string{"gpt-5.4"})
	channelB := createGroupRouteTestChannel(t, srv, "sticky-b", "https://sticky-b.example.com", []string{"gpt-5.4"})
	group, err := srv.store.CreateGroup(context.Background(), &model.Group{
		Name:            "gpt-5",
		Mode:            model.GroupModeRoundRobin,
		SessionKeepTime: 60,
		Items: []model.GroupItem{
			{ChannelID: channelA.ID, ModelName: "gpt-5.4", Priority: 1, Weight: 1},
			{ChannelID: channelB.ID, ModelName: "gpt-5.4", Priority: 2, Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	setGroupStickySession("sticky-token", "gpt-5", channelB.ID)

	candidates, err := srv.selectGroupRouteCandidates(ctx, group, "openai")
	if err != nil {
		t.Fatalf("selectGroupRouteCandidates failed: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Config.ID != channelB.ID {
		t.Fatalf("expected sticky channel %d first, got %+v", channelB.ID, candidates[0])
	}
}

func TestHandleProxyRequest_GroupRouteRetriesSiblingModelOnSameChannel(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	var (
		mu         sync.Mutex
		seenModels []string
	)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()

		var payload struct {
			Model string `json:"model"`
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal request failed: %v", err)
		}

		mu.Lock()
		seenModels = append(seenModels, payload.Model)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if payload.Model == "gpt-5.5" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"gpt-5.5 failed"}`))
			return
		}

		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))

	channel := createGroupRouteTestChannel(t, srv, "same-channel", upstream.URL, []string{"gpt-5.5", "gpt-5.4"})
	if err := srv.store.SetChannelCooldown(ctx, channel.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetChannelCooldown failed: %v", err)
	}

	_, err := srv.store.CreateGroup(ctx, &model.Group{
		Name: "gpt-5",
		Mode: model.GroupModeFailover,
		Items: []model.GroupItem{
			{ChannelID: channel.ID, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
			{ChannelID: channel.ID, ModelName: "gpt-5.4", Priority: 2, Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	req := newJSONRequestBytes(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"gpt-5","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	c, w := newTestContext(t, req)
	srv.HandleProxyRequest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", w.Code, w.Body.String())
	}

	mu.Lock()
	firstSeen := append([]string(nil), seenModels...)
	seenModels = nil
	mu.Unlock()
	if len(firstSeen) != 2 || firstSeen[0] != "gpt-5.5" || firstSeen[1] != "gpt-5.4" {
		t.Fatalf("unexpected first attempt models: %+v", firstSeen)
	}

	modelCooldowns, err := srv.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns failed: %v", err)
	}
	if modelCooldowns[channel.ID] == nil || !modelCooldowns[channel.ID]["gpt-5.5"].After(time.Now()) {
		t.Fatalf("expected gpt-5.5 model cooldown, got %+v", modelCooldowns)
	}

	channelCooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	if !channelCooldowns[channel.ID].After(time.Now()) {
		t.Fatalf("expected channel cooldown to stay untouched for group route, got %+v", channelCooldowns)
	}

	req = newJSONRequestBytes(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"gpt-5","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	c, w = newTestContext(t, req)
	srv.HandleProxyRequest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", w.Code, w.Body.String())
	}

	mu.Lock()
	secondSeen := append([]string(nil), seenModels...)
	mu.Unlock()
	if len(secondSeen) != 1 || secondSeen[0] != "gpt-5.4" {
		t.Fatalf("expected second request to use only gpt-5.4, got %+v", secondSeen)
	}
}

func TestHandleProxyErrorResponse_GroupActualModelCoolsModelOnly(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()
	channel := createGroupRouteTestChannel(t, srv, "error-channel", "https://api.example.com", []string{"gpt-5.5"})

	reqCtx := &proxyRequestContext{
		originalModel:    "gpt-5",
		groupActualModel: "gpt-5.5",
		startTime:        time.Now(),
		attemptStartTime: time.Now(),
	}
	res := &fwResult{
		Status: http.StatusServiceUnavailable,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(`{"error":"boom"}`),
	}

	_, action := srv.handleProxyErrorResponse(ctx, channel, 0, "gpt-5.5", "sk-group-route", res, 0.1, reqCtx, false)
	if action != cooldown.ActionRetryChannel {
		t.Fatalf("expected retry channel action, got %v", action)
	}

	modelCooldowns, err := srv.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns failed: %v", err)
	}
	if modelCooldowns[channel.ID] == nil || !modelCooldowns[channel.ID]["gpt-5.5"].After(time.Now()) {
		t.Fatalf("expected gpt-5.5 model cooldown, got %+v", modelCooldowns)
	}

	channelCooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	if _, exists := channelCooldowns[channel.ID]; exists {
		t.Fatalf("channel cooldown should stay untouched, got %+v", channelCooldowns)
	}
}

func TestHandleProxySuccess_GroupActualModelClearsModelCooldownWithoutTouchingChannelCooldown(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()
	channel := createGroupRouteTestChannel(t, srv, "success-channel", "https://api.example.com", []string{"gpt-5.5"})

	if _, err := srv.store.BumpModelCooldown(ctx, channel.ID, "gpt-5.5", time.Now(), http.StatusServiceUnavailable); err != nil {
		t.Fatalf("BumpModelCooldown failed: %v", err)
	}
	channelUntil := time.Now().Add(time.Hour)
	if err := srv.store.SetChannelCooldown(ctx, channel.ID, channelUntil); err != nil {
		t.Fatalf("SetChannelCooldown failed: %v", err)
	}

	reqCtx := &proxyRequestContext{
		originalModel:    "gpt-5",
		groupActualModel: "gpt-5.5",
		startTime:        time.Now(),
		attemptStartTime: time.Now(),
	}
	res := &fwResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(`{"id":"ok"}`),
	}

	_, action := srv.handleProxySuccess(ctx, channel, 0, "gpt-5.5", "sk-group-route", res, 0.1, reqCtx)
	if action != cooldown.ActionReturnClient {
		t.Fatalf("expected return client action, got %v", action)
	}

	modelCooldowns, err := srv.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns failed: %v", err)
	}
	if channelModels := modelCooldowns[channel.ID]; channelModels != nil {
		if _, exists := channelModels["gpt-5.5"]; exists {
			t.Fatalf("expected gpt-5.5 model cooldown cleared, got %+v", modelCooldowns)
		}
	}

	channelCooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	if !channelCooldowns[channel.ID].After(time.Now()) {
		t.Fatalf("expected channel cooldown to remain, got %+v", channelCooldowns)
	}
}

func TestHandleProxyRequest_GroupRouteHonorsFirstTokenTimeout(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	var (
		mu         sync.Mutex
		seenModels []string
	)
	slowUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		time.Sleep(1200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"slow"}`))
	}))
	fastUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()

		var payload struct {
			Model string `json:"model"`
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal request failed: %v", err)
		}

		mu.Lock()
		seenModels = append(seenModels, payload.Model)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fast"}`))
	}))

	slowChannel := createGroupRouteTestChannel(t, srv, "slow-channel", slowUpstream.URL, []string{"gpt-5.5"})
	fastChannel := createGroupRouteTestChannel(t, srv, "fast-channel", fastUpstream.URL, []string{"gpt-5.4"})
	_, err := srv.store.CreateGroup(ctx, &model.Group{
		Name:              "gpt-5",
		Mode:              model.GroupModeFailover,
		FirstTokenTimeOut: 1,
		Items: []model.GroupItem{
			{ChannelID: slowChannel.ID, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
			{ChannelID: fastChannel.ID, ModelName: "gpt-5.4", Priority: 2, Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	req := newJSONRequestBytes(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"gpt-5","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	c, w := newTestContext(t, req)
	start := time.Now()
	srv.HandleProxyRequest(c)
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if elapsed < time.Second || elapsed > 2500*time.Millisecond {
		t.Fatalf("expected request to switch after group first token timeout, elapsed=%v", elapsed)
	}

	modelCooldowns, err := srv.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns failed: %v", err)
	}
	if modelCooldowns[slowChannel.ID] == nil || !modelCooldowns[slowChannel.ID]["gpt-5.5"].After(time.Now()) {
		t.Fatalf("expected slow model cooldown after timeout, got %+v", modelCooldowns)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenModels) != 1 || seenModels[0] != "gpt-5.4" {
		t.Fatalf("expected only fast upstream to finish successfully, got %+v", seenModels)
	}
}

func TestHandleProxyRequest_GroupSessionKeepTimeSticksToSuccessfulChannel(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	upstreamA := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"channel":"A"}`))
	}))
	upstreamB := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"channel":"B"}`))
	}))

	channelA := createGroupRouteTestChannel(t, srv, "sticky-a", upstreamA.URL, []string{"gpt-5.4"})
	channelB := createGroupRouteTestChannel(t, srv, "sticky-b", upstreamB.URL, []string{"gpt-5.4"})
	_, err := srv.store.CreateGroup(ctx, &model.Group{
		Name:            "gpt-5",
		Mode:            model.GroupModeRoundRobin,
		SessionKeepTime: 60,
		Items: []model.GroupItem{
			{ChannelID: channelA.ID, ModelName: "gpt-5.4", Priority: 1, Weight: 1},
			{ChannelID: channelB.ID, ModelName: "gpt-5.4", Priority: 2, Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	doRequest := func() string {
		req := newJSONRequestBytes(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"gpt-5","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		c, w := newTestContext(t, req)
		c.Set("token_hash", "sticky-token")
		srv.HandleProxyRequest(c)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	firstBody := doRequest()
	secondBody := doRequest()
	if firstBody != secondBody {
		t.Fatalf("expected sticky session to reuse same channel, first=%s second=%s", firstBody, secondBody)
	}
}

func createGroupRouteTestChannel(t testing.TB, srv *Server, name string, upstreamURL string, models []string) *model.Config {
	t.Helper()

	ctx := context.Background()
	entries := make([]model.ModelEntry, 0, len(models))
	for _, modelName := range models {
		entries = append(entries, model.ModelEntry{Model: modelName})
	}

	channel, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         name,
		URL:          upstreamURL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: entries,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   channel.ID,
		KeyIndex:    0,
		APIKey:      "sk-group-route",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	return channel
}
