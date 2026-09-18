package middleware

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ilter-ai/ilter/internal/features/mcp"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

// writeBufferedResponse copies rec's headers/status to w then writes body
// (either rec's original bytes, or a transformed replacement).
func writeBufferedResponse(w http.ResponseWriter, rec *bufferedResponseWriter, body []byte) {
	copyHeaders(w.Header(), rec.header)
	w.WriteHeader(rec.code)
	_, _ = w.Write(body)
}

// writeSSEDone writes the terminal "data: [DONE]" SSE event and flushes.
func writeSSEDone(w http.ResponseWriter) {
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// detectNonStreamToolCalls extracts tool calls from chatResp, falling back
// to parsing text-embedded tool-call XML when the model doesn't natively
// support tool calling.
func (m *MCPInjectMiddleware) detectNonStreamToolCalls(chatResp *model.ChatCompletionResponse, modelName string) []model.ToolCall {
	toolCalls := mcp.ExtractToolCalls(chatResp)
	if len(toolCalls) == 0 && m.supportsToolsFn != nil && !m.supportsToolsFn(modelName) {
		if mcp.NormalizeTextToolCalls(chatResp) {
			toolCalls = mcp.ExtractToolCalls(chatResp)
		}
	}
	return toolCalls
}

// respondNoToolCalls writes chatResp back to the client (its tool-call XML
// blocks stripped) for a turn that produced no tool calls at all.
func respondNoToolCalls(w http.ResponseWriter, rec *bufferedResponseWriter, chatResp model.ChatCompletionResponse, markerIdx int) int {
	for i := range chatResp.Choices {
		var content string
		content, markerIdx = mcp.StripToolCallXML(chatResp.Choices[i].Message.Content, markerIdx)
		chatResp.Choices[i].Message.Content = content
	}
	cleanBytes, _ := json.Marshal(chatResp)
	writeBufferedResponse(w, rec, cleanBytes)
	return markerIdx
}

// respondDuplicateToolCalls writes chatResp back to the client with tool
// calls cleared and finish_reason forced to "stop", for a turn where every
// detected tool call turned out to be a duplicate of one already issued.
func respondDuplicateToolCalls(w http.ResponseWriter, rec *bufferedResponseWriter, chatResp model.ChatCompletionResponse, markerIdx int) int {
	mcpLog.Warn("all tool calls are duplicates, writing original response")
	for i := range chatResp.Choices {
		var content string
		content, markerIdx = mcp.StripToolCallXML(chatResp.Choices[i].Message.Content, markerIdx)
		chatResp.Choices[i].Message.ToolCalls = nil
		chatResp.Choices[i].FinishReason = "stop"
		chatResp.Choices[i].Message.Content = content
	}
	cleanBytes, _ := json.Marshal(chatResp)
	writeBufferedResponse(w, rec, cleanBytes)
	return markerIdx
}

// respondEmptyToolExecution writes chatResp back with an explanatory
// message in place of the tool call, when tool execution produced no
// result messages at all (e.g. the MCP server was unreachable).
func respondEmptyToolExecution(w http.ResponseWriter, rec *bufferedResponseWriter, chatResp model.ChatCompletionResponse, toolCalls []model.ToolCall, markerIdx int) int {
	mcpLog.Warn("tool execution produced no messages")
	for i := range chatResp.Choices {
		var content string
		content, markerIdx = mcp.StripToolCallXML(chatResp.Choices[i].Message.Content, markerIdx)
		if content == "" {
			content = fmt.Sprintf("Tool %s could not be executed. The MCP server may be offline or not responding.", toolCalls[0].Function.Name)
		}
		chatResp.Choices[i].Message.Content = content
		chatResp.Choices[i].Message.ToolCalls = nil
		chatResp.Choices[i].FinishReason = "stop"
	}
	cleanBytes, _ := json.Marshal(chatResp)
	writeBufferedResponse(w, rec, cleanBytes)
	return markerIdx
}

func (m *MCPInjectMiddleware) handleNonStreamingOnce(
	w http.ResponseWriter,
	r *http.Request,
	req *model.ChatCompletionRequest,
	next http.Handler,
	markerOffset int,
) (bool, []model.Message, []bool, int) {
	rec := &bufferedResponseWriter{
		header: make(http.Header),
		buf:    &bytes.Buffer{},
	}

	next.ServeHTTP(rec, r)

	if rec.code != http.StatusOK {
		writeBufferedResponse(w, rec, rec.buf.Bytes())
		return false, nil, nil, markerOffset
	}

	var chatResp model.ChatCompletionResponse
	if err := json.Unmarshal(rec.buf.Bytes(), &chatResp); err != nil {
		writeBufferedResponse(w, rec, rec.buf.Bytes())
		return false, nil, nil, markerOffset
	}

	toolCalls := m.detectNonStreamToolCalls(&chatResp, req.Model)

	if len(toolCalls) == 0 {
		markerIdx := respondNoToolCalls(w, rec, chatResp, markerOffset)
		return false, nil, nil, markerIdx
	}

	keyID := reqmeta.GetKeyID(r.Context())

	toolCalls = mcp.FilterNewToolCalls(req.Messages, toolCalls)

	mcpLog.Info("[PROXY] LLM model returned tool calls", "count", len(toolCalls), "tool_calls", toolCalls)

	cleanedAssistantText := ""
	markerIdx := markerOffset
	if len(chatResp.Choices) > 0 {
		cleanedAssistantText, markerIdx = mcp.StripToolCallXML(chatResp.Choices[0].Message.Content, markerIdx)
	}

	for i := range toolCalls {
		if toolCalls[i].Type == "" {
			toolCalls[i].Type = "function"
		}
	}

	assistantMsg := model.Message{
		Role:      "assistant",
		Content:   cleanedAssistantText,
		ToolCalls: toolCalls,
	}

	if len(toolCalls) == 0 {
		markerIdx = respondDuplicateToolCalls(w, rec, chatResp, markerIdx)
		return false, nil, nil, markerIdx
	}

	toolMsgs, toolErrors := m.executeFn(r.Context(), keyID, "", toolCalls)
	if len(toolMsgs) == 0 {
		markerIdx = respondEmptyToolExecution(w, rec, chatResp, toolCalls, markerIdx)
		return false, nil, nil, markerIdx
	}

	allMsgs := []model.Message{assistantMsg}
	allMsgs = append(allMsgs, toolMsgs...)
	return true, allMsgs, toolErrors, markerIdx
}

// mergeTextToolCalls appends any text-embedded tool calls found in fullText
// to reconstructedToolCalls when the model doesn't natively support tool
// calling and the stream itself carried no structured tool calls.
func (m *MCPInjectMiddleware) mergeTextToolCalls(modelName, fullText string, toolCallFound bool, reconstructedToolCalls []model.ToolCall) ([]model.ToolCall, bool) {
	if m.supportsToolsFn == nil || m.supportsToolsFn(modelName) || (toolCallFound && len(reconstructedToolCalls) > 0) {
		return reconstructedToolCalls, toolCallFound
	}
	allTCs, _ := mcp.FindAllToolCallsInText(fullText)
	if len(allTCs) == 0 {
		return reconstructedToolCalls, toolCallFound
	}
	mcpLog.Debug("found text tool calls in stream", "count", len(allTCs))
	for i := range allTCs {
		tc := &allTCs[i]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_stream_%d", i)
		}
		if tc.Type == "" {
			tc.Type = "function"
		}
		reconstructedToolCalls = append(reconstructedToolCalls, *tc)
	}
	return reconstructedToolCalls, true
}

// stripReasoningFromChunk attempts to parse data as a ChatCompletionChunk
// and clear any reasoning_content deltas. Returns the re-marshaled bytes
// and true if it changed anything; otherwise ok is false and the caller
// should relay data unmodified.
func stripReasoningFromChunk(data string) (out []byte, ok bool) {
	body, isData := strings.CutPrefix(data, "data: ")
	if !isData || body == "[DONE]" {
		return nil, false
	}
	var cc model.ChatCompletionChunk
	if err := json.Unmarshal([]byte(body), &cc); err != nil {
		return nil, false
	}
	modified := false
	for i := range cc.Choices {
		if cc.Choices[i].Delta.ReasoningContent != "" {
			cc.Choices[i].Delta.ReasoningContent = ""
			modified = true
		}
	}
	if !modified {
		return nil, false
	}
	newData, _ := json.Marshal(cc)
	return newData, true
}

// isSSEStream reports whether data contains SSE "data:" events, as opposed
// to a single JSON (or other non-SSE) error body. It reads from a copy so
// the caller's buffer is left intact.
func isSSEStream(data []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			return true
		}
	}
	return false
}

// relayOriginalChunksStrippingReasoning re-emits every original SSE chunk
// verbatim, except reasoning_content deltas are cleared (already surfaced
// separately during streaming; echoing them again in the final relay would
// duplicate them client-side).
func relayOriginalChunksStrippingReasoning(w http.ResponseWriter, chunks []mcp.SSEChunk) {
	for _, c := range chunks {
		if newData, ok := stripReasoningFromChunk(string(c.Data)); ok {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(newData))
			continue
		}
		_, _ = w.Write(c.Data)
		_, _ = w.Write([]byte("\n\n"))
	}
}

// relayCleanStream writes the SSE stream back to the client when no tool
// calls were found: either a single synthesized chunk (if content changed
// due to marker stripping) or every original chunk with any reasoning
// content stripped out.
func relayCleanStream(w http.ResponseWriter, rec *reasoningTeeWriter, chunks []mcp.SSEChunk, fullText, cleanedText string) {
	copyHeaders(w.Header(), rec.header)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(rec.code)
	if cleanedText != strings.TrimSpace(fullText) {
		chunk := baseChunkFromChunks(chunks)
		chunk.Choices = []model.ChunkChoice{{
			Index:        0,
			Delta:        model.Delta{Content: cleanedText},
			FinishReason: new("stop"),
		}}
		chunkBytes, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(chunkBytes))
	} else {
		relayOriginalChunksStrippingReasoning(w, chunks)
	}
	writeSSEDone(w)
}

// relayDuplicateToolCallsStream emits a final chunk with cleanedText (if
// there's anything to show) followed by [DONE], for a streaming turn where
// every detected tool call turned out to be a duplicate.
func relayDuplicateToolCallsStream(w http.ResponseWriter, rec *reasoningTeeWriter, chunks []mcp.SSEChunk, cleanedText, reasoningText string, toolCallCount int) {
	copyHeaders(w.Header(), rec.header)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(rec.code)
	if cleanedText != "" || reasoningText != "" || toolCallCount > 0 {
		chunk := baseChunkFromChunks(chunks)
		chunk.Choices = []model.ChunkChoice{{
			Index:        0,
			Delta:        model.Delta{Content: cleanedText},
			FinishReason: new("stop"),
		}}
		chunkBytes, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(chunkBytes))
	}
	writeSSEDone(w)
}

// emitAssistantContentChunk sends the assistant's visible content as one SSE
// chunk, if there's anything to show. No tool-call position markers are ever
// emitted into client-visible text.
func emitAssistantContentChunk(w http.ResponseWriter, chunks []mcp.SSEChunk, cleanedText, reasoningText string, toolCallCount int) {
	if cleanedText == "" && reasoningText == "" && toolCallCount == 0 {
		return
	}
	chunk := baseChunkFromChunks(chunks)
	chunk.Choices = []model.ChunkChoice{{
		Index: 0,
		Delta: model.Delta{Content: cleanedText},
	}}
	chunkBytes, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", string(chunkBytes))
}

// emitStreamingToolExecutionError sends an explanatory chunk (falling back
// to a generic message) plus [DONE], when tool execution produced no result
// messages at all.
func emitStreamingToolExecutionError(w http.ResponseWriter, chunks []mcp.SSEChunk, cleanedText, reasoningText string, reconstructedToolCalls []model.ToolCall) {
	mcpLog.Warn("streaming tool execution produced no messages")
	errContent := cleanedText
	if stripped, _ := mcp.StripToolCallXML(errContent, 0); stripped != "" {
		errContent = stripped
	}
	if errContent == "" && len(reconstructedToolCalls) > 0 {
		errContent = fmt.Sprintf(
			"Tool %s could not be executed. The MCP server may be offline or not responding.",
			reconstructedToolCalls[0].Function.Name,
		)
	}
	if errContent != "" || reasoningText != "" || len(reconstructedToolCalls) > 0 {
		errChunk := baseChunkFromChunks(chunks)
		errChunk.Choices = []model.ChunkChoice{{
			Index:        0,
			Delta:        model.Delta{Content: errContent},
			FinishReason: new("stop"),
		}}
		errBytes, _ := json.Marshal(errChunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(errBytes))
	}
	writeSSEDone(w)
}

func (m *MCPInjectMiddleware) handleStreamingOnce(
	w http.ResponseWriter,
	r *http.Request,
	req *model.ChatCompletionRequest,
	next http.Handler,
	toolOffset int,
) (bool, []model.Message, []bool, int) {
	flusher, _ := w.(http.Flusher)
	rec := &reasoningTeeWriter{
		w:       w,
		flusher: flusher,
		header:  make(http.Header),
		buf:     &bytes.Buffer{},
	}

	next.ServeHTTP(rec, r)

	if rec.code != http.StatusOK {
		copyHeaders(w.Header(), rec.header)
		w.WriteHeader(rec.code)
		// reasoningTeeWriter already surfaced reasoning_content deltas live.
		// If the buffered body is an SSE stream, re-emit it with reasoning
		// stripped so the client does not see those deltas twice; otherwise
		// (e.g. a JSON error body) relay it verbatim.
		if isSSEStream(rec.buf.Bytes()) {
			chunks, _, _ := mcp.ParseSSEStream(rec.buf)
			relayOriginalChunksStrippingReasoning(w, chunks)
		} else {
			_, _ = w.Write(rec.buf.Bytes())
		}
		return false, nil, nil, toolOffset
	}

	chunks, toolCallFound, reconstructedToolCalls := mcp.ParseSSEStream(rec.buf)
	fullText, reasoningText := mcp.ReassembleStreamContent(chunks)
	reconstructedToolCalls, toolCallFound = m.mergeTextToolCalls(req.Model, fullText, toolCallFound, reconstructedToolCalls)

	cleanedText, markerIdx := mcp.StripToolCallXML(fullText, toolOffset)

	if !toolCallFound || len(reconstructedToolCalls) == 0 {
		relayCleanStream(w, rec, chunks, fullText, cleanedText)
		return false, nil, nil, markerIdx
	}

	mcpLog.Debug("streaming response contained tool_calls", "count", len(reconstructedToolCalls))
	keyID := reqmeta.GetKeyID(r.Context())

	newToolCalls := mcp.FilterNewToolCalls(req.Messages, reconstructedToolCalls)
	if len(newToolCalls) == 0 {
		mcpLog.Debug("all tool calls are duplicates in stream, writing clean response", "tool", reconstructedToolCalls[0].Function.Name)
		relayDuplicateToolCallsStream(w, rec, chunks, cleanedText, reasoningText, len(reconstructedToolCalls))
		return false, nil, nil, markerIdx
	}
	reconstructedToolCalls = newToolCalls

	// This is the first write to the real w in this turn (every other branch
	// above already copies headers before its first write) — without this,
	// X-Ilter-Model-Actual and friends never reach the client whenever the
	// loop needs 2+ turns: headers can only be set before the first byte, and
	// Go silently drops any header set after that on a later, header-copying
	// turn once this one has already committed the response.
	copyHeaders(w.Header(), rec.header)

	emitAssistantContentChunk(w, chunks, cleanedText, reasoningText, len(reconstructedToolCalls))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	toolMsgs, toolErrors := m.executeFn(r.Context(), keyID, "", reconstructedToolCalls)

	assistantMsg := model.Message{
		Role:      "assistant",
		Content:   cleanedText,
		ToolCalls: reconstructedToolCalls,
	}

	if len(toolMsgs) == 0 {
		emitStreamingToolExecutionError(w, chunks, cleanedText, reasoningText, reconstructedToolCalls)
		return false, nil, nil, markerIdx
	}

	allMsgs := []model.Message{assistantMsg}
	allMsgs = append(allMsgs, toolMsgs...)

	return true, allMsgs, toolErrors, markerIdx
}
