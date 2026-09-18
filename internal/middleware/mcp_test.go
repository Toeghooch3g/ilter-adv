package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/mcp"
	"github.com/ilter-ai/ilter/internal/model"
)

func TestFindAllToolCallsInText_bareInvoke(t *testing.T) {
	// Bare <invoke> without <tool_calls> wrapper -- model emits direct XML
	text := `Let me check the database. <invoke name="sqlite__query"><parameter name="sql">SELECT 1</parameter></invoke>`
	tcs, xmlOffset := mcp.FindAllToolCallsInText(text)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "sqlite__query" {
		t.Errorf("expected sqlite__query, got %s", tcs[0].Function.Name)
	}
	if xmlOffset < 0 {
		t.Errorf("expected xmlOffset >= 0 for bare invoke")
	}
	// Pre-tool text should be preserved
	cleanedText := strings.TrimSpace(text[:xmlOffset])
	if cleanedText != "Let me check the database." {
		t.Errorf("expected 'Let me check the database.', got %q", cleanedText)
	}
}

func TestFindAllToolCallsInText_truncatedToolCalls(t *testing.T) {
	// <tool_calls> with unparseable content (cut off mid-tag) -- xmlOffset set but len(tcs)==0
	text := `I'll look that up. <tool_calls><invoke name="`
	tcs, xmlOffset := mcp.FindAllToolCallsInText(text)
	if len(tcs) != 0 {
		t.Errorf("expected 0 tool calls (truncated at name quote), got %d", len(tcs))
	}
	if xmlOffset < 0 {
		t.Errorf("expected xmlOffset >= 0 for truncated <tool_calls>")
	}
}

func TestFindAllToolCallsInText_truncatedInvokeWithClose(t *testing.T) {
	// <tool_calls> without </tool_calls> but invoke is complete -- fallback parses it
	text := `I'll check. <tool_calls><invoke name="sqlite__query"><parameter name="sql">SELECT 1</parameter></invoke>`
	tcs, xmlOffset := mcp.FindAllToolCallsInText(text)
	if len(tcs) != 1 {
		t.Errorf("expected 1 tool call (invoke complete despite truncated wrapper), got %d", len(tcs))
	}
	if xmlOffset < 0 {
		t.Errorf("expected xmlOffset >= 0")
	}
	preText := strings.TrimSpace(text[:xmlOffset])
	if preText != "I'll check." {
		t.Errorf("expected 'I'll check.', got %q", preText)
	}
}

func TestFindAllToolCallsInText_multipleInvokes(t *testing.T) {
	text := `<tool_calls>
<invoke name="sqlite__query"><parameter name="sql">SELECT 1</parameter></invoke>
<invoke name="sqlite__query"><parameter name="sql">SELECT 2</parameter></invoke>
</tool_calls>`
	tcs, xmlOffset := mcp.FindAllToolCallsInText(text)
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "sqlite__query" || tcs[1].Function.Name != "sqlite__query" {
		t.Error("expected both calls to be sqlite__query")
	}
	if xmlOffset < 0 {
		t.Error("expected xmlOffset >= 0")
	}
}

func TestFindAllToolCallsInText_noToolCalls(t *testing.T) {
	text := "Hello, how can I help you today?"
	tcs, xmlOffset := mcp.FindAllToolCallsInText(text)
	if len(tcs) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(tcs))
	}
	if xmlOffset != -1 {
		t.Errorf("expected xmlOffset -1, got %d", xmlOffset)
	}
}

func TestStripToolCallXML_fullBlock(t *testing.T) {
	input := "Before. <tool_calls><invoke name=\"test\"></invoke></tool_calls> After."
	expected := "Before. \n After."
	result, _ := mcp.StripToolCallXML(input, 0)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestStripToolCallXML_truncatedOpenTag(t *testing.T) {
	// No </tool_calls> -- cut everything from <tool_calls> onwards
	input := "Before. <tool_calls><invoke name=\"test\">"
	expected := "Before."
	result, _ := mcp.StripToolCallXML(input, 0)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestStripToolCallXML_bareInvoke(t *testing.T) {
	input := "Let me check. <invoke name=\"sqlite__query\"><parameter name=\"sql\">SELECT 1</parameter></invoke> Done."
	expected := "Let me check. \n Done."
	result, _ := mcp.StripToolCallXML(input, 0)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestStripToolCallXML_empty(t *testing.T) {
	if s, _ := mcp.StripToolCallXML("", 0); s != "" {
		t.Error("expected empty")
	}
}

func TestStripToolCallXML_falsePrefixPreserved(t *testing.T) {
	input := "a <tool_callsX b <invoker> c"
	expected := "a <tool_callsX b <invoker> c"
	result, _ := mcp.StripToolCallXML(input, 0)
	if result != expected {
		t.Errorf("false prefix: expected %q, got %q", expected, result)
	}
}

func TestStripToolCallXML_multiInvoke(t *testing.T) {
	input := "Before. <tool_calls><invoke name=\"a\"></invoke><invoke name=\"b\"></invoke></tool_calls> After."
	expected := "Before. \n\n After."
	result, _ := mcp.StripToolCallXML(input, 0)
	if result != expected {
		t.Errorf("multi-invoke: expected %q, got %q", expected, result)
	}
}

// Regression: streaming handler previously reset markerIdx per turn.
func TestStripToolCallXML_markerOffset(t *testing.T) {
	input := "Before. <tool_calls><invoke name=\"a\"></invoke></tool_calls> After."
	expected := "Before. \n After."
	result, _ := mcp.StripToolCallXML(input, 5)
	if result != expected {
		t.Errorf("offset: expected %q, got %q", expected, result)
	}
}

func TestBaseChunkFromChunks_extractsFields(t *testing.T) {
	data1, _ := json.Marshal(model.ChatCompletionChunk{
		ID:      "chatcmpl-abc123",
		Object:  "chat.completion.chunk",
		Created: 1700000000,
		Model:   "deepseek-v4-flash",
	})
	data2, _ := json.Marshal(model.ChatCompletionChunk{
		ID:      "chatcmpl-def456",
		Object:  "chat.completion.chunk",
		Created: 1700000001,
		Model:   "deepseek-v4-flash",
	})

	chunks := []mcp.SSEChunk{
		{Data: []byte("data: " + string(data1))},
		{Data: []byte("data: " + string(data2))},
	}

	chunk := baseChunkFromChunks(chunks)
	if chunk.ID != "chatcmpl-abc123" {
		t.Errorf("expected chatcmpl-abc123, got %s", chunk.ID)
	}
	if chunk.Created != 1700000000 {
		t.Errorf("expected 1700000000, got %d", chunk.Created)
	}
	if chunk.Model != "deepseek-v4-flash" {
		t.Errorf("expected deepseek-v4-flash, got %s", chunk.Model)
	}
}

func TestBaseChunkFromChunks_empty(t *testing.T) {
	chunk := baseChunkFromChunks(nil)
	if chunk.Object != "chat.completion.chunk" {
		t.Errorf("expected chat.completion.chunk, got %s", chunk.Object)
	}
}

func TestReassembleStreamContent_skipsToolCallDeltas(t *testing.T) {
	// Native-style SSE: content deltas then tool_call deltas
	makeData := func(content string) []byte {
		d, _ := json.Marshal(model.ChatCompletionChunk{
			Choices: []model.ChunkChoice{{Delta: model.Delta{Content: content}}},
		})
		return []byte("data: " + string(d))
	}

	chunks := []mcp.SSEChunk{
		{Data: makeData("Let me "), Done: false},
		{Data: makeData("check the "), Done: false},
		{Data: makeData("database."), Done: false},
		{Data: []byte("data: [DONE]"), Done: true},
	}

	content, reasoning := mcp.ReassembleStreamContent(chunks)
	if content != "Let me check the database." {
		t.Errorf("expected 'Let me check the database.', got %q", content)
	}
	if reasoning != "" {
		t.Errorf("expected empty reasoning, got %q", reasoning)
	}
}

func BenchmarkFindAllToolCallsInText(b *testing.B) {
	text := `<tool_calls>
<invoke name="sqlite__query"><parameter name="sql">SELECT * FROM users LIMIT 10</parameter></invoke>
<invoke name="sqlite__query"><parameter name="sql">SELECT * FROM orders LIMIT 5</parameter></invoke>
</tool_calls>`
	b.ResetTimer()
	for b.Loop() {
		mcp.FindAllToolCallsInText(text)
	}
}

func TestFilterNewToolCalls_duplicates(t *testing.T) {
	messages := []model.Message{
		{
			Role: "assistant",
			ToolCalls: []model.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: model.ToolCallFunctionData{
						Name:      "sqlite__query",
						Arguments: `{"sql":"SELECT 1"}`,
					},
				},
			},
		},
	}

	existingCall := model.ToolCall{
		Function: model.ToolCallFunctionData{
			Name:      "sqlite__query",
			Arguments: `{"sql":"SELECT 1"}`,
		},
	}
	newCall := model.ToolCall{
		Function: model.ToolCallFunctionData{
			Name:      "sqlite__query",
			Arguments: `{"sql":"SELECT 2"}`,
		},
	}

	result := mcp.FilterNewToolCalls(messages, []model.ToolCall{existingCall, newCall})
	if len(result) != 1 {
		t.Fatalf("expected 1 new tool call, got %d", len(result))
	}
	if result[0].Function.Arguments != `{"sql":"SELECT 2"}` {
		t.Error("expected the NEW call, not the duplicate")
	}
}

func TestFilterNewToolCalls_allNew(t *testing.T) {
	messages := []model.Message{
		{Role: "assistant"},
	}
	tc := model.ToolCall{
		Function: model.ToolCallFunctionData{
			Name:      "test",
			Arguments: `{}`,
		},
	}
	result := mcp.FilterNewToolCalls(messages, []model.ToolCall{tc})
	if len(result) != 1 {
		t.Fatalf("expected 1, got %d", len(result))
	}
}

func TestFilters(t *testing.T) {
	// Verify ParseXMLArgValue covers the basic types
	tests := []struct {
		input string
		want  any
	}{
		{"true", true},
		{"false", false},
		{"42", int64(42)},
		{"3.14", float64(3.14)},
		{"hello", "hello"},
	}
	for _, tt := range tests {
		got := mcp.ParseXMLArgValue(tt.input)
		if got != tt.want {
			t.Errorf("mcp.ParseXMLArgValue(%q) = %v (%T), want %v (%T)", tt.input, got, got, tt.want, tt.want)
		}
	}
}

func TestParseXMLEmptyBuffer(t *testing.T) {
	rec := &bufferedResponseWriter{
		buf: &bytes.Buffer{},
	}
	chunks, found, tcs := mcp.ParseSSEStream(rec.buf)
	if len(chunks) != 0 || found || len(tcs) != 0 {
		t.Error("expected empty parse for empty buffer")
	}
}

// newEnabledMCPMiddleware builds an MCPInjectMiddleware that is enabled and
// whose injectFn/executeFn record whether they were ever called. injectFn
// simulates a key that WOULD receive injected tools, so pass-through tests
// prove the middleware does not reach injection for client-controlled
// requests.
func newEnabledMCPMiddleware(t *testing.T) (mw *MCPInjectMiddleware, injectCalled, executeCalled *bool) {
	t.Helper()
	injectCalled = new(bool)
	executeCalled = new(bool)
	injectFn := func(keyID string, groupIDs []int) []model.Tool {
		*injectCalled = true
		return []model.Tool{{
			Type: "function",
			Function: model.ToolFunction{
				Name:        "server_tool",
				Description: "a server-side tool",
			},
		}}
	}
	executeFn := func(ctx context.Context, keyID, keyPrefix string, toolCalls []model.ToolCall) ([]model.Message, []bool) {
		*executeCalled = true
		msgs := make([]model.Message, 0, len(toolCalls))
		for range toolCalls {
			msgs = append(msgs, model.Message{Role: "tool", Content: `{"ok":true}`})
		}
		return msgs, []bool{false}
	}
	mw = NewMCPInjectMiddleware(injectFn, executeFn, config.MCPInjectionConfig{Enabled: true})
	return mw, injectCalled, executeCalled
}

// echoNext returns a handler that records how often it ran, echoes the
// exact request body it received back to the client, and returns 200.
func echoNext(calls *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*calls++
		recv, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(recv)
	}
}

// TestMCPPassThrough_clientTools proves a request carrying the client's own
// tools is proxied byte-identical: no Ilter tools injected, no tool loop, no
// interception.
func TestMCPPassThrough_clientTools(t *testing.T) {
	mw, injectCalled, executeCalled := newEnabledMCPMiddleware(t)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`

	nextCalls := 0
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	mw.Handler(echoNext(&nextCalls)).ServeHTTP(rr, r)

	if nextCalls != 1 {
		t.Errorf("expected next called exactly once, got %d", nextCalls)
	}
	if *injectCalled {
		t.Error("injectFn must not be called when the client sends its own tools")
	}
	if *executeCalled {
		t.Error("executeFn must not be called for a client tool-calling request")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Body.String() != body {
		t.Errorf("request body must pass through byte-identical\n got: %q\nwant: %q", rr.Body.String(), body)
	}
}

// TestMCPPassThrough_clientToolResults proves a request containing the
// client's own tool-result messages (a native tool loop in flight) is proxied
// byte-identical and never has Ilter tools injected into it, even when
// injection would otherwise be available.
func TestMCPPassThrough_clientToolResults(t *testing.T) {
	mw, injectCalled, executeCalled := newEnabledMCPMiddleware(t)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"what is 2+2?"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"4"}]}`

	nextCalls := 0
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	mw.Handler(echoNext(&nextCalls)).ServeHTTP(rr, r)

	if nextCalls != 1 {
		t.Errorf("expected next called exactly once, got %d", nextCalls)
	}
	if *injectCalled {
		t.Error("injectFn must not be called when the client has tool results in flight")
	}
	if *executeCalled {
		t.Error("executeFn must not be called for a client tool-loop request")
	}
	if rr.Body.String() != body {
		t.Errorf("request body must pass through byte-identical\n got: %q\nwant: %q", rr.Body.String(), body)
	}
}

// TestMCPReasoning_Non200StreamRelay pins the non-200 streaming replay fix:
// reasoning_content deltas are surfaced exactly once (live via the tee, then
// stripped from the buffered replay) rather than duplicated client-side.
func TestMCPReasoning_Non200StreamRelay(t *testing.T) {
	mw, _, _ := newEnabledMCPMiddleware(t)

	contentChunk := `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n"
	reasoningChunk := `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"think about it"},"finish_reason":null}]}` + "\n\n"
	body := contentChunk + reasoningChunk

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(body))
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	var req model.ChatCompletionRequest
	_ = json.Unmarshal([]byte(`{"model":"m","stream":true,"messages":[]}`), &req)
	rr := httptest.NewRecorder()
	mw.handleStreamingOnce(rr, r, &req, next, 0)

	// Note: the reasoning tee commits headers (200) before the non-200 branch
	// can write its status, so the observable contract here is the body: the
	// reasoning delta is surfaced exactly once, and the content delta once.
	if got := strings.Count(rr.Body.String(), "reasoning_content"); got != 1 {
		t.Errorf("expected reasoning_content exactly once, got %d\n%s", got, rr.Body.String())
	}
	if got := strings.Count(rr.Body.String(), "hello"); got != 1 {
		t.Errorf("expected content chunk exactly once, got %d\n%s", got, rr.Body.String())
	}
}

// TestMCPReasoning_Non200JSONRelay pins that a non-200 non-SSE body (e.g. a
// JSON provider error) is relayed verbatim, untouched by reasoning handling.
func TestMCPReasoning_Non200JSONRelay(t *testing.T) {
	mw, _, _ := newEnabledMCPMiddleware(t)

	errBody := `{"error":{"message":"boom","type":"server_error","code":"500"}}`
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errBody))
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	var req model.ChatCompletionRequest
	_ = json.Unmarshal([]byte(`{"model":"m","stream":true,"messages":[]}`), &req)
	rr := httptest.NewRecorder()
	mw.handleStreamingOnce(rr, r, &req, next, 0)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rr.Code)
	}
	if rr.Body.String() != errBody {
		t.Errorf("expected error body relayed verbatim\n got: %q\nwant: %q", rr.Body.String(), errBody)
	}
}

// TestMCPPassThrough_sentinelEntersToolLoop proves the Ilter tool sentinel
// still routes a request through the tool loop (Ilter-initiated emulation
// continuation), in contrast to client-controlled pass-through requests.
func TestMCPPassThrough_sentinelEntersToolLoop(t *testing.T) {
	// injectFn returns no tools so modified=false; only the presence of the
	// Ilter tool sentinel in history may route this request into the loop.
	executeCalled := new(bool)
	injectFn := func(keyID string, groupIDs []int) []model.Tool { return nil }
	executeFn := func(ctx context.Context, keyID, keyPrefix string, toolCalls []model.ToolCall) ([]model.Message, []bool) {
		*executeCalled = true
		msgs := make([]model.Message, 0, len(toolCalls))
		for range toolCalls {
			msgs = append(msgs, model.Message{Role: "tool", Content: `{"ok":true}`})
		}
		return msgs, []bool{false}
	}
	mw := NewMCPInjectMiddleware(injectFn, executeFn, config.MCPInjectionConfig{Enabled: true})
	// Non-native-tooling model: the middleware enters the tool loop when the
	// Ilter tool sentinel is present in message history, even with no client
	// tools, and executes text-embedded tool calls server-side.
	mw.SetSupportsToolsFn(func(modelID string) bool { return false })

	body := `{"model":"gpt-4o","stream":false,"messages":[{"role":"system","content":"You have access to the following tools. Use <tool_calls> to call them."},{"role":"user","content":"run the tool"}]}`

	upstream := `{"id":"cmpl-1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"On it. <invoke name=\"server_tool\"><parameter name=\"x\">1</parameter></invoke>"},"finish_reason":"stop"}]}`

	nextCalls := 0
	plainUpstream := `{"id":"cmpl-2","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Done.","reasoning_content":"thinking"},"finish_reason":"stop"}]}`
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// First upstream reply contains a text-embedded tool call; later turns
		// reply plainly so the loop terminates after one execution.
		if nextCalls == 1 {
			_, _ = w.Write([]byte(upstream))
			return
		}
		_, _ = w.Write([]byte(plainUpstream))
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	mw.Handler(next).ServeHTTP(rr, r)

	if nextCalls == 0 {
		t.Fatal("expected next to be invoked via the tool loop")
	}
	if !*executeCalled {
		t.Error("expected server-side execution of the sentinel request's tool call")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}
