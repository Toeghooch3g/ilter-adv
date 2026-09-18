package semanticcache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ilter-ai/ilter/internal/platform/rediskeys"
)

// RedisBackend implements CacheBackend on Redis Stack (the FT.CREATE VSS +
// HASH keyset). It wraps *redis.Client and the optional Embedder.
type RedisBackend struct {
	client     *redis.Client
	embedder   Embedder
	maxEntries int
}

// NewRedisBackend wraps a *redis.Client with an optional embedder. embedder
// may be nil for exact-only mode (no VSS index created).
func NewRedisBackend(client *redis.Client, embedder Embedder) *RedisBackend {
	return &RedisBackend{client: client, embedder: embedder}
}

func (r *RedisBackend) Mode() string {
	if r.client == nil {
		return "disabled"
	}
	if r.embedder != nil {
		return "semantic"
	}
	return "exact"
}

func (r *RedisBackend) Close() error {
	if r.client == nil {
		return nil
	}
	return r.client.Close()
}

// initIndex creates the Redis VSS index (idx:cache:v3) for embedder.Dim()
// vectors. If an index already exists with a different DIM, it is dropped and
// recreated to match.
func (r *RedisBackend) initIndex(dim int) {
	if dim <= 0 {
		dim = 768
	}
	if err := r.createIndex(dim); err != nil {
		if strings.Contains(err.Error(), "Index already exists") {
			if existing := r.indexDim(); existing > 0 && existing != dim {
				slog.Warn("Redis VSS index has mismatched DIM, recreating", "existing", existing, "want", dim)
				_ = r.client.Do(context.Background(), "FT.DROPINDEX", "idx:cache:v3").Err()
				if err2 := r.createIndex(dim); err2 != nil {
					slog.Error("Failed to recreate Redis VSS index", "error", err2)
				}
			}
			return
		}
		slog.Error("Failed to create Redis VSS index", "error", err)
	}
}

func (r *RedisBackend) createIndex(dim int) error {
	cmd := redis.NewStringCmd(context.Background(), "FT.CREATE", "idx:cache:v3", "ON", "HASH", "PREFIX", "1", "ilter:cache:", "SCHEMA", "vector", "VECTOR", "FLAT", "6", "TYPE", "FLOAT32", "DIM", fmt.Sprintf("%d", dim), "DISTANCE_METRIC", "COSINE", "response", "TEXT")
	return r.client.Process(context.Background(), cmd)
}

// indexDim reads the current DIM of the Redis VSS index via FT.INFO, or 0 if
// the index does not exist / no DIM attribute is present.
func (r *RedisBackend) indexDim() int {
	cmd := redis.NewSliceCmd(context.Background(), "FT.INFO", "idx:cache:v3")
	if err := r.client.Process(context.Background(), cmd); err != nil {
		return 0
	}
	fields := cmd.Val()
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		if key != "attributes" {
			continue
		}
		attrs, ok := fields[i+1].([]any)
		if !ok {
			continue
		}
		for _, attr := range attrs {
			attrFields, ok := attr.([]any)
			if !ok {
				continue
			}
			for j := 0; j+1 < len(attrFields); j += 2 {
				if attrFields[j] == "DIM" {
					if dim, ok := attrFields[j+1].(int64); ok {
						return int(dim)
					}
				}
			}
		}
	}
	return 0
}

// searchNearest performs a KNN k search on the Redis VSS index and returns up
// to k hits (closest first). When k == 1 and threshold > 0, only a hit within
// maxDistance is returned.
func (r *RedisBackend) searchNearest(ctx context.Context, emb []float32, threshold float64, k int) []Hit {
	if k <= 0 {
		k = 1
	}
	maxDistance := 1.0 - threshold
	searchCmd := redis.NewCmd(ctx, "FT.SEARCH", "idx:cache:v3", fmt.Sprintf("*=>[KNN %d @vector $vec AS score]", k), "PARAMS", "2", "vec", float32ToByte(emb), "RETURN", "2", "response", "score", "DIALECT", "2")
	if err := r.client.Process(ctx, searchCmd); err != nil {
		slog.Error("FT.SEARCH failed", "err", err)
		return nil
	}
	return parseSearchResults(searchCmd.Val(), maxDistance, k)
}

// Get implements CacheBackend.Get. exactKey is used for the exact-match
// fallback when no semantic hit qualifies (k <= 1).
func (r *RedisBackend) Get(ctx context.Context, embedding []float32, exactKey string, threshold float64, k int) ([]Hit, error) {
	if r.client == nil {
		return nil, nil
	}
	if embedding == nil {
		// exact-only mode
		hit, ok := r.exactGet(ctx, exactKey)
		if !ok {
			return nil, nil
		}
		return []Hit{hit}, nil
	}
	if k > 1 {
		return r.searchNearest(ctx, embedding, 0, k), nil
	}
	hits := r.searchNearest(ctx, embedding, threshold, 1)
	if len(hits) > 0 {
		return hits, nil
	}
	// fall back to exact match, mirroring legacy GetFull behaviour
	hit, ok := r.exactGet(ctx, exactKey)
	if !ok {
		return nil, nil
	}
	return []Hit{hit}, nil
}

// Set implements CacheBackend.Set. The exact-match entry is always stored;
// the vector entry only when embedding is non-nil.
func (r *RedisBackend) Set(ctx context.Context, embedding []float32, exactKey string, response string, ttl time.Duration) error {
	if r.client == nil {
		return nil
	}
	if err := r.exactSet(ctx, exactKey, response, ttl); err != nil {
		slog.Warn("Failed to store exact cache entry", "error", err)
	}
	if embedding == nil {
		return nil
	}
	if r.maxEntries > 0 {
		count, err := r.entryCount(ctx)
		if err == nil && count >= r.maxEntries {
			slog.Warn("semantic cache at capacity, skipping VSS store", "count", count, "max", r.maxEntries)
			return nil // exactSet already stored the exact-match entry
		}
	}
	return r.store(ctx, embedding, response, ttl)
}

// store persists an embedding+response in a Redis hash discoverable by VSS index.
func (r *RedisBackend) store(ctx context.Context, emb []float32, response string, ttl time.Duration) error {
	cacheKey := rediskeys.CacheKey()
	if err := r.client.HSet(ctx, cacheKey, map[string]any{
		"vector":   float32ToByte(emb),
		"response": response,
	}).Err(); err != nil {
		return err
	}
	return r.client.Expire(ctx, cacheKey, ttl).Err()
}

// exactGet reads a response by its exact_key. Returns a single Hit with score 0.
func (r *RedisBackend) exactGet(ctx context.Context, exactKey string) (Hit, bool) {
	resp, err := r.client.Get(ctx, cacheKey(exactKey)).Result()
	if err != nil {
		return Hit{}, false
	}
	return Hit{ID: exactKey, Response: resp, Score: 0}, true
}

func (r *RedisBackend) exactSet(ctx context.Context, exactKey, response string, ttl time.Duration) error {
	return r.client.Set(ctx, cacheKey(exactKey), response, ttl).Err()
}

// countEntriesScript atomically counts ilter:cache:* keys in Redis via non-blocking SCAN.
// Returns the exact count, not an estimate.
const countEntriesScript = `
local cursor = '0'
local count = 0
repeat
    local result = redis.call('SCAN', cursor, 'MATCH', 'ilter:cache:*', 'COUNT', '5000')
    cursor = result[1]
    count = count + #result[2]
until cursor == '0'
return count
`

var countEntriesCmd = redis.NewScript(countEntriesScript)

// entryCount returns the current number of cached entries matching ilter:cache:*.
// Returns -1 if Redis is unavailable.
func (r *RedisBackend) entryCount(ctx context.Context) (int, error) {
	if r.client == nil {
		return -1, fmt.Errorf("redis client is nil")
	}
	n, err := countEntriesCmd.Run(ctx, r.client, nil).Int()
	if err != nil {
		return -1, fmt.Errorf("entry count: %w", err)
	}
	return n, nil
}

// parseSearchResponse extracts the closest cached response within maxDistance
// (single hit). Kept for backward compatibility with existing package tests.
func parseSearchResponse(val any, maxDistance float64) (string, float64, bool) {
	hits := parseSearchResults(val, maxDistance, 1)
	if len(hits) == 0 {
		return "", 0, false
	}
	return hits[0].Response, hits[0].Score, true
}

// parseSearchResults extracts up to k cached responses, closest (smallest
// distance) first, each within maxDistance when maxDistance > 0.
func parseSearchResults(val any, maxDistance float64, k int) []Hit {
	var out []Hit
	for _, hit := range extractResults(val) {
		resp, score, ok := extractHit(hit)
		if !ok || resp == "" || math.IsNaN(score) {
			continue
		}
		if maxDistance > 0 && score > maxDistance {
			continue
		}
		out = append(out, Hit{ID: "", Response: resp, Score: score})
		if k > 0 && len(out) >= k {
			break
		}
	}
	return out
}

func extractResults(val any) []any {
	switch v := val.(type) {
	case nil:
		return nil
	case map[string]any:
		r, _ := v["results"].([]any)
		return r
	case map[any]any:
		r, _ := v["results"].([]any)
		return r
	case []any:
		if len(v) <= 2 {
			return nil
		}
		count, ok := v[0].(int64)
		if !ok || count <= 0 {
			return nil
		}
		out := make([]any, 0, count)
		for i := 2; i < len(v); i += 2 {
			if fields, ok := v[i].([]any); ok {
				out = append(out, fields)
			}
		}
		return out
	default:
		return nil
	}
}

func extractHit(item any) (string, float64, bool) {
	switch v := item.(type) {
	case map[string]any:
		extra, _ := v["extra_attributes"].(map[string]any)
		if extra == nil {
			return "", 0, false
		}
		resp, _ := extra["response"].(string)
		return resp, toFloat64(extra["score"]), resp != ""
	case map[any]any:
		extra, _ := v["extra_attributes"].(map[any]any)
		if extra == nil {
			return "", 0, false
		}
		resp, _ := extra["response"].(string)
		return resp, toFloat64(extra["score"]), resp != ""
	case []any:
		return extractRESP2Hit(v)
	default:
		return "", 0, false
	}
}

// extractRESP2Hit handles RESP2 flat field list [key1, val1, key2, val2, ...].
func extractRESP2Hit(fields []any) (string, float64, bool) {
	var resp, scoreStr string
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		val, _ := fields[i+1].(string)
		switch key {
		case "response":
			resp = val
		case "score":
			scoreStr = val
		}
	}
	if resp == "" || scoreStr == "" {
		return "", 0, false
	}
	score, err := strconv.ParseFloat(scoreStr, 64)
	if err != nil {
		return "", 0, false
	}
	return resp, score, true
}

func toFloat64(v any) float64 {
	switch vv := v.(type) {
	case float64:
		if math.IsNaN(vv) || math.IsInf(vv, 0) {
			return math.NaN()
		}
		return vv
	case string:
		f, err := strconv.ParseFloat(vv, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return math.NaN()
		}
		return f
	case int64:
		return float64(vv)
	default:
		return math.NaN()
	}
}

func float32ToByte(vec []float32) []byte {
	b := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

// exactKeyPrefix keys the exact-match Redis entry for a prompt.
const exactKeyPrefix = "ilter:cache:exact:"

func cacheKey(prompt string) string {
	return exactKeyPrefix + fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
}
