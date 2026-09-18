package models

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ilter-ai/ilter/internal/model"

	"github.com/go-chi/chi/v5"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
	"github.com/ilter-ai/ilter/internal/model/catalog"
)

// Handler serves model-related admin endpoints.
type Handler struct {
	store *db.SQLiteStore
	cfg   *config.Config
	lb    *smartrouter.LoadBalancer
}

func NewModelsHandler(store *db.SQLiteStore, cfg *config.Config, lb *smartrouter.LoadBalancer) *Handler {
	return &Handler{store: store, cfg: cfg, lb: lb}
}

type ModelResponseItem struct {
	Name                    string  `json:"name"`
	Provider                string  `json:"provider"`
	Type                    string  `json:"type"`
	OwnedBy                 string  `json:"owned_by"`
	Active                  bool    `json:"active"`
	Configured              bool    `json:"configured"`
	DisplayName             string  `json:"display_name,omitempty"`
	Category                string  `json:"category,omitempty"`
	CostPerInputToken       float64 `json:"cost_per_input_token,omitempty"`
	CostPerOutputToken      float64 `json:"cost_per_output_token,omitempty"`
	CostPerCachedInputToken float64 `json:"cost_per_cached_input_token,omitempty"`
	CostPerCacheWriteToken  float64 `json:"cost_per_cache_write_token,omitempty"`
}

// buildLBModelItems converts the load balancer's live model infos into
// response items, enriching each from the catalog (preferred) or DB
// fallback. Returns the items plus the set of provider:model keys seen, so
// the caller can skip them when merging in DB-only models.
func buildLBModelItems(lbInfos []smartrouter.ModelInfo, dbMap map[string]db.ProviderModel) ([]ModelResponseItem, map[string]bool) {
	lbSeen := make(map[string]bool, len(lbInfos))
	items := make([]ModelResponseItem, 0, len(lbInfos))
	for _, info := range lbInfos {
		lbSeen[info.Provider+":"+info.Name] = true
		item := ModelResponseItem{
			Name:       info.Name,
			Provider:   info.Provider,
			Type:       info.Type,
			OwnedBy:    info.OwnedBy,
			Active:     info.Active,
			Configured: true,
		}
		if regInfo, ok := catalog.GetModel(info.Name); ok {
			item.DisplayName = regInfo.DisplayName
			item.Category = regInfo.Category
			item.CostPerInputToken = regInfo.CostPerInputToken
			item.CostPerOutputToken = regInfo.CostPerOutputToken
			item.CostPerCachedInputToken = regInfo.CostPerCachedInputToken
			item.CostPerCacheWriteToken = regInfo.CostPerCacheWriteToken
		} else if dbEntry, ok := dbMap[info.Provider+":"+info.Name]; ok {
			item.Category = dbEntry.Category
			item.CostPerInputToken = dbEntry.CostIn
			item.CostPerOutputToken = dbEntry.CostOut
			item.CostPerCachedInputToken = dbEntry.CostCacheRead
			item.CostPerCacheWriteToken = dbEntry.CostCacheWrite
		}
		items = append(items, item)
	}
	return items, lbSeen
}

// enrichModelItemFromCatalog overrides item's display fields with the
// catalog's registered info for its model name, when present and more
// specific than the DB-sourced default.
func enrichModelItemFromCatalog(item *ModelResponseItem, modelName string) {
	regInfo, ok := catalog.GetModel(modelName)
	if !ok {
		return
	}
	item.DisplayName = regInfo.DisplayName
	if regInfo.Category != "" && regInfo.Category != "standard" {
		item.Category = regInfo.Category
	}
	if regInfo.CostPerInputToken > 0 {
		item.CostPerInputToken = regInfo.CostPerInputToken
	}
	if regInfo.CostPerOutputToken > 0 {
		item.CostPerOutputToken = regInfo.CostPerOutputToken
	}
	if regInfo.CostPerCachedInputToken > 0 {
		item.CostPerCachedInputToken = regInfo.CostPerCachedInputToken
	}
	if regInfo.CostPerCacheWriteToken > 0 {
		item.CostPerCacheWriteToken = regInfo.CostPerCacheWriteToken
	}
}

// buildDBOnlyModelItems adds response items for DB-known models that
// belong to a configured provider but weren't already returned by the load
// balancer (lbSeen), enriching each from the catalog when available.
func buildDBOnlyModelItems(dbModels []db.ProviderModel, configuredProviders, lbSeen map[string]bool) []ModelResponseItem {
	items := make([]ModelResponseItem, 0, len(dbModels))
	for _, pm := range dbModels {
		if !configuredProviders[pm.Provider] || lbSeen[pm.Provider+":"+pm.Model] {
			continue
		}
		lbSeen[pm.Provider+":"+pm.Model] = true
		item := ModelResponseItem{
			Name:                    pm.Model,
			Provider:                pm.Provider,
			Type:                    pm.Provider,
			OwnedBy:                 pm.Provider,
			Active:                  pm.Active,
			Configured:              false,
			Category:                pm.Category,
			CostPerInputToken:       pm.CostIn,
			CostPerOutputToken:      pm.CostOut,
			CostPerCachedInputToken: pm.CostCacheRead,
			CostPerCacheWriteToken:  pm.CostCacheWrite,
		}
		enrichModelItemFromCatalog(&item, pm.Model)
		items = append(items, item)
	}
	return items
}

func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	lbInfos := h.lb.GetAvailableModelInfos()

	configuredProviders := make(map[string]bool, len(h.cfg.Providers))
	for _, p := range h.cfg.Providers {
		configuredProviders[p.Name] = true
	}

	dbModels, err := h.store.GetAllProviderModels(r.Context())
	if err != nil {
		slog.Error("Failed to query provider_models", "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	dbMap := make(map[string]db.ProviderModel, len(dbModels))
	for _, pm := range dbModels {
		key := pm.Provider + ":" + pm.Model
		dbMap[key] = pm
	}

	catalog.ModelsMu.RLock()
	defer catalog.ModelsMu.RUnlock()

	lbItems, lbSeen := buildLBModelItems(lbInfos, dbMap)
	dbOnlyItems := buildDBOnlyModelItems(dbModels, configuredProviders, lbSeen)

	resp := make([]ModelResponseItem, 0, len(lbItems)+len(dbOnlyItems))
	resp = append(resp, lbItems...)
	resp = append(resp, dbOnlyItems...)

	model.WriteJSON(w, http.StatusOK, resp)
}

type ToggleModelRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Active   bool   `json:"active"`
}

func (h *Handler) HandleToggleModel(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req ToggleModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}
	if req.Name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Model name is required")
		return
	}
	if req.Provider == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Provider is required")
		return
	}

	if err := h.store.SaveModelStatus(r.Context(), req.Provider, req.Name, req.Active); err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to save model status to DB")
		return
	}

	inactiveList, err := h.store.GetInactiveModels(r.Context())
	if err == nil {
		h.lb.SetInactiveModels(inactiveList)
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) HandleUpdateModelByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Model ID is required")
		return
	}

	var req ToggleModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}
	if req.Name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Model name is required")
		return
	}
	if req.Provider == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Provider is required")
		return
	}

	if err := h.store.SaveModelStatus(r.Context(), req.Provider, req.Name, req.Active); err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to save model status to DB")
		return
	}

	inactiveList, err := h.store.GetInactiveModels(r.Context())
	if err == nil {
		h.lb.SetInactiveModels(inactiveList)
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"model_id": id,
		"name":     req.Name,
		"active":   req.Active,
	})
}

type UpdateModelCategoryRequest struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

func (h *Handler) HandleUpdateModelCategory(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req UpdateModelCategoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}
	if req.Name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Model name is required")
		return
	}
	validCategories := make(map[string]bool, len(DefaultCategories)+4)
	for _, c := range h.categoryList(r.Context()) {
		validCategories[c] = true
	}
	if req.Category == "" || !validCategories[req.Category] {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid category: must be an existing category")
		return
	}

	catalog.ModelsMu.Lock()
	if infos, ok := catalog.Models[req.Name]; ok {
		for i := range infos {
			infos[i].Category = req.Category
		}
		catalog.Models[req.Name] = infos
	}
	catalog.ModelsMu.Unlock()

	if err := h.store.SaveModelCategory(r.Context(), req.Name, req.Category); err != nil {
		slog.Error("Failed to persist model category to DB", "model", req.Name, "category", req.Category, "error", err)
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
