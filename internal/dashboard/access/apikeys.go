package access

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ilter-ai/ilter/internal/model"

	"github.com/go-chi/chi/v5"

	"github.com/ilter-ai/ilter/internal/auth"
	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

type CreateAPIKeyRequest struct {
	Name             string            `json:"name"`
	GroupID          *int              `json:"group_id,omitempty"`
	UserID           *int              `json:"user_id,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
	RateLimitRPM     int               `json:"rate_limit_rpm"`
	RateLimitTPM     int64             `json:"rate_limit_tpm"`
	AllowedModels    []string          `json:"allowed_models,omitempty"`
	AllowedProviders []string          `json:"allowed_providers,omitempty"`
}

type UpdateAPIKeyRequest struct {
	Name             *string            `json:"name,omitempty"`
	GroupID          *int               `json:"group_id,omitempty"`
	UserID           *int               `json:"user_id,omitempty"`
	Tags             *map[string]string `json:"tags,omitempty"`
	RateLimitRPM     *int               `json:"rate_limit_rpm,omitempty"`
	RateLimitTPM     *int64             `json:"rate_limit_tpm,omitempty"`
	AllowedModels    *[]string          `json:"allowed_models,omitempty"`
	AllowedProviders *[]string          `json:"allowed_providers,omitempty"`
	Enabled          *bool              `json:"enabled,omitempty"`
}

func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	var keys []auth.APIKey
	var err error

	if groupIDStr := r.URL.Query().Get("group_id"); groupIDStr != "" {
		gid, parseErr := strconv.Atoi(groupIDStr)
		if parseErr != nil {
			model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid group_id")
			return
		}
		keys, err = h.store.ListAPIKeys(r.Context(), gid)
	} else {
		keys, err = h.store.ListAPIKeys(r.Context())
	}
	if err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to list virtual keys")
		return
	}

	type akResponse struct {
		ID               string            `json:"id"`
		Name             string            `json:"name"`
		GroupID          *int              `json:"group_id,omitempty"`
		UserID           *int              `json:"user_id,omitempty"`
		Tags             map[string]string `json:"tags"`
		RateLimitRPM     int               `json:"rate_limit_rpm"`
		RateLimitTPM     int64             `json:"rate_limit_tpm"`
		AllowedModels    []string          `json:"allowed_models"`
		AllowedProviders []string          `json:"allowed_providers"`
		Enabled          bool              `json:"enabled"`
		CreatedAt        string            `json:"created_at"`
		UpdatedAt        string            `json:"updated_at"`
	}

	items := make([]akResponse, 0, len(keys))
	for _, k := range keys {
		items = append(items, akResponse{
			ID:               k.ID,
			Name:             k.Name,
			GroupID:          k.GroupID,
			UserID:           k.UserID,
			Tags:             k.Tags,
			RateLimitRPM:     k.RateLimitRPM,
			RateLimitTPM:     k.RateLimitTPM,
			AllowedModels:    k.AllowedModels,
			AllowedProviders: k.AllowedProviders,
			Enabled:          k.Enabled,
			CreatedAt:        k.CreatedAt.Format(time.RFC3339),
			UpdatedAt:        k.UpdatedAt.Format(time.RFC3339),
		})
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"api_keys": items,
	})
}

// checkAPIKeyOwnerExists verifies groupID/userID (if set) reference real
// rows, returning a non-empty error code + message for the first failing
// check, or ("", "") if both are fine (or unset).
func (h *Handler) checkAPIKeyOwnerExists(ctx context.Context, groupID, userID *int) (errCode, errMsg string) {
	if groupID != nil {
		if _, err := h.store.GetGroup(ctx, *groupID); err != nil {
			return "not_found", "Group not found"
		}
	}
	if userID != nil {
		if _, err := h.store.GetUser(ctx, *userID); err != nil {
			return "not_found", "User not found"
		}
	}
	return "", ""
}

// auditAPIKeyCreate logs an api_key creation, if an auditor is configured.
func (h *Handler) auditAPIKeyCreate(r *http.Request, vk *auth.APIKey, rawToken string) {
	if h.auditor == nil {
		return
	}
	vals := map[string]any{
		"name":              vk.Name,
		"rate_limit_rpm":    vk.RateLimitRPM,
		"rate_limit_tpm":    vk.RateLimitTPM,
		"allowed_models":    vk.AllowedModels,
		"allowed_providers": vk.AllowedProviders,
		"enabled":           vk.Enabled,
		"api_key":           rawToken,
	}
	if vk.GroupID != nil {
		vals["group_id"] = *vk.GroupID
	}
	if vk.UserID != nil {
		vals["user_id"] = *vk.UserID
	}
	if vk.Tags != nil {
		vals["tags"] = vk.Tags
	}
	if err := h.auditor.LogCreate(r.Context(), "api_key", vk.ID, vals, reqmeta.GetKeyID(r.Context())); err != nil {
		slog.Error("failed to log audit create api_key", "error", err)
	}
}

func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req CreateAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}

	if req.Name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Name is required")
		return
	}

	if errCode, errMsg := h.checkAPIKeyOwnerExists(r.Context(), req.GroupID, req.UserID); errCode != "" {
		model.WriteJSONError(w, http.StatusNotFound, errCode, errMsg)
		return
	}

	vk, rawToken, err := h.store.CreateAPIKey(
		r.Context(), req.Name, req.GroupID, req.UserID,
		0, 0,
		req.RateLimitRPM, req.RateLimitTPM,
		req.AllowedModels, req.AllowedProviders,
		req.Tags,
	)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "already exists") {
			model.WriteJSONError(w, http.StatusConflict, "duplicate_key", "Virtual key with this name already exists")
			return
		}
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "Invalid virtual key parameters: "+errStr)
		return
	}

	h.auditAPIKeyCreate(r, vk, rawToken)

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"id":       vk.ID,
		"name":     vk.Name,
		"key":      rawToken,
		"group_id": vk.GroupID,
		"user_id":  vk.UserID,
	})
}

func (h *Handler) GetAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	vk, err := h.store.GetAPIKey(r.Context(), id)
	if err != nil {
		model.WriteJSONError(w, http.StatusNotFound, "not_found", "Virtual key not found")
		return
	}

	resp := map[string]any{
		"id":                vk.ID,
		"name":              vk.Name,
		"tags":              vk.Tags,
		"rate_limit_rpm":    vk.RateLimitRPM,
		"rate_limit_tpm":    vk.RateLimitTPM,
		"allowed_models":    vk.AllowedModels,
		"allowed_providers": vk.AllowedProviders,
		"enabled":           vk.Enabled,
		"created_at":        vk.CreatedAt.Format(time.RFC3339),
		"updated_at":        vk.UpdatedAt.Format(time.RFC3339),
	}
	if vk.GroupID != nil {
		resp["group_id"] = *vk.GroupID
	}
	if vk.UserID != nil {
		resp["user_id"] = *vk.UserID
	}
	model.WriteJSON(w, http.StatusOK, resp)
}

// parseUpdateAPIKeyRequest decodes body into an UpdateAPIKeyRequest and
// determines which optional fields were explicitly sent, since a plain
// *int can't distinguish "field omitted" (leave unchanged) from "field
// explicitly null" (clear it).
func parseUpdateAPIKeyRequest(body []byte) (req UpdateAPIKeyRequest, groupIDSent, userIDSent bool, err error) {
	if err := json.Unmarshal(body, &req); err != nil {
		return req, false, false, err
	}

	var rawFields map[string]json.RawMessage
	if uErr := json.Unmarshal(body, &rawFields); uErr != nil {
		slog.Error("failed to unmarshal raw fields for field presence detection", "error", uErr)
	}
	_, groupIDSent = rawFields["group_id"]
	_, userIDSent = rawFields["user_id"]
	return req, groupIDSent, userIDSent, nil
}

// applyAPIKeyGroupUpdate sets existing.GroupID to groupID after validating
// it references a real group (or clears it when groupID is nil).
func (h *Handler) applyAPIKeyGroupUpdate(ctx context.Context, existing *auth.APIKey, groupID *int) (errCode, errMsg string) {
	if groupID == nil {
		existing.GroupID = nil
		return "", ""
	}
	if _, err := h.store.GetGroup(ctx, *groupID); err != nil {
		return "not_found", "Group not found"
	}
	existing.GroupID = groupID
	return "", ""
}

// applyAPIKeyUserUpdate sets existing.UserID to userID after validating it
// references a real user (or clears it when userID is nil).
func (h *Handler) applyAPIKeyUserUpdate(ctx context.Context, existing *auth.APIKey, userID *int) (errCode, errMsg string) {
	if userID == nil {
		existing.UserID = nil
		return "", ""
	}
	if _, err := h.store.GetUser(ctx, *userID); err != nil {
		return "not_found", "User not found"
	}
	existing.UserID = userID
	return "", ""
}

// applyAPIKeyUpdate mutates existing in place with req's set fields,
// validating that a newly-set group_id/user_id references a real row.
// Returns a non-empty error code + message if that validation fails.
func (h *Handler) applyAPIKeyUpdate(ctx context.Context, existing *auth.APIKey, req UpdateAPIKeyRequest, groupIDSent, userIDSent bool) (errCode, errMsg string) {
	if req.Name != nil {
		existing.Name = *req.Name
	}
	if groupIDSent {
		if errCode, errMsg := h.applyAPIKeyGroupUpdate(ctx, existing, req.GroupID); errCode != "" {
			return errCode, errMsg
		}
	}
	if userIDSent {
		if errCode, errMsg := h.applyAPIKeyUserUpdate(ctx, existing, req.UserID); errCode != "" {
			return errCode, errMsg
		}
	}
	if req.Tags != nil {
		existing.Tags = *req.Tags
	}
	if req.RateLimitRPM != nil {
		existing.RateLimitRPM = *req.RateLimitRPM
	}
	if req.RateLimitTPM != nil {
		existing.RateLimitTPM = *req.RateLimitTPM
	}
	if req.AllowedModels != nil {
		existing.AllowedModels = *req.AllowedModels
	}
	if req.AllowedProviders != nil {
		existing.AllowedProviders = *req.AllowedProviders
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	return "", ""
}

// apiKeyAuditVals renders vk's audited fields as a map, for before/after
// diffing in the audit log.
func apiKeyAuditVals(vk *auth.APIKey) map[string]any {
	vals := map[string]any{
		"name":              vk.Name,
		"rate_limit_rpm":    vk.RateLimitRPM,
		"rate_limit_tpm":    vk.RateLimitTPM,
		"allowed_models":    vk.AllowedModels,
		"allowed_providers": vk.AllowedProviders,
		"enabled":           vk.Enabled,
		"tags":              vk.Tags,
	}
	if vk.GroupID != nil {
		vals["group_id"] = *vk.GroupID
	}
	if vk.UserID != nil {
		vals["user_id"] = *vk.UserID
	}
	return vals
}

func (h *Handler) UpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}

	req, groupIDSent, userIDSent, err := parseUpdateAPIKeyRequest(body)
	if err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}

	existing, err := h.store.GetAPIKey(r.Context(), id)
	if err != nil {
		model.WriteJSONError(w, http.StatusNotFound, "not_found", "Virtual key not found")
		return
	}
	existingOld := *existing

	if errCode, errMsg := h.applyAPIKeyUpdate(r.Context(), existing, req, groupIDSent, userIDSent); errCode != "" {
		model.WriteJSONError(w, http.StatusNotFound, errCode, errMsg)
		return
	}

	oldVals := apiKeyAuditVals(&existingOld)
	newVals := apiKeyAuditVals(existing)

	clearGroupID := groupIDSent && req.GroupID == nil
	clearUserID := userIDSent && req.UserID == nil
	if err := h.store.UpdateAPIKey(r.Context(), id, *existing, clearGroupID, clearUserID); err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to update virtual key")
		return
	}

	if h.auditor != nil {
		if err := h.auditor.LogUpdate(r.Context(), "api_key", id, oldVals, newVals, reqmeta.GetKeyID(r.Context())); err != nil {
			slog.Error("failed to log audit update api_key", "error", err)
		}
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"id":     id,
	})
}

func (h *Handler) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	vk, fetchErr := h.store.GetAPIKey(r.Context(), id)

	if err := h.store.DeleteAPIKey(r.Context(), id); err != nil {
		model.WriteJSONError(w, http.StatusNotFound, "not_found", "Virtual key not found")
		return
	}

	if h.auditor != nil && fetchErr == nil && vk != nil {
		vals := map[string]any{
			"name":              vk.Name,
			"rate_limit_rpm":    vk.RateLimitRPM,
			"rate_limit_tpm":    vk.RateLimitTPM,
			"allowed_models":    vk.AllowedModels,
			"allowed_providers": vk.AllowedProviders,
			"enabled":           vk.Enabled,
			"tags":              vk.Tags,
		}
		if vk.GroupID != nil {
			vals["group_id"] = *vk.GroupID
		}
		if vk.UserID != nil {
			vals["user_id"] = *vk.UserID
		}
		if err := h.auditor.LogDelete(r.Context(), "api_key", id, vals, reqmeta.GetKeyID(r.Context())); err != nil {
			slog.Error("failed to log audit delete api_key", "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) GetAPIKeyUsage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	fromDate := r.URL.Query().Get("from")
	toDate := r.URL.Query().Get("to")
	today := time.Now().UTC().Format("2006-01-02")
	if fromDate == "" {
		fromDate = today
	}
	if toDate == "" {
		toDate = today
	}

	usage, err := h.store.GetKeyUsage(r.Context(), id, fromDate, toDate)
	if err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to get usage")
		return
	}

	type usageItem struct {
		Date         string  `json:"date"`
		Model        string  `json:"model"`
		Provider     string  `json:"provider"`
		TokensIn     int64   `json:"tokens_in"`
		TokensOut    int64   `json:"tokens_out"`
		CostUSD      float64 `json:"cost_usd"`
		RequestCount int64   `json:"request_count"`
	}

	items := make([]usageItem, 0, len(usage))
	for _, u := range usage {
		items = append(items, usageItem{
			Date:         u.Date.Format("2006-01-02"),
			Model:        u.Model,
			Provider:     u.Provider,
			TokensIn:     u.TokensIn,
			TokensOut:    u.TokensOut,
			CostUSD:      u.CostUSD,
			RequestCount: u.RequestCount,
		})
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"key_id": id,
		"from":   fromDate,
		"to":     toDate,
		"items":  items,
	})
}

func (h *Handler) GetAPIKeysSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := h.store.GetAPIKeySummary(r.Context())
	if err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to get summary")
		return
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"total_keys":       summary.TotalKeys,
		"enabled_keys":     summary.EnabledKeys,
		"total_requests":   summary.TotalRequests,
		"total_cost_usd":   summary.TotalCostUSD,
		"total_tokens_in":  summary.TotalTokensIn,
		"total_tokens_out": summary.TotalTokensOut,
	})
}
