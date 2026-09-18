package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type preRequest struct {
	ComplexityScore  float64
	SelectedModel    string
	CostEstimate     float64
	AlternativeCost  float64
	SavingsPotential float64
}

type postResponse struct {
	Model               string
	PromptTokens        int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	InputCost           float64
	CachedCost          float64
	CacheWriteCost      float64
	OutputCost          float64
	ActualCost          float64
}

func setPreRequest(w http.ResponseWriter, p preRequest, emitStandard bool) {
	w.Header().Set("X-Ilter-Complexity-Score", fmt.Sprintf("%.1f", p.ComplexityScore))
	w.Header().Set("X-Ilter-Model-Selected", p.SelectedModel)
	w.Header().Set("X-Ilter-Cost-Estimate", fmt.Sprintf("%.6f", p.CostEstimate))

	if p.AlternativeCost > 0 {
		w.Header().Set("X-Ilter-Alternative-Cost", fmt.Sprintf("%.6f", p.AlternativeCost))
		w.Header().Set("X-Ilter-Savings-Potential", fmt.Sprintf("%.0f%%", p.SavingsPotential))
	}

	if emitStandard {
		w.Header().Set("X-Request-Cost", fmt.Sprintf("%.6f", p.CostEstimate))
	}
}

func setPostResponse(w http.ResponseWriter, p postResponse, emitStandard bool) {
	if !emitStandard {
		return
	}
	if p.ActualCost <= 0 && p.PromptTokens <= 0 && p.CompletionTokens <= 0 {
		return
	}
	pricing := map[string]any{
		"model":                 p.Model,
		"prompt_tokens":         p.PromptTokens,
		"completion_tokens":     p.CompletionTokens,
		"cached_tokens":         p.CachedTokens,
		"cache_creation_tokens": p.CacheCreationTokens,
		"input_cost":            p.InputCost,
		"cached_cost":           p.CachedCost,
		"cache_write_cost":      p.CacheWriteCost,
		"output_cost":           p.OutputCost,
		"cost_usd":              p.ActualCost,
	}
	b, err := json.Marshal(pricing)
	if err != nil {
		return
	}
	w.Header().Set("X-Request-Pricing", string(b))
}
