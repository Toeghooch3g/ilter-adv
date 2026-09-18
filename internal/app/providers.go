package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"

	"github.com/ilter-ai/ilter/internal/config"
)

// providerFingerprint returns a stable digest of a provider set (names, types,
// endpoints, keys, and model overrides). The app uses it to detect when the
// runtime provider set actually changed, so the registry and routes are only
// rebuilt on real provider changes rather than on every config-cache refresh
// (which also fires for unrelated sections like feature flags or PII).
func providerFingerprint(providers []config.ProviderConfig) string {
	h := sha256.New()
	// Encoder writes compact JSON — a stable serialization for hashing. The
	// digest (not the JSON) is the only output and is never exposed; API keys
	// must be hashed so key rotation triggers a provider reload.
	//nolint:gosec // G117: fingerprint of a provider set intentionally includes keys.
	_ = json.NewEncoder(h).Encode(providers)
	return hex.EncodeToString(h.Sum(nil))
}

// watchProviderReload registers a config-cache callback that hot-reloads the
// live provider registry, config, and load-balancer routes whenever the
// runtime provider set changes. This is what makes provider creation/edits and
// model-override uploads take effect without a restart — the same immediacy
// the rest of the runtime configuration enjoys. Call once after the load
// balancer is available.
func (a *App) watchProviderReload() {
	// Seed the fingerprint with the already-applied (boot) provider set so a
	// subsequent refresh that hasn't actually changed providers is a no-op.
	a.providerFingerprint = providerFingerprint(a.cfg.Providers)
	a.cfgCache.OnChange(a.maybeReloadProviders)
}

// maybeReloadProviders rebuilds the provider registry and routes when the
// provider set changed. Serialized with a mutex so concurrent config changes
// apply atomically; the fingerprint is re-checked after acquiring the lock.
//
// The config-cache snapshot only carries runtime_config (DB) providers;
// env-seeded providers (ILTER_PROVIDER_<NAME>_API_KEY, no DB row) are
// re-applied here via config.ReapplyEnvKeys so they survive a cache refresh
// identically to a boot (they must not be dropped from the live set).
func (a *App) maybeReloadProviders(snap *config.Snapshot) {
	providers := config.ReapplyEnvKeys(snap.Providers())
	if providerFingerprint(providers) == a.providerFingerprint {
		return
	}

	a.providerReloadMu.Lock()
	defer a.providerReloadMu.Unlock()

	// Re-evaluate under the lock: another change may have already applied.
	providers = config.ReapplyEnvKeys(snap.Providers())
	if providerFingerprint(providers) == a.providerFingerprint {
		return
	}

	if err := a.reg.InitFromCache(snap); err != nil {
		slog.Warn("provider reload: failed to rebuild registry", "error", err)
		return
	}
	a.cfg.Providers = providers
	a.providerFingerprint = providerFingerprint(a.cfg.Providers)

	// Re-discover (bypassing the discovery cooldown) and resync models + routes.
	// syncModelsToDB persists new models and rebuilds load-balancer routes.
	a.discoverModels(true)
	a.syncModelsToDB()
	slog.Info("providers reloaded from runtime config", "count", len(a.cfg.Providers))
}
