package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/model/catalog"
)

type OpenRouterProvider struct {
	*OpenAIProvider
}

func NewOpenRouterProvider(cfg config.ProviderConfig) *OpenRouterProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://openrouter.ai/api/v1"
	}
	cfg.Type = "openrouter"
	return &OpenRouterProvider{
		OpenAIProvider: &OpenAIProvider{
			config:   cfg,
			client:   NewResilientClient(cfg),
			provType: "openrouter",
		},
	}
}

func (p *OpenRouterProvider) Type() string {
	return "openrouter"
}

func (p *OpenRouterProvider) TransformRequest(ctx context.Context, req *model.ChatCompletionRequest) (*http.Request, error) {
	httpReq, err := p.OpenAIProvider.TransformRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("HTTP-Referer", "https://github.com/ilter-ai/ilter")
	httpReq.Header.Set("X-Title", "ILTER Gateway")
	return httpReq, nil
}

func (p *OpenRouterProvider) DiscoverModels(ctx context.Context) ([]catalog.ModelInfo, error) {
	if len(p.config.GetAPIKeys()) == 0 {
		slog.Debug("skipping model discovery, no credentials configured",
			"provider", p.config.Name, "type", "openrouter")
		return nil, nil
	}

	url := fmt.Sprintf("%s/models", p.config.BaseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	for k, v := range p.config.Headers {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to discover models, status %d: %s", resp.StatusCode, strings.ReplaceAll(strings.ReplaceAll(string(bodyBytes), "\n", " "), "\r", ""))
	}

	var orResp openRouterModelsResponse
	if err := json.Unmarshal(bodyBytes, &orResp); err != nil {
		return nil, err
	}

	var models []catalog.ModelInfo
	for _, entry := range orResp.Data {
		if entry.ID == "" {
			continue
		}
		models = append(models, openRouterModelInfoFromEntry(entry, p.config.BaseURL))
	}
	return models, nil
}

// openRouterModelInfoFromEntry converts one OpenRouter /models catalog entry
// into a catalog.ModelInfo, estimating tier from price-per-token and
// capabilities from the entry's supported_parameters and ID naming.
func openRouterModelInfoFromEntry(entry openRouterModelEntry, baseURL string) catalog.ModelInfo {
	costIn := parseOpenRouterPrice(entry.Pricing.Prompt)
	costOut := parseOpenRouterPrice(entry.Pricing.Completion)

	tier := "standard"
	switch {
	case costIn == 0 && costOut == 0:
		tier = "free"
	case costIn >= 0.000005:
		tier = "premium"
	case costIn < 0.0000005:
		tier = "economy"
	}

	ctxLen := entry.ContextLength
	if ctxLen == 0 {
		ctxLen = 128000
	}

	return catalog.ModelInfo{
		ID:                 entry.ID,
		Provider:           "openrouter",
		DisplayName:        entry.Name,
		MaxContextTokens:   ctxLen,
		MaxOutputTokens:    4096,
		CostPerInputToken:  costIn,
		CostPerOutputToken: costOut,
		Tier:               tier,
		Capabilities:       openRouterCapabilities(entry),
		DefaultBaseURL:     baseURL,
	}
}

// openRouterCapabilities derives capability tags for an entry from its
// supported_parameters (tool calling) and naming conventions in its ID
// (vision, JSON mode).
func openRouterCapabilities(entry openRouterModelEntry) []string {
	var caps []string
	hasTools := false
	for _, param := range entry.SupportedParameters {
		if param == "tools" || param == "tool_choice" {
			hasTools = true
			break
		}
	}
	if hasTools {
		caps = append(caps, "function_calling")
	}
	idLower := strings.ToLower(entry.ID)
	if strings.Contains(idLower, "vision") || strings.Contains(idLower, "multimodal") || strings.Contains(idLower, "gemini") || strings.Contains(idLower, "claude-3-5") {
		caps = append(caps, "vision")
	}
	if strings.Contains(idLower, "gpt") || strings.Contains(idLower, "claude") {
		caps = append(caps, "json_mode")
	}
	return caps
}
