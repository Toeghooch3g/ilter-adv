package semanticcache

import (
	"context"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
)

func TestNew(t *testing.T) {
	// With nil client and disabled cfg, New should not panic.
	sc := New(cfgDisabled(), nil, nil, nil, "")
	if sc == nil {
		t.Fatal("New returned nil")
	}
}

func TestNew_WithConfig(t *testing.T) {
	cfg := configWithDefaults()
	sc := New(cfg, nil, nil, nil, "")
	if sc == nil {
		t.Fatal("New returned nil")
	}
	if sc.cfg.SimilarityThreshold != 0.92 {
		t.Errorf("expected similarity threshold 0.92, got %f", sc.cfg.SimilarityThreshold)
	}
	if sc.backend != nil {
		t.Errorf("expected nil backend, got %v", sc.backend)
	}
	if sc.Mode() != CacheModeDisabled {
		t.Errorf("expected disabled mode without a client, got %q", sc.Mode())
	}
}

// TestNew_TypeDisabled verifies ILTER_CACHE_TYPE=disabled produces a fully
// inert cache: no backend, Mode()==disabled, and GetFull/SetFull no-op —
// even when an embedder is present, so no embedding provider is probed.
func TestNew_TypeDisabled(t *testing.T) {
	cfg := config.CacheConfig{Enabled: true, Type: "disabled", SimilarityThreshold: 0.7}
	sc := New(cfg, &stubEmbedder{}, nil, nil, "")

	if sc.Mode() != CacheModeDisabled {
		t.Fatalf("Mode() = %q, want disabled", sc.Mode())
	}
	if sc.backend != nil {
		t.Fatalf("expected no backend for disabled type, got %v", sc.backend)
	}

	if resp, _, found := sc.GetFull(t.Context(), "hello", "exact-key"); found || resp != "" {
		t.Fatalf("GetFull must no-op on a disabled cache, got found=%v resp=%q", found, resp)
	}
	if err := sc.SetFull(t.Context(), "hello", "exact-key", "response"); err != nil {
		t.Fatalf("SetFull must no-op without error, got %v", err)
	}
}

// stubEmbedder satisfies Embedder without any real embedding backend.
type stubEmbedder struct{}

func (stubEmbedder) Dim() int      { return 4 }
func (stubEmbedder) Model() string { return "stub" }
func (stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 2, 3, 4}, nil
}

func TestFloat32ToByte(t *testing.T) {
	tests := []struct {
		name string
		vec  []float32
		want int // expected byte length
	}{
		{"empty slice", []float32{}, 0},
		{"single element", []float32{1.0}, 4},
		{"three elements", []float32{1.0, 2.0, 3.0}, 12},
		{"1536 dims", make([]float32, 1536), 1536 * 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := float32ToByte(tt.vec)
			if len(got) != tt.want {
				t.Errorf("float32ToByte length = %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestParseSearchResponse_Nil(t *testing.T) {
	resp, score, found := parseSearchResponse(nil, 0.5)
	if found {
		t.Error("expected found=false for nil input")
	}
	if resp != "" {
		t.Errorf("expected empty response, got %q", resp)
	}
	if score != 0 {
		t.Errorf("expected score 0, got %f", score)
	}
}

func TestParseSearchResponse_RESP2(t *testing.T) {
	// Simulate RESP2 array: [count, _, [key, val, key, val], ...]
	// FT.SEARCH returns: [1, "key:name", ["response", "hello", "score", "0.05"]]
	val := []any{
		int64(1),
		"ilter:cache:abc",
		[]any{
			"response", "hello world",
			"score", "0.05",
		},
	}

	resp, score, found := parseSearchResponse(val, 0.08)
	if !found {
		t.Fatal("expected found=true for RESP2 within threshold")
	}
	if resp != "hello world" {
		t.Errorf("expected 'hello world', got %q", resp)
	}
	if score != 0.05 {
		t.Errorf("expected score 0.05, got %f", score)
	}
}

func TestParseSearchResponse_RESP2_AboveThreshold(t *testing.T) {
	val := []any{
		int64(1),
		"ilter:cache:abc",
		[]any{
			"response", "far away",
			"score", "0.5",
		},
	}

	resp, score, found := parseSearchResponse(val, 0.08)
	if found {
		t.Error("expected found=false for distance above threshold")
	}
	if resp != "" {
		t.Errorf("expected empty response, got %q", resp)
	}
	if score != 0 {
		t.Errorf("expected score 0, got %f", score)
	}
}

func TestParseSearchResponse_RESP3_MapString(t *testing.T) {
	val := map[string]any{
		"results": []any{
			map[string]any{
				"extra_attributes": map[string]any{
					"response": "cached response",
					"score":    "0.03",
				},
			},
		},
	}

	resp, score, found := parseSearchResponse(val, 0.08)
	if !found {
		t.Fatal("expected found=true for RESP3 map[string]")
	}
	if resp != "cached response" {
		t.Errorf("expected 'cached response', got %q", resp)
	}
	if score != 0.03 {
		t.Errorf("expected score 0.03, got %f", score)
	}
}

func TestParseSearchResponse_RESP3_MapInterface(t *testing.T) {
	val := map[any]any{
		"results": []any{
			map[any]any{
				"extra_attributes": map[any]any{
					"response": "cached response",
					"score":    "0.03",
				},
			},
		},
	}

	resp, score, found := parseSearchResponse(val, 0.08)
	if !found {
		t.Fatal("expected found=true for RESP3 map[interface{}]")
	}
	if resp != "cached response" {
		t.Errorf("expected 'cached response', got %q", resp)
	}
	if score != 0.03 {
		t.Errorf("expected score 0.03, got %f", score)
	}
}

func TestParseSearchResponse_RESP3_Float64Score(t *testing.T) {
	val := map[string]any{
		"results": []any{
			map[string]any{
				"extra_attributes": map[string]any{
					"response": "float score",
					"score":    float64(0.04),
				},
			},
		},
	}

	resp, score, found := parseSearchResponse(val, 0.08)
	if !found {
		t.Fatal("expected found=true for float64 score")
	}
	if resp != "float score" {
		t.Errorf("expected 'float score', got %q", resp)
	}
	if score != 0.04 {
		t.Errorf("expected score 0.04, got %f", score)
	}
}

func TestParseSearchResponse_RESP3_Int64Score(t *testing.T) {
	val := map[string]any{
		"results": []any{
			map[string]any{
				"extra_attributes": map[string]any{
					"response": "int score",
					"score":    int64(0),
				},
			},
		},
	}

	resp, score, found := parseSearchResponse(val, 0.08)
	if !found {
		t.Fatal("expected found=true for int64 score (0 <= 0.08)")
	}
	if resp != "int score" {
		t.Errorf("expected 'int score', got %q", resp)
	}
	if score != 0 {
		t.Errorf("expected score 0, got %f", score)
	}
}

func TestParseSearchResponse_EmptyResults(t *testing.T) {
	val := map[string]any{
		"results": []any{},
	}

	resp, score, found := parseSearchResponse(val, 0.08)
	if found {
		t.Error("expected found=false for empty results")
	}
	if resp != "" {
		t.Errorf("expected empty response, got %q", resp)
	}
	if score != 0 {
		t.Errorf("expected score 0, got %f", score)
	}
}

// Helpers

func cfgDisabled() config.CacheConfig {
	return config.CacheConfig{Enabled: false}
}

func configWithDefaults() config.CacheConfig {
	return config.CacheConfig{
		Enabled:             true,
		SimilarityThreshold: 0.92,
		TTL:                 3600000000000, // 1 hour
	}
}
