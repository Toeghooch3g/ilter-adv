package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/model/catalog"
)

// CalculateCost returns the dollar cost for a request, billing cached-input
// (cache-read) tokens at CostPerCachedInputToken and Anthropic cache-write
// tokens at CostPerCacheWriteToken when those prices are configured. The
// 6-decimal rounding protects the SQLite REAL column from float64
// representation noise (PRD 04-IMPLEMENTATION-PLAN.md Sprint 2.2 floating
// point risk). u may be nil (cost 0).
//
// Token semantics: OpenAI/DeepSeek report prompt_tokens INCLUDING the cached
// portion (CacheReadIncludedInPrompt=true), so cached tokens are a subset of
// prompt tokens and only the non-cached remainder bills at the input rate.
// Anthropic reports input_tokens EXCLUDING cache (CacheReadIncludedInPrompt=
// false), so the cache-read and cache-write portions bill at their own rates
// on top of the full input count.
func CalculateCost(m config.ModelConfig, u *model.Usage) float64 {
	if u == nil {
		return 0
	}
	promptTokens := u.PromptTokens
	cachedTokens := 0
	if u.PromptTokensDetails != nil {
		cachedTokens = u.PromptTokensDetails.CachedTokens
	}
	inputTokens := promptTokens
	if u.CacheReadIncludedInPrompt {
		inputTokens = promptTokens - cachedTokens
	}

	inputCost := float64(inputTokens) * m.CostPerInputToken
	cachedCost := float64(cachedTokens) * m.CostPerCachedInputToken
	writeCost := float64(u.CacheCreationInputTokens) * m.CostPerCacheWriteToken
	outputCost := float64(u.CompletionTokens) * m.CostPerOutputToken
	return math.Round((inputCost+cachedCost+writeCost+outputCost)*1e6) / 1e6
}

// countContentWords counts words in a Message.Content value, which may be
// a plain string or a multi-part []any content array (text/image blocks).
func countContentWords(content any) int {
	switch v := content.(type) {
	case string:
		return len(strings.Fields(v))
	case []any:
		count := 0
		for _, item := range v {
			if s, ok := item.(string); ok {
				count += len(strings.Fields(s))
			} else if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					count += len(strings.Fields(text))
				}
			}
		}
		return count
	default:
		return 0
	}
}

func estimateInputTokens(messages []model.Message) int {
	var wordCount int
	for _, msg := range messages {
		if msg.Content == nil {
			continue
		}
		wordCount += countContentWords(msg.Content)
	}
	if wordCount < 4 {
		wordCount = 4
	}
	return int(float64(wordCount) * 1.3)
}

// normalizeTier treats "free" as "economy" for alternative-cost matching.
func normalizeTier(tier string) string {
	if tier == "free" {
		return "economy"
	}
	return tier
}

// cheapestCostInTier scans catalog.Models (caller must hold ModelsMu) for
// the cheapest model in targetTier other than excludeModel, returning
// found=false if none exists.
func cheapestCostInTier(targetTier, excludeModel string, inputTokens, outputTokens int) (cost float64, found bool) {
	for name, infos := range catalog.Models {
		if name == excludeModel || len(infos) == 0 {
			continue
		}
		info := infos[0]
		if normalizeTier(info.Category) != targetTier {
			continue
		}
		c := float64(inputTokens)*info.CostPerInputToken + float64(outputTokens)*info.CostPerOutputToken
		if !found || c < cost {
			cost = c
			found = true
		}
	}
	return cost, found
}

func findCheapestAlternativeCost(selectedModel string, inputTokens, outputTokens int) float64 {
	catalog.ModelsMu.RLock()
	defer catalog.ModelsMu.RUnlock()

	selectedInfos, found := catalog.Models[selectedModel]
	if !found || len(selectedInfos) == 0 {
		return 0
	}
	targetTier := normalizeTier(selectedInfos[0].Category)

	cost, foundCheaper := cheapestCostInTier(targetTier, selectedModel, inputTokens, outputTokens)
	if !foundCheaper {
		return 0
	}
	return math.Round(cost*1e6) / 1e6
}

func providerErrorStatus(err error) int {
	if err == nil {
		return http.StatusBadGateway
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "quota"), strings.Contains(msg, "insufficient"), strings.Contains(msg, "billing"),
		strings.Contains(msg, "limit exceeded"), strings.Contains(msg, "rate limit"), strings.Contains(msg, "credit"),
		strings.Contains(msg, "balance"), strings.Contains(msg, "payment required"), strings.Contains(msg, "funds"), strings.Contains(msg, "429"):
		return http.StatusTooManyRequests
	case strings.Contains(msg, "401"), strings.Contains(msg, "unauthorized"), strings.Contains(msg, "forbidden"), strings.Contains(msg, "403"):
		return http.StatusUnauthorized
	case strings.Contains(msg, "400"), strings.Contains(msg, "invalid_request"):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func sanitizeProviderErrorMessage(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return trimmed
	}

	var envelope struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err == nil {
		if envelope.Error.Message != "" {
			return envelope.Error.Message
		}
		if envelope.Message != "" {
			return envelope.Message
		}
	}

	return trimmed
}

func computeCostEstimates(messages []model.Message, modelCfg config.ModelConfig, selectedModel string, maxTokens *int) (costEstimate, altCost, savingsPotential float64) {
	estInputTokens := estimateInputTokens(messages)
	estOutputTokens := 150
	if maxTokens != nil && *maxTokens > 0 {
		estOutputTokens = *maxTokens
	}

	costEstimate = float64(estInputTokens)*modelCfg.CostPerInputToken +
		float64(estOutputTokens)*modelCfg.CostPerOutputToken
	costEstimate = math.Round(costEstimate*1e6) / 1e6

	altCost = findCheapestAlternativeCost(selectedModel, estInputTokens, estOutputTokens)

	if altCost > 0 && costEstimate > 0 && altCost < costEstimate {
		savingsPotential = (1.0 - altCost/costEstimate) * 100
	}
	return
}
