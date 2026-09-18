package models

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db/dbtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCategoriesTestHandler(t *testing.T) *Handler {
	t.Helper()
	store := dbtest.New(t)
	cfg := &config.Config{}
	return NewModelsHandler(store, cfg, nil)
}

// TestHandleListCategories verifies defaults are always present and a fresh
// DB has no user-added categories.
func TestHandleListCategories(t *testing.T) {
	h := newCategoriesTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/models/categories", nil)
	rr := httptest.NewRecorder()
	h.HandleListCategories(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Categories []string `json:"categories"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	for _, want := range DefaultCategories {
		assert.Contains(t, resp.Categories, want, "default category %q must be present", want)
	}
	assert.Len(t, resp.Categories, len(DefaultCategories))
}

// TestHandleCreateDeleteCategory round-trips a user-added category, verifies
// it shows up in the list, can't be created twice, can't be removed while a
// model uses it, and can be removed once empty.
func TestHandleCreateDeleteCategory(t *testing.T) {
	h := newCategoriesTestHandler(t)

	// Create.
	req := httptest.NewRequest(http.MethodPost, "/models/categories", bytes.NewReader([]byte(`{"name":"vision"}`)))
	rr := httptest.NewRecorder()
	h.HandleCreateCategory(rr, req)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	// Duplicate rejected.
	req = httptest.NewRequest(http.MethodPost, "/models/categories", bytes.NewReader([]byte(`{"name":"vision"}`)))
	rr = httptest.NewRecorder()
	h.HandleCreateCategory(rr, req)
	assert.Equal(t, http.StatusConflict, rr.Code)

	// Invalid slug rejected.
	req = httptest.NewRequest(http.MethodPost, "/models/categories", bytes.NewReader([]byte(`{"name":"Bad Name!"}`)))
	rr = httptest.NewRecorder()
	h.HandleCreateCategory(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Listed now.
	listReq := httptest.NewRequest(http.MethodGet, "/models/categories", nil)
	listRR := httptest.NewRecorder()
	h.HandleListCategories(listRR, listReq)
	var list struct {
		Categories []string `json:"categories"`
	}
	require.NoError(t, json.Unmarshal(listRR.Body.Bytes(), &list))
	assert.Contains(t, list.Categories, "vision")

	// In use -> cannot delete.
	_, err := h.store.DB.Exec("INSERT INTO provider_models (provider, model, active, category, cost_in, cost_out) VALUES ('p','m',1,'vision',0,0)")
	require.NoError(t, err)
	delReq := httptest.NewRequest(http.MethodDelete, "/models/categories/vision", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "vision")
	delReq = delReq.WithContext(context.WithValue(delReq.Context(), chi.RouteCtxKey, rctx))
	delRR := httptest.NewRecorder()
	h.HandleDeleteCategory(delRR, delReq)
	assert.Equal(t, http.StatusConflict, delRR.Code)

	// Reassign the model, then delete works.
	_, err = h.store.DB.Exec("UPDATE provider_models SET category='standard' WHERE model='m'")
	require.NoError(t, err)
	delRR = httptest.NewRecorder()
	h.HandleDeleteCategory(delRR, delReq)
	assert.Equal(t, http.StatusNoContent, delRR.Code)

	// Default categories are not removable.
	delDefaultReq := httptest.NewRequest(http.MethodDelete, "/models/categories/premium", nil)
	rctx = chi.NewRouteContext()
	rctx.URLParams.Add("name", "premium")
	delDefaultReq = delDefaultReq.WithContext(context.WithValue(delDefaultReq.Context(), chi.RouteCtxKey, rctx))
	delDefaultRR := httptest.NewRecorder()
	h.HandleDeleteCategory(delDefaultRR, delDefaultReq)
	assert.Equal(t, http.StatusBadRequest, delDefaultRR.Code)
}
