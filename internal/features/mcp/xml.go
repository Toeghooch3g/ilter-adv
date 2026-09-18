package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/ilter-ai/ilter/internal/model"
)

// FindToolCallsOpen finds an opening <tool_calls> tag starting from `from`.
func FindToolCallsOpen(s string, from int) (start, contentStart int, ok bool) {
	const prefix = "<tool_calls"
	for {
		i := strings.Index(s[from:], prefix)
		if i < 0 {
			return 0, 0, false
		}
		start = from + i
		k := start + len(prefix)
		if k >= len(s) {
			return 0, 0, false
		}
		switch s[k] {
		case ' ', '>', '\n', '\r', '\t', '/':
			e := strings.IndexByte(s[k:], '>')
			if e < 0 {
				return 0, 0, false
			}
			return start, k + e + 1, true
		default:
			from = k
		}
	}
}

// countInvokes counts "<invoke" tag occurrences in inner, treating any
// "<invoke" not immediately followed by a tag-boundary character as a false
// match (e.g. "<invokeX").
func countInvokes(inner string) int {
	count := 0
	scanOff := 0
	for {
		ii := strings.Index(inner[scanOff:], "<invoke")
		if ii < 0 {
			break
		}
		invokeStart := scanOff + ii
		if len(inner) > invokeStart+7 {
			ic := inner[invokeStart+7]
			if ic != ' ' && ic != '>' && ic != '\n' && ic != '\r' && ic != '\t' && ic != '/' {
				scanOff = invokeStart + 7
				continue
			}
		}
		count++
		iiClose := strings.Index(inner[invokeStart:], "</invoke>")
		if iiClose < 0 {
			scanOff = invokeStart + 7
		} else {
			scanOff = invokeStart + iiClose + len("</invoke>")
		}
	}
	return count
}

// stripToolCallsBlocks replaces every <tool_calls>...</tool_calls> block in
// content with one newline per <invoke> it contains (at least one, even if
// none were found — a malformed block still consumed a turn). The markerIdx
// still counts consumed tool calls so callers can keep per-turn offsets
// unique; no position markers are emitted into client-visible text.
func stripToolCallsBlocks(content string, markerIdx int) (string, int) {
	tcOff := 0
	for {
		iStart, iContentEnd, ok := FindToolCallsOpen(content, tcOff)
		if !ok {
			break
		}
		closeTag := "</tool_calls>"
		end := strings.Index(content[iContentEnd:], closeTag)
		if end < 0 {
			content = strings.TrimSpace(content[:iStart])
			tcOff = 0
			continue
		}
		fullEnd := iContentEnd + end + len(closeTag)
		invokeCount := countInvokes(content[iContentEnd:fullEnd])
		if invokeCount == 0 {
			invokeCount = 1
		}
		markerIdx += invokeCount
		replacement := strings.Repeat("\n", invokeCount)
		content = content[:iStart] + replacement + content[fullEnd:]
		tcOff = iStart + len(replacement)
	}
	return content, markerIdx
}

// stripBareInvokes replaces every bare <invoke>...</invoke> block (not
// already consumed inside a <tool_calls> block) with a newline.
func stripBareInvokes(content string, markerIdx int) (string, int) {
	invokeOff := 0
	for {
		i := strings.Index(content[invokeOff:], "<invoke")
		if i < 0 {
			break
		}
		start := invokeOff + i
		if len(content) > start+7 {
			c := content[start+7]
			if c != ' ' && c != '>' && c != '\n' && c != '\r' && c != '\t' && c != '/' {
				invokeOff = start + 7
				continue
			}
		}
		end := strings.Index(content[start:], "</invoke>")
		if end < 0 {
			content = strings.TrimSpace(content[:start])
			invokeOff = 0
			continue
		}
		markerIdx++
		content = content[:start] + "\n" + content[start+end+len("</invoke>"):]
		invokeOff = start + 1
	}
	return content, markerIdx
}

// stripOrphanParameters removes any <parameter>...</parameter> blocks left
// over outside a stripped <invoke>/<tool_calls> block.
func stripOrphanParameters(content string) string {
	for {
		idx := strings.Index(content, "<parameter")
		if idx < 0 {
			break
		}
		gt := strings.IndexByte(content[idx:], '>')
		closeTagParam := "</parameter>"
		ci := strings.Index(content[idx:], closeTagParam)
		if gt >= 0 && ci >= 0 {
			end := idx + ci + len(closeTagParam)
			content = content[:idx] + content[end:]
		} else {
			content = strings.TrimSpace(content[:idx])
		}
	}
	return content
}

// stripDanglingCloseTags removes any leftover closing tags (</tool_calls>,
// </invoke>, </parameter>) that the earlier passes didn't already consume.
func stripDanglingCloseTags(content string) string {
	for _, tag := range []string{"</tool_calls>", "</invoke>", "</parameter>"} {
		for {
			idx := strings.Index(content, tag)
			if idx < 0 {
				break
			}
			content = content[:idx] + content[idx+len(tag):]
		}
	}
	return content
}

// StripToolCallXML removes tool call XML blocks, leaving clean text in
// place of each block. markerOffset is the starting index for consumed tool
// calls; callers that need indices to be globally unique across turns pass
// their per-request toolOffset. Callers without offset tracking pass 0.
func StripToolCallXML(content string, markerOffset int) (string, int) {
	if content == "" {
		return content, 0
	}
	markerIdx := markerOffset

	content, markerIdx = stripToolCallsBlocks(content, markerIdx)
	content, markerIdx = stripBareInvokes(content, markerIdx)
	content = stripOrphanParameters(content)
	content = stripDanglingCloseTags(content)

	return strings.TrimSpace(content), markerIdx
}

// ParseXMLArgValue converts a string to its typed value (bool, int64, float64, or string).
func ParseXMLArgValue(v string) any {
	if v == "true" || v == "false" {
		return v == "true"
	}
	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}

// parseXMLParams extracts every <parameter name="...">value</parameter>
// entry from rest, stopping once a </invoke> boundary is reached (or a
// malformed parameter tag breaks the scan).
func parseXMLParams(rest string) map[string]any {
	args := make(map[string]any)
	paramTag := "<parameter name=\""
	for {
		invEnd := strings.Index(rest, "</invoke>")
		pIdx := strings.Index(rest, paramTag)
		if pIdx < 0 || (invEnd >= 0 && invEnd < pIdx) {
			break
		}
		keyStart := pIdx + len(paramTag)
		keyEnd := strings.IndexByte(rest[keyStart:], '"')
		if keyEnd < 0 {
			break
		}
		key := rest[keyStart : keyStart+keyEnd]
		valStart := keyStart + keyEnd + 1
		vIdx := strings.Index(rest[valStart:], ">")
		if vIdx < 0 {
			break
		}
		actualValStart := valStart + vIdx + 1
		endTag := "</parameter>"
		eIdx := strings.Index(rest[actualValStart:], endTag)
		if eIdx < 0 {
			break
		}
		val := rest[actualValStart : actualValStart+eIdx]
		args[key] = ParseXMLArgValue(val)
		rest = rest[actualValStart+eIdx+len(endTag):]
	}
	return args
}

// remainingTextAfterToolCall returns the text following the enclosing
// </tool_calls> tag if present, else the text following this <invoke>'s own
// </invoke> tag (searched starting at invokeSearchStart), else empty.
func remainingTextAfterToolCall(text string, invokeSearchStart int) string {
	closeTag := "</tool_calls>"
	if _, after, ok := strings.Cut(text, closeTag); ok {
		return after
	}
	invClose := "</invoke>"
	if icIdx := strings.Index(text[invokeSearchStart:], invClose); icIdx >= 0 {
		return text[invokeSearchStart+icIdx+len(invClose):]
	}
	return ""
}

// parseXMLToolCall parses a single <invoke> block and returns the ToolCall and remaining text.
func parseXMLToolCall(text string) (*model.ToolCall, string) {
	startTag := "<invoke name=\""
	if !strings.Contains(text, startTag) {
		return nil, ""
	}

	nameStart := strings.Index(text, startTag) + len(startTag)
	nameEnd := strings.IndexByte(text[nameStart:], '"')
	if nameEnd < 0 {
		return nil, ""
	}
	toolName := text[nameStart : nameStart+nameEnd]
	if toolName == "" {
		return nil, ""
	}

	args := parseXMLParams(text[nameStart+nameEnd:])

	argsJSON, _ := json.Marshal(args)
	var idBuf [8]byte
	_, _ = rand.Read(idBuf[:])
	tc := &model.ToolCall{
		ID:   "call_xml_" + hex.EncodeToString(idBuf[:]),
		Type: "function",
		Function: model.ToolCallFunctionData{
			Name:      toolName,
			Arguments: string(argsJSON),
		},
	}

	return tc, remainingTextAfterToolCall(text, nameStart+nameEnd)
}

// tryParseToolCallsBlock looks for a <tool_calls>...</tool_calls> block in
// text and parses every <invoke> inside it. found reports whether an
// opening <tool_calls> tag was located at all (xmlOffset is only
// meaningful when found is true); tcs may still be empty even when found.
func tryParseToolCallsBlock(text string) (tcs []model.ToolCall, xmlOffset int, found bool) {
	start, contentEnd, found := FindToolCallsOpen(text, 0)
	if !found {
		return nil, -1, false
	}
	xmlOffset = start

	closeTag := "</tool_calls>"
	closeIdx := strings.Index(text[contentEnd:], closeTag)
	if closeIdx < 0 {
		return nil, xmlOffset, true
	}

	afterClose := contentEnd + closeIdx + len(closeTag)
	if _, _, ok2 := FindToolCallsOpen(text, afterClose); ok2 {
		mcpLog.Warn("multiple <tool_calls> blocks found, only first parsed", "first_offset", start)
	}

	remaining := text[contentEnd : contentEnd+closeIdx]
	for {
		tc, after := parseXMLToolCall(remaining)
		if tc == nil {
			break
		}
		tcs = append(tcs, *tc)
		remaining = after
	}
	return tcs, xmlOffset, true
}

// FindAllToolCallsInText scans text for <tool_calls> and bare <invoke> XML blocks.
func FindAllToolCallsInText(text string) ([]model.ToolCall, int) {
	tcs, xmlOffset, found := tryParseToolCallsBlock(text)
	if found && len(tcs) > 0 {
		return tcs, xmlOffset
	}
	if !found {
		xmlOffset = -1
	}

	tc, _ := parseXMLToolCall(text)
	if tc != nil {
		tcs = append(tcs, *tc)
		if xmlOffset < 0 {
			if idx := strings.Index(text, "<invoke"); idx >= 0 {
				xmlOffset = idx
			}
		}
	}

	return tcs, xmlOffset
}

// FilterNewToolCalls removes tool calls that duplicate existing ones in the last assistant message.
func FilterNewToolCalls(messages []model.Message, toolCalls []model.ToolCall) []model.ToolCall {
	if len(messages) == 0 {
		return toolCalls
	}

	var lastAssistant *model.Message
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			lastAssistant = &messages[i]
			break
		}
	}
	if lastAssistant == nil {
		return toolCalls
	}

	var newTCs []model.ToolCall
	for _, tc := range toolCalls {
		if !slices.ContainsFunc(lastAssistant.ToolCalls, func(existing model.ToolCall) bool {
			return existing.Function.Name == tc.Function.Name && existing.Function.Arguments == tc.Function.Arguments
		}) {
			newTCs = append(newTCs, tc)
		}
	}
	return newTCs
}

// ExtractToolCalls extracts tool calls from a ChatCompletionResponse.
func ExtractToolCalls(resp *model.ChatCompletionResponse) []model.ToolCall {
	for _, ch := range resp.Choices {
		if len(ch.Message.ToolCalls) > 0 {
			tcs := ch.Message.ToolCalls
			for i := range tcs {
				if tcs[i].Type == "" {
					tcs[i].Type = "function"
				}
			}
			return tcs
		}
	}
	return nil
}

// NormalizeTextToolCalls finds XML tool calls in response text content and
// promotes them to structured ToolCall objects.
func NormalizeTextToolCalls(resp *model.ChatCompletionResponse) bool {
	if len(resp.Choices) == 0 {
		return false
	}

	anyFound := false
	for i := range resp.Choices {
		ch := &resp.Choices[i]
		tcs, xmlOffset := FindAllToolCallsInText(ch.Message.Content)
		if len(tcs) == 0 {
			continue
		}

		beforeText := ch.Message.Content
		if xmlOffset >= 0 {
			beforeText = strings.TrimSpace(ch.Message.Content[:xmlOffset])
		}
		// If beforeText contains <think> block, remove thinking text before tool calls
		if idx := strings.Index(beforeText, "<think>"); idx >= 0 {
			if endIdx := strings.Index(beforeText[idx:], "</think>"); endIdx >= 0 {
				beforeText = strings.TrimSpace(beforeText[:idx] + beforeText[idx+endIdx+8:])
			} else {
				beforeText = strings.TrimSpace(beforeText[:idx])
			}
		}
		ch.Message.Content = beforeText

		ch.Message.ToolCalls = append(ch.Message.ToolCalls, tcs...)
		ch.FinishReason = "tool_calls"

		slog.Debug(
			"normalized text tool calls",
			"count", len(tcs),
			"first_tool", tcs[0].Function.Name,
		)

		anyFound = true
	}
	return anyFound
}

// HasToolResult checks if any message has a "tool" role.
func HasToolResult(messages []model.Message) bool {
	for i := range messages {
		if messages[i].Role == "tool" {
			return true
		}
	}
	return false
}

// HasToolSentinel checks if any message contains the tool sentinel prefix.
func HasToolSentinel(messages []model.Message) bool {
	for i := range messages {
		if s, ok := messages[i].Content.(string); ok && strings.Contains(s, ToolSentinelPrefix) {
			return true
		}
	}
	return false
}
