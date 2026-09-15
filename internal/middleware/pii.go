package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/circuitbreaker"
	"github.com/ilter-ai/ilter/internal/features/pii"
	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

type contextKey string

const piiStateKey contextKey = "pii_state"

const piiRedisKey contextKey = "pii_redis"

func GetPIIState(ctx context.Context) *pii.ReversibleState {
	if val := ctx.Value(piiStateKey); val != nil {
		if state, ok := val.(*pii.ReversibleState); ok {
			return state
		}
	}
	return nil
}

func GetPIIRedisStore(ctx context.Context) *PIIRedisStore {
	if val := ctx.Value(piiRedisKey); val != nil {
		if rs, ok := val.(*PIIRedisStore); ok {
			return rs
		}
	}
	return nil
}

// placeholderFindRE matches fully formed PII placeholders (12 hex chars).
var placeholderFindRE = regexp.MustCompile(`PII[:_][A-Z0-9_]+[:_][a-f0-9]{12}`)

func replacePIISingle(text, hash, replacement string) string {
	return regexp.MustCompile("(?i)PII[:_][A-Z0-9_]+[:_]"+regexp.QuoteMeta(hash)).ReplaceAllString(text, replacement)
}

// findPlaceholders extracts unique placeholder strings from text.
func findPlaceholders(text string) []string {
	matches := placeholderFindRE.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	unique := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			unique = append(unique, m)
		}
	}
	return unique
}

// unmaskUsingMappings replaces every placeholder in text that has a hash
// matching a key in mappings with its original value.
func unmaskUsingMappings(text string, mappings map[string]string) string {
	for placeholder, original := range mappings {
		if hash := pii.ExtractHash(placeholder); hash != "" {
			text = replacePIISingle(text, hash, original)
		}
	}
	return text
}

// UnmaskResponse unmasks PII placeholders in text using the per-request
// ReversibleState and the Redis mapping store from context.
func UnmaskResponse(ctx context.Context, text string) string {
	if state := GetPIIState(ctx); state != nil {
		text = unmaskUsingMappings(text, state.GetMappings())
	}

	rs := GetPIIRedisStore(ctx)
	keyID := reqmeta.GetKeyID(ctx)
	if rs != nil && keyID != "" {
		if phs := findPlaceholders(text); len(phs) > 0 {
			text = unmaskUsingMappings(text, rs.GetMappings(ctx, keyID, phs))
		}
	}
	return text
}

// GetPIIMappings returns only the per-request state mappings (not cross-request).
// Cross-request unmapping for streaming is handled by piiResponseWriter via Redis.
func GetPIIMappings(ctx context.Context) map[string]string {
	res := make(map[string]string)
	state := GetPIIState(ctx)
	if state != nil {
		maps.Copy(res, state.GetMappings())
	}
	return res
}

// PIIRedisStore stores and retrieves PII placeholder mappings in Redis.
// Key format: ilter:pii:{keyID}:{placeholder} → original_value
// TTL: 1 hour, sliding (refreshed on every access).
type PIIRedisStore struct {
	guard *circuitbreaker.RedisBreaker
}

func NewPIIRedisStore(guard *circuitbreaker.RedisBreaker) *PIIRedisStore {
	return &PIIRedisStore{guard: guard}
}

func (s *PIIRedisStore) redisKey(keyID, placeholder string) string {
	return fmt.Sprintf("ilter:pii:%s:%s", keyID, placeholder)
}

// StoreMapping stores a placeholder→original mapping with 1h TTL.
func (s *PIIRedisStore) StoreMapping(ctx context.Context, keyID, placeholder, original string) {
	if s == nil || s.guard == nil {
		return
	}
	key := s.redisKey(keyID, placeholder)
	s.guard.Do(ctx, func(c context.Context, rdb *redis.Client) error {
		return rdb.Set(c, key, original, 1*time.Hour).Err()
	})
}

// GetMapping retrieves a single placeholder mapping. Returns (original, true) on hit.
func (s *PIIRedisStore) GetMapping(ctx context.Context, keyID, placeholder string) (string, bool) {
	if s == nil || s.guard == nil {
		return "", false
	}
	key := s.redisKey(keyID, placeholder)
	var val string
	degraded := s.guard.Do(ctx, func(c context.Context, rdb *redis.Client) error {
		v, err := rdb.Get(c, key).Result()
		if err != nil {
			return err
		}
		rdb.Expire(c, key, 1*time.Hour)
		val = v
		return nil
	})
	if degraded || val == "" {
		return "", false
	}
	return val, true
}

// fetchMappingsBatch runs the MGET + pipelined EXPIRE for GetMappings' Redis
// call, populating result with every found placeholder→value pair.
func (s *PIIRedisStore) fetchMappingsBatch(c context.Context, rdb *redis.Client, keys, placeholders []string, result map[string]string) error {
	vals, err := rdb.MGet(c, keys...).Result()
	if err != nil {
		return err
	}
	pipe := rdb.Pipeline()
	for i, val := range vals {
		if val == nil {
			continue
		}
		ph := placeholders[i]
		if s, ok := val.(string); ok {
			result[ph] = s
			pipe.Expire(c, keys[i], 1*time.Hour)
		}
	}
	if _, err := pipe.Exec(c); err != nil {
		slog.Warn("Failed to execute PII batch pipeline", "error", err)
	}
	return nil
}

// GetMappings batch-lookup multiple placeholders. Returns only found mappings.
func (s *PIIRedisStore) GetMappings(ctx context.Context, keyID string, placeholders []string) map[string]string {
	if s == nil || s.guard == nil || len(placeholders) == 0 {
		return nil
	}
	keys := make([]string, len(placeholders))
	for i, ph := range placeholders {
		keys[i] = s.redisKey(keyID, ph)
	}

	result := make(map[string]string, len(placeholders))
	s.guard.Do(ctx, func(c context.Context, rdb *redis.Client) error {
		return s.fetchMappingsBatch(c, rdb, keys, placeholders, result)
	})
	return result
}

// PIIMaskerMiddleware masks, reverses, or blocks PII in request payloads.
type PIIMaskerMiddleware struct {
	masker     *pii.Masker
	store      *db.SQLiteStore
	redisStore *PIIRedisStore
	enabled    atomic.Bool
	cfgCache   *config.Cache
}

func (m *PIIMaskerMiddleware) SetEnabled(on bool) { m.enabled.Store(on) }
func (m *PIIMaskerMiddleware) IsEnabled() bool    { return m.enabled.Load() }

// NewPIIMasker creates a new PIIMaskerMiddleware. Pass an optional PIIRedisStore for cross-request unmask.
// When cfgCache is provided, the enabled state is controlled by the "pii" feature flag
// and automatically updated on config refresh via OnChange callback.
func NewPIIMaskerMiddleware(store *db.SQLiteStore, cfg config.PIIConfig, redisGuard *circuitbreaker.RedisBreaker, cfgCache *config.Cache) *PIIMaskerMiddleware {
	m := &PIIMaskerMiddleware{
		masker:   pii.NewMasker(cfg.Mode, cfg.Patterns),
		store:    store,
		cfgCache: cfgCache,
	}
	if redisGuard != nil {
		m.redisStore = NewPIIRedisStore(redisGuard)
	}

	if cfgCache != nil {
		m.SetEnabled(IsEnabled(cfgCache, "pii"))
		cfgCache.OnChange(func(_ *config.Snapshot) {
			m.SetEnabled(IsEnabled(cfgCache, "pii"))
		})
	} else {
		m.SetEnabled(cfg.Enabled)
	}

	return m
}

// piiResponseWriter intercepts writes to perform response unmasking for reversible PII mode.
type piiResponseWriter struct {
	http.ResponseWriter
	ctx        context.Context
	state      *pii.ReversibleState
	masker     *pii.Masker
	buffer     []byte
	redisStore *PIIRedisStore
	keyID      string
}

func (w *piiResponseWriter) Write(b []byte) (int, error) {
	w.buffer = append(w.buffer, b...)
	err := w.flushBuffer(false)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *piiResponseWriter) flushBuffer(force bool) error {
	if len(w.buffer) == 0 {
		return nil
	}
	s := string(w.buffer)
	writeLen := len(s)
	if !force {
		// Keep last maxPlaceholderLen-1 bytes as tail (split-placeholder safety)
		const maxPlaceholderLen = 32
		if keep := maxPlaceholderLen - 1; writeLen > keep {
			writeLen -= keep
		} else {
			writeLen = 0
		}
	}
	if writeLen > 0 {
		toWrite := s[:writeLen]
		sModified := w.masker.Unmask(toWrite, w.state)
		sModified = w.unmaskFromRedis(sModified)
		_, err := w.ResponseWriter.Write([]byte(sModified))
		if err != nil {
			return err
		}
		w.buffer = w.buffer[writeLen:]
	}
	return nil
}

func (w *piiResponseWriter) unmaskFromRedis(text string) string {
	if w.redisStore == nil || w.keyID == "" {
		return text
	}
	phs := findPlaceholders(text)
	if len(phs) == 0 {
		return text
	}
	mappings := w.redisStore.GetMappings(w.ctx, w.keyID, phs)
	for ph, original := range mappings {
		if hash := pii.ExtractHash(ph); hash != "" {
			text = replacePIISingle(text, hash, original)
		}
	}
	return text
}

func (w *piiResponseWriter) Flush() {
	if err := w.flushBuffer(false); err != nil {
		slog.Debug("pii flush buffer error", "error", err)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// piiPreviewMaxLen caps how much of a PII-affected message is stored/logged
// as a preview.
const piiPreviewMaxLen = 200

// truncatePIIPreview caps s to piiPreviewMaxLen characters for storage/logging.
func truncatePIIPreview(s string) string {
	if len(s) > piiPreviewMaxLen {
		return s[:piiPreviewMaxLen]
	}
	return s
}

// recordPIIEvent inserts one pii_events row, if a store is configured.
func (m *PIIMaskerMiddleware) recordPIIEvent(keyID, clientIP string, match pii.Match, preview string) {
	if m.store == nil {
		return
	}
	_, _ = m.store.DB.Exec("INSERT INTO pii_events (key_id, pii_type, action_taken, masked_prompt_preview, pii_value, client_ip) VALUES (?, ?, ?, ?, ?, ?)", keyID, match.Type, piiActionToAuditLabel(match.Action), preview, match.Value, clientIP)
}

// handleBlockedPII marks the request as PII-blocked in meta, records a
// pii_events row per match, and writes the standard PII-blocked error
// response.
func (m *PIIMaskerMiddleware) handleBlockedPII(w http.ResponseWriter, meta *reqmeta.RequestLoggingMetadata, keyID, clientIP, contentStr string, matches []pii.Match) {
	if meta != nil {
		meta.SetPIIBlocked(true)
	}
	preview := truncatePIIPreview(contentStr)
	for _, match := range matches {
		m.recordPIIEvent(keyID, clientIP, match, preview)
	}
	writePIIBlockedError(w)
}

// recordMaskedPII records request metadata and audit events for a message
// whose content was changed by masking.
func (m *PIIMaskerMiddleware) recordMaskedPII(meta *reqmeta.RequestLoggingMetadata, keyID, clientIP, masked string, matches []pii.Match) {
	preview := truncatePIIPreview(masked)
	if meta != nil {
		meta.SetPIIMasked(true)
		for _, match := range matches {
			meta.AddPIIEvent(match.Type, piiActionToAuditLabel(match.Action), preview, match.Value, clientIP)
		}
	}
	for _, match := range matches {
		m.recordPIIEvent(keyID, clientIP, match, preview)
	}
}

// maskMessageContent detects and masks PII in one message's string content,
// recording audit events as needed. Returns the message's (possibly masked)
// content, whether processing hit a block (the caller must abort the
// request; the blocked-error response has already been written), and
// whether the content actually changed.
func (m *PIIMaskerMiddleware) maskMessageContent(w http.ResponseWriter, meta *reqmeta.RequestLoggingMetadata, state *pii.ReversibleState, keyID, clientIP, contentStr string) (result string, blocked, changed bool) {
	matches := m.masker.DetectPII(contentStr)
	masked, err := m.masker.ProcessText(contentStr, state)
	if err != nil {
		if errors.Is(err, model.ErrPIIBlocked) {
			m.handleBlockedPII(w, meta, keyID, clientIP, contentStr, matches)
			return contentStr, true, false
		}
		masked = contentStr
	}
	if masked == contentStr {
		return contentStr, false, false
	}
	m.recordMaskedPII(meta, keyID, clientIP, masked, matches)
	return masked, false, true
}

// maskRequestMessages masks PII in every string-content message of req in
// place. Returns true if the request was blocked (caller must return
// immediately without proceeding) and true if any message was modified.
func (m *PIIMaskerMiddleware) maskRequestMessages(w http.ResponseWriter, r *http.Request, meta *reqmeta.RequestLoggingMetadata, state *pii.ReversibleState, keyID string, req *model.ChatCompletionRequest) (blocked, modified bool) {
	for i, msg := range req.Messages {
		contentStr, ok := msg.Content.(string)
		if !ok {
			continue
		}
		masked, isBlocked, changed := m.maskMessageContent(w, meta, state, keyID, r.RemoteAddr, contentStr)
		if isBlocked {
			return true, modified
		}
		if changed {
			req.Messages[i].Content = masked
			modified = true
		}
	}
	return false, modified
}

// storeRedisMappings persists every placeholder→original mapping from state
// to Redis, so a later response can unmask them for this key.
func (m *PIIMaskerMiddleware) storeRedisMappings(ctx context.Context, keyID string, state *pii.ReversibleState) {
	if m.redisStore == nil || keyID == "" {
		return
	}
	for placeholder, original := range state.GetMappings() {
		m.redisStore.StoreMapping(ctx, keyID, placeholder, original)
	}
}

// rebuildRequestBody re-marshals req as the new request body when modified
// is true (falling back to the original bytes on marshal failure), or
// restores the original bytes unchanged otherwise.
func rebuildRequestBody(r *http.Request, req model.ChatCompletionRequest, bodyBytes []byte, modified bool) {
	if modified {
		if newBody, err := json.Marshal(req); err == nil {
			r.Body = io.NopCloser(bytes.NewBuffer(newBody))
			r.ContentLength = int64(len(newBody))
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
}

// Handler intercepts request and response payloads.
func (m *PIIMaskerMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled.Load() {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			next.ServeHTTP(w, r)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		_ = r.Body.Close()

		var req model.ChatCompletionRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			next.ServeHTTP(w, r)
			return
		}

		state := pii.NewReversibleState()
		defer state.Clear()

		keyID := reqmeta.GetKeyID(r.Context())
		meta := reqmeta.GetRequestMetadata(r.Context())
		blocked, modified := m.maskRequestMessages(w, r, meta, state, keyID, &req)
		if blocked {
			return
		}

		m.storeRedisMappings(r.Context(), keyID, state)
		rebuildRequestBody(r, req, bodyBytes, modified)

		ctx := context.WithValue(r.Context(), piiStateKey, state)
		ctx = context.WithValue(ctx, piiRedisKey, m.redisStore)
		r = r.WithContext(ctx)

		wrappedWriter := &piiResponseWriter{
			ResponseWriter: w,
			ctx:            ctx,
			state:          state,
			masker:         m.masker,
			redisStore:     m.redisStore,
			keyID:          keyID,
		}

		next.ServeHTTP(wrappedWriter, r)
		if err := wrappedWriter.flushBuffer(true); err != nil {
			slog.Debug("pii final flush error", "error", err)
		}
	})
}

// MaskMessages applies PII masking to all string-typed message content in place.
// Mappings are stored in Redis (if available) for cross-request unmasking.
func (m *PIIMaskerMiddleware) MaskMessages(ctx context.Context, messages []model.Message) error {
	state := pii.NewReversibleState()
	defer state.Clear()

	for i, msg := range messages {
		contentStr, ok := msg.Content.(string)
		if !ok || contentStr == "" {
			continue
		}

		masked, err := m.masker.ProcessText(contentStr, state)
		if err != nil {
			if errors.Is(err, model.ErrPIIBlocked) {
				return err
			}
			continue
		}
		if masked != contentStr {
			messages[i].Content = masked
		}
	}

	m.storeRedisMappings(ctx, reqmeta.GetKeyID(ctx), state)

	return nil
}

func (m *PIIMaskerMiddleware) Masker() *pii.Masker {
	return m.masker
}

// LogPIIEvent records a PII detection to the pii_events table and request metadata.
// Call after ProcessText to ensure maskedPreview contains the masked (safe) version.
func (m *PIIMaskerMiddleware) LogPIIEvent(ctx context.Context, actionTaken, keyID, clientIP string, match pii.Match, maskedPreview string) {
	meta := reqmeta.GetRequestMetadata(ctx)
	if meta != nil {
		if actionTaken == "blocked" {
			meta.SetPIIBlocked(true)
		} else {
			meta.SetPIIMasked(true)
		}
		meta.AddPIIEvent(match.Type, actionTaken, maskedPreview, match.Value, clientIP)
	}
	if m.store != nil {
		preview := maskedPreview
		if len(preview) > 200 {
			preview = preview[:200]
		}
		_, _ = m.store.DB.Exec(
			"INSERT INTO pii_events (key_id, pii_type, action_taken, masked_prompt_preview, pii_value, client_ip) VALUES (?, ?, ?, ?, ?, ?)",
			keyID, match.Type, actionTaken, preview, match.Value, clientIP,
		)
	}
}

// piiActionToAuditLabel maps an internal PII pattern action constant to the
// canonical audit-log label written to the pii_events.action_taken column and
// to request metadata. mask + mask_reversible both surface as "masked",
// log_only surfaces as "logged" (text untouched), and block surfaces as
// "blocked". Unknown values default to "masked".
func piiActionToAuditLabel(action string) string {
	switch action {
	case pii.ActionBlock:
		return "blocked"
	case pii.ActionLogOnly:
		return "logged"
	default:
		return "masked"
	}
}

func writePIIBlockedError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	errResp := map[string]any{
		"error": map[string]any{
			"message": "pii_blocked",
			"type":    "pii_blocked",
			"code":    "pii_blocked",
		},
	}
	if err := json.NewEncoder(w).Encode(errResp); err != nil {
		slog.Error("failed to encode PII blocked response", "error", err)
	}
}
