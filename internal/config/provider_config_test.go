package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProviderConfigsFromSections verifies the shared provider-set parser used
// by both the boot loader and the config-cache snapshot.
func TestProviderConfigsFromSections(t *testing.T) {
	providerEntries := map[string]string{
		"my-custom": `{"name":"my-custom","provider":"openai","base_url":"https://api.example.com/v1","api_secret_key":"sk-1"}`,
		// Unparseable rows must be skipped without failing the whole set.
		"bad": `not-json`,
	}
	overrideEntries := map[string]string{
		"my-custom": `[{"id":"m1","category":"premium","cost_per_input_token":0.01,"cost_per_output_token":0.02}]`,
	}

	providers := ProviderConfigsFromSections(providerEntries, overrideEntries)
	require.Len(t, providers, 1, "unparseable row should be skipped")

	p := providers[0]
	assert.Equal(t, "my-custom", p.Name)
	assert.Equal(t, "openai", p.Type)
	assert.Equal(t, "https://api.example.com/v1", p.BaseURL)
	assert.Equal(t, "sk-1", p.APIKey)

	// Overrides from the model_overrides section are attached to the provider.
	require.Len(t, p.ModelOverrides, 1)
	assert.Equal(t, "m1", p.ModelOverrides[0].ID)
	assert.Equal(t, "premium", p.ModelOverrides[0].Category)
	assert.Equal(t, 0.01, p.ModelOverrides[0].CostPerInputToken)
}

// TestProviderConfigsFromSections_UnknownProviderHasNoOverrides verifies a
// provider without a matching model_overrides row gets an empty override list.
func TestProviderConfigsFromSections_UnknownProviderHasNoOverrides(t *testing.T) {
	providerEntries := map[string]string{
		"plain": `{"name":"plain","provider":"openai","base_url":"https://x/v1"}`,
	}
	providers := ProviderConfigsFromSections(providerEntries, nil)
	require.Len(t, providers, 1)
	assert.Empty(t, providers[0].ModelOverrides)
}
