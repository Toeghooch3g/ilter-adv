package config

import (
	"encoding/json"
	"log/slog"

	"github.com/ilter-ai/ilter/internal/model"
)

// modelOverridesSectionName is the runtime_config section holding per-provider
// manual model-override documents (keyed by provider name).
const modelOverridesSectionName = "model_overrides"

// ProviderConfigsFromSections builds the provider configuration set from the
// runtime_config "provider" and "model_overrides" sections. It is the single
// source used both at boot (app.loadProvidersFromDB) and by the config cache
// snapshot (loadStateFromStores), so the live provider set and the cached set
// stay identical and can be hot-swapped on cache changes.
//
// Each provider row is parsed as a model.ProviderRegistration; the matching
// model_overrides entry (if any) is merged in, falling back to the provider's
// ModelOverridesFile path when no runtime entry exists. Unparseable rows are
// logged and skipped so one bad row never prevents the rest from loading.
func ProviderConfigsFromSections(providerEntries, overrideEntries map[string]string) []ProviderConfig {
	var providers []ProviderConfig
	for name, raw := range providerEntries {
		var reg model.ProviderRegistration
		if err := json.Unmarshal([]byte(raw), &reg); err != nil {
			slog.Warn("config: skipping unparseable provider", "name", name, "error", err)
			continue
		}
		providers = append(providers, providerConfigFromRegistration(reg, overrideEntries[name]))
	}
	return providers
}

func providerConfigFromRegistration(reg model.ProviderRegistration, overrideJSON string) ProviderConfig {
	p := ProviderConfig{
		Name:               reg.Name,
		Type:               reg.Provider,
		BaseURL:            reg.BaseURL,
		APIKey:             reg.APISecretKey,
		Timeout:            reg.Timeout,
		MaxRetries:         reg.MaxRetries,
		Headers:            reg.Headers,
		DiscoveryPublic:    reg.DiscoveryPublic,
		ModelOverridesFile: reg.ModelOverridesFile,
		ServiceTier:        reg.ServiceTier,
		CircuitBreaker: CircuitBreakerConfig{
			MaxFailures:         reg.CircuitBreaker.MaxFailures,
			Timeout:             reg.CircuitBreaker.Timeout,
			HalfOpenMaxRequests: reg.CircuitBreaker.HalfOpenMaxRequests,
		},
	}
	p.ModelOverrides = loadModelOverrides(overrideJSON, p.ModelOverridesFile)
	return p
}

// loadModelOverrides resolves a provider's manual model list. The runtime
// "model_overrides" section value wins when present (it is what the dashboard
// upload writes); otherwise, when a file path is configured, the file is read
// as a seed so a custom provider's manual metadata can live in version control.
// Invalid documents are logged and ignored so a bad override never prevents
// the provider from starting.
func loadModelOverrides(runtimeJSON, filePath string) []ModelOverride {
	if runtimeJSON != "" {
		overrides, err := ParseModelOverrides([]byte(runtimeJSON))
		if err != nil {
			slog.Warn("config: ignoring invalid runtime model_overrides", "error", err)
			return nil
		}
		return overrides
	}
	if filePath != "" {
		overrides, err := ReadModelOverridesFile(filePath)
		if err != nil {
			slog.Warn("config: ignoring model_overrides file", "path", filePath, "error", err)
			return nil
		}
		return overrides
	}
	return nil
}
