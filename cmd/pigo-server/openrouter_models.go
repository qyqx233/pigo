package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultOpenRouterModelsURL = "https://openrouter.ai/api/v1/models"

type openRouterModelsResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

// fetchOpenRouterFreeModels reads OpenRouter's live catalog. The models endpoint
// is public, so provider credentials are deliberately not attached to this
// request. Only free text-generation models suitable for the chat UI are kept.
func (s *apiServer) fetchOpenRouterFreeModels(r *http.Request) ([]modelResponse, error) {
	client := s.modelHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	url := s.openRouterModelsURL
	if strings.TrimSpace(url) == "" {
		url = defaultOpenRouterModelsURL
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pigo-server")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, fmt.Errorf("OpenRouter returned %s", response.Status)
	}

	var payload openRouterModelsResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode OpenRouter models: %w", err)
	}
	models := make([]modelResponse, 0, len(payload.Data))
	// Every model's window, not just the listed ones: a session may use a
	// model the free list no longer shows.
	windows := make(map[string]int, len(payload.Data))
	for _, model := range payload.Data {
		if id := strings.TrimSpace(model.ID); id != "" && model.ContextLength > 0 {
			windows[id] = model.ContextLength
		}
	}
	s.openRouterWindows.store(windows)
	for _, model := range payload.Data {
		if !zeroPrice(model.Pricing.Prompt) || !zeroPrice(model.Pricing.Completion) {
			continue
		}
		if !textOnlyOutput(model.Architecture.OutputModalities) || isSafetyClassifier(model) {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		label := strings.TrimSpace(model.Name)
		if label == "" {
			label = id
		}
		models = append(models, modelResponse{ID: id, Label: label, Provider: "openrouter", ContextWindow: model.ContextLength})
	}
	sort.Slice(models, func(i, j int) bool {
		left, right := strings.ToLower(models[i].Label), strings.ToLower(models[j].Label)
		if left == right {
			return models[i].ID < models[j].ID
		}
		return left < right
	})
	return models, nil
}

func zeroPrice(value string) bool {
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return err == nil && n == 0
}

func textOnlyOutput(values []string) bool {
	return len(values) == 1 && strings.EqualFold(values[0], "text")
}

func isSafetyClassifier(model openRouterModel) bool {
	value := strings.ToLower(model.ID + " " + model.Name)
	for _, marker := range []string{"content-safety", "moderation", "llama-guard", "shieldgemma"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
