package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/ilter-ai/ilter/internal/model/catalog"
)

func (a *App) discoverModels(force bool) {
	cfg := a.cfg
	reg := a.reg
	store := a.store
	discoveryCtx, discoveryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer discoveryCancel()

	cooldown := time.Hour
	discoveredCount := 0
	skippedCount := 0

	for _, pCfg := range cfg.Providers {
		lastDisc, errDisc := store.GetLatestDiscovery(pCfg.Name)
		if !force && errDisc == nil && !lastDisc.IsZero() && time.Since(lastDisc) < cooldown {
			slog.Debug("models still fresh, skipping discovery",
				"provider", pCfg.Name, "last_discovery", lastDisc.Format("15:04:05"))
			skippedCount++
			continue
		}

		p, errGet := reg.Get(pCfg.Name)
		if errGet != nil {
			continue
		}
		discoveredModels, errDisc := p.DiscoverModels(discoveryCtx)
		if errDisc != nil {
			slog.Warn("Failed to discover models for provider", "provider", pCfg.Name, "error", errDisc)
			continue
		}
		for _, info := range discoveredModels {
			catalog.ModelsMu.Lock()
			catalog.Models[info.ID] = append(catalog.Models[info.ID], info)
			discoveredCount++
			catalog.ModelsMu.Unlock()
		}
	}

	if discoveredCount > 0 {
		slog.Debug("discovered new models", "count", discoveredCount)
	}
	if skippedCount > 0 {
		slog.Debug("used cached models", "providers", skippedCount)
	}
}

// The dashboard reads from the DB, so this runs at startup.
func (a *App) syncModelsToDB() {
	store := a.store
	catalog.ModelsMu.RLock()
	byProvider := make(map[string][]catalog.ModelInfo)
	for _, models := range catalog.Models {
		for _, m := range models {
			byProvider[m.Provider] = append(byProvider[m.Provider], m)
		}
	}
	providers := make([]string, 0, len(byProvider))
	for p := range byProvider {
		providers = append(providers, p)
	}
	catalog.ModelsMu.RUnlock()

	if len(providers) == 0 {
		slog.Warn("no models to sync")
		return
	}

	for _, provider := range providers {
		models := byProvider[provider]
		// context.Background() is intentional: this runs at startup, before
		// any request exists.
		if err := store.SaveDiscoveredModels(context.Background(), provider, models); err != nil {
			slog.Error("Failed to sync models to DB for provider", "provider", provider, "error", err)
		} else {
			slog.Debug("models synced", "provider", provider, "count", len(models))
		}
	}

	if err := catalog.LoadFromDB(store.GetAllModelInfo); err != nil {
		slog.Error("Failed to reload models from DB after sync", "error", err)
	}
	slog.Info("models reloaded", "providers", len(providers))

	// Rebuild in-memory routes so the new/updated models are available
	// without a restart. Nil-safe: at boot a.lb is nil here (initLoadBalancer
	// runs after sync), but it reads provider_models directly so routes are
	// correct. At runtime after a seed/manual INSERT, a.lb is non-nil and
	// RebuildProviders syncs the in-memory routes from persisted rows.
	if a.lb != nil && a.reg != nil {
		if err := a.lb.RebuildProviders(a.reg, providerModelEntries(a.store)); err != nil {
			slog.Error("failed to rebuild routes after model sync", "error", err)
		} else {
			slog.Info("routes rebuilt after model sync")
		}
	}
}
