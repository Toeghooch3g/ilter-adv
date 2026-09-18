package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/loopdetect"
	"github.com/ilter-ai/ilter/internal/features/pii"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
	"github.com/ilter-ai/ilter/internal/middleware"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
	"github.com/ilter-ai/ilter/internal/provider"
)

func writeFlushedChunk(w io.Writer, content, modelID string) {
	if content == "" {
		return
	}
	chunk := &model.ChatCompletionChunk{
		ID:      "chatcmpl-flush",
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelID,
		Choices: []model.ChunkChoice{
			{Index: 0, Delta: model.Delta{Content: content}},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		slog.Debug("write chunk error", "error", err)
	}
}

func processStreamChunk(chunk *model.ChatCompletionChunk, unmasker *pii.StreamUnmasker, accumulated *string, finalUsage **model.Usage) {
	if chunk.Usage != nil {
		*finalUsage = chunk.Usage
	}
	for i := range chunk.Choices {
		unmasked := unmasker.Process(chunk.Choices[i].Delta.Content)
		chunk.Choices[i].Delta.Content = unmasked
		*accumulated += unmasked
	}
}

func writeSSEData(w io.Writer, data []byte) bool {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return false
	}
	if _, err := w.Write(data); err != nil {
		return false
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return false
	}
	return true
}

func writeStreamDone(w io.Writer, flusher http.Flusher) {
	if _, err := fmt.Fprintf(w, "data: [DONE]\n\n"); err != nil {
		slog.Debug("write done error", "error", err)
	}
	flusher.Flush()
}

// handleOutputLoop feeds the output detector with delta content from chunk choices.
// Returns true if an output loop was detected and handled (stream termination sent).
func handleOutputLoop(
	choices []model.ChunkChoice,
	detector *loopdetect.OutputDetector,
	cancel context.CancelFunc,
	w io.Writer,
	flusher http.Flusher,
	requestedModel string,
) bool {
	for i := range choices {
		delta := choices[i].Delta.Content
		if delta == "" {
			continue
		}
		result := detector.Feed(delta)
		if !result.Detected {
			continue
		}

		slog.Warn(
			"Output loop detected",
			"repeat_count", result.RepeatCount,
			"sentence", result.Sentence,
			"mode", result.Mode,
		)

		if result.Mode == "enforce" {
			// Cancel upstream provider to stop token billing
			cancel()

			// Send a chunk with the custom finish_reason so clients can differentiate
			reason := "loop_detected"
			loopTermination := model.ChatCompletionChunk{
				ID:      "chatcmpl-output-loop",
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   requestedModel,
				Choices: []model.ChunkChoice{
					{
						Index:        0,
						Delta:        model.Delta{Content: ""},
						FinishReason: &reason,
					},
				},
			}
			loopBytes, err := json.Marshal(loopTermination)
			if err == nil {
				writeSSEData(w, loopBytes)
			}
			writeStreamDone(w, flusher)
			return true
		}

		// Observe mode: log only, continue streaming
		return false
	}
	return false
}

// writeStreamingUnsupportedError responds when the underlying
// ResponseWriter doesn't support flushing (so SSE can't be delivered),
// recording an audit entry for the failed request.
func (h *Handler) writeStreamingUnsupportedError(w http.ResponseWriter, originalRequest *http.Request, route *smartrouter.Route, requestedModel string, start time.Time, req *model.ChatCompletionRequest) {
	model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
	var msgs []model.Message
	if req != nil {
		msgs = req.Messages
	}
	h.recordAudit(originalRequest, route, requestedModel, nil, http.StatusInternalServerError, start, false, msgs)
}

// writeStreamHeaders sets the SSE response headers, including the actual
// (provider-qualified) model name header, and flushes them immediately.
func writeStreamHeaders(w http.ResponseWriter, flusher http.Flusher, route *smartrouter.Route) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	actualModel := route.Model.Name
	if route.Provider != nil {
		actualModel = route.Provider.Name() + "/" + route.Model.Name
	}
	w.Header().Set("X-Ilter-Model-Actual", actualModel)
	flusher.Flush()
}

// newOutputLoopDetector builds the output-loop detector for a streaming
// request, or nil if output-loop detection is disabled.
func newOutputLoopDetector(cfg *config.Config) *loopdetect.OutputDetector {
	if cfg == nil || cfg.CostGuard.LoopSettings.OutputLoopMode == "off" {
		return nil
	}
	return loopdetect.NewOutputDetector(
		cfg.CostGuard.LoopSettings.OutputLoopThreshold,
		cfg.CostGuard.LoopSettings.OutputMinSentence,
		cfg.CostGuard.LoopSettings.OutputLoopMode,
	)
}

// streamTokenCounts returns the actual prompt/completion token counts from
// finalUsage if the stream carried one, or else a rough word-count-based
// estimate from the request/response text.
func streamTokenCounts(finalUsage *model.Usage, req *model.ChatCompletionRequest, accumulatedCompletionText string) (pTokens, cTokens int) {
	if finalUsage != nil {
		return finalUsage.PromptTokens, finalUsage.CompletionTokens
	}
	if req == nil {
		return 0, 0
	}
	charCount := 0
	for _, m := range req.Messages {
		if sContent, ok := m.Content.(string); ok {
			charCount += len(sContent)
		}
	}
	pTokens = max(charCount/4, 1)
	cTokens = len(accumulatedCompletionText) / 4
	return pTokens, cTokens
}

// recordStreamAudit records the audit-log entry (and downstream
// budget/cost bookkeeping) for a finished (or aborted) streaming response.
//
// Deliberately uses originalRequest.Context(), not the stream's ctx: ctx
// can be canceled mid-stream (e.g. by an output-loop abort via cancel()),
// but usage/audit for tokens already consumed must still be recorded even
// when the stream was cut short.
func (h *Handler) recordStreamAudit(
	originalRequest *http.Request,
	route *smartrouter.Route,
	requestedModel string,
	start time.Time,
	statusCode int,
	req *model.ChatCompletionRequest,
	finalUsage *model.Usage,
	accumulatedCompletionText string,
) {
	pTokens, cTokens := streamTokenCounts(finalUsage, req, accumulatedCompletionText)

	auditUsage := &model.Usage{
		PromptTokens:     pTokens,
		CompletionTokens: cTokens,
		TotalTokens:      pTokens + cTokens,
	}
	if finalUsage != nil {
		if finalUsage.PromptTokensDetails != nil {
			auditUsage.PromptTokensDetails = &model.PromptTokensDetails{
				CachedTokens: finalUsage.PromptTokensDetails.CachedTokens,
			}
		}
		auditUsage.CacheCreationInputTokens = finalUsage.CacheCreationInputTokens
		auditUsage.CacheReadIncludedInPrompt = finalUsage.CacheReadIncludedInPrompt
	}

	auditResp := &model.ChatCompletionResponse{
		Usage: auditUsage,
		Choices: []model.Choice{
			{
				Message: model.ChoiceMessage{
					Role:    "assistant",
					Content: accumulatedCompletionText,
				},
			},
		},
	}

	var msgs []model.Message
	if req != nil {
		msgs = req.Messages
	}
	h.recordAudit(originalRequest, route, requestedModel, auditResp, statusCode, start, false, msgs)

	keyID := reqmeta.GetKeyID(originalRequest.Context())
	cost := CalculateCost(route.Model, auditUsage)

	if meta := reqmeta.GetRequestMetadata(originalRequest.Context()); meta != nil {
		meta.SetTokensAndCost(pTokens, cTokens, cost)
	}

	if h.budgetEnforcer != nil {
		if err := h.budgetEnforcer.RecordUsage(originalRequest.Context(), keyID, cost); err != nil {
			slog.Error("failed to record budget usage for stream", "error", err)
		}
	}
	if h.loopDetector != nil {
		h.loopDetector.RecordCost(keyID, cost)
	}
}

func forwardRawLine(w io.Writer, line []byte) {
	if _, err := w.Write(line); err != nil {
		slog.Debug("forward line error", "error", err)
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		slog.Debug("forward newline error", "error", err)
	}
}

// enrichAndCheckLoop applies PII-unmask accumulation, per-chunk billing
// enrichment, and output-loop detection to one streamed chunk, in the same
// order handleStreaming has always applied them. Returns true if the
// caller should stop reading the stream (an output loop was detected and
// enforced).
func enrichAndCheckLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	w io.Writer,
	flusher http.Flusher,
	chunk *model.ChatCompletionChunk,
	route *smartrouter.Route,
	requestedModel string,
	unmasker *pii.StreamUnmasker,
	outputDetector *loopdetect.OutputDetector,
	accumulated *string,
	finalUsage **model.Usage,
) (stop bool) {
	processStreamChunk(chunk, unmasker, accumulated, finalUsage)

	if chunk.Usage != nil {
		cost := CalculateCost(route.Model, chunk.Usage)
		chunk.Usage.IlterCost = cost
		if id, ok := ctx.Value(reqmeta.KeyIDContextKey).(string); ok && id != "" {
			chunk.Usage.IlterBillingKey = id
		}
	}

	return outputDetector != nil && cancel != nil && handleOutputLoop(chunk.Choices, outputDetector, cancel, w, flusher, requestedModel)
}

// processProviderChunk handles one SSE line using the provider's own
// TransformStreamChunk, writing the translated chunk (or forwarding
// completion) to w. Returns true if the caller should stop reading the
// stream.
func processProviderChunk(
	ctx context.Context,
	cancel context.CancelFunc,
	w io.Writer,
	flusher http.Flusher,
	dataBytes []byte,
	streamProvider provider.StreamingProvider,
	route *smartrouter.Route,
	requestedModel string,
	unmasker *pii.StreamUnmasker,
	outputDetector *loopdetect.OutputDetector,
	accumulated *string,
	finalUsage **model.Usage,
) (stop bool) {
	chunk, done, err := streamProvider.TransformStreamChunk(dataBytes)
	if err != nil {
		slog.Error("Error transforming stream chunk", "error", err)
		return false
	}
	if done {
		writeFlushedChunk(w, unmasker.Flush(), requestedModel)
		writeStreamDone(w, flusher)
		return true
	}
	if chunk == nil {
		return false
	}

	if enrichAndCheckLoop(ctx, cancel, w, flusher, chunk, route, requestedModel, unmasker, outputDetector, accumulated, finalUsage) {
		return true
	}

	chunkBytes, err := json.Marshal(chunk)
	if err != nil {
		return false
	}
	if !writeSSEData(w, chunkBytes) {
		return true
	}
	flusher.Flush()
	return false
}

// processRawChunk handles one SSE line by parsing it directly as a
// ChatCompletionChunk (no provider-specific transform needed), writing the
// translated line (or forwarding completion) to w. Returns true if the
// caller should stop reading the stream.
func processRawChunk(
	ctx context.Context,
	cancel context.CancelFunc,
	w io.Writer,
	flusher http.Flusher,
	dataBytes []byte,
	route *smartrouter.Route,
	requestedModel string,
	unmasker *pii.StreamUnmasker,
	outputDetector *loopdetect.OutputDetector,
	accumulated *string,
	finalUsage **model.Usage,
) (stop bool) {
	if bytes.Equal(dataBytes, []byte("[DONE]")) {
		writeFlushedChunk(w, unmasker.Flush(), requestedModel)
		writeStreamDone(w, flusher)
		return true
	}

	var rawChunk model.ChatCompletionChunk
	if errJSON := json.Unmarshal(dataBytes, &rawChunk); errJSON == nil {
		if enrichAndCheckLoop(ctx, cancel, w, flusher, &rawChunk, route, requestedModel, unmasker, outputDetector, accumulated, finalUsage) {
			return true
		}
		if newDataBytes, err := json.Marshal(rawChunk); err == nil {
			dataBytes = newDataBytes
		}
	}

	if !writeSSEData(w, dataBytes) {
		return true
	}
	flusher.Flush()
	return false
}

// processStreamLine handles one scanned line: forwarding comment/event
// lines as-is, extracting the SSE data payload, and dispatching it to the
// provider-specific or raw chunk handler. Returns true if the caller
// should stop reading the stream.
func processStreamLine(
	ctx context.Context,
	cancel context.CancelFunc,
	w http.ResponseWriter,
	flusher http.Flusher,
	line []byte,
	streamProvider provider.StreamingProvider,
	route *smartrouter.Route,
	requestedModel string,
	unmasker *pii.StreamUnmasker,
	outputDetector *loopdetect.OutputDetector,
	accumulated *string,
	finalUsage **model.Usage,
) (stop bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return false
	}

	if bytes.HasPrefix(line, []byte(":")) || bytes.HasPrefix(line, []byte("event:")) {
		forwardRawLine(w, line)
		flusher.Flush()
		return false
	}

	dataBytes := line
	if after, ok0 := bytes.CutPrefix(line, []byte("data: ")); ok0 {
		dataBytes = after
	}

	if streamProvider != nil {
		return processProviderChunk(ctx, cancel, w, flusher, dataBytes, streamProvider, route, requestedModel, unmasker, outputDetector, accumulated, finalUsage)
	}
	return processRawChunk(ctx, cancel, w, flusher, dataBytes, route, requestedModel, unmasker, outputDetector, accumulated, finalUsage)
}

// runStreamLoop reads upstream SSE lines from body until EOF, a client
// disconnect, or a terminal chunk, translating and forwarding each line to
// w. accumulated/finalUsage accumulate the streamed completion for the
// audit trail the caller records afterward.
func runStreamLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	w http.ResponseWriter,
	flusher http.Flusher,
	body io.Reader,
	streamProvider provider.StreamingProvider,
	route *smartrouter.Route,
	requestedModel string,
	unmasker *pii.StreamUnmasker,
	outputDetector *loopdetect.OutputDetector,
	accumulated *string,
	finalUsage **model.Usage,
) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			slog.Debug("client disconnected during stream")
			return
		default:
		}

		if processStreamLine(ctx, cancel, w, flusher, scanner.Bytes(), streamProvider, route, requestedModel, unmasker, outputDetector, accumulated, finalUsage) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("stream scanner error", "error", err)
	}

	writeFlushedChunk(w, unmasker.Flush(), requestedModel)
}

func (h *Handler) handleStreaming(
	ctx context.Context,
	cancel context.CancelFunc,
	w http.ResponseWriter,
	resp *http.Response,
	p provider.Provider,
	route *smartrouter.Route,
	requestedModel string,
	start time.Time,
	originalRequest *http.Request,
	req *model.ChatCompletionRequest,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeStreamingUnsupportedError(w, originalRequest, route, requestedModel, start, req)
		return
	}

	writeStreamHeaders(w, flusher, route)

	defer func() { _ = resp.Body.Close() }()

	var accumulatedCompletionText string
	var finalUsage *model.Usage
	statusCode := http.StatusOK

	defer func() {
		h.recordStreamAudit(originalRequest, route, requestedModel, start, statusCode, req, finalUsage, accumulatedCompletionText)
	}()

	mappings := middleware.GetPIIMappings(ctx)
	unmasker := pii.NewStreamUnmasker(mappings)
	outputDetector := newOutputLoopDetector(h.cfg)
	streamProvider, _ := p.(provider.StreamingProvider)

	runStreamLoop(ctx, cancel, w, flusher, resp.Body, streamProvider, route, requestedModel, unmasker, outputDetector, &accumulatedCompletionText, &finalUsage)
}
