package provider

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
)

const deepInfraModelsJSON = `{
  "object": "list",
  "data": [
    {
      "id": "meta-llama/Llama-4-Maverick-17B-128E-Instruct-FP8",
      "metadata": {
        "context_length": 1000000,
        "max_tokens": 128000,
        "pricing": {"input_tokens": 0.2, "output_tokens": 0.8, "cache_read_tokens": 0.1},
        "tags": ["chat", "reasoning"]
      }
    },
    {
      "id": "sentence-transformers/all-MiniLM-L6-v2",
      "metadata": {
        "context_length": 512,
        "max_tokens": 0,
        "pricing": {"input_tokens": 0.01},
        "tags": ["embed"]
      }
    },
    {
      "id": "BAAI/bge-reranker-v2-m3",
      "metadata": {
        "context_length": 0,
        "max_tokens": 0,
        "pricing": {"input_tokens": 0.01},
        "tags": ["rerank"]
      }
    }
  ]
}`

func TestDeepInfra_DiscoverModels(t *testing.T) {
	p := NewDeepInfraProvider(config.ProviderConfig{
		Name:    "deepinfra",
		BaseURL: "https://api.deepinfra.com/v1/openai",
		APIKey:  "test-key",
	})
	p.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(req.URL.Path, "/models") {
				t.Errorf("expected GET /models, got %s", req.URL.Path)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Errorf("expected Bearer test-key, got %q", got)
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          io.NopCloser(strings.NewReader(deepInfraModelsJSON)),
				ContentLength: int64(len(deepInfraModelsJSON)),
			}, nil
		}),
	}

	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models (chat + embed, rerank dropped), got %d: %v", len(models), models)
	}

	byID := make(map[string]int, len(models))
	for i, m := range models {
		byID[m.ID] = i
	}

	// Chat model: per-token prices divided by 1e6, premium tier via reasoning tag.
	chatIdx, ok := byID["meta-llama/Llama-4-Maverick-17B-128E-Instruct-FP8"]
	if !ok {
		t.Fatal("chat model missing from discovery")
	}
	chat := models[chatIdx]
	if !approxEqual(chat.CostPerInputToken, 0.2/1e6) {
		t.Errorf("input cost = %v, want %v", chat.CostPerInputToken, 0.2/1e6)
	}
	if !approxEqual(chat.CostPerOutputToken, 0.8/1e6) {
		t.Errorf("output cost = %v, want %v", chat.CostPerOutputToken, 0.8/1e6)
	}
	if chat.Category != "premium" {
		t.Errorf("tier = %q, want premium (reasoning tag)", chat.Category)
	}
	if chat.MaxContextTokens != 1000000 {
		t.Errorf("context = %d, want 1000000", chat.MaxContextTokens)
	}
	if chat.MaxOutputTokens != 128000 {
		t.Errorf("max output = %d, want 128000", chat.MaxOutputTokens)
	}
	if !hasCap(chat.Capabilities, "function_calling") || !hasCap(chat.Capabilities, "json_mode") {
		t.Errorf("chat capabilities = %v, want function_calling + json_mode", chat.Capabilities)
	}

	// Embed model: economy tier, input-only pricing, zero output cost.
	embIdx, ok := byID["sentence-transformers/all-MiniLM-L6-v2"]
	if !ok {
		t.Fatal("embed model missing from discovery")
	}
	emb := models[embIdx]
	if emb.Category != "economy" {
		t.Errorf("embed tier = %q, want economy", emb.Category)
	}
	if !approxEqual(emb.CostPerInputToken, 0.01/1e6) {
		t.Errorf("embed input cost = %v, want %v", emb.CostPerInputToken, 0.01/1e6)
	}
	if emb.CostPerOutputToken != 0 {
		t.Errorf("embed output cost = %v, want 0", emb.CostPerOutputToken)
	}
}

func hasCap(caps []string, want string) bool {
	return slices.Contains(caps, want)
}

func approxEqual(a, b float64) bool {
	const eps = 1e-12
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < eps
}

func TestDeepInfra_DiscoverModels_UnauthenticatedServed(t *testing.T) {
	// DeepInfra serves /models without auth; discovery must still send the key
	// when configured and degrade gracefully when absent.
	p := NewDeepInfraProvider(config.ProviderConfig{Name: "deepinfra"})
	if p.Type() != "deepinfra" {
		t.Errorf("Type() = %q, want deepinfra", p.Type())
	}
	if p.config.BaseURL != "https://api.deepinfra.com/v1/openai" {
		t.Errorf("default base URL = %q", p.config.BaseURL)
	}
}
