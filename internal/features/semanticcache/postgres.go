package semanticcache

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	// pgx stdlib registers the "pgx" driver with database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ilter-ai/ilter/internal/config"
)

// sanitizeTableName replaces any character not in [A-Za-z0-9_] with '_' so the
// model id can be embedded in a Postgres table name safely.
var sanitizeTableName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// PostgresBackend implements CacheBackend on PostgreSQL using the pgvector
// (and optionally pgvectorscale) extension. The table is per embedding model
// (ilter_semantic_cache_<sanitized_model>) so deployments can switch models
// without losing cached data.
type PostgresBackend struct {
	db       *sql.DB
	cfg      config.CacheConfig
	embedder Embedder
	table    string
	dim      int
	mode     string // "exact" | "pgvector" | "pgvectorscale"

	ttlCancel context.CancelFunc
	ttlWG     sync.WaitGroup
}

// NewPostgresBackend opens a Postgres connection, detects the vector
// extensions, and bootstraps the per-model cache table + index. embedder nil
// selects exact-only mode.
func NewPostgresBackend(ctx context.Context, cfg config.CacheConfig, embedder Embedder) (*PostgresBackend, error) {
	if cfg.PostgresDSN == "" {
		return nil, fmt.Errorf("postgres cache backend requires ILTER_CACHE_PG_DSN")
	}

	db, err := sql.Open("pgx", cfg.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("postgres cache backend: open: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres cache backend: ping: %w", err)
	}

	hasVector, hasVectorscale := pgExtensions(ctx, db)

	table := "ilter_semantic_cache"
	dim := 0
	mode := "exact"
	if embedder != nil {
		dim = embedder.Dim()
		suffix := sanitizeTableName.ReplaceAllString(embedder.Model(), "_")
		if suffix != "" {
			table = "ilter_semantic_cache_" + suffix
		}
		mode = selectVectorMode(hasVector, hasVectorscale, cfg.PostgresRequireVectorscale)
		if mode == "" {
			_ = db.Close()
			return nil, fmt.Errorf("postgres cache backend requires the vector extension; install it or unset the vector fields")
		}
	}

	b := &PostgresBackend{
		db:       db,
		cfg:      cfg,
		embedder: embedder,
		table:    table,
		dim:      dim,
		mode:     mode,
	}

	if err := b.ensureSchema(ctx, hasVector, hasVectorscale); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres cache backend: schema: %w", err)
	}

	// Start the TTL sweep loop. It runs for the backend's lifetime and must
	// not be cancelled when this constructor returns, so it gets its own root.
	sweepCtx, ttlCancel := context.WithCancel(context.Background())
	b.ttlCancel = ttlCancel
	b.ttlWG.Add(1)
	//nolint:contextcheck // intentional: sweep outlives the caller's context.
	go b.ttlLoop(sweepCtx)

	return b, nil
}

// pgExtensions returns whether the vector and vectorscale extensions are present.
func pgExtensions(ctx context.Context, db *sql.DB) (vector, vectorscale bool) {
	rows, err := db.QueryContext(ctx, "SELECT extname FROM pg_extension WHERE extname IN ('vector', 'vectorscale')")
	if err != nil {
		slog.Warn("postgres cache backend: failed to query pg_extension", "error", err)
		return false, false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			continue
		}
		switch ext {
		case "vector":
			vector = true
		case "vectorscale":
			vectorscale = true
		}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("postgres cache backend: pg_extension scan", "error", err)
	}
	return vector, vectorscale
}

// selectVectorMode returns the storage mode, or "" when the extension
// requirements are not satisfied.
func selectVectorMode(hasVector, hasVectorscale, requireVectorscale bool) string {
	if !hasVector {
		return ""
	}
	if hasVectorscale {
		return "pgvectorscale"
	}
	if requireVectorscale {
		return ""
	}
	return "pgvector"
}

// ensureSchema creates the per-model table and vector index (idempotent).
func (b *PostgresBackend) ensureSchema(ctx context.Context, hasVector, hasVectorscale bool) error {
	// In exact-only mode (no embedder) the table omits the embedding column
	// entirely; pgvector rejects vector(0).
	embeddingCol := ""
	if b.dim > 0 {
		embeddingCol = fmt.Sprintf(",\n\tembedding vector(%d)", b.dim)
	}
	schemaCmds := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id BIGSERIAL PRIMARY KEY,
			exact_key TEXT NOT NULL UNIQUE,
			response TEXT NOT NULL%s,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, b.table, embeddingCol),
	}
	if b.dim > 0 {
		if b.mode == "pgvectorscale" && hasVectorscale {
			schemaCmds = append(schemaCmds,
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_embedding_idx ON %s USING diskann (embedding vector_cosine_ops)`, b.table, b.table))
		} else if b.mode == "pgvector" && hasVector {
			// Try cosine ops first; fall back to L2 on older pgvector builds.
			if err := b.execIndex(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_embedding_idx ON %s USING hnsw (embedding vector_cosine_ops)`, b.table, b.table)); err != nil {
				slog.Warn("postgres cache backend: vector_cosine_ops unavailable, falling back to vector_l2_ops", "error", err)
				if err2 := b.execIndex(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_embedding_idx ON %s USING hnsw (embedding vector_l2_ops)`, b.table, b.table)); err2 != nil {
					return fmt.Errorf("index (l2): %w", err2)
				}
			}
		}
	}
	for _, cmd := range schemaCmds {
		if err := b.execIndex(ctx, cmd); err != nil {
			return err
		}
	}
	return nil
}

func (b *PostgresBackend) execIndex(ctx context.Context, cmd string) error {
	_, err := b.db.ExecContext(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	return nil
}

func (b *PostgresBackend) Mode() string { return b.mode }

func (b *PostgresBackend) Close() error {
	if b.ttlCancel != nil {
		b.ttlCancel()
		b.ttlWG.Wait()
	}
	if b.db == nil {
		return nil
	}
	return b.db.Close()
}

// Get implements CacheBackend.Get.
func (b *PostgresBackend) Get(ctx context.Context, embedding []float32, exactKey string, threshold float64, k int) ([]Hit, error) {
	if b.db == nil {
		return nil, nil
	}
	if err := b.db.PingContext(ctx); err != nil {
		slog.Warn("postgres cache backend: connection lost, returning miss", "error", err)
		return nil, nil
	}
	if embedding == nil || b.dim == 0 {
		// exact-only mode
		hit, ok := b.exactGet(ctx, exactKey)
		if !ok {
			return nil, nil
		}
		return []Hit{hit}, nil
	}
	if k > 1 {
		return b.search(ctx, embedding, 0, k)
	}
	hits, err := b.search(ctx, embedding, threshold, 1)
	if err != nil {
		return nil, err
	}
	if len(hits) > 0 {
		return hits, nil
	}
	hit, ok := b.exactGet(ctx, exactKey)
	if !ok {
		return nil, nil
	}
	return []Hit{hit}, nil
}

// search runs a KNN query. When threshold > 0 and k == 1, only a row within
// (1 - threshold) cosine distance qualifies. Returns the closest k otherwise.
func (b *PostgresBackend) search(ctx context.Context, embedding []float32, threshold float64, k int) ([]Hit, error) {
	if k <= 0 {
		k = 1
	}
	vec := embedLiteral(embedding)
	var (
		rows *sql.Rows
		err  error
	)
	if threshold > 0 && k == 1 {
		maxDistance := 1.0 - threshold
		rows, err = b.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT exact_key, response, embedding <=> $1::vector AS distance
				FROM %s
				WHERE embedding <=> $1::vector <= $2
				ORDER BY embedding <=> $1::vector
				LIMIT 1`, b.table), vec, maxDistance)
	} else {
		rows, err = b.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT exact_key, response, embedding <=> $1::vector AS distance
				FROM %s
				ORDER BY embedding <=> $1::vector
				LIMIT $2`, b.table), vec, k)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres cache backend: search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Hit
	for rows.Next() {
		var key, resp string
		var dist float64
		if err := rows.Scan(&key, &resp, &dist); err != nil {
			return nil, fmt.Errorf("postgres cache backend: scan: %w", err)
		}
		out = append(out, Hit{ID: key, Response: resp, Score: dist})
	}
	return out, rows.Err()
}

// exactGet returns the response stored with the given exact_key.
func (b *PostgresBackend) exactGet(ctx context.Context, exactKey string) (Hit, bool) {
	var resp string
	err := b.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT response FROM %s WHERE exact_key = $1`, b.table), exactKey).Scan(&resp)
	if err != nil {
		return Hit{}, false
	}
	return Hit{ID: exactKey, Response: resp, Score: 0}, true
}

// Set implements CacheBackend.Set. The exact_key row is always upserted;
// embedding is stored only when the schema has the embedding column.
func (b *PostgresBackend) Set(ctx context.Context, embedding []float32, exactKey string, response string, ttl time.Duration) error {
	if b.db == nil {
		return nil
	}
	if err := b.db.PingContext(ctx); err != nil {
		slog.Warn("postgres cache backend: connection lost, skipping store", "error", err)
		return nil
	}
	// Exact-only mode: the table has no embedding column.
	if b.dim == 0 {
		_, err := b.db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (exact_key, response, created_at)
				VALUES ($1, $2, now())
				ON CONFLICT (exact_key) DO UPDATE
					SET response = EXCLUDED.response,
						created_at = now()`, b.table),
			exactKey, response)
		if err != nil {
			return fmt.Errorf("postgres cache backend: store: %w", err)
		}
		return nil
	}
	var emb any
	if embedding != nil {
		emb = embedLiteral(embedding)
	}
	_, err := b.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (exact_key, response, embedding, created_at)
			VALUES ($1, $2, $3::vector, now())
			ON CONFLICT (exact_key) DO UPDATE
				SET response = EXCLUDED.response,
					embedding = EXCLUDED.embedding,
					created_at = now()`, b.table),
		exactKey, response, emb)
	if err != nil {
		return fmt.Errorf("postgres cache backend: store: %w", err)
	}
	return nil
}

// ttlLoop periodically deletes rows whose created_at is older than the TTL.
func (b *PostgresBackend) ttlLoop(ctx context.Context) {
	defer b.ttlWG.Done()
	interval := 5 * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ttl := b.cfg.TTL
			if ttl <= 0 {
				ttl = 1 * time.Hour
			}
			_, err := b.db.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM %s WHERE created_at < now() - $1::interval`, b.table),
				ttl.String())
			if err != nil {
				slog.Warn("postgres cache backend: TTL sweep failed", "error", err)
			}
		}
	}
}

// embedLiteral formats a float32 vector as a pgvector literal: "[1,2,3]".
func embedLiteral(vec []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%.6f", v)
	}
	sb.WriteByte(']')
	return sb.String()
}
