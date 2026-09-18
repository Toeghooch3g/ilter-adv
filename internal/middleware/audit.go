package middleware

import (
	"context"
	"log/slog"
	"sync"

	"github.com/ilter-ai/ilter/internal/db"
)

// AuditLogEntry holds one request's audit data queued for asynchronous
// persistence to the audit_log table.
type AuditLogEntry struct {
	KeyID               string
	Model               string
	Provider            string
	PromptTokens        int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	TotalCost           float64
	LatencyMs           int
	StatusCode          int
	CacheHit            bool
	PromptPreview       string
	RequestBody         string
	ResponseBody        string
	ComplexityScore     float64
	IPAddress           string
}

// AuditLoggerMiddleware asynchronously persists audit log entries to the
// database via a buffered channel and background worker.
type AuditLoggerMiddleware struct {
	store *db.SQLiteStore
	ch    chan AuditLogEntry
	wg    sync.WaitGroup
	done  chan struct{}
}

// NewAuditLoggerMiddleware creates an AuditLoggerMiddleware backed by store
// and starts its background worker goroutine.
func NewAuditLoggerMiddleware(store *db.SQLiteStore) *AuditLoggerMiddleware {
	l := &AuditLoggerMiddleware{
		store: store,
		ch:    make(chan AuditLogEntry, 1000),
		done:  make(chan struct{}),
	}
	l.wg.Go(func() {
		l.worker()
	})
	return l
}

func (l *AuditLoggerMiddleware) worker() {
	for {
		select {
		case entry := <-l.ch:
			_, err := l.store.DB.ExecContext(
				context.Background(),
				`INSERT INTO audit_log
					(key_id, model, provider, prompt_tokens, completion_tokens, cached_tokens, cache_creation_tokens, total_cost,
					 latency_ms, status_code, cache_hit, prompt_preview, request_body, response_body, complexity_score, client_ip)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				nullIfEmpty(entry.KeyID),
				entry.Model,
				entry.Provider,
				entry.PromptTokens,
				entry.CompletionTokens,
				entry.CachedTokens,
				entry.CacheCreationTokens,
				entry.TotalCost,
				entry.LatencyMs,
				entry.StatusCode,
				boolToInt(entry.CacheHit),
				nullIfEmpty(entry.PromptPreview),
				nullIfEmpty(entry.RequestBody),
				nullIfEmpty(entry.ResponseBody),
				entry.ComplexityScore,
				nullIfEmpty(entry.IPAddress),
			)
			if err != nil {
				slog.Error("Failed to write audit log", "error", err)
			}
		case <-l.done:
			return
		}
	}
}

// LogAsync enqueues entry for asynchronous persistence, dropping it and
// logging a warning if the internal buffer is full.
func (l *AuditLoggerMiddleware) LogAsync(entry AuditLogEntry) {
	select {
	case l.ch <- entry:
	default:
		slog.Warn("audit log channel full, dropping entry")
	}
}

// Close stops the background worker and waits for it to finish.
func (l *AuditLoggerMiddleware) Close() {
	close(l.done)
	l.wg.Wait()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
