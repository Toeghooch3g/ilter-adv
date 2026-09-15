package access

import (
	"net/http"
	"strconv"

	iltdb "github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/model"
)

type configAuditEntry struct {
	ID          int64   `json:"id"`
	EntityType  string  `json:"entity_type"`
	EntityID    string  `json:"entity_id"`
	Action      string  `json:"action"`
	OldValues   *string `json:"old_values,omitempty"`
	NewValues   *string `json:"new_values,omitempty"`
	PerformedBy *string `json:"performed_by,omitempty"`
	PerformedAt string  `json:"performed_at"`
}

// configAuditLogFilter holds the query-string filters accepted by
// ListConfigAuditLog.
type configAuditLogFilter struct {
	entityType string
	action     string
	start      string
	end        string
}

func parseAuditLogPageLimit(r *http.Request) (page, limit int) {
	page = 1
	if pStr := r.URL.Query().Get("page"); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			page = p
		}
	}
	limit = 50
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
			limit = l
		}
	}
	return page, limit
}

func parseConfigAuditLogFilter(r *http.Request) configAuditLogFilter {
	q := r.URL.Query()
	return configAuditLogFilter{
		entityType: q.Get("entity_type"),
		action:     q.Get("action"),
		start:      q.Get("start"),
		end:        q.Get("end"),
	}
}

// buildConfigAuditLogQuery builds the data + count SQL (and their args) for
// ListConfigAuditLog, applying f's filters identically to both queries.
func buildConfigAuditLogQuery(f configAuditLogFilter) (query, countQuery string, args, countArgs []any) {
	query = `SELECT id, entity_type, entity_id, action, old_values, new_values, performed_by, performed_at
		FROM config_audit_log WHERE 1=1`
	countQuery = `SELECT COUNT(*) FROM config_audit_log WHERE 1=1`

	if f.entityType != "" {
		query += " AND entity_type = ?"
		countQuery += " AND entity_type = ?"
		args = append(args, f.entityType)
		countArgs = append(countArgs, f.entityType)
	}
	if f.action != "" {
		query += " AND action = ?"
		countQuery += " AND action = ?"
		args = append(args, f.action)
		countArgs = append(countArgs, f.action)
	}
	// datetime(...) on both sides: performed_at is a bare "YYYY-MM-DD HH:MM:SS"
	// string, while the frontend sends a JS Date.toISOString() value
	// ("...T....000Z"). A raw string comparison between those two formats is
	// lexicographically wrong (space < 'T') and silently drops same-day rows;
	// wrapping both in datetime() normalizes them before comparing.
	if f.start != "" {
		query += " AND datetime(performed_at) >= datetime(?)"
		countQuery += " AND datetime(performed_at) >= datetime(?)"
		args = append(args, f.start)
		countArgs = append(countArgs, f.start)
	}
	if f.end != "" {
		query += " AND datetime(performed_at) <= datetime(?)"
		countQuery += " AND datetime(performed_at) <= datetime(?)"
		args = append(args, f.end)
		countArgs = append(countArgs, f.end)
	}
	return query, countQuery, args, countArgs
}

// ListConfigAuditLog returns admin config-change history (API keys, users,
// groups, MCP grants) recorded in config_audit_log.
func (h *Handler) ListConfigAuditLog(w http.ResponseWriter, r *http.Request) {
	page, limit := parseAuditLogPageLimit(r)
	offset := (page - 1) * limit

	filter := parseConfigAuditLogFilter(r)
	query, countQuery, args, countArgs := buildConfigAuditLogQuery(filter)

	db := h.store.DB

	var total int
	if err := db.QueryRow(countQuery, countArgs...).Scan(&total); err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to count audit log")
		return
	}

	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to list audit log")
		return
	}
	defer func() { _ = rows.Close() }()

	items := make([]configAuditEntry, 0)
	for rows.Next() {
		var e configAuditEntry
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Action, &e.OldValues, &e.NewValues, &e.PerformedBy, &e.PerformedAt); err != nil {
			continue
		}
		e.PerformedAt = iltdb.FormatSQLiteTimestamp(e.PerformedAt)
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to list audit log")
		return
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}
