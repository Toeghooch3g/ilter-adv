package dashboard

import (
	"github.com/redis/go-redis/v9"

	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/db/audit"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
)

// Handler holds the shared dependencies used by the dashboard's admin HTTP
// handlers.
type Handler struct {
	store   *db.SQLiteStore
	lb      *smartrouter.LoadBalancer
	redis   *redis.Client
	auditor *audit.SQLiteConfigAuditor
}

// NewAdminHandler builds a Handler from its shared dependencies.
func NewAdminHandler(store *db.SQLiteStore, lb *smartrouter.LoadBalancer, redisClient *redis.Client, auditor *audit.SQLiteConfigAuditor) *Handler {
	return &Handler{
		store:   store,
		lb:      lb,
		redis:   redisClient,
		auditor: auditor,
	}
}
