package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/model"
)

func TestExtractRequestedChannelFilter(t *testing.T) {
	tests := []struct {
		name       string
		reqURL     string
		headers    map[string]string
		wantFilter bool
		wantID     int64
		wantName   string
	}{
		{
			name:       "no filter",
			reqURL:     "/v1/chat/completions",
			wantFilter: false,
		},
		{
			name:       "query channel_id ignored",
			reqURL:     "/v1/chat/completions?channel_id=364",
			wantFilter: false,
		},
		{
			name:       "query channel ignored",
			reqURL:     "/v1/chat/completions?channel=openai-backup",
			wantFilter: false,
		},
		{
			name:       "header x-ccload-channel-id lowercase",
			reqURL:     "/v1/chat/completions",
			headers:    map[string]string{"x-ccload-channel-id": "364"},
			wantFilter: true,
			wantID:     364,
		},
		{
			name:       "header X-CCLoad-Channel-ID canonical",
			reqURL:     "/v1/chat/completions",
			headers:    map[string]string{"X-CCLoad-Channel-ID": "128"},
			wantFilter: true,
			wantID:     128,
		},
		{
			name:       "header x-ccload-channel lowercase by name",
			reqURL:     "/v1/chat/completions",
			headers:    map[string]string{"x-ccload-channel": "[公益] https://image.mlgb7.com"},
			wantFilter: true,
			wantName:   "[公益] https://image.mlgb7.com",
		},
		{
			name:       "header x-ccload-channel numeric id fallback",
			reqURL:     "/v1/chat/completions",
			headers:    map[string]string{"x-ccload-channel": "364"},
			wantFilter: true,
			wantID:     364,
		},
		{
			name:       "header X-CCLoad-Channel canonical by name",
			reqURL:     "/v1/chat/completions",
			headers:    map[string]string{"X-CCLoad-Channel": "custom-ch"},
			wantFilter: true,
			wantName:   "custom-ch",
		},
		{
			name:       "unprefixed X-Channel-ID ignored",
			reqURL:     "/v1/chat/completions",
			headers:    map[string]string{"X-Channel-ID": "364"},
			wantFilter: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.reqURL, nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			got := extractRequestedChannelFilter(req)
			if got.hasFilter != tt.wantFilter {
				t.Fatalf("hasFilter = %v, want %v", got.hasFilter, tt.wantFilter)
			}
			if got.channelID != tt.wantID {
				t.Fatalf("channelID = %v, want %v", got.channelID, tt.wantID)
			}
			if got.channelName != tt.wantName {
				t.Fatalf("channelName = %v, want %v", got.channelName, tt.wantName)
			}
		})
	}
}

func TestFilterByRequestedChannel(t *testing.T) {
	c1 := &model.Config{ID: 10, Name: "channel-10"}
	c2 := &model.Config{ID: 20, Name: "channel-20"}
	c3 := &model.Config{ID: 364, Name: "[公益] https://image.mlgb7.com"}
	cands := []*model.Config{c1, c2, c3}

	t.Run("filter by id matching", func(t *testing.T) {
		res, applied := filterByRequestedChannel(cands, requestedChannelFilter{hasFilter: true, channelID: 364})
		if !applied || len(res) != 1 || res[0].ID != 364 {
			t.Fatalf("expected channel 364, got %+v (applied=%v)", res, applied)
		}
	})

	t.Run("filter by id not found", func(t *testing.T) {
		res, applied := filterByRequestedChannel(cands, requestedChannelFilter{hasFilter: true, channelID: 999})
		if !applied || len(res) != 0 {
			t.Fatalf("expected empty result, got %+v (applied=%v)", res, applied)
		}
	})

	t.Run("filter by name matching exact", func(t *testing.T) {
		res, applied := filterByRequestedChannel(cands, requestedChannelFilter{hasFilter: true, channelName: "[公益] https://image.mlgb7.com"})
		if !applied || len(res) != 1 || res[0].ID != 364 {
			t.Fatalf("expected channel 364, got %+v", res)
		}
	})

	t.Run("filter by name matching case-insensitive", func(t *testing.T) {
		res, applied := filterByRequestedChannel(cands, requestedChannelFilter{hasFilter: true, channelName: "CHANNEL-20"})
		if !applied || len(res) != 1 || res[0].ID != 20 {
			t.Fatalf("expected channel 20, got %+v", res)
		}
	})

	t.Run("no filter returns all", func(t *testing.T) {
		res, applied := filterByRequestedChannel(cands, requestedChannelFilter{hasFilter: false})
		if applied || len(res) != 3 {
			t.Fatalf("expected 3 channels and applied=false, got len=%d applied=%v", len(res), applied)
		}
	})
}
