package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db/dbtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	store := dbtest.New(t)
	cfg := &config.Config{Providers: []config.ProviderConfig{}}
	// reg and configCache are not needed by HandleCreateProvider directly
	// (it persists and refreshes the cache, which the app wires up).
	return NewHandler(store, cfg, nil, nil, nil)
}

func postCreate(t *testing.T, h *Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/providers/create", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	h.HandleCreateProvider(rr, req)
	return rr
}

// TestHandleCreateProvider_CreatesCustomProvider verifies a custom OpenAI-
// compatible provider is defaulted to the "openai" type and persisted to the
// runtime_config "provider" section.
func TestHandleCreateProvider_CreatesCustomProvider(t *testing.T) {
	h := newTestHandler(t)

	rr := postCreate(t, h, map[string]any{
		"name":     "my-custom",
		"base_url": "https://api.example.com/v1",
		"api_key":  "sk-test",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	entry, err := h.store.GetRuntimeConfigEntry(context.Background(), "provider", "my-custom")
	require.NoError(t, err)
	require.NotNil(t, entry)

	var reg map[string]any
	require.NoError(t, json.Unmarshal([]byte(entry.Value), &reg))
	assert.Equal(t, "openai", reg["provider"], "custom providers default to the openai type")
	assert.Equal(t, "https://api.example.com/v1", reg["base_url"])
	assert.Equal(t, "sk-test", reg["api_secret_key"])
}

// TestHandleCreateProvider_Validation verifies bad inputs are rejected.
func TestHandleCreateProvider_Validation(t *testing.T) {
	h := newTestHandler(t)

	t.Run("missing name", func(t *testing.T) {
		rr := postCreate(t, h, map[string]any{"base_url": "https://x/v1"})
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("missing or relative base_url", func(t *testing.T) {
		rr := postCreate(t, h, map[string]any{"name": "a", "base_url": "/v1"})
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// TestHandleCreateProvider_DuplicateRejected verifies a name collision returns
// a conflict and does not overwrite the existing provider.
func TestHandleCreateProvider_DuplicateRejected(t *testing.T) {
	h := newTestHandler(t)

	first := postCreate(t, h, map[string]any{"name": "dup", "base_url": "https://a.example.com/v1"})
	require.Equal(t, http.StatusOK, first.Code)

	second := postCreate(t, h, map[string]any{"name": "dup", "base_url": "https://b.example.com/v1"})
	assert.Equal(t, http.StatusConflict, second.Code)

	entry, err := h.store.GetRuntimeConfigEntry(context.Background(), "provider", "dup")
	require.NoError(t, err)
	assert.Contains(t, entry.Value, "https://a.example.com", "original provider must be preserved")
}
