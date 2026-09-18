package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
)

func TestBoolToInt(t *testing.T) {
	if boolToInt(true) != 1 {
		t.Error("expected 1 for true")
	}
	if boolToInt(false) != 0 {
		t.Error("expected 0 for false")
	}
}

func TestNullIfEmpty(t *testing.T) {
	if nullIfEmpty("") != nil {
		t.Error("expected nil for empty string")
	}
	if v := nullIfEmpty("hello"); v == nil {
		t.Fatal("expected non-nil for non-empty string")
	} else if s, ok := v.(string); !ok {
		t.Errorf("expected string, got %T", v)
	} else if s != "hello" {
		t.Errorf("expected 'hello', got %q", s)
	}
}

// TestAuditQueryCostRoundTrip verifies the Cost column written on persist is
// surfaced through AuditLogger.Query (the dashboard audit view path).
func TestAuditQueryCostRoundTrip(t *testing.T) {
	store, err := db.NewSQLiteStore(config.StorageConfig{Type: "sqlite", SqlitePath: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger := NewAuditLogger(store)
	logger.LogAsync(AuditEntry{
		APIKeyID:   "key-1",
		Tool:       "extract",
		ServerID:   "kagi",
		Method:     "tools/call",
		Params:     `{"urls":["a","b"]}`,
		StatusCode: 200,
		Success:    true,
		Cost:       0.024,
	})

	// Poll until the async worker persists the row.
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _, err := logger.Query(context.Background(), AuditFilter{Tool: "extract"})
		if err == nil && len(entries) == 1 {
			if entries[0].Cost != 0.024 {
				t.Fatalf("queried cost = %v, want 0.024", entries[0].Cost)
			}
			if !entries[0].Success {
				t.Fatal("expected success=true")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit entry not queried in time: entries=%d err=%v", len(entries), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
