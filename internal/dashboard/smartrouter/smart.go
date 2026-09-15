package smartrouter

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/model/catalog"
	"github.com/ilter-ai/ilter/internal/provider"
)

type OptimizeRequest struct {
	Prompt       string `json:"prompt"`
	CurrentModel string `json:"current_model"`
}

type OptimizeResponse struct {
	ComplexityScore     float64          `json:"complexity_score"`
	CurrentCostEstimate float64          `json:"current_cost_estimate"`
	Recommendations     []Recommendation `json:"recommendations"`
}

type Recommendation struct {
	Model          string  `json:"model"`
	EstimatedCost  float64 `json:"estimated_cost"`
	SavingsPercent int     `json:"savings_percent"`
	QualityImpact  string  `json:"quality_impact"`
}

type updateProviderRequest struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	APIKey  *string  `json:"api_key"`  // nil = keep current, "" = clear, "sk-..." = set
	APIKeys []string `json:"api_keys"` // optional list of multi-keys
}

// qualityImpactForScore labels how risky it is to downgrade to an economy/
// free-tier model, given the prompt's complexity score.
func qualityImpactForScore(score float64) string {
	switch {
	case score >= 50:
		return "medium — quality may degrade for complex reasoning tasks"
	case score >= 20:
		return "low — sufficient for standard tasks"
	default:
		return "minimal — sufficient for simple questions"
	}
}

// buildOptimizeRecommendation returns a cheaper-model recommendation for
// mName/mInfo if it's a cheaper economy/free-tier alternative to
// currentModel with a positive estimated saving, or ok=false otherwise.
func buildOptimizeRecommendation(mName string, mInfo catalog.ModelInfo, currentModel string, inputTokens, outputTokens int, currentCostEstimate, score float64) (rec Recommendation, ok bool) {
	if (mInfo.Tier != "economy" && mInfo.Tier != "free") || mName == currentModel {
		return rec, false
	}
	estCost := float64(inputTokens)*mInfo.CostPerInputToken + float64(outputTokens)*mInfo.CostPerOutputToken
	if currentCostEstimate <= 0 || estCost >= currentCostEstimate {
		return rec, false
	}
	savingsPercent := int((1.0 - estCost/currentCostEstimate) * 100)
	if savingsPercent <= 0 {
		return rec, false
	}
	return Recommendation{
		Model:          mName,
		EstimatedCost:  estCost,
		SavingsPercent: savingsPercent,
		QualityImpact:  qualityImpactForScore(score),
	}, true
}

func (h *Handler) HandleOptimize(w http.ResponseWriter, r *http.Request) {
	var req OptimizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	wordCount := len(strings.Fields(req.Prompt))
	inputTokens := int(float64(wordCount) * 1.3)
	outputTokens := 150

	currentInputCost := 0.0000025
	currentOutputCost := 0.00001

	currentModelInfos, foundCurrent := catalog.Models[req.CurrentModel]
	if foundCurrent && len(currentModelInfos) > 0 {
		currentInputCost = currentModelInfos[0].CostPerInputToken
		currentOutputCost = currentModelInfos[0].CostPerOutputToken
	}
	currentCostEstimate := float64(inputTokens)*currentInputCost + float64(outputTokens)*currentOutputCost

	messages := []model.Message{{Role: "user", Content: req.Prompt}}
	score := smartrouter.ScoreComplexity(r.Context(), messages)

	var recommendations []Recommendation
	for mName, mInfos := range catalog.Models {
		if len(mInfos) == 0 {
			continue
		}
		if rec, ok := buildOptimizeRecommendation(mName, mInfos[0], req.CurrentModel, inputTokens, outputTokens, currentCostEstimate, score); ok {
			recommendations = append(recommendations, rec)
		}
	}

	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].SavingsPercent > recommendations[j].SavingsPercent
	})

	resp := OptimizeResponse{
		ComplexityScore:     score,
		CurrentCostEstimate: currentCostEstimate,
		Recommendations:     recommendations,
	}

	model.WriteJSON(w, http.StatusOK, resp)
}

// resolveUpdateProviderKeys extracts the effective single API key and
// cleaned multi-key list from req, with APIKeys taking precedence for the
// single-key fallback when APIKey itself isn't set.
func resolveUpdateProviderKeys(req updateProviderRequest) (apiKeyToSave string, cleanedAPIKeys []string) {
	if req.APIKey != nil {
		apiKeyToSave = *req.APIKey
	}
	for _, k := range req.APIKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			cleanedAPIKeys = append(cleanedAPIKeys, trimmed)
		}
	}
	if len(cleanedAPIKeys) > 0 && apiKeyToSave == "" {
		apiKeyToSave = cleanedAPIKeys[0]
	}
	return apiKeyToSave, cleanedAPIKeys
}

// applyProviderKeysAtRuntime pushes the new base URL/keys into the live
// provider registry entry, if one exists and supports runtime
// reconfiguration.
func (h *Handler) applyProviderKeysAtRuntime(name, baseURL, apiKeyToSave string, cleanedAPIKeys []string) {
	prov, err := h.reg.Get(name)
	if err != nil {
		slog.Debug("Provider not found in registry for runtime update", "provider", name, "error", err)
		return
	}
	cp, ok := prov.(provider.ConfigurableProvider)
	if !ok {
		return
	}
	if len(cleanedAPIKeys) > 0 {
		cp.UpdateKeys(baseURL, apiKeyToSave, cleanedAPIKeys)
	} else {
		cp.UpdateConfig(baseURL, apiKeyToSave)
	}
}

// updateProviderConfigEntry finds req.Name in providers and applies req's
// set fields to it in place.
func updateProviderConfigEntry(providers []config.ProviderConfig, req updateProviderRequest, apiKeyToSave string, cleanedAPIKeys []string) {
	for i := range providers {
		p := &providers[i]
		if p.Name != req.Name {
			continue
		}
		if req.BaseURL != "" {
			p.BaseURL = req.BaseURL
		}
		if req.APIKey != nil || len(cleanedAPIKeys) > 0 {
			p.APIKey = apiKeyToSave
		}
		if len(cleanedAPIKeys) > 0 {
			p.APIKeys = cleanedAPIKeys
		} else if req.APIKey != nil && *req.APIKey == "" {
			p.APIKeys = nil
		}
		return
	}
}

// syncProviderModelsAsync discovers and persists name's models in the
// background so the HTTP response doesn't wait on a live provider call.
// Background context is intentional: this must outlive the HTTP request
// that triggered the provider update.
func (h *Handler) syncProviderModelsAsync(name string) {
	go func() { //nolint:contextcheck // detached background sync, not request-scoped
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer syncCancel()
		prov, err := h.reg.Get(name)
		if err != nil {
			return
		}
		models, err := prov.DiscoverModels(syncCtx)
		if err != nil {
			slog.Warn("Failed to discover models after provider update", "provider", name, "error", err)
			return
		}
		if err := h.store.SaveDiscoveredModels(syncCtx, name, models); err != nil {
			slog.Warn("Failed to save discovered models after provider update", "provider", name, "error", err)
			return
		}
		slog.Debug("Discovered and saved models after provider update", "provider", name, "count", len(models))
	}()
}

func (h *Handler) HandleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req updateProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}
	if req.Name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Provider name is required")
		return
	}

	apiKeyToSave, cleanedAPIKeys := resolveUpdateProviderKeys(req)

	h.applyProviderKeysAtRuntime(req.Name, req.BaseURL, apiKeyToSave, cleanedAPIKeys)
	updateProviderConfigEntry(h.cfg.Providers, req, apiKeyToSave, cleanedAPIKeys)
	h.syncProviderModelsAsync(req.Name) //nolint:contextcheck // detached background sync, not request-scoped (must outlive this HTTP request)

	model.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
