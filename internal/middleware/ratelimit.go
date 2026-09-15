package middleware

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/circuitbreaker"
	"github.com/ilter-ai/ilter/internal/features/ratelimit"
	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/platform/rediskeys"
	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

// RateLimitMiddleware is the thin HTTP middleware adapter wrapping ratelimit.RateLimiter.
type RateLimitMiddleware struct {
	limiter *ratelimit.RateLimiter
}

// NewRateLimitMiddleware creates a new RateLimitMiddleware adapter.
func NewRateLimitMiddleware(cfg *config.RateLimitConfig, g *circuitbreaker.RedisBreaker, cfgCache *config.Cache) (*RateLimitMiddleware, error) {
	limiter, err := ratelimit.NewRateLimiter(cfg, g, cfgCache)
	if err != nil {
		return nil, err
	}
	return &RateLimitMiddleware{limiter: limiter}, nil
}

// Limiter returns the underlying ratelimit.RateLimiter core.
func (m *RateLimitMiddleware) Limiter() *ratelimit.RateLimiter {
	return m.limiter
}

const defaultRetryAfter = 60

// resolveRateLimitKey determines the key ID to rate-limit by and whether
// the request should bypass rate limiting entirely (feature disabled, admin
// bypass, or no Redis guard configured).
func (m *RateLimitMiddleware) resolveRateLimitKey(r *http.Request) (keyID string, bypass bool) {
	enabled := m.limiter.Cfg.Enabled
	if m.limiter.CfgCache != nil {
		enabled = config.IsEnabled(m.limiter.CfgCache, "rate_limit")
	}
	if !enabled {
		return "", true
	}

	keyID = reqmeta.GetKeyID(r.Context())
	if keyID == "admin" && m.limiter.Cfg.AdminBypass {
		return "", true
	}
	if keyID == "" {
		keyID = "anonymous"
	}

	if m.limiter.Guard == nil {
		return "", true
	}
	return keyID, false
}

// resolveKeyLimit returns the per-minute request limit for the caller's API
// key: the value carried in context if positive, else the configured default.
func (m *RateLimitMiddleware) resolveKeyLimit(r *http.Request) int64 {
	keyLimit := int64(m.limiter.Cfg.DefaultRPM)
	if rateLimitVal := r.Context().Value(reqmeta.APIKeyRateLimitContextKey); rateLimitVal != nil {
		if l, ok := rateLimitVal.(int); ok && l > 0 {
			keyLimit = int64(l)
		}
	}
	return keyLimit
}

// rateLimitState tracks the tightest active limit and whether the request
// is over any configured limit (key/user/group), across the checks in Handler.
type rateLimitState struct {
	activeLimit int64
	limited     bool
	retryAfter  int
}

// applyTighterLimit narrows activeLimit to limit if limit is stricter.
func (s *rateLimitState) applyTighterLimit(limit int64) {
	if limit < s.activeLimit {
		s.activeLimit = limit
	}
}

// markLimited flags the request as rate-limited and raises retryAfter if
// the new value is a longer wait than what's already recorded.
func (s *rateLimitState) markLimited(retryAfter int) {
	s.limited = true
	if retryAfter > 0 && retryAfter > s.retryAfter {
		s.retryAfter = retryAfter
	}
}

// checkUserRateLimit applies the user-level rate limit (if any) to state.
func (m *RateLimitMiddleware) checkUserRateLimit(ctx context.Context, userID int, now time.Time, state *rateLimitState) {
	userLimit := m.limiter.GetUserRateLimit(ctx, userID)
	if userLimit <= 0 {
		return
	}
	userMinuteKey := rediskeys.UserRateLimitCounterKey(userID, now)
	userCount, err := m.limiter.IncrementAndGetCount(ctx, userMinuteKey)
	if err != nil {
		return
	}
	state.applyTighterLimit(int64(userLimit))
	if userCount > int64(userLimit) {
		state.markLimited(m.limiter.GetUserRetryAfter(ctx, userID))
	}
}

// checkGroupRateLimit applies one group's rate limit (if any) to state.
func (m *RateLimitMiddleware) checkGroupRateLimit(ctx context.Context, groupID int, now time.Time, state *rateLimitState) {
	groupLimit := m.limiter.GetGroupRateLimit(ctx, groupID)
	if groupLimit <= 0 {
		return
	}
	groupMinuteKey := rediskeys.GroupRateLimitCounterKey(groupID, now)
	groupCount, err := m.limiter.IncrementAndGetCount(ctx, groupMinuteKey)
	if err != nil {
		return
	}
	state.applyTighterLimit(int64(groupLimit))
	if groupCount > int64(groupLimit) {
		state.markLimited(m.limiter.GetGroupRetryAfter(ctx, groupID))
	}
}

// writeRateLimitHeaders sets the X-RateLimit-Limit/Remaining response headers.
func writeRateLimitHeaders(w http.ResponseWriter, activeLimit, keyCount int64) {
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(activeLimit, 10))
	remaining := max(activeLimit-keyCount, 0)
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
}

// computeRateLimitState increments the key's request counter and evaluates
// it against the key/user/group limits, returning the resulting state and
// the key's current count.
func (m *RateLimitMiddleware) computeRateLimitState(ctx context.Context, keyID string, now time.Time, keyLimit int64) (*rateLimitState, int64, error) {
	minuteKey := rediskeys.RateLimitKey(keyID, now)
	keyCount, err := m.limiter.IncrementAndGetCount(ctx, minuteKey)
	if err != nil {
		return nil, 0, err
	}

	state := &rateLimitState{activeLimit: keyLimit}
	if keyCount > state.activeLimit {
		state.limited = true
	}

	if userID := reqmeta.GetUserID(ctx); userID != nil {
		m.checkUserRateLimit(ctx, *userID, now, state)
	}
	for _, groupID := range reqmeta.GetGroupIDs(ctx) {
		m.checkGroupRateLimit(ctx, groupID, now, state)
	}
	return state, keyCount, nil
}

// writeRateLimitedResponse marks the request as rate-limited in meta and
// writes the standard 429 response.
func writeRateLimitedResponse(w http.ResponseWriter, meta *reqmeta.RequestLoggingMetadata, retryAfter int) {
	if meta != nil {
		meta.SetRateLimited(true)
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	model.WriteJSONError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Rate limit exceeded")
}

// Handler returns the Chi-compatible HTTP middleware handler.
func (m *RateLimitMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyID, bypass := m.resolveRateLimitKey(r)
		if bypass {
			next.ServeHTTP(w, r)
			return
		}

		now := time.Now()
		ctx := r.Context()
		state, keyCount, err := m.computeRateLimitState(ctx, keyID, now, m.resolveKeyLimit(r))
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		writeRateLimitHeaders(w, state.activeLimit, keyCount)

		retryAfter := state.retryAfter
		if retryAfter <= 0 {
			retryAfter = defaultRetryAfter
		}

		if state.limited {
			writeRateLimitedResponse(w, reqmeta.GetRequestMetadata(ctx), retryAfter)
			return
		}

		next.ServeHTTP(w, r)
	})
}
