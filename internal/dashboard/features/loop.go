package features

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/loopdetect"
)

// loopSettingsRequest is the JSON shape for GET/PUT /api/loop-settings.
type loopSettingsRequest struct {
	RateThreshold         int     `json:"rate_threshold"`
	FingerprintWindow     int     `json:"fingerprint_window"`
	FingerprintDuplicates int     `json:"fingerprint_duplicates"`
	CostWindow            string  `json:"cost_window"`
	CostThreshold         float64 `json:"cost_threshold"`
	SessionMaxRequests    int     `json:"session_max_requests"`
	OutputLoopMode        string  `json:"output_loop_mode"`
	OutputLoopThreshold   int     `json:"output_loop_threshold"`
	OutputMinSentence     int     `json:"output_min_sentence_len"`
}

// toAPI converts the internal LoopSettingsConfig to the API response shape.
func toAPI(cfg config.LoopSettingsConfig) loopSettingsRequest {
	return loopSettingsRequest{
		RateThreshold:         cfg.RateThreshold,
		FingerprintWindow:     cfg.FingerprintWindow,
		FingerprintDuplicates: cfg.FingerprintDuplicates,
		CostWindow:            cfg.CostWindow.String(),
		CostThreshold:         cfg.CostThreshold,
		SessionMaxRequests:    cfg.SessionMaxRequests,
		OutputLoopMode:        cfg.OutputLoopMode,
		OutputLoopThreshold:   cfg.OutputLoopThreshold,
		OutputMinSentence:     cfg.OutputMinSentence,
	}
}

func validLoopMode(mode string) bool {
	return mode == "off" || mode == "observe" || mode == "enforce"
}

// mergeCostWindow validates and merges costWindow into out, if set.
func mergeCostWindow(out *config.LoopSettingsConfig, costWindow string) error {
	if costWindow == "" {
		return nil
	}
	d, err := time.ParseDuration(costWindow)
	if err != nil {
		return fmt.Errorf("invalid cost_window: %w", err)
	}
	if d <= 0 {
		return fmt.Errorf("cost_window must be positive, got %s", costWindow)
	}
	out.CostWindow = d
	return nil
}

// mergeOutputLoopMode validates and merges mode into out, if set.
func mergeOutputLoopMode(out *config.LoopSettingsConfig, mode string) error {
	if mode == "" {
		return nil
	}
	if !validLoopMode(mode) {
		return fmt.Errorf("invalid output_loop_mode: %q (must be off/observe/enforce)", mode)
	}
	out.OutputLoopMode = mode
	return nil
}

// mergeOutputLoopThreshold validates and merges threshold into out, if set.
func mergeOutputLoopThreshold(out *config.LoopSettingsConfig, threshold int) error {
	if threshold <= 0 {
		return nil
	}
	if threshold < 2 {
		return fmt.Errorf("output_loop_threshold must be >= 2, got %d", threshold)
	}
	out.OutputLoopThreshold = threshold
	return nil
}

// mergeOutputMinSentence validates and merges minSentence into out, if set.
func mergeOutputMinSentence(out *config.LoopSettingsConfig, minSentence int) error {
	if minSentence <= 0 {
		return nil
	}
	if minSentence < 1 {
		return fmt.Errorf("output_min_sentence_len must be >= 1, got %d", minSentence)
	}
	out.OutputMinSentence = minSentence
	return nil
}

// fromAPI validates and merges the API request into the existing config.
func fromAPI(req loopSettingsRequest, fallback config.LoopSettingsConfig) (config.LoopSettingsConfig, error) {
	out := fallback

	if req.RateThreshold > 0 {
		out.RateThreshold = req.RateThreshold
	}
	if req.FingerprintWindow > 0 {
		out.FingerprintWindow = req.FingerprintWindow
	}
	if req.FingerprintDuplicates > 0 {
		out.FingerprintDuplicates = req.FingerprintDuplicates
	}
	if err := mergeCostWindow(&out, req.CostWindow); err != nil {
		return out, err
	}
	if req.CostThreshold >= 0 {
		out.CostThreshold = req.CostThreshold
	}
	if req.SessionMaxRequests > 0 {
		out.SessionMaxRequests = req.SessionMaxRequests
	}
	if err := mergeOutputLoopMode(&out, req.OutputLoopMode); err != nil {
		return out, err
	}
	if err := mergeOutputLoopThreshold(&out, req.OutputLoopThreshold); err != nil {
		return out, err
	}
	if err := mergeOutputMinSentence(&out, req.OutputMinSentence); err != nil {
		return out, err
	}

	return out, nil
}

// loopSettingsMu serializes concurrent read/write to cfg.CostGuard.LoopSettings.
var loopSettingsMu sync.RWMutex

// SetLoopDetector wires the live detector instance so saved settings take
// effect immediately instead of only on next process restart.
func (h *Handler) SetLoopDetector(d *loopdetect.Detector) {
	h.loopDetector = d
}

// persistLoopSettings writes updated's fields into runtime_config so they
// survive a restart, logging (not failing) on a per-key write error.
func (h *Handler) persistLoopSettings(ctx context.Context, updated config.LoopSettingsConfig) {
	if h.store == nil {
		return
	}
	for key, value := range config.LoopSettingsToRuntimeConfigValues(updated) {
		if err := h.store.UpsertRuntimeConfig(ctx, "loop_settings", key, value, "dashboard"); err != nil {
			slog.Error("Failed to persist loop setting", "key", key, "error", err)
		}
	}
}

func (h *Handler) handleLoopSettingsPut(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req loopSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}

	loopSettingsMu.RLock()
	updated, err := fromAPI(req, h.cfg.CostGuard.LoopSettings)
	loopSettingsMu.RUnlock()
	if err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	loopSettingsMu.Lock()
	h.cfg.CostGuard.LoopSettings = updated
	loopSettingsMu.Unlock()

	// Apply live so the change takes effect immediately, not just after restart.
	if h.loopDetector != nil {
		h.loopDetector.UpdateSettings(updated)
	}

	h.persistLoopSettings(r.Context(), updated)

	slog.Info(
		"Loop settings updated via dashboard",
		"rate_threshold", updated.RateThreshold,
		"output_loop_mode", updated.OutputLoopMode,
		"output_loop_threshold", updated.OutputLoopThreshold,
	)

	model.WriteJSON(w, http.StatusOK, toAPI(updated))
}

func (h *Handler) HandleLoopSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		loopSettingsMu.RLock()
		resp := toAPI(h.cfg.CostGuard.LoopSettings)
		loopSettingsMu.RUnlock()
		model.WriteJSON(w, http.StatusOK, resp)

	case http.MethodPut:
		h.handleLoopSettingsPut(w, r)

	default:
		model.WriteJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET and PUT are accepted")
	}
}
