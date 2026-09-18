package providers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
)

// createProviderRequest is the JSON body accepted by the create-provider
// endpoint. Type defaults to "openai" (a custom OpenAI-compatible provider),
// which reuses the full OpenAI provider stack (chat, streaming, embeddings,
// rerank, discovery) against the supplied base URL.
type createProviderRequest struct {
	Name            string                 `json:"name"`
	Type            string                 `json:"type"`
	BaseURL         string                 `json:"base_url"`
	APISecretKey    string                 `json:"api_key"`
	ServiceTier     string                 `json:"service_tier,omitempty"`
	Headers         map[string]string      `json:"headers,omitempty"`
	DiscoveryPublic bool                   `json:"discovery_public,omitempty"`
	ModelOverrides  []config.ModelOverride `json:"model_overrides,omitempty"`
}

// HandleCreateProvider registers a brand-new provider and makes it live without
// a restart. It persists a model.ProviderRegistration to the runtime_config
// "provider" section, then refreshes the config cache; the app's provider
// reload watcher (app.Provider reload) rebuilds the registry and discovers
// models so the new provider is immediately routable.
func (h *Handler) HandleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req createProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "provider name is required")
		return
	}
	if h.providerExists(name) {
		model.WriteJSONError(w, http.StatusConflict, "already_exists", "provider with this name already exists")
		return
	}
	// The in-memory provider set can lag the DB momentarily (a just-created
	// provider is picked up by the reload watcher asynchronously), so consult
	// the runtime_config section too — it is the authoritative source.
	if _, err := h.store.GetRuntimeConfigEntry(r.Context(), "provider", name); err == nil {
		model.WriteJSONError(w, http.StatusConflict, "already_exists", "provider with this name already exists")
		return
	}

	// Custom providers are OpenAI-compatible by default.
	providerType := strings.TrimSpace(req.Type)
	if providerType == "" {
		providerType = "openai"
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	if u, err := url.Parse(baseURL); err != nil || !u.IsAbs() {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "base_url must be an absolute URL")
		return
	}

	reg := model.ProviderRegistration{
		Name:            name,
		Provider:        providerType,
		BaseURL:         baseURL,
		APISecretKey:    req.APISecretKey,
		ServiceTier:     req.ServiceTier,
		IsActive:        true,
		Headers:         req.Headers,
		DiscoveryPublic: req.DiscoveryPublic,
	}
	if err := reg.Validate(); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	data, err := json.Marshal(reg)
	if err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to marshal provider")
		return
	}
	if err := h.store.UpsertRuntimeConfig(r.Context(), "provider", name, string(data), "admin-api"); err != nil {
		slog.Error("Failed to persist provider", "name", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to persist provider")
		return
	}

	// Store any accompanying model overrides in their own section (uploaded
	// separately via the Models JSON control); kept here for convenience.
	if len(req.ModelOverrides) > 0 {
		if ovJSON, err := json.Marshal(req.ModelOverrides); err == nil {
			_ = h.store.UpsertRuntimeConfig(r.Context(), modelOverridesSection, name, string(ovJSON), "admin-api")
		}
	}

	// Refresh the cache so the provider reload watcher picks up the new set.
	if h.configCache != nil {
		if err := h.configCache.Refresh(r.Context(), &config.RuntimeStores{RuntimeConfig: h.store}); err != nil {
			slog.Warn("Failed to refresh config cache after provider creation", "error", err)
		}
	}

	slog.Info("provider created", "name", name, "type", providerType, "base_url", baseURL)
	model.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "name": name, "type": providerType})
}
