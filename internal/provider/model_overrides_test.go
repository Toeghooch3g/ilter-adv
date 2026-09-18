package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model/catalog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func override(id string, mutate func(*config.ModelOverride)) config.ModelOverride {
	o := config.ModelOverride{ID: id}
	if mutate != nil {
		mutate(&o)
	}
	return o
}

// TestApplyModelOverrides_EndpointWinsPerField verifies the priority rule:
// for a model reported by both the endpoint and the override, the endpoint's
// value wins per field and the override fills only fields the endpoint left
// unset.
func TestApplyModelOverrides_EndpointWinsPerField(t *testing.T) {
	endpoint := []catalog.ModelInfo{
		{
			ID:                 "gpt-x",
			Provider:           "custom-a",
			DisplayName:        "",
			CostPerInputToken:  0.0001, // endpoint-reported
			CostPerOutputToken: 0,      // not reported by endpoint
			Category:           "standard",
			MaxContextTokens:   0,
		},
	}
	overrides := []config.ModelOverride{
		override("gpt-x", func(o *config.ModelOverride) {
			o.DisplayName = "GPT X (manual)"
			o.Category = "economy" // override tier — endpoint reported "standard", so endpoint wins
			o.CostPerInputToken = 0.5
			o.CostPerOutputToken = 0.001 // endpoint didn't report output cost, override fills
			o.MaxContextTokens = 16000
		}),
	}

	got := applyModelOverrides(endpoint, "custom-a", "https://custom-a/v1", overrides)
	require.Len(t, got, 1)
	m := got[0]

	// Endpoint-reported fields win.
	assert.Equal(t, "standard", m.Category)
	assert.Equal(t, 0.0001, m.CostPerInputToken)

	// Override fills the gaps the endpoint left unset.
	assert.Equal(t, "GPT X (manual)", m.DisplayName)
	assert.Equal(t, 0.001, m.CostPerOutputToken)
	assert.Equal(t, 16000, m.MaxContextTokens)
}

// TestApplyModelOverrides_KeepsOverrideOnlyModels verifies that models present
// only in the manual override list are kept and stamped with the owning
// provider instance + base URL so they route back to the right provider.
func TestApplyModelOverrides_KeepsOverrideOnlyModels(t *testing.T) {
	overrides := []config.ModelOverride{
		override("manual-only", func(o *config.ModelOverride) {
			o.Category = "free"
			o.CostPerInputToken = 0
			o.MaxContextTokens = 4096
		}),
	}

	got := applyModelOverrides(nil, "custom-b", "https://custom-b/v1", overrides)
	require.Len(t, got, 1)
	m := got[0]
	assert.Equal(t, "manual-only", m.ID)
	assert.Equal(t, "custom-b", m.Provider)
	assert.Equal(t, "https://custom-b/v1", m.DefaultBaseURL)
	assert.Equal(t, "free", m.Category)
	assert.Equal(t, "manual-only", m.DisplayName) // falls back to ID
}

// TestApplyModelOverrides_KeepsEndpointOnlyModels verifies endpoint models with
// no matching override pass through unchanged.
func TestApplyModelOverrides_KeepsEndpointOnlyModels(t *testing.T) {
	endpoint := []catalog.ModelInfo{
		{ID: "endpoint-only", Provider: "custom-a"},
	}
	got := applyModelOverrides(endpoint, "custom-a", "https://custom-a/v1", nil)
	require.Len(t, got, 1)
	assert.Equal(t, "endpoint-only", got[0].ID)
	assert.Equal(t, "custom-a", got[0].Provider)
}

// TestOpenAIProvider_DiscoverModels_AppliesOverrides verifies the full discovery
// pipeline: a custom provider's /v1/models endpoint returns bare IDs, and the
// manually-specified overrides supply the pricing/context details (endpoint
// wins where it reports values, override fills the rest).
func TestOpenAIProvider_DiscoverModels_AppliesOverrides(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data": [{"id": "my-custom-model"}, {"id": "other-model"}]}`))
	}))
	defer server.Close()

	cfg := config.ProviderConfig{
		Name:    "custom-a",
		Type:    "openai",
		BaseURL: server.URL,
		APIKey:  "sk-a",
		ModelOverrides: []config.ModelOverride{
			{
				ID:                 "my-custom-model",
				DisplayName:        "My Custom Model",
				Category:           "premium",
				CostPerInputToken:  0.001,
				CostPerOutputToken: 0.002,
				MaxContextTokens:   128000,
				MaxOutputTokens:    8192,
			},
		},
	}
	p := NewOpenAIProvider(cfg)

	models, err := p.DiscoverModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 2)

	byID := make(map[string]catalog.ModelInfo, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}

	// Override-only details applied to the endpoint-reported model.
	overridden := byID["my-custom-model"]
	assert.Equal(t, "custom-a", overridden.Provider)
	assert.Equal(t, "My Custom Model", overridden.DisplayName) // override wins over ID
	assert.Equal(t, "premium", overridden.Category)
	assert.Equal(t, 0.001, overridden.CostPerInputToken)
	assert.Equal(t, 0.002, overridden.CostPerOutputToken)
	assert.Equal(t, 128000, overridden.MaxContextTokens)
	assert.Equal(t, 8192, overridden.MaxOutputTokens)
	assert.Equal(t, server.URL, overridden.DefaultBaseURL)

	// Endpoint-only model uses heuristic defaults.
	other := byID["other-model"]
	assert.Equal(t, "custom-a", other.Provider)
	assert.NotEmpty(t, other.Category)
	assert.Equal(t, other.DisplayName, "other-model")
}
