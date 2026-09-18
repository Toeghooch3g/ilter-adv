package providers

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
)

// HandleDeleteProvider removes a provider entirely: its runtime_config
// "provider" row, its "model_overrides" row, its provider_models rows, and its
// live registry entry, then refreshes the config cache so the app's provider
// reload watcher rebuilds routes without it.
//
// Env-seeded providers (ILTER_PROVIDER_<NAME>_API_KEY with no DB row) are
// rejected with a 409: the env var would re-register them at the next boot, so
// removal must happen by unsetting the var (user decision — no tombstone).
func (h *Handler) HandleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Provider name is required")
		return
	}

	// Provider must exist.
	var exists bool
	for _, p := range h.cfg.Providers {
		if p.Name == name {
			exists = true
			break
		}
	}
	if !exists {
		model.WriteJSONError(w, http.StatusNotFound, "not_found", "Provider not found")
		return
	}

	// Env-seeded providers cannot be removed via the API — the env var owns
	// their lifecycle.
	if envVar := providerEnvVarSet(name); envVar != "" {
		model.WriteJSONError(w, http.StatusConflict, "env_seeded_provider",
			"provider is env-seeded via "+envVar+"; unset the env var to remove it")
		return
	}

	// Delete runtime_config provider + model_overrides rows (best-effort per
	// row — a missing row is fine).
	if err := h.store.DeleteRuntimeConfig(r.Context(), "provider", name); err != nil {
		slog.Error("failed to delete provider runtime_config row", "provider", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to delete provider")
		return
	}
	_ = h.store.DeleteRuntimeConfig(r.Context(), modelOverridesSection, name)

	// Delete discovered models.
	if _, err := h.store.DB.Exec("DELETE FROM provider_models WHERE provider = ?", name); err != nil {
		slog.Error("failed to delete provider models", "provider", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to delete provider models")
		return
	}

	// Drop from the live registry.
	if h.reg != nil {
		h.reg.Remove(name)
	}

	// Refresh so maybeReloadProviders rebuilds cfg.Providers/routes without
	// the provider (and re-applies env seeding to the DB-only snapshot).
	if h.configCache != nil {
		if err := h.configCache.Refresh(r.Context(), &config.RuntimeStores{RuntimeConfig: h.store}); err != nil {
			slog.Warn("Failed to refresh config cache after provider delete", "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// providerEnvVarSet returns the ILTER_PROVIDER_<NAME>_API_KEY(S) env var that
// currently supplies name's key (plural wins), or "" when none is set to a
// non-empty value. Mirrors providerKeysFromEnv's "set + non-empty" test so a
// provider that would be re-seeded at the next boot is detected here.
func providerEnvVarSet(name string) string {
	if v, ok := os.LookupEnv(config.ProviderKeysEnv(name)); ok && strings.TrimSpace(v) != "" {
		return config.ProviderKeysEnv(name)
	}
	if v, ok := os.LookupEnv(config.ProviderKeyEnv(name)); ok && strings.TrimSpace(v) != "" {
		return config.ProviderKeyEnv(name)
	}
	return ""
}
