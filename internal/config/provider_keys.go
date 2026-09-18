package config

import (
	"log/slog"
	"os"
	"sort"
	"strings"
)

// ProviderKeyEnv returns the environment variable name for a provider's single API key.
func ProviderKeyEnv(name string) string {
	return "ILTER_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_API_KEY"
}

// ProviderKeysEnv returns the environment variable name for a provider's multiple API keys.
// Convention: ILTER_PROVIDER_<UPPER_NAME>_API_KEYS (comma or newline separated).
func ProviderKeysEnv(name string) string {
	return "ILTER_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_API_KEYS"
}

// ProviderServiceTierEnv returns the environment variable name for a
// provider's default service tier ("priority" | "flex" | "default"). It is
// per-name dynamic (matching the API-key vars), so it surfaces in `ilter
// config show` via the provider registry rather than a static RegisterEnv.
func ProviderServiceTierEnv(name string) string {
	return "ILTER_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_SERVICE_TIER"
}

// providerKeysFromEnv reads ILTER_PROVIDER_<NAME>_API_KEY(S) for name and
// returns the parsed keys, which env var supplied them, and whether either
// was set. The *_KEYS (plural) var wins when both are set.
func providerKeysFromEnv(name string) (keys []string, source string, ok bool) {
	envMulti := ProviderKeysEnv(name)
	if v, isSet := os.LookupEnv(envMulti); isSet && strings.TrimSpace(v) != "" {
		rawKeys := strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == '\n' || r == '\r'
		})
		for _, k := range rawKeys {
			if trimmed := strings.TrimSpace(k); trimmed != "" {
				keys = append(keys, trimmed)
			}
		}
		if len(keys) > 0 {
			return keys, envMulti, true
		}
	}

	envSingle := ProviderKeyEnv(name)
	if v, isSet := os.LookupEnv(envSingle); isSet && strings.TrimSpace(v) != "" {
		trimmed := strings.TrimSpace(v)
		return []string{trimmed}, envSingle, true
	}

	return nil, "", false
}

// AnyProviderKeyEnvSet reports whether at least one known provider's
// ILTER_PROVIDER_<NAME>_API_KEY(S) env var is set, regardless of whether
// that provider is already registered in cfg.Providers.
func AnyProviderKeyEnvSet() bool {
	for t := range DefaultBaseURLs {
		if _, _, ok := providerKeysFromEnv(t); ok {
			return true
		}
	}
	return false
}

// applyEnvKeys resolves the environment-var provider seeding on a provider
// set and returns a new slice.
//
// Semantics (user decision: "DB row wins"): providers already present in the
// slice keep their stored values — the env var does NOT override an existing
// provider's API key (a runtime_config provider row, once materialized, is
// authoritative; this is what makes UI-configured env-seeded providers behave
// identically to UI-added ones across restarts). Env service-tier overrides
// still apply. Providers with NO entry for a known type are auto-registered
// from their env var — setting ILTER_PROVIDER_<NAME>_API_KEY is enough to
// enable that provider, no `ilter init` required.
func applyEnvKeys(providers []ProviderConfig) []ProviderConfig {
	out := make([]ProviderConfig, 0, len(providers)+len(DefaultBaseURLs))
	configured := make(map[string]bool, len(providers))

	for i := range providers {
		p := providers[i]
		configured[p.Type] = true

		if v, isSet := os.LookupEnv(ProviderServiceTierEnv(p.Name)); isSet && strings.TrimSpace(v) != "" {
			p.ServiceTier = strings.TrimSpace(v)
		}

		// DB row wins: never override an existing provider's keys from env.
		if len(p.GetAPIKeys()) > 0 {
			p.APIKeySource = "db"
		}
		out = append(out, p)
	}

	types := make([]string, 0, len(DefaultBaseURLs))
	for t := range DefaultBaseURLs {
		types = append(types, t)
	}
	sort.Strings(types)

	for _, t := range types {
		if configured[t] {
			continue
		}
		keys, source, ok := providerKeysFromEnv(t)
		if !ok {
			continue
		}
		out = append(out, ProviderConfig{
			Name:         t,
			Type:         t,
			BaseURL:      DefaultBaseURLs[t],
			APIKey:       keys[0],
			APIKeys:      keys,
			APIKeySource: source,
			ServiceTier:  strings.TrimSpace(os.Getenv(ProviderServiceTierEnv(t))),
		})
	}
	return out
}

// ReapplyEnvKeys applies the same env-key resolution a boot performs to a
// provider set. It is used by the app's provider hot-reload path so that
// env-seeded providers (which have no runtime_config row) survive a config
// cache refresh identically to a boot, instead of being dropped when the
// reload swaps in the DB-only snapshot.
func ReapplyEnvKeys(providers []ProviderConfig) []ProviderConfig {
	return applyEnvKeys(providers)
}

// ResolveProviderKeys resolves env-key seeding on cfg.Providers at boot.
// It delegates to the shared applyEnvKeys helper so boot and hot reload
// agree on the resulting provider set.
func ResolveProviderKeys(cfg *Config, _ *slog.Logger) {
	cfg.Providers = applyEnvKeys(cfg.Providers)
}
