package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ilter-ai/ilter/internal/model"
)

// ToolSentinelPrefix is the prefix injected into system messages
// when tools are emulated for non-tool-calling models.
const ToolSentinelPrefix = "You have access to the following tools."

// toolSentinelInstructions is the boilerplate injected as a system message
// telling a non-tool-calling model how to emit <tool_calls> XML instead.
const toolSentinelInstructions = ToolSentinelPrefix + "\n" +
	"CRITICAL RULES FOR TOOL CALLS:\n" +
	"1. NEVER invent, fake, or simulate tool outputs or JSON data in natural language text.\n" +
	"2. Whenever you need data from a tool, your response MUST contain <tool_calls>...</tool_calls>.\n" +
	"3. Do NOT write conversational text claiming you called a tool or showing fake results without emitting <tool_calls>.\n" +
	"4. Stop immediately after closing </tool_calls>. Wait for the real tool result before answering the user.\n\n" +
	"To call a tool, use this EXACT XML format:\n\n" +
	"<tool_calls>\n<invoke name=\"tool_name\">\n<parameter name=\"arg1\">value1</parameter>\n</invoke>\n</tool_calls>" +
	"\n\nAvailable tools:\n"

// buildToolSentinelMessage renders the tool-emulation system message
// listing every tool's name, description, and parameter schema.
func buildToolSentinelMessage(tools []model.Tool) model.Message {
	var sb strings.Builder
	sb.WriteString(toolSentinelInstructions)
	for _, t := range tools {
		fn := t.Function
		fmt.Fprintf(&sb, "- %s", fn.Name)
		if fn.Description != "" {
			fmt.Fprintf(&sb, ": %s", fn.Description)
		}
		sb.WriteString("\n")
		paramsJSON, _ := json.Marshal(fn.Parameters)
		fmt.Fprintf(&sb, "  params: %s\n", string(paramsJSON))
	}
	return model.Message{Role: "system", Content: sb.String()}
}

// convertAssistantToolCallsToText rewrites an assistant message's
// structured ToolCalls into inline <tool_calls>/<invoke> XML text, which
// tool-emulation mode sends instead of the native tool_calls field.
func convertAssistantToolCallsToText(msg *model.Message) {
	var tcSB strings.Builder
	tcSB.WriteString("<tool_calls>\n")
	for _, tc := range msg.ToolCalls {
		fmt.Fprintf(&tcSB, "<invoke name=\"%s\">\n", tc.Function.Name)
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
			for k, v := range args {
				valStr, _ := json.Marshal(v)
				fmt.Fprintf(&tcSB, "<parameter name=\"%s\">%s</parameter>\n", k, string(valStr))
			}
		}
		tcSB.WriteString("</invoke>\n")
	}
	tcSB.WriteString("</tool_calls>")

	if s, ok := msg.Content.(string); ok && strings.TrimSpace(s) != "" {
		msg.Content = s + "\n\n" + tcSB.String()
	} else {
		msg.Content = tcSB.String()
	}
	msg.ToolCalls = nil
}

// convertToolResultToText rewrites a tool-role message into a user-role
// message wrapping its content in <tool_result> XML, escaping any nested
// tags to prevent prompt injection.
func convertToolResultToText(msg *model.Message) {
	contentStr := ""
	if s, ok := msg.Content.(string); ok {
		contentStr = s
	}
	// Escape XML tags to prevent prompt injection
	contentStr = strings.ReplaceAll(contentStr, "</tool_result>", "&lt;/tool_result&gt;")
	contentStr = strings.ReplaceAll(contentStr, "<tool_result", "&lt;tool_result")

	toolName := msg.Name
	if toolName == "" {
		toolName = msg.ToolCallID
	}
	msg.Role = "user"
	msg.Content = fmt.Sprintf("<tool_result tool=\"%s\">\n%s\n</tool_result>", toolName, contentStr)
	msg.ToolCallID = ""
	msg.Name = ""
}

// convertMessagesToTextFormat rewrites every assistant tool-call message
// and tool-result message in messages into the inline-XML text form that
// tool-emulation mode uses instead of native tool_calls/tool-role messages.
func convertMessagesToTextFormat(messages []model.Message) {
	for i := range messages {
		msg := &messages[i]
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			convertAssistantToolCallsToText(msg)
		}
		if msg.Role == "tool" {
			convertToolResultToText(msg)
		}
	}
}

// findSentinelMessageIndex returns the index of the first message whose
// string content contains the tool-sentinel prefix, or -1 if none.
func findSentinelMessageIndex(messages []model.Message) int {
	for i := range messages {
		if s, ok := messages[i].Content.(string); ok && strings.Contains(s, ToolSentinelPrefix) {
			return i
		}
	}
	return -1
}

// applySentinelTransition injects, updates, or refreshes the tool-sentinel
// system message on cp.Messages, per the state ToWire computed.
func applySentinelTransition(cp *model.ChatCompletionRequest, tools []model.Tool, alreadyInjected, exhausted bool) {
	switch {
	case !alreadyInjected && len(tools) > 0:
		sysMsg := buildToolSentinelMessage(tools)
		cp.Messages = append([]model.Message{sysMsg}, cp.Messages...)

	case alreadyInjected && exhausted:
		if idx := findSentinelMessageIndex(cp.Messages); idx >= 0 {
			cp.Messages[idx].Content = "Tools have been executed. Answer the user directly using the tool results above. Do NOT emit any XML or tool calls."
		}

	case alreadyInjected && !exhausted && len(tools) > 0:
		if idx := findSentinelMessageIndex(cp.Messages); idx >= 0 {
			cp.Messages = append(cp.Messages[:idx], cp.Messages[idx+1:]...)
		}
		sysMsg := buildToolSentinelMessage(tools)
		cp.Messages = append([]model.Message{sysMsg}, cp.Messages...)
	}
}

// ToWire converts a ChatCompletionRequest to the wire format suitable for
// models that don't support native tool calling. When exhausted is true and
// the sentinel has already been injected, it replaces it with a "tools done"
// instruction.
func ToWire(req *model.ChatCompletionRequest, exhausted bool) *model.ChatCompletionRequest {
	cp := *req
	cp.Messages = make([]model.Message, len(req.Messages))
	copy(cp.Messages, req.Messages)

	applySentinelTransition(&cp, req.Tools, HasToolSentinel(cp.Messages), exhausted)

	cp.Tools = nil
	cp.ToolChoice = nil

	convertMessagesToTextFormat(cp.Messages)

	return &cp
}
