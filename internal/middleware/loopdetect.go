package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/loopdetect"
	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

// LoopDetectorMiddleware runs loop detection as middleware before the semantic cache.
// This ensures cache hits don't bypass loop detection.
type LoopDetectorMiddleware struct {
	detector *loopdetect.Detector
	store    *db.SQLiteStore
	cfgCache *config.Cache
}

func NewLoopDetectorMiddleware(detector *loopdetect.Detector, store *db.SQLiteStore, cfgCache *config.Cache) *LoopDetectorMiddleware {
	return &LoopDetectorMiddleware{
		detector: detector,
		store:    store,
		cfgCache: cfgCache,
	}
}

// loopCheckApplies reports whether r is a candidate for loop detection: the
// feature is on and it's a chat completions POST.
func (ld *LoopDetectorMiddleware) loopCheckApplies(r *http.Request) bool {
	enabled := true
	if ld.cfgCache != nil {
		enabled = IsEnabled(ld.cfgCache, "loop_detection")
	}
	return enabled && r.Method == "POST" && r.URL.Path == "/v1/chat/completions"
}

// recordLoopEvent inserts a loop_events row if the detector found any
// active signals and a store is configured.
func (ld *LoopDetectorMiddleware) recordLoopEvent(r *http.Request, keyID string, res loopdetect.CheckResult) {
	if res.ActiveSignals == 0 || ld.store == nil {
		return
	}
	action := "alerted"
	switch {
	case res.Blocked:
		action = "blocked"
	case res.Delay > 0:
		action = "throttled"
	}
	_, dbErr := ld.store.DB.Exec(
		"INSERT INTO loop_events (key_id, client_ip, prompt_hash, repeat_count, window_seconds, action_taken) VALUES (?, ?, ?, ?, ?, ?)",
		keyID, r.RemoteAddr, res.PromptHash, res.RepeatCount, res.WindowSeconds, action,
	)
	if dbErr != nil {
		slog.Error("Failed to record loop event in DB", "error", dbErr)
	}
}

// applyLoopResult writes the blocked-error response, waits out any imposed
// delay, or sets the warning header, per res. Returns true if the caller
// must stop processing the request (blocked, or the client disconnected
// during the delay wait).
func applyLoopResult(w http.ResponseWriter, r *http.Request, meta *reqmeta.RequestLoggingMetadata, keyID string, res loopdetect.CheckResult) bool {
	if res.Blocked {
		if meta != nil {
			meta.SetLoopBlocked(true)
		}
		model.WriteJSONError(w, http.StatusTooManyRequests, model.ErrTypeLoopDetected, "Runaway loop detected: multiple loop signals triggered. Request blocked to prevent excessive costs.")
		return true
	}

	if res.Delay > 0 {
		slog.Info("Loop detector delaying request", "key_id", keyID, "delay", res.Delay)
		select {
		case <-time.After(res.Delay):
		case <-r.Context().Done():
			return true
		}
	}

	if res.Warning {
		if meta != nil {
			meta.SetLoopWarning(true)
		}
		w.Header().Set("X-Ilter-Loop-Warning", "true")
	}
	return false
}

func (ld *LoopDetectorMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ld.loopCheckApplies(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Read + re-buffer the body so downstream middlewares can read it too
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		var req model.ChatCompletionRequest
		if err = json.Unmarshal(bodyBytes, &req); err != nil {
			// Pass through unparseable bodies — let the handler fail them further down
			next.ServeHTTP(w, r)
			return
		}

		keyID := reqmeta.GetKeyID(r.Context())
		meta := reqmeta.GetRequestMetadata(r.Context())

		sessionID := r.Header.Get("X-Ilter-Session-Id")
		res, err := ld.detector.Check(keyID, sessionID, req.Messages)
		if err != nil {
			slog.Error("Loop detector check failed", "error", err)
			next.ServeHTTP(w, r)
			return
		}

		ld.recordLoopEvent(r, keyID, res)

		if applyLoopResult(w, r, meta, keyID, res) {
			return
		}

		next.ServeHTTP(w, r)
	})
}
