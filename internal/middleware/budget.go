package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/budget"
	"github.com/ilter-ai/ilter/internal/features/circuitbreaker"
	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/platform/rediskeys"
	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

// BudgetMiddleware is the thin HTTP middleware adapter wrapping budget.Enforcer.
type BudgetMiddleware struct {
	enforcer *budget.Enforcer
}

// NewBudgetMiddleware creates a new BudgetMiddleware adapter.
func NewBudgetMiddleware(cfg config.BudgetConfig, g *circuitbreaker.RedisBreaker, store *db.SQLiteStore, cfgCache *config.Cache) *BudgetMiddleware {
	enforcer := budget.NewEnforcer(cfg, g, store, cfgCache)
	return &BudgetMiddleware{enforcer: enforcer}
}

// Enforcer returns the underlying budget.Enforcer core.
func (m *BudgetMiddleware) Enforcer() *budget.Enforcer {
	return m.enforcer
}

// Handler returns the Chi-compatible HTTP middleware handler.
func (m *BudgetMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.isEnabled() {
			next.ServeHTTP(w, r)
			return
		}

		keyID := reqmeta.GetKeyID(r.Context())
		if keyID == "" || m.enforcer.Guard == nil {
			next.ServeHTTP(w, r)
			return
		}

		meta := reqmeta.GetRequestMetadata(r.Context())
		now := time.Now()
		monthlyLimitMicro, dailyLimitMicro := m.resolveLimits(r.Context())

		monthlySpent, dailySpent, monthlyExceeded, dailyExceeded := m.enforcer.ReadBudgetState(r.Context(), keyID, now, monthlyLimitMicro, dailyLimitMicro)

		monthlyRemaining := max(monthlyLimitMicro-monthlySpent, 0)
		dailyRemaining := max(dailyLimitMicro-dailySpent, 0)
		writeBudgetHeaders(w, monthlyLimitMicro, monthlyRemaining, dailyLimitMicro, dailyRemaining)

		if respondIfExceeded(w, meta, monthlyExceeded, dailyExceeded) {
			return
		}

		monthlyRemaining, dailyRemaining = m.refineRemainingFromRedis(r.Context(), now, monthlyLimitMicro, dailyLimitMicro, monthlyRemaining, dailyRemaining)

		w.Header().Set("X-Budget-Remaining", fmt.Sprintf("%.4f", float64(math.Max(0, float64(monthlyRemaining)))/1_000_000))
		w.Header().Set("X-Budget-Daily-Remaining", fmt.Sprintf("%.4f", float64(math.Max(0, float64(dailyRemaining)))/1_000_000))
		if respondIfExceeded(w, meta, monthlyLimitMicro > 0 && monthlyRemaining <= 0, dailyLimitMicro > 0 && dailyRemaining <= 0) {
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isEnabled reports whether budget enforcement is currently on, preferring
// the live config-cache feature flag over the static config default.
func (m *BudgetMiddleware) isEnabled() bool {
	if m.enforcer.CfgCache != nil {
		return config.IsEnabled(m.enforcer.CfgCache, "budget")
	}
	return m.enforcer.Cfg.Enabled
}

// resolveLimits computes the effective monthly/daily budget limits (in micro-USD),
// preferring per-request overrides carried in ctx over the configured defaults.
func (m *BudgetMiddleware) resolveLimits(ctx context.Context) (monthlyLimitMicro, dailyLimitMicro int64) {
	monthlyLimitMicro = budget.CostToMicrosCeil(m.enforcer.Cfg.DefaultMonthlyLimit)
	if b, ok := reqmeta.GetAPIKeyBudget(ctx); ok && b > 0 {
		monthlyLimitMicro = budget.CostToMicrosCeil(b)
	}
	dailyLimitMicro = budget.CostToMicrosCeil(m.enforcer.Cfg.DefaultDailyLimit)
	if d, ok := reqmeta.GetAPIKeyDailyLimit(ctx); ok && d > 0 {
		dailyLimitMicro = budget.CostToMicrosCeil(d)
	}
	return monthlyLimitMicro, dailyLimitMicro
}

// writeBudgetHeaders sets the X-Budget-* response headers from the resolved limits/remaining.
func writeBudgetHeaders(w http.ResponseWriter, monthlyLimitMicro, monthlyRemaining, dailyLimitMicro, dailyRemaining int64) {
	w.Header().Set("X-Budget-Limit", fmt.Sprintf("%.4f", float64(monthlyLimitMicro)/1_000_000))
	w.Header().Set("X-Budget-Remaining", fmt.Sprintf("%.4f", float64(monthlyRemaining)/1_000_000))
	w.Header().Set("X-Budget-Daily-Limit", fmt.Sprintf("%.4f", float64(dailyLimitMicro)/1_000_000))
	w.Header().Set("X-Budget-Daily-Remaining", fmt.Sprintf("%.4f", float64(dailyRemaining)/1_000_000))
}

// respondIfExceeded writes a 429 budget_exceeded error for whichever of
// monthlyExceeded/dailyExceeded is true (monthly takes precedence), marks it
// in request metadata, and reports whether it did so.
func respondIfExceeded(w http.ResponseWriter, meta *reqmeta.RequestLoggingMetadata, monthlyExceeded, dailyExceeded bool) bool {
	switch {
	case monthlyExceeded:
		return respondBudgetExceeded(w, meta, "Budget limit exceeded")
	case dailyExceeded:
		return respondBudgetExceeded(w, meta, "Daily budget limit exceeded")
	default:
		return false
	}
}

// respondBudgetExceeded writes a 429 budget_exceeded error and marks it in
// request metadata.
func respondBudgetExceeded(w http.ResponseWriter, meta *reqmeta.RequestLoggingMetadata, msg string) bool {
	if meta != nil {
		meta.SetBudgetExceeded(true)
	}
	model.WriteJSONError(w, http.StatusTooManyRequests, "budget_exceeded", msg)
	return true
}

// refineRemainingFromRedis tightens the monthly/daily remaining budget using
// live Redis counters for the request's user and group(s), taking the
// minimum across key-level, user-level, and group-level remaining amounts.
func (m *BudgetMiddleware) refineRemainingFromRedis(ctx context.Context, now time.Time, monthlyLimitMicro, dailyLimitMicro, monthlyRemaining, dailyRemaining int64) (int64, int64) {
	if uid := reqmeta.GetUserID(ctx); uid != nil {
		userKey := rediskeys.UserBudgetKey(*uid, now)
		userDayKey := rediskeys.UserDailyBudgetKey(*uid, now)
		monthlyRemaining, dailyRemaining = m.tightenRemaining(ctx, userKey, userDayKey, monthlyLimitMicro, dailyLimitMicro, monthlyRemaining, dailyRemaining)
	}
	for _, gid := range reqmeta.GetGroupIDs(ctx) {
		gKey := rediskeys.GroupBudgetKey(gid, now)
		gDayKey := rediskeys.GroupDailyBudgetKey(gid, now)
		monthlyRemaining, dailyRemaining = m.tightenRemaining(ctx, gKey, gDayKey, monthlyLimitMicro, dailyLimitMicro, monthlyRemaining, dailyRemaining)
	}
	return monthlyRemaining, dailyRemaining
}

// tightenRemaining reads the monthly/daily spend counters at monthlyKey/dailyKey
// from Redis and lowers monthlyRemaining/dailyRemaining if those counters imply
// a tighter budget than what's already tracked.
func (m *BudgetMiddleware) tightenRemaining(ctx context.Context, monthlyKey, dailyKey string, monthlyLimitMicro, dailyLimitMicro, monthlyRemaining, dailyRemaining int64) (int64, int64) {
	m.enforcer.Guard.Do(ctx, func(cctx context.Context, cl *redis.Client) error {
		monthlyRemaining = tightenFromCounter(cctx, cl, monthlyKey, monthlyLimitMicro, monthlyRemaining, true)
		dailyRemaining = tightenFromCounter(cctx, cl, dailyKey, dailyLimitMicro, dailyRemaining, dailyLimitMicro > 0)
		return nil
	})
	return monthlyRemaining, dailyRemaining
}

// tightenFromCounter reads the spend counter at key from Redis and returns a
// tighter remaining value if the counter implies less budget is left than
// remaining already reflects. A no-op (returns remaining unchanged) when
// enabled is false or the counter can't be read/parsed.
func tightenFromCounter(ctx context.Context, cl *redis.Client, key string, limitMicro, remaining int64, enabled bool) int64 {
	if !enabled {
		return remaining
	}
	s, err := cl.Get(ctx, key).Result()
	if err != nil {
		return remaining
	}
	v, pe := strconv.ParseInt(s, 10, 64)
	if pe != nil {
		return remaining
	}
	if rem := limitMicro - v; rem < remaining {
		return rem
	}
	return remaining
}
