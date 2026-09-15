package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/ilter-ai/ilter/internal/model"
)

// SSEChunk represents a single SSE data chunk from a streaming response.
type SSEChunk struct {
	Data []byte
	Done bool
}

// sseParseState accumulates the results of ParseSSEStream while scanning
// lines one at a time.
type sseParseState struct {
	currentData   bytes.Buffer
	chunks        []SSEChunk
	toolCallFound bool
	toolCalls     []model.ToolCall
}

// detectAndMergeToolCalls updates toolCallFound from cc's choices and, once
// found, merges any tool-call deltas into toolCalls.
func (s *sseParseState) detectAndMergeToolCalls(cc model.ChatCompletionChunk) {
	for _, c := range cc.Choices {
		if len(c.Delta.ToolCalls) > 0 {
			s.toolCallFound = true
		}
		if c.FinishReason != nil && *c.FinishReason == "tool_calls" {
			s.toolCallFound = true
		}
	}
	if s.toolCallFound {
		s.toolCalls = MergeStreamToolCalls(s.toolCalls, cc)
	}
}

// flushCurrentData appends the buffered currentData as a chunk, if non-empty.
func (s *sseParseState) flushCurrentData() {
	if s.currentData.Len() > 0 {
		s.chunks = append(s.chunks, SSEChunk{Data: bytes.Clone(s.currentData.Bytes())})
		s.currentData.Reset()
	}
}

// handleDataLine processes a "data: ..." line. It returns true when the
// stream is done (a "[DONE]" sentinel was seen).
func (s *sseParseState) handleDataLine(line, data string) (done bool) {
	if data == "[DONE]" {
		s.chunks = append(s.chunks, SSEChunk{Data: []byte(line), Done: true})
		return true
	}
	var cc model.ChatCompletionChunk
	if err := json.Unmarshal([]byte(data), &cc); err == nil {
		s.detectAndMergeToolCalls(cc)
	}
	s.currentData.Reset()
	s.currentData.WriteString(line)
	return false
}

// handleLine processes a single scanned line, returning true if parsing
// should stop.
func (s *sseParseState) handleLine(line string) (done bool) {
	switch data, ok := strings.CutPrefix(line, "data: "); {
	case ok:
		if s.handleDataLine(line, data) {
			return true
		}
	case strings.HasPrefix(line, "event:") || strings.HasPrefix(line, ":"):
		s.currentData.Reset()
		s.currentData.WriteString(line)
	case line == "":
		s.flushCurrentData()
		return false
	default:
		s.currentData.WriteString("\n")
		s.currentData.WriteString(line)
	}
	if strings.TrimSpace(line) == "" && s.currentData.Len() > 0 {
		s.flushCurrentData()
	}
	return false
}

// ParseSSEStream parses an SSE byte stream into chunks and detects tool calls.
func ParseSSEStream(buf *bytes.Buffer) (chunks []SSEChunk, toolCallFound bool, toolCalls []model.ToolCall) {
	scanner := bufio.NewScanner(buf)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)

	state := &sseParseState{}
	for scanner.Scan() {
		if state.handleLine(scanner.Text()) {
			break
		}
	}
	state.flushCurrentData()
	return state.chunks, state.toolCallFound, state.toolCalls
}

// updateExistingToolCall merges a streamed tool-call delta into dst, an
// already-known tool call at the same index.
func updateExistingToolCall(dst *model.ToolCall, tc model.ChunkToolCall) {
	existingArgs := dst.Function.Arguments
	newArgs := tc.Function.Arguments
	switch {
	case existingArgs == "" || existingArgs == "{}":
		dst.Function.Arguments = newArgs
	case strings.HasPrefix(newArgs, existingArgs):
		dst.Function.Arguments = newArgs
	default:
		dst.Function.Arguments += newArgs
	}

	if tc.ID != "" {
		dst.ID = tc.ID
	}
	if tc.Function.Name != "" {
		dst.Function.Name = tc.Function.Name
	}
	if tc.Type != "" {
		dst.Type = tc.Type
	}
	if dst.Type == "" {
		dst.Type = "function"
	}
}

// newToolCallFromDelta builds a fresh model.ToolCall from a streamed delta.
func newToolCallFromDelta(tc model.ChunkToolCall) model.ToolCall {
	tcType := tc.Type
	if tcType == "" {
		tcType = "function"
	}
	return model.ToolCall{
		ID:   tc.ID,
		Type: tcType,
		Function: model.ToolCallFunctionData{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		},
	}
}

// mergeStreamToolCallDelta merges a single streamed tool-call delta into
// existing, growing the slice if the delta targets a new index.
func mergeStreamToolCallDelta(existing []model.ToolCall, tc model.ChunkToolCall) []model.ToolCall {
	idx := tc.Index
	if idx < len(existing) {
		updateExistingToolCall(&existing[idx], tc)
		return existing
	}
	for len(existing) <= idx {
		existing = append(existing, model.ToolCall{})
	}
	existing[idx] = newToolCallFromDelta(tc)
	return existing
}

// MergeStreamToolCalls merges tool call deltas from streaming chunks.
func MergeStreamToolCalls(existing []model.ToolCall, chunk model.ChatCompletionChunk) []model.ToolCall {
	for _, c := range chunk.Choices {
		for _, tc := range c.Delta.ToolCalls {
			existing = mergeStreamToolCallDelta(existing, tc)
		}
	}
	return existing
}

// ReassembleStreamContent concatenates all content deltas from SSE chunks.
func ReassembleStreamContent(chunks []SSEChunk) (string, string) {
	var sb, reasoning strings.Builder
	for _, c := range chunks {
		if c.Done {
			break
		}
		body, ok := strings.CutPrefix(string(c.Data), "data: ")
		if !ok {
			continue
		}
		if body == "[DONE]" {
			break
		}
		var cc model.ChatCompletionChunk
		if err := json.Unmarshal([]byte(body), &cc); err != nil {
			continue
		}
		for _, ch := range cc.Choices {
			sb.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.ReasoningContent)
		}
	}
	return sb.String(), reasoning.String()
}
