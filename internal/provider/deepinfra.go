package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model/catalog"
)

// DeepInfraProvider is a native DeepInfra adapter. DeepInfra serves an
// OpenAI-compatible API, so it embeds OpenAIProvider (chat, streaming, and
// embeddings all work for free) and only overrides Type and model discovery.
type DeepInfraProvider struct {
	*OpenAIProvider
}

func NewDeepInfraProvider(cfg config.ProviderConfig) *DeepInfraProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.deepinfra.com/v1/openai"
	}
	cfg.Type = "deepinfra"
	return &DeepInfraProvider{
		OpenAIProvider: &OpenAIProvider{
			config:   cfg,
			client:   NewResilientClient(cfg),
			provType: "deepinfra",
		},
	}
}

func (p *DeepInfraProvider) Type() string {
	return "deepinfra"
}

// DiscoverModels fetches the DeepInfra /models catalog and converts it to
// catalog.ModelInfo. The endpoint serves metadata: pricing (USD per million
// tokens), context_length, max_tokens, and tags ("chat", "embed", "reasoning",
// "vision"/"vlm"). Prices are divided by 1e6 to per-token. Any field that
// goes missing degrades to a zero/fallback value rather than an error, so
// routing still works if the upstream shape drifts.
func (p *DeepInfraProvider) DiscoverModels(ctx context.Context) ([]catalog.ModelInfo, error) {
	if len(p.config.GetAPIKeys()) == 0 && !p.config.DiscoveryPublic {
		slog.Debug("skipping model discovery, no credentials configured",
			"provider", p.config.Name, "type", "deepinfra")
		return nil, nil
	}

	url := fmt.Sprintf("%s/models", p.config.BaseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if key := SelectedAPIKeyFromContext(ctx); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	} else if len(p.config.GetAPIKeys()) > 0 {
		req.Header.Set("Authorization", "Bearer "+p.config.GetAPIKeys()[0])
	}
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

	var diResp deepInfraModelsResponse
	if err := json.Unmarshal(bodyBytes, &diResp); err != nil {
		return nil, err
	}

	models := make([]catalog.ModelInfo, 0, len(diResp.Data))
	for _, entry := range diResp.Data {
		if entry.ID == "" {
			continue
		}
		if info, ok := deepInfraModelInfoFromEntry(entry, p.config.BaseURL, p.modelLabel()); ok {
			models = append(models, info)
		}
	}
	return models, nil
}

// deepInfraModelInfoFromEntry converts one DeepInfra /models entry into a
// catalog.ModelInfo. ok is false when the entry is neither a chat nor an
// embed model (e.g. rerank-only entries, which ilter does not route).
func deepInfraModelInfoFromEntry(entry deepInfraModelEntry, baseURL, provider string) (catalog.ModelInfo, bool) {
	hasChat := slices.Contains(entry.Metadata.Tags, "chat")
	hasEmbed := slices.Contains(entry.Metadata.Tags, "embed")

	// The plan scope covers chat + embeddings (no rerank endpoint). Rerank
	// models (tag "rerank", input-only pricing) are dropped.
	if !hasChat && !hasEmbed {
		return catalog.ModelInfo{}, false
	}

	costIn := entry.Metadata.Pricing.InputTokens / 1e6
	costOut := entry.Metadata.Pricing.OutputTokens / 1e6
	costCacheRead := entry.Metadata.Pricing.CacheReadTokens / 1e6
	costCacheWrite := entry.Metadata.Pricing.CacheWriteTokens / 1e6

	ctxLen := entry.Metadata.ContextLength
	if ctxLen == 0 {
		ctxLen = 128000
	}
	maxOut := entry.Metadata.MaxTokens
	if maxOut == 0 {
		maxOut = 4096
	}

	info := catalog.ModelInfo{
		ID:                      entry.ID,
		Provider:                provider,
		DisplayName:             entry.ID,
		MaxContextTokens:        ctxLen,
		MaxOutputTokens:         maxOut,
		CostPerInputToken:       costIn,
		CostPerOutputToken:      costOut,
		CostPerCachedInputToken: costCacheRead,
		CostPerCacheWriteToken:  costCacheWrite,
		DefaultBaseURL:          baseURL,
	}

	if hasEmbed {
		// Embedding models only price input tokens; the cache embedder routes
		// deepinfra:<model> to OpenAIProvider.Embed.
		info.Category = "economy"
		info.CostPerOutputToken = 0
		return info, true
	}

	// Chat model.
	if slices.Contains(entry.Metadata.Tags, "reasoning") || slices.Contains(entry.Metadata.Tags, "reasoning_effort") {
		info.Category = "premium"
	} else {
		info.Category = "standard"
	}
	caps := []string{"function_calling", "json_mode"}
	if slices.Contains(entry.Metadata.Tags, "vision") || slices.Contains(entry.Metadata.Tags, "vlm") {
		caps = append(caps, "vision")
	}
	info.Capabilities = caps
	return info, true
}

// deepInfraModelEntry mirrors the DeepInfra /models catalog shape. Fields are
// optional by design: absent metadata/pricing degrade to zero values, keeping
// discovery non-fatal if the upstream shape changes.
type deepInfraModelEntry struct {
	ID       string `json:"id"`
	Metadata struct {
		ContextLength int `json:"context_length"`
		MaxTokens     int `json:"max_tokens"`
		Pricing       struct {
			InputTokens      float64 `json:"input_tokens"`
			OutputTokens     float64 `json:"output_tokens"`
			CacheReadTokens  float64 `json:"cache_read_tokens"`
			CacheWriteTokens float64 `json:"cache_write_tokens"`
		} `json:"pricing"`
		Tags []string `json:"tags"`
	} `json:"metadata"`
}

type deepInfraModelsResponse struct {
	Data []deepInfraModelEntry `json:"data"`
}
