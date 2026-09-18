package semanticcache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
)

// ─────────────────────────────────────────────────────────────────────
// Pure unit tests — no database required.
// ─────────────────────────────────────────────────────────────────────

func TestEmbedLiteral(t *testing.T) {
	got := embedLiteral([]float32{1, 2, 3})
	want := "[1.000000,2.000000,3.000000]"
	if got != want {
		t.Errorf("embedLiteral = %q, want %q", got, want)
	}
	if embedLiteral(nil) != "[]" {
		t.Errorf("embedLiteral(nil) = %q, want []", embedLiteral(nil))
	}
}

func TestSanitizeTableName(t *testing.T) {
	got := sanitizeTableName.ReplaceAllString("openai:text-embedding-3-small", "_")
	want := "openai_text_embedding_3_small"
	if got != want {
		t.Errorf("sanitize = %q, want %q", got, want)
	}
}

func TestSelectVectorMode(t *testing.T) {
	tests := []struct {
		name      string
		hasV      bool
		hasVS     bool
		requireVS bool
		want      string
	}{
		{"both-present", true, true, true, "pgvectorscale"},
		{"both-present-norequire", true, true, false, "pgvectorscale"},
		{"vector-only-require", true, false, true, ""},
		{"vector-only-fallback", true, false, false, "pgvector"},
		{"no-vector", false, false, true, ""},
		{"no-vector-norequire", false, false, false, ""},
		{"vectorscale-only", false, true, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectVectorMode(tt.hasV, tt.hasVS, tt.requireVS); got != tt.want {
				t.Errorf("selectVectorMode(%v,%v,%v) = %q, want %q", tt.hasV, tt.hasVS, tt.requireVS, got, tt.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Integration tests — require a real Postgres + ILTER_TEST_POSTGRES=1.
// Skips cleanly when the env is not configured (e.g. local `go test`).
// ─────────────────────────────────────────────────────────────────────

func postgresTestEnv(t *testing.T) string {
	t.Helper()
	if os.Getenv("ILTER_TEST_POSTGRES") != "1" {
		t.Skip("ILTER_TEST_POSTGRES=1 not set; skipping Postgres integration tests")
	}
	dsn := os.Getenv("ILTER_CACHE_PG_DSN")
	if dsn == "" {
		t.Skip("ILTER_CACHE_PG_DSN not set; skipping Postgres integration tests")
	}
	return dsn
}

func testPostgresCfg(dsn string) config.CacheConfig {
	return config.CacheConfig{
		Enabled:     true,
		Type:        "postgres",
		PostgresDSN: dsn,
		TTL:         1 * time.Hour,
	}
}

func TestPostgresBackend_ExactOnly(t *testing.T) {
	dsn := postgresTestEnv(t)
	b, err := NewPostgresBackend(context.Background(), testPostgresCfg(dsn), nil)
	if err != nil {
		t.Fatalf("NewPostgresBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.Mode() != "exact" {
		t.Fatalf("Mode() = %q, want exact", b.Mode())
	}
	if err := b.Set(context.Background(), nil, "key-1", "response-1", time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	hits, err := b.Get(context.Background(), nil, "key-1", 0, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(hits) != 1 || hits[0].Response != "response-1" {
		t.Fatalf("exact round-trip failed: %+v", hits)
	}
}

func TestPostgresBackend_DynamicDim(t *testing.T) {
	dsn := postgresTestEnv(t)
	embedder := &fakeEmbedder{dim: 4, model: "fake:dimtest"}
	b, err := NewPostgresBackend(context.Background(), testPostgresCfg(dsn), embedder)
	if err != nil {
		t.Fatalf("NewPostgresBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.dim != 4 {
		t.Errorf("b.dim = %d, want 4", b.dim)
	}
	if b.table != "ilter_semantic_cache_fake_dimtest" {
		t.Errorf("b.table = %q, want ilter_semantic_cache_fake_dimtest", b.table)
	}
}

func TestPostgresBackend_SemanticHitAndMiss(t *testing.T) {
	dsn := postgresTestEnv(t)
	embedder := &fakeEmbedder{dim: 4, model: "fake:smh"}
	b, err := NewPostgresBackend(context.Background(), testPostgresCfg(dsn), embedder)
	if err != nil {
		t.Fatalf("NewPostgresBackend: %v", err)
	}
	defer func() { _ = b.Close() }()

	emb := []float32{0.1, 0.2, 0.3, 0.4}
	if err := b.Set(context.Background(), emb, "sem-key", "sem-response", time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Hit with a permissive threshold.
	hits, err := b.Get(context.Background(), emb, "sem-key", 0.0, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(hits) != 1 || hits[0].Response != "sem-response" {
		t.Fatalf("semantic hit failed: %+v", hits)
	}

	// Miss with an impossible threshold (0.0 distance to the probe vector).
	hits2, err := b.Get(context.Background(), emb, "sem-key", 0.99, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Falls back to exact match by exact_key, which exists.
	if len(hits2) != 1 || hits2[0].Response != "sem-response" {
		t.Fatalf("expected exact fallback after semantic miss: %+v", hits2)
	}
}

func TestPostgresBackend_TopK(t *testing.T) {
	dsn := postgresTestEnv(t)
	embedder := &fakeEmbedder{dim: 4, model: "fake:topk"}
	b, err := NewPostgresBackend(context.Background(), testPostgresCfg(dsn), embedder)
	if err != nil {
		t.Fatalf("NewPostgresBackend: %v", err)
	}
	defer func() { _ = b.Close() }()

	emb := []float32{0.1, 0.2, 0.3, 0.4}
	emb2 := []float32{0.11, 0.21, 0.31, 0.41}
	emb3 := []float32{0.5, 0.5, 0.5, 0.5}
	for i, e := range [][]float32{emb, emb2, emb3} {
		if err := b.Set(context.Background(), e, "tk-"+string(rune('a'+i)), "resp-"+string(rune('a'+i)), time.Hour); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	hits, err := b.Get(context.Background(), emb, "tk-a", 0, 3)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 top-K hits, got %d", len(hits))
	}
}
