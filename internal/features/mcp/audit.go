package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ilter-ai/ilter/internal/db"
)

// AuditEntry is a single MCP tool-call record queued for asynchronous
// persistence by AuditLogger.
type AuditEntry struct {
	APIKeyID   string
	Tool       string
	ServerID   string
	Method     string
	Params     string
	DurationMs float64
	StatusCode int
	Success    bool
	ErrorMsg   string
	ClientIP   string
}

// AuditLogger asynchronously persists MCP audit entries to the database
// through a buffered channel and a single background worker.
type AuditLogger struct {
	store *db.SQLiteStore
	ch    chan AuditEntry
	wg    sync.WaitGroup
	done  chan struct{}
}

// NewAuditLogger creates an AuditLogger backed by store and starts its
// background worker goroutine.
func NewAuditLogger(store *db.SQLiteStore) *AuditLogger {
	l := &AuditLogger{
		store: store,
		ch:    make(chan AuditEntry, 1000),
		done:  make(chan struct{}),
	}
	l.wg.Go(func() {
		l.worker()
	})
	return l
}

func (l *AuditLogger) worker() {
	for {
		select {
		case entry, ok := <-l.ch:
			if !ok {
				return
			}
			l.persist(entry)
		case <-l.done:
			return
		}
	}
}

func (l *AuditLogger) persist(entry AuditEntry) {
	query := `INSERT INTO mcp_audit_log
	(key_id, tool, server_id, method, params, duration_ms, status_code, success, error_msg, client_ip)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	paramsJSON := sanitizedParamsJSON(entry.Params)

	// This runs on the background worker goroutine, decoupled from any
	// request; there is no caller context to inherit here.
	_, err := l.store.DB.ExecContext(
		context.Background(),
		query,
		nullIfEmpty(entry.APIKeyID),
		entry.Tool,
		nullIfEmpty(entry.ServerID),
		entry.Method,
		paramsJSON,
		entry.DurationMs,
		entry.StatusCode,
		boolToInt(entry.Success),
		nullIfEmpty(entry.ErrorMsg),
		nullIfEmpty(entry.ClientIP),
	)
	if err != nil {
		mcpLog.Error("failed to write audit log", "error", err)
	}
}

// sanitizedParamsJSON returns params re-marshaled as JSON with sensitive
// keys redacted, or "{}" if params is empty or not valid JSON.
func sanitizedParamsJSON(params string) string {
	paramsJSON := "{}"
	if params == "" {
		return paramsJSON
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(params), &m); err != nil {
		return paramsJSON
	}
	sanitized := make(map[string]any)
	for k, v := range m {
		if isSensitiveParam(k) {
			sanitized[k] = "***"
		} else {
			sanitized[k] = v
		}
	}
	if b, err := json.Marshal(sanitized); err == nil {
		paramsJSON = string(b)
	}
	return paramsJSON
}

// LogAsync enqueues entry for asynchronous persistence, dropping it and
// logging a warning if the internal buffer is full.
func (l *AuditLogger) LogAsync(entry AuditEntry) {
	select {
	case l.ch <- entry:
	default:
		mcpLog.Warn("audit log channel full, dropping entry")
	}
}

// Close stops the background worker and blocks until it has drained.
func (l *AuditLogger) Close() {
	close(l.done)
	l.wg.Wait()
}

func isSensitiveParam(key string) bool {
	sensitive := map[string]bool{
		"api_key":       true,
		"apiKey":        true,
		"password":      true,
		"secret":        true,
		"token":         true,
		"authorization": true,
	}
	return sensitive[key]
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

// AuditFilter narrows an AuditLogger.Query call by tool, server, method,
// success, time range, and call origin.
type AuditFilter struct {
	Tool     string
	ServerID string
	Method   string
	Success  *bool
	From     string
	To       string
	Limit    int
	Offset   int
	// Source narrows results by call origin using the server_id sentinel the
	// OpenAPI bridge logs under: "mcp" excludes server_id='openapi' rows,
	// "openapi" includes only them. Empty means no filtering by source.
	Source string
}

// AuditLogEntry is a single row returned by AuditLogger.Query, shaped for
// API/UI consumption.
type AuditLogEntry struct {
	ID         int     `json:"id"`
	APIKeyID   *string `json:"key_id,omitempty"`
	Tool       string  `json:"tool"`
	ServerID   string  `json:"server_id"`
	Method     string  `json:"method"`
	Params     string  `json:"params,omitempty"`
	DurationMs float64 `json:"duration_ms"`
	StatusCode int     `json:"status_code"`
	Success    bool    `json:"success"`
	ErrorMsg   *string `json:"error_msg,omitempty"`
	ClientIP   *string `json:"client_ip,omitempty"`
	CreatedAt  string  `json:"created_at"`
}

// Query returns audit log entries matching filter, along with the total
// count of matching rows (ignoring filter.Limit/filter.Offset).
func (l *AuditLogger) Query(ctx context.Context, filter AuditFilter) ([]AuditLogEntry, int, error) {
	conds, args := buildAuditConditions(filter)

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	countSQL := "SELECT COUNT(*) FROM mcp_audit_log" + where
	if err := l.store.DB.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit logs: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset := filter.Offset

	dataSQL := `SELECT id, key_id, tool, server_id, method, params,
		duration_ms, status_code, success, error_msg, client_ip, created_at
		FROM mcp_audit_log` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := l.store.DB.QueryContext(ctx, dataSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query audit logs: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	entries, err := scanAuditLogEntries(rows)
	if err != nil {
		return nil, 0, err
	}

	return entries, total, nil
}

// buildAuditConditions translates filter into SQL WHERE-clause fragments and
// their positional args, in the order they must be bound.
func buildAuditConditions(filter AuditFilter) ([]string, []any) {
	args := []any{}
	conds := []string{}

	if filter.Tool != "" {
		conds = append(conds, "tool = ?")
		args = append(args, filter.Tool)
	}
	if filter.ServerID != "" {
		conds = append(conds, "server_id = ?")
		args = append(args, filter.ServerID)
	}
	if filter.Method != "" {
		conds = append(conds, "method = ?")
		args = append(args, filter.Method)
	}
	switch filter.Source {
	case "openapi":
		conds = append(conds, "server_id = 'openapi'")
	case "mcp":
		conds = append(conds, "server_id != 'openapi'")
	}
	if filter.Success != nil {
		if *filter.Success {
			conds = append(conds, "success = 1")
		} else {
			conds = append(conds, "success = 0")
		}
	}
	// datetime(...) on both sides: created_at is a bare "YYYY-MM-DD HH:MM:SS"
	// string, while the frontend sends a JS Date.toISOString() value
	// ("...T....000Z") — already a precise timestamp, not a bare date, so no
	// end-of-day suffix is needed (appending " 23:59:59" to a full timestamp
	// only produced a doubly-malformed string). A raw string comparison
	// between the two formats is lexicographically wrong (space < 'T') and
	// silently drops same-day rows; datetime() normalizes both before comparing.
	if filter.From != "" {
		conds = append(conds, "datetime(created_at) >= datetime(?)")
		args = append(args, filter.From)
	}
	if filter.To != "" {
		conds = append(conds, "datetime(created_at) <= datetime(?)")
		args = append(args, filter.To)
	}

	return conds, args
}

// auditRowNulls holds the nullable columns of one mcp_audit_log row, scanned
// as sql.Null* so they can be copied into an AuditLogEntry's pointer/plain
// fields afterward.
type auditRowNulls struct {
	keyID, serverID, params, errorMsg, clientIP, createdAt sql.NullString
	statusCode, successInt                                 sql.NullInt64
	durationMs                                             sql.NullFloat64
}

// scanAuditLogEntries reads every row of rows into an AuditLogEntry slice.
func scanAuditLogEntries(rows *sql.Rows) ([]AuditLogEntry, error) {
	var entries []AuditLogEntry
	for rows.Next() {
		var e AuditLogEntry
		var n auditRowNulls

		if err := rows.Scan(&e.ID, &n.keyID, &e.Tool, &n.serverID,
			&e.Method, &n.params, &n.durationMs, &n.statusCode, &n.successInt, &n.errorMsg, &n.clientIP, &n.createdAt); err != nil {
			return nil, fmt.Errorf("scan audit log: %w", err)
		}

		n.applyTo(&e)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit log rows: %w", err)
	}

	return entries, nil
}

// applyTo copies the valid (non-NULL) columns in n into e's fields.
func (n auditRowNulls) applyTo(e *AuditLogEntry) {
	if n.keyID.Valid {
		e.APIKeyID = &n.keyID.String
	}
	if n.serverID.Valid {
		e.ServerID = n.serverID.String
	}
	if n.params.Valid {
		e.Params = n.params.String
	}
	if n.durationMs.Valid {
		e.DurationMs = n.durationMs.Float64
	}
	if n.statusCode.Valid {
		e.StatusCode = int(n.statusCode.Int64)
	}
	if n.successInt.Valid {
		e.Success = n.successInt.Int64 == 1
	}
	if n.errorMsg.Valid {
		e.ErrorMsg = &n.errorMsg.String
	}
	if n.clientIP.Valid {
		e.ClientIP = &n.clientIP.String
	}
	if n.createdAt.Valid {
		e.CreatedAt = db.FormatSQLiteTimestamp(n.createdAt.String)
	}
}
