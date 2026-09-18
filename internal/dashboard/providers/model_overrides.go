// Package providers implements the dashboard API for managing upstream LLM
// providers and their per-provider model metadata.
package providers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
)

// modelOverridesSection is the runtime_config section that stores a provider's
// manually-supplied model list. Each row is keyed by provider name and holds a
// JSON array of config.ModelOverride entries — the lowest-priority model
// source; the provider's /v1/models endpoint wins per-field (see
// provider.OpenAIProvider.DiscoverModels).
const modelOverridesSection = "model_overrides"

// providerExists reports whether a provider is configured (by name). Used to
// reject uploads for unknown providers so a typo'd name cannot silently create
// an orphaned override row.
func (h *Handler) providerExists(name string) bool {
	for _, p := range h.cfg.Providers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// HandleGetModelOverrides returns the stored model-override document for a
// provider (download). Returns an empty JSON array when none is stored.
func (h *Handler) HandleGetModelOverrides(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !h.providerExists(name) {
		model.WriteJSONError(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}

	entry, err := h.store.GetRuntimeConfigEntry(r.Context(), modelOverridesSection, name)
	if errors.Is(err, sql.ErrNoRows) {
		// No entry yet — an empty override list is a valid download.
		model.WriteJSON(w, http.StatusOK, []config.ModelOverride{})
		return
	}
	if err != nil {
		slog.Error("Failed to read model overrides", "provider", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read model overrides")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Served as application/json to an authenticated admin; the stored
	// override document is echoed verbatim so the dashboard round-trips it.
	//nolint:gosec // G705: JSON content type, not HTML — no XSS surface.
	_, _ = w.Write([]byte(entry.Value))
}

// HandlePutModelOverrides replaces a provider's model-override document from
// an uploaded JSON file (upload). The body must be a valid JSON array of
// config.ModelOverride entries. Changes are persisted to the runtime_config
// "model_overrides" section and the config cache is refreshed so a live
// provider picks them up on its next discovery/reload.
func (h *Handler) HandlePutModelOverrides(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !h.providerExists(name) {
		model.WriteJSONError(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}

	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}

	overrides, err := config.ParseModelOverrides(body)
	if err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if len(overrides) == 0 {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "model overrides list is empty")
		return
	}

	if err := h.store.UpsertRuntimeConfig(r.Context(), modelOverridesSection, name, string(body), ""); err != nil {
		slog.Error("Failed to store model overrides", "provider", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to store model overrides")
		return
	}

	// Pick the change up in the running config snapshot so discovery/routes
	// can reload without a restart. The app wires cfgCache.OnChange to the
	// provider registry + load balancer rebuild.
	if h.configCache != nil {
		if err := h.configCache.Refresh(r.Context(), &config.RuntimeStores{RuntimeConfig: h.store}); err != nil {
			slog.Warn("Failed to refresh config cache after model overrides upload", "error", err)
		}
	}

	if err := json.NewEncoder(w).Encode(map[string]any{"status": "ok", "count": len(overrides)}); err != nil {
		slog.Error("Failed to write model overrides upload response", "error", err)
	}
}
