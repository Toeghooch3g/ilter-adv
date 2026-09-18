package semanticcache

import (
	"context"
	"strings"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/provider"
)

// newTestRegistry returns a registry with one OpenAI provider (implements
// EmbeddingProvider) and one Anthropic provider (implements neither Embedding
// nor Rerank), both pointed at a dead localhost so no network call is made.
func newTestRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()
	reg.Register(provider.NewOpenAIProvider(config.ProviderConfig{
		Name:    "openai",
		Type:    "openai",
		BaseURL: "http://127.0.0.1:1",
		APIKey:  "test",
	}))
	reg.Register(provider.NewAnthropicProvider(config.ProviderConfig{
		Name:    "anthropic",
		Type:    "anthropic",
		BaseURL: "http://127.0.0.1:1",
		APIKey:  "test",
	}))
	return reg
}

func TestResolveEmbedder_EmptyConfig(t *testing.T) {
	reg := newTestRegistry(t)
	e, err := ResolveEmbedder(reg, config.CacheConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e != nil {
		t.Fatalf("expected nil embedder, got %v", e)
	}
}

func TestResolveEmbedder_LegacyOllama(t *testing.T) {
	reg := newTestRegistry(t)
	e, err := ResolveEmbedder(reg, config.CacheConfig{OllamaURL: "http://localhost:11434"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oe, ok := e.(*OllamaEmbedder)
	if !ok {
		t.Fatalf("expected *OllamaEmbedder, got %T", e)
	}
	if oe.Model() != "ollama:nomic-embed-text" {
		t.Errorf("Model() = %q, want %q", oe.Model(), "ollama:nomic-embed-text")
	}
	if oe.Dim() != 768 {
		t.Errorf("Dim() = %d, want 768", oe.Dim())
	}
}

func TestResolveEmbedder_BadFormat(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := ResolveEmbedder(reg, config.CacheConfig{EmbeddingModel: "openai"})
	if err == nil {
		t.Fatal("expected error for missing colon")
	}
	want := "provider:model"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should mention %q", err.Error(), want)
	}
}

func TestResolveEmbedder_UnknownProvider(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := ResolveEmbedder(reg, config.CacheConfig{EmbeddingModel: "doesnotexist:foo"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "doesnotexist") {
		t.Errorf("error %q should name the provider", err.Error())
	}
}

func TestResolveEmbedder_ProviderLacksCapability(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := ResolveEmbedder(reg, config.CacheConfig{EmbeddingModel: "anthropic:foo"})
	if err == nil {
		t.Fatal("expected error for provider lacking EmbeddingProvider")
	}
	if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "EmbeddingProvider") {
		t.Errorf("error %q should name the provider and capability", err.Error())
	}
}

func TestNewProviderEmbedder_Success(t *testing.T) {
	reg := newTestRegistry(t)
	pe, err := NewProviderEmbedder(reg, "openai", "text-embedding-3-small")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pe.Model() != "text-embedding-3-small" {
		t.Errorf("Model() = %q, want %q", pe.Model(), "text-embedding-3-small")
	}
}

func TestNewProviderReranker_Success(t *testing.T) {
	reg := newTestRegistry(t)
	r, err := NewProviderReranker(reg, "openai", "cohere-rerank-v3.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Model() != "cohere-rerank-v3.5" {
		t.Errorf("Model() = %q, want %q", r.Model(), "cohere-rerank-v3.5")
	}
}

// fakeEmbedder returns a deterministic fixed-dim vector so postgres/redis
// backends can be exercised without a real provider.
type fakeEmbedder struct {
	dim   int
	model string
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	v := make([]float32, f.dim)
	for i := range v {
		v[i] = float32(i+1) / 100
	}
	return v, nil
}

func (f *fakeEmbedder) Dim() int { return f.dim }

func (f *fakeEmbedder) Model() string {
	if f.model == "" {
		return "fake:model"
	}
	return f.model
}
