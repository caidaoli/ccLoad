package accioauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Model preserves the catalog's modelName and declared capabilities.
type Model struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	Protocol       string   `json:"protocol,omitempty"`
	Efforts        []string `json:"efforts,omitempty"`
	ContextWindow  int64    `json:"context_window,omitempty"`
	SupportsImages bool     `json:"supports_images"`
}

// FetchModels returns the account's visible chat models without a static fallback.
func (s *Service) FetchModels(ctx context.Context, credential *Credential) ([]Model, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	data, err := s.authDo(ctx, c, http.MethodPost, "/api/llm/config", map[string]any{"token": c.AccessToken, "supportAutoModel": true})
	if err != nil {
		return nil, err
	}
	root := data["data"]
	if len(root) == 0 {
		root, _ = json.Marshal(data)
	}
	var providers []struct {
		Models []json.RawMessage `json:"modelList"`
	}
	if json.Unmarshal(root, &providers) != nil {
		var catalog struct {
			Providers []struct {
				Models []json.RawMessage `json:"modelList"`
			} `json:"providers"`
		}
		if json.Unmarshal(root, &catalog) != nil || catalog.Providers == nil {
			return nil, errors.New("invalid Accio model catalog")
		}
		providers = catalog.Providers
	}
	if providers == nil {
		return nil, errors.New("invalid Accio model catalog")
	}
	models := make([]Model, 0)
	seen := make(map[string]bool)
	for _, provider := range providers {
		for _, raw := range provider.Models {
			var entry struct {
				NativeName    string   `json:"modelName"`
				Name          string   `json:"modelDisplayName"`
				Protocol      string   `json:"protocol"`
				Visible       *bool    `json:"visible"`
				Operations    []string `json:"supportedOperationList"`
				Efforts       []string `json:"reasoningEfforts"`
				ContextWindow int64    `json:"contextWindow"`
				Multimodal    bool     `json:"multimodal"`
			}
			if json.Unmarshal(raw, &entry) != nil {
				return nil, errors.New("invalid Accio model entry")
			}
			if entry.Visible != nil && !*entry.Visible {
				continue
			}
			blocked := false
			for _, operation := range entry.Operations {
				switch strings.ToUpper(operation) {
				case "VIDEO_GENERATION", "IMAGE_GENERATION", "AUDIO_GENERATION":
					blocked = true
				}
			}
			if blocked {
				continue
			}
			id := strings.TrimSpace(entry.NativeName)
			if id == "" || seen[strings.ToLower(id)] {
				continue
			}
			seen[strings.ToLower(id)] = true
			models = append(models, Model{ID: id, Name: strings.TrimSpace(entry.Name), Protocol: strings.ToLower(strings.TrimSpace(entry.Protocol)), Efforts: entry.Efforts, ContextWindow: entry.ContextWindow, SupportsImages: entry.Multimodal})
		}
	}
	if len(models) == 0 {
		return nil, errors.New("accio catalog contains no visible chat models")
	}
	return models, nil
}
