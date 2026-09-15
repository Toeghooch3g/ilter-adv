package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
)

type PromptInjectionMiddleware struct {
	store *db.SQLiteStore
}

func NewPromptInjectionMiddleware(store *db.SQLiteStore) *PromptInjectionMiddleware {
	return &PromptInjectionMiddleware{store: store}
}

// parsePromptVars parses the X-Prompt-Vars header as JSON, if present,
// logging (not failing) on invalid JSON.
func parsePromptVars(r *http.Request) map[string]any {
	varsHeader := r.Header.Get("X-Prompt-Vars")
	if varsHeader == "" {
		return nil
	}
	var vars map[string]any
	if err := json.Unmarshal([]byte(varsHeader), &vars); err != nil {
		slog.Warn("Failed to parse X-Prompt-Vars header", "error", err)
		return nil
	}
	return vars
}

// injectSystemPrompt prepends rendered as a system message to
// requestMap["messages"] and returns the re-marshaled request body.
func injectSystemPrompt(requestMap map[string]any, rendered string) ([]byte, error) {
	messages, _ := requestMap["messages"].([]any)
	systemMsg := map[string]any{
		"role":    "system",
		"content": rendered,
	}
	requestMap["messages"] = append([]any{systemMsg}, messages...)
	return json.Marshal(requestMap)
}

// bailWithPromptError increments the prompt-injection error counter and
// passes r through to next unmodified.
func bailWithPromptError(w http.ResponseWriter, r *http.Request, next http.Handler) {
	promptErrors.Add(r.Context(), 1)
	next.ServeHTTP(w, r)
}

func (m *PromptInjectionMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		promptName := r.Header.Get("X-Prompt-Name")

		if promptName == "" {
			next.ServeHTTP(w, r)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		_ = r.Body.Close()

		var requestMap map[string]any
		if err = json.Unmarshal(bodyBytes, &requestMap); err != nil {
			next.ServeHTTP(w, r)
			return
		}

		tmpl, err := m.store.GetPromptTemplateByName(r.Context(), promptName)
		if err != nil || tmpl == nil {
			bailWithPromptError(w, r, next)
			return
		}

		rendered, err := config.RenderPrompt(tmpl, parsePromptVars(r))
		if err != nil {
			bailWithPromptError(w, r, next)
			return
		}

		modifiedBytes, err := injectSystemPrompt(requestMap, rendered)
		if err != nil {
			bailWithPromptError(w, r, next)
			return
		}

		promptRequests.Add(r.Context(), 1)
		r.Body = io.NopCloser(bytes.NewReader(modifiedBytes))
		r.ContentLength = int64(len(modifiedBytes))
		r.Header.Set("Content-Length", fmt.Sprintf("%d", len(modifiedBytes)))
		next.ServeHTTP(w, r)
	})
}
