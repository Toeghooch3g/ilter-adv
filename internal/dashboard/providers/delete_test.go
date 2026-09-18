package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db/dbtest"
	"github.com/ilter-ai/ilter/internal/provider"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDeleteTestHandler builds a handler whose cfg.Providers holds the given
// set, backed by a real store and registry.
func newDeleteTestHandler(t *testing.T, providers []config.ProviderConfig) *Handler {
	t.Helper()
	store := dbtest.New(t)
	cfg := &config.Config{Providers: providers}
	reg := provider.NewRegistry()
	for _, p := range providers {
		reg.Register(provider.NewOpenAIProvider(p))
	}
	return NewHandler(store, cfg, nil, reg, nil)
}

func doDelete(t *testing.T, h *Handler, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/providers/"+name, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.HandleDeleteProvider(rr, req)
	return rr
}

// TestHandleDeleteProvider_RemovesEverything verifies a UI-added provider is
// fully removed: runtime_config rows, model rows, and the registry entry.
func TestHandleDeleteProvider_RemovesEverything(t *testing.T) {
	h := newDeleteTestHandler(t, []config.ProviderConfig{
		{Name: "my-custom", Type: "openai", BaseURL: "https://api.example.com/v1", APIKey: "sk-test"},
	})

	// Seed runtime_config + model rows so we can assert they're cleaned up.
	require.NoError(t, h.store.UpsertRuntimeConfig(context.Background(), "provider", "my-custom",
		`{"name":"my-custom","provider":"openai","base_url":"https://api.example.com/v1"}`, "test"))
	require.NoError(t, h.store.UpsertRuntimeConfig(context.Background(), modelOverridesSection, "my-custom",
		`[{"id":"gpt-4o"}]`, "test"))
	_, err := h.store.DB.Exec("INSERT INTO provider_models (provider, model, category, cost_in, cost_out) VALUES ('my-custom','gpt-4o','standard',0.1,0.2)")
	require.NoError(t, err)

	rr := doDelete(t, h, "my-custom")
	assert.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	_, err = h.store.GetRuntimeConfigEntry(context.Background(), "provider", "my-custom")
	assert.Error(t, err, "provider runtime_config row should be deleted")
	_, err = h.store.GetRuntimeConfigEntry(context.Background(), modelOverridesSection, "my-custom")
	assert.Error(t, err, "model_overrides row should be deleted")

	var modelCount int
	require.NoError(t, h.store.DB.QueryRow("SELECT COUNT(*) FROM provider_models WHERE provider = 'my-custom'").Scan(&modelCount))
	assert.Zero(t, modelCount, "provider_models rows should be deleted")

	if _, err := h.reg.Get("my-custom"); err == nil {
		t.Error("registry should no longer contain the deleted provider")
	}
}

// TestHandleDeleteProvider_UnknownProvider verifies a 404 for a name not in
// cfg.Providers.
func TestHandleDeleteProvider_UnknownProvider(t *testing.T) {
	h := newDeleteTestHandler(t, nil)
	rr := doDelete(t, h, "ghost")
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// TestHandleDeleteProvider_EnvSeededRejected verifies an env-seeded provider
// (ILTER_PROVIDER_<NAME>_API_KEY set) is rejected with a 409 and left intact.
func TestHandleDeleteProvider_EnvSeededRejected(t *testing.T) {
	t.Setenv("ILTER_PROVIDER_OPENAI_API_KEY", "sk-env")
	h := newDeleteTestHandler(t, []config.ProviderConfig{
		{Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-env"},
	})

	rr := doDelete(t, h, "openai")
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "ILTER_PROVIDER_OPENAI_API_KEY")

	// Nothing deleted.
	if _, err := h.reg.Get("openai"); err != nil {
		t.Error("env-seeded provider must stay registered")
	}
}

// TestProviderEnvVarSet sanity checks the env-seeding detection: plural wins,
// empty vars are ignored, unknown names return "".
func TestProviderEnvVarSet(t *testing.T) {
	t.Setenv("ILTER_PROVIDER_OPENAI_API_KEY", "sk-one")
	assert.Equal(t, "ILTER_PROVIDER_OPENAI_API_KEY", providerEnvVarSet("openai"))

	t.Setenv("ILTER_PROVIDER_OPENAI_API_KEYS", "sk-a,sk-b")
	assert.Equal(t, "ILTER_PROVIDER_OPENAI_API_KEYS", providerEnvVarSet("openai"), "plural var wins")

	t.Setenv("ILTER_PROVIDER_OPENAI_API_KEY", "")
	t.Setenv("ILTER_PROVIDER_OPENAI_API_KEYS", "")
	assert.Equal(t, "", providerEnvVarSet("openai"), "empty vars are not env-seeded")

	t.Setenv("ILTER_PROVIDER_GHOST_API_KEY", "")
	assert.Equal(t, "", providerEnvVarSet("ghost"))
}
