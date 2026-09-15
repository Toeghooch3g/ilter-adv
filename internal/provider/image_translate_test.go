package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilter-ai/ilter/internal/model"
)

func TestTranslateContentToAnthropic_ImageURLBase64(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "what is this?"},
		map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/png;base64,QUJD"},
		},
	}

	out, ok := translateContentToAnthropic(content).([]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"type": "text", "text": "what is this?"}, out[0])
	assert.Equal(t, map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": "image/png",
			"data":       "QUJD",
		},
	}, out[1])
}

func TestTranslateContentToAnthropic_ImageURLRemote(t *testing.T) {
	content := []any{
		map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "https://example.com/cat.png"},
		},
	}

	out, ok := translateContentToAnthropic(content).([]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": "https://example.com/cat.png"},
	}, out[0])
}

func TestTranslateContentToAnthropic_PassthroughNonBlockContent(t *testing.T) {
	assert.Equal(t, "hello", translateContentToAnthropic("hello"))
	assert.Nil(t, translateContentToAnthropic(nil))
}

func TestTranslateContentToAnthropic_AlreadyAnthropicShapeUnchanged(t *testing.T) {
	block := map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": "xyz"},
	}
	out, ok := translateContentToAnthropic([]any{block}).([]any)
	require.True(t, ok)
	assert.Equal(t, block, out[0])
}

func TestTranslateContentToOpenAI_ImageBase64(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "describe this"},
		map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/jpeg",
				"data":       "QUJD",
			},
		},
	}

	out, ok := translateContentToOpenAI(content).([]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"type": "text", "text": "describe this"}, out[0])
	assert.Equal(t, map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/jpeg;base64,QUJD"},
	}, out[1])
}

func TestTranslateContentToOpenAI_ImageURLSource(t *testing.T) {
	content := []any{
		map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "url", "url": "https://example.com/dog.png"},
		},
	}

	out, ok := translateContentToOpenAI(content).([]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "https://example.com/dog.png"},
	}, out[0])
}

func TestTranslateContentToOpenAI_AlreadyOpenAIShapeUnchanged(t *testing.T) {
	block := map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "https://example.com/dog.png"},
	}
	out, ok := translateContentToOpenAI([]any{block}).([]any)
	require.True(t, ok)
	assert.Equal(t, block, out[0])
}

func TestTranslateMessagesToOpenAI_DoesNotMutateOriginal(t *testing.T) {
	original := []model.Message{
		{Role: "user", Content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
		}},
	}

	translated := translateMessagesToOpenAI(original)

	// Original slice/blocks must be untouched: it may be reused by the load
	// balancer for a retry against a different (Anthropic) provider.
	origContent, ok := original[0].Content.([]any)
	require.True(t, ok)
	origBlock, ok := origContent[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image", origBlock["type"])

	translatedContent, ok := translated[0].Content.([]any)
	require.True(t, ok)
	translatedBlock, ok := translatedContent[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image_url", translatedBlock["type"])
}
