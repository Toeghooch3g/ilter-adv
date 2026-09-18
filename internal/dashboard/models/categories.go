package models

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ilter-ai/ilter/internal/model"
)

// DefaultCategories are the model categories that ship out of the box. They
// cannot be removed via the API; only user-added categories are removable.
var DefaultCategories = []string{"free", "economy", "standard", "premium", "embedding"}

// modelCategoriesSection is the runtime_config section holding user-added
// model categories as a JSON array under the "list" key. Defaults are kept in
// code (DefaultCategories) and merged in at read time, so the out-of-box set
// is always present even on a fresh DB.
const modelCategoriesSection = "model_categories"

// categorySlugPattern matches a valid category name: lowercase letters,
// digits, underscores and hyphens.
var categorySlugPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// categoryList returns the merged category list: defaults followed by any
// user-added categories persisted in runtime_config (deduplicated, order
// preserved). Never errors — an unreadable/absent runtime entry degrades to
// the defaults.
func (h *Handler) categoryList(ctx context.Context) []string {
	entries, err := h.store.GetBySection(ctx, modelCategoriesSection)
	if err != nil {
		slog.Warn("failed to read model categories", "error", err)
		return slices.Clone(DefaultCategories)
	}
	var userAdded []string
	if raw, ok := entries["list"]; ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &userAdded); err != nil {
			slog.Warn("invalid model_categories JSON, using defaults", "error", err)
			return slices.Clone(DefaultCategories)
		}
	}
	out := slices.Clone(DefaultCategories)
	for _, c := range userAdded {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// persistCategoryList writes user-added categories back to runtime_config.
func (h *Handler) persistCategoryList(ctx context.Context, userAdded []string) error {
	// Prune anything that is a default (it must not be re-persisted) and
	// dedupe.
	clean := make([]string, 0, len(userAdded))
	for _, c := range userAdded {
		if slices.Contains(DefaultCategories, c) || slices.Contains(clean, c) {
			continue
		}
		clean = append(clean, c)
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	return h.store.UpsertRuntimeConfig(ctx, modelCategoriesSection, "list", string(b), "admin-api")
}

// HandleListCategories returns the merged category list (defaults + user-added).
func (h *Handler) HandleListCategories(w http.ResponseWriter, r *http.Request) {
	model.WriteJSON(w, http.StatusOK, map[string]any{"categories": h.categoryList(r.Context())})
}

// createCategoryRequest is the POST /api/models/categories body.
type createCategoryRequest struct {
	Name string `json:"name"`
}

// HandleCreateCategory adds a user-defined model category. Validates the slug
// format, rejects duplicates, and persists the merged list to runtime_config.
func (h *Handler) HandleCreateCategory(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req createCategoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || !categorySlugPattern.MatchString(name) {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Category must be lowercase letters, digits, underscores, or hyphens")
		return
	}
	if slices.Contains(h.categoryList(r.Context()), name) {
		model.WriteJSONError(w, http.StatusConflict, "duplicate_category", "Category already exists")
		return
	}
	if err := h.persistCategoryList(r.Context(), append(h.categoryList(r.Context()), name)); err != nil {
		slog.Error("failed to persist category", "category", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to save category")
		return
	}
	model.WriteJSON(w, http.StatusCreated, map[string]any{"status": "ok", "name": name})
}

// HandleDeleteCategory removes a user-defined category. Default categories
// are rejected (400), and a category still used by any provider_models row is
// rejected (409) — remove it from models first.
func (h *Handler) HandleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Category name is required")
		return
	}
	if slices.Contains(DefaultCategories, name) {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Default categories cannot be removed")
		return
	}
	cur := h.categoryList(r.Context())
	if !slices.Contains(cur, name) {
		model.WriteJSONError(w, http.StatusNotFound, "not_found", "Category not found")
		return
	}

	var inUse int
	if err := h.store.DB.QueryRow("SELECT COUNT(*) FROM provider_models WHERE category = ?", name).Scan(&inUse); err != nil {
		slog.Error("failed to count models in category", "category", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to check category usage")
		return
	}
	if inUse > 0 {
		model.WriteJSONError(w, http.StatusConflict, "category_in_use", "Category still has models assigned; reassign them first")
		return
	}

	next := make([]string, 0, len(cur))
	for _, c := range cur {
		if c != name {
			next = append(next, c)
		}
	}
	if err := h.persistCategoryList(r.Context(), next); err != nil {
		slog.Error("failed to persist category removal", "category", name, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to remove category")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
