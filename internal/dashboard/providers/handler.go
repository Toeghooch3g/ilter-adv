package providers

import (
	"net/http"

	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
	"github.com/ilter-ai/ilter/internal/provider"
)

type Handler struct {
	store       *db.SQLiteStore
	cfg         *config.Config
	lb          *smartrouter.LoadBalancer
	reg         *provider.Registry
	configCache *config.Cache
}

func NewHandler(store *db.SQLiteStore, cfg *config.Config, lb *smartrouter.LoadBalancer, reg *provider.Registry, configCache *config.Cache) *Handler {
	return &Handler{store: store, cfg: cfg, lb: lb, reg: reg, configCache: configCache}
}

func (h *Handler) Providers(w http.ResponseWriter, _ *http.Request) {
	providers := h.lb.GetProviderStatus()

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"providers": providers,
	})
}
