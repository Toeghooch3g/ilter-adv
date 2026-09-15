package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ilter-ai/ilter/internal/features/guardrails"
	"github.com/ilter-ai/ilter/internal/features/mcp"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

// toolCallMeta holds the name/args of a tool call, keyed by call ID, so
// processToolResultMessage can log them alongside the matching result.
type toolCallMeta struct{ name, args string }

// normalizeAssistantToolCallContent clears msg.Content when it's empty
// (blank string or already nil) — assistant messages that only carry tool
// calls must not send an empty-string content field to the LLM.
func normalizeAssistantToolCallContent(msg *model.Message) {
	if s, ok := msg.Content.(string); ok {
		if strings.TrimSpace(s) == "" {
			msg.Content = nil
		}
	} else if msg.Content == nil {
		msg.Content = nil
	}
}

// normalizeToolCallFields fills in defaults the LLM/tool-loop machinery
// requires but a provider may omit: tool-call Type and empty Arguments.
func normalizeToolCallFields(toolCalls []model.ToolCall) {
	for j := range toolCalls {
		if toolCalls[j].Type == "" {
			toolCalls[j].Type = "function"
		}
		if strings.TrimSpace(toolCalls[j].Function.Arguments) == "" {
			toolCalls[j].Function.Arguments = "{}"
		}
	}
}

// normalizeToolCallMessages cleans up assistant/tool message quirks before
// sending accumulatedMessages to the LLM: empty content on tool-calling
// assistant messages, missing tool-call types/args, and any leftover Name
// on tool messages.
func normalizeToolCallMessages(accumulatedMessages []model.Message) {
	for i := range accumulatedMessages {
		msg := &accumulatedMessages[i]
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			normalizeAssistantToolCallContent(msg)
			normalizeToolCallFields(msg.ToolCalls)
		}
		if msg.Role == "tool" {
			msg.Name = ""
		}
	}
}

// buildTurnRequest marshals curReq (through mcp.ToWire emulation if the
// model doesn't natively support tools) and clones r into a new
// *http.Request carrying that body, for one tool-loop turn.
func (m *MCPInjectMiddleware) buildTurnRequest(r *http.Request, curReq *model.ChatCompletionRequest, isLastTurn bool) (*http.Request, *model.ChatCompletionRequest, error) {
	useEmulation := m.supportsToolsFn != nil && !m.supportsToolsFn(curReq.Model)
	var wireReq *model.ChatCompletionRequest
	if useEmulation {
		wireReq = mcp.ToWire(curReq, isLastTurn)
	} else {
		wireReq = curReq
	}

	wireBody, err := json.Marshal(wireReq)
	if err != nil {
		return nil, nil, err
	}

	turnReq := r.Clone(r.Context())
	turnReq.Body = io.NopCloser(bytes.NewBuffer(wireBody))
	turnReq.ContentLength = int64(len(wireBody))
	return turnReq, wireReq, nil
}

// buildToolCallInfo indexes the name/args of every tool call issued by
// assistant messages in accumulatedMessages, keyed by call ID, for the
// logging done in processToolResultMessage.
func buildToolCallInfo(accumulatedMessages []model.Message) map[string]toolCallMeta {
	info := make(map[string]toolCallMeta)
	for _, msg := range accumulatedMessages {
		if msg.Role == "assistant" {
			for _, tc := range msg.ToolCalls {
				info[tc.ID] = toolCallMeta{tc.Function.Name, tc.Function.Arguments}
			}
		}
	}
	return info
}

// maskToolResultPII masks any detected PII in s and logs a PII event per
// match found. Returns the (possibly masked) string.
func (m *MCPInjectMiddleware) maskToolResultPII(r *http.Request, s string) string {
	if m.piiMasker == nil || m.piiMasker.Masker() == nil {
		return s
	}
	matches := m.piiMasker.Masker().DetectPII(s)
	masked, piiErr := m.piiMasker.Masker().ProcessText(s, nil)
	if piiErr == nil {
		s = masked
	}
	if len(matches) > 0 {
		keyID := reqmeta.GetKeyID(r.Context())
		clientIP := r.RemoteAddr
		for _, match := range matches {
			m.piiMasker.LogPIIEvent(r.Context(), piiActionToAuditLabel(match.Action), keyID, clientIP, match, s)
		}
	}
	return s
}

// checkToolResultGuardrails replaces s with a block notice if guardrails
// flag it; otherwise returns s unchanged.
func (m *MCPInjectMiddleware) checkToolResultGuardrails(ctx context.Context, toolCallID, s string) string {
	if m.guardrailsChecker == nil {
		return s
	}
	res := m.guardrailsChecker.Check(ctx, []guardrails.Message{{Role: "user", Content: s}})
	if res.Blocked {
		mcpLog.Warn("guardrail blocked tool result", "tool_id", toolCallID, "rule_id", res.RuleID)
		return "[Tool result blocked by security guardrails]"
	}
	return s
}

// cleanToolResultContent runs PII masking then guardrail checks on tm's
// content, if it's a non-empty string. Non-string/empty content passes
// through unchanged.
func (m *MCPInjectMiddleware) cleanToolResultContent(r *http.Request, tm model.Message) any {
	s, ok := tm.Content.(string)
	if !ok || s == "" {
		return tm.Content
	}
	s = m.maskToolResultPII(r, s)
	s = m.checkToolResultGuardrails(r.Context(), tm.ToolCallID, s)
	return s
}

// logToolExecution logs one tool's execution outcome and, if configured,
// emits a tool_result event to the client.
func (m *MCPInjectMiddleware) logToolExecution(w http.ResponseWriter, tm model.Message, cleanedContent any, toolCallInfo map[string]toolCallMeta, isErr bool, turn int, turnStart time.Time) {
	toolName := "unknown"
	toolArgs := ""
	if info, ok := toolCallInfo[tm.ToolCallID]; ok {
		toolName = info.name
		toolArgs = info.args
	}
	resultSize := 0
	if s, ok := cleanedContent.(string); ok {
		resultSize = len(s)
	}
	mcpLog.Info(
		"[PROXY] Tool executed",
		"tool", toolName,
		"turn", turn+1,
		"call_id", tm.ToolCallID,
		"is_error", isErr,
		"args", toolArgs,
		"result_bytes", resultSize,
		"duration_ms", time.Since(turnStart).Milliseconds(),
	)

	if m.toolEventWriter != nil {
		evtPayload, _ := json.Marshal(map[string]any{
			"call_id":  tm.ToolCallID,
			"content":  cleanedContent,
			"is_error": isErr,
		})
		m.toolEventWriter(w, "ilter.tool_result", evtPayload)
	}
}

// processToolResultMessage applies PII masking and guardrail checks to a
// tool result message, logs the tool execution, and (if configured) emits a
// tool_result event to the client. Returns the message to accumulate.
func (m *MCPInjectMiddleware) processToolResultMessage(
	w http.ResponseWriter,
	r *http.Request,
	tm model.Message,
	toolCallInfo map[string]toolCallMeta,
	isErr bool,
	turn int,
	turnStart time.Time,
) model.Message {
	cleanedContent := m.cleanToolResultContent(r, tm)

	toolMsg := tm
	toolMsg.Content = cleanedContent

	m.logToolExecution(w, tm, cleanedContent, toolCallInfo, isErr, turn, turnStart)

	return toolMsg
}

// prepareTurnRequest builds this turn's ChatCompletionRequest: re-enabling
// tool_choice=auto after the first turn (unless the caller pinned something
// other than "auto"), forcing tool_choice=none with no tools on the final
// turn, and normalizing accumulatedMessages before attaching them.
func prepareTurnRequest(req *model.ChatCompletionRequest, accumulatedMessages []model.Message, turn, maxTurns int) model.ChatCompletionRequest {
	curReq := *req
	if turn > 0 && curReq.ToolChoice != nil && curReq.ToolChoice != "auto" {
		curReq.ToolChoice = "auto"
	}
	if turn == maxTurns-1 {
		curReq.ToolChoice = "none"
		curReq.Tools = nil
	}
	normalizeToolCallMessages(accumulatedMessages)
	curReq.Messages = accumulatedMessages
	return curReq
}

// appendFirstAssistantToolCallMessage appends the first assistant message
// found in toolMsgs (there is at most one per turn) to accumulatedMessages.
func appendFirstAssistantToolCallMessage(accumulatedMessages, toolMsgs []model.Message) []model.Message {
	for _, tm := range toolMsgs {
		if tm.Role == "assistant" {
			return append(accumulatedMessages, tm)
		}
	}
	return accumulatedMessages
}

// processToolMessages cleans, logs, and accumulates every tool-role message
// in toolMsgs, returning the updated message list and how many were processed.
func (m *MCPInjectMiddleware) processToolMessages(
	w http.ResponseWriter,
	r *http.Request,
	accumulatedMessages, toolMsgs []model.Message,
	toolErrors []bool,
	toolCallInfo map[string]toolCallMeta,
	turn int,
	turnStart time.Time,
) ([]model.Message, int) {
	toolIdx := 0
	for _, tm := range toolMsgs {
		if tm.Role != "tool" {
			continue
		}
		isErr := toolIdx < len(toolErrors) && toolErrors[toolIdx]
		toolMsg := m.processToolResultMessage(w, r, tm, toolCallInfo, isErr, turn, turnStart)
		accumulatedMessages = append(accumulatedMessages, toolMsg)
		toolIdx++
	}
	return accumulatedMessages, toolIdx
}

// runTurn executes one turn of the tool-call loop: sends curReq (via the
// streaming or non-streaming path, matching the original request's mode)
// and returns whether the LLM asked for more tool calls, those tool
// messages, and their per-call error flags.
func (m *MCPInjectMiddleware) runTurn(w http.ResponseWriter, turnReq *http.Request, wireReq *model.ChatCompletionRequest, next http.Handler, originalStream bool, markerOffset int) (hasMore bool, toolMsgs []model.Message, toolErrors []bool, newMarkerOffset int) {
	if originalStream {
		return m.handleStreamingOnce(w, turnReq, wireReq, next, markerOffset)
	}
	return m.handleNonStreamingOnce(w, turnReq, wireReq, next, originalStream, markerOffset)
}

// toolCallLoop orchestrates the multi-turn tool execution loop.
func (m *MCPInjectMiddleware) toolCallLoop(w http.ResponseWriter, r *http.Request, req *model.ChatCompletionRequest, next http.Handler) {
	maxTurns := 50
	originalStream := req.Stream
	accumulatedMessages := append([]model.Message(nil), req.Messages...)

	totalToolCount := 0
	markerOffset := 0
	for turn := range maxTurns {
		turnStart := time.Now()
		isLastTurn := turn == maxTurns-1
		curReq := prepareTurnRequest(req, accumulatedMessages, turn, maxTurns)

		turnReq, wireReq, err := m.buildTurnRequest(r, &curReq, isLastTurn)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		hasMore, toolMsgs, toolErrors, newMarkerOffset := m.runTurn(w, turnReq, wireReq, next, originalStream, markerOffset)
		markerOffset = newMarkerOffset

		if !hasMore || len(toolMsgs) == 0 {
			mcpLog.Info("[PROXY] Tool loop finished - LLM completed response", "turn", turn+1)
			return
		}

		accumulatedMessages = appendFirstAssistantToolCallMessage(accumulatedMessages, toolMsgs)
		toolCallInfo := buildToolCallInfo(accumulatedMessages)

		var processed int
		accumulatedMessages, processed = m.processToolMessages(w, r, accumulatedMessages, toolMsgs, toolErrors, toolCallInfo, turn, turnStart)
		totalToolCount += processed
	}

	mcpLog.Warn("[PROXY] Reached max tool execution turns", "max_turns", maxTurns)
	if originalStream {
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}
