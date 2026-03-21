package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/dotnode/gatelm/internal/logging"
)

// anthropicCodec handles the Anthropic Messages API protocol.
type anthropicCodec struct {
	debug *logging.DebugLog
}

func newAnthropicCodec(debug *logging.DebugLog) *anthropicCodec {
	return &anthropicCodec{debug: debug}
}

func (c *anthropicCodec) Name() string { return "anthropic" }

func (c *anthropicCodec) DetectInbound(path string, headers http.Header) bool {
	if isAnthropicPath(path) {
		return true
	}
	return strings.TrimSpace(headers.Get("anthropic-version")) != ""
}

// ── Request direction ──

// ToCanonical converts Anthropic request → canonical (Responses) format.
// Delegates to the existing convertAnthropicRequestToOpenAIResponses().
func (c *anthropicCodec) ToCanonical(body []byte, opts EncodeOpts) ([]byte, error) {
	converted, _, err := convertAnthropicRequestToOpenAIResponses(body, opts.ReasoningEffort, opts.SystemPrompt)
	return converted, err
}

// FromCanonical converts canonical → Anthropic outbound format.
// TODO: implement when Anthropic backends are needed.
func (c *anthropicCodec) FromCanonical(body []byte) ([]byte, string, error) {
	return body, "/v1/messages", nil
}

// InjectDefaults performs lightweight injection on Anthropic body.
func (c *anthropicCodec) InjectDefaults(body []byte, opts DefaultOpts) []byte {
	body = ensureMaxTokens(body, opts.MaxTokens, "anthropic")
	body = ensureTemperature(body, opts.Temperature)
	return body
}

// InjectSystemPrompt injects system prompt into Anthropic body.
func (c *anthropicCodec) InjectSystemPrompt(body []byte, prompt string) []byte {
	if prompt == "" {
		return body
	}
	// Anthropic uses top-level "system" field
	return injectAnthropicSystemPrompt(body, prompt)
}

// ── Response direction ──

// ResponseToCanonical converts Anthropic response → canonical.
// TODO: implement when Anthropic backends are needed.
func (c *anthropicCodec) ResponseToCanonical(body []byte, _ int) ([]byte, error) {
	return body, nil
}

// ResponseFromCanonical converts canonical (Responses) response → Anthropic format.
// Delegates to the existing convertOpenAIResponsesResponseToAnthropic().
func (c *anthropicCodec) ResponseFromCanonical(body []byte, statusCode int) ([]byte, error) {
	return convertOpenAIResponsesResponseToAnthropic(body, statusCode)
}

// ── Streaming ──

// NewStreamDecoder creates a decoder for Anthropic SSE → canonical events.
// TODO: implement when Anthropic backends are needed.
func (c *anthropicCodec) NewStreamDecoder() StreamDecoder {
	return &anthropicStreamDecoder{debug: c.debug}
}

// NewStreamEncoder creates an encoder: canonical events → Anthropic SSE.
// This contains the state machine extracted from handleStreamingResponsesConversion.
func (c *anthropicCodec) NewStreamEncoder(w http.ResponseWriter) StreamEncoder {
	return &anthropicStreamEncoder{w: w, debug: c.debug}
}

// ── Utilities ──

func (c *anthropicCodec) ExtractUsage(body []byte) logging.UsageInfo {
	return logging.ExtractUsage("anthropic", body)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// injectAnthropicSystemPrompt merges a system prompt into Anthropic format.
func injectAnthropicSystemPrompt(body []byte, prompt string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	existing := ""
	if sys, ok := payload["system"].(string); ok {
		existing = sys
	}

	if existing != "" {
		payload["system"] = prompt + "\n\n" + existing
	} else {
		payload["system"] = prompt
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// ---------------------------------------------------------------------------
// Anthropic stream decoder (placeholder — for Anthropic backend SSE input)
// ---------------------------------------------------------------------------

type anthropicStreamDecoder struct {
	debug *logging.DebugLog
	usage logging.UsageInfo
}

func (d *anthropicStreamDecoder) Decode(_ string) ([]CanonicalEvent, error) {
	// TODO: implement Anthropic SSE → canonical events when Anthropic backends are added
	return nil, nil
}

func (d *anthropicStreamDecoder) Flush() ([]CanonicalEvent, logging.UsageInfo) {
	return nil, d.usage
}

// ---------------------------------------------------------------------------
// Anthropic stream encoder (canonical Responses events → Anthropic SSE)
//
// State machine: manages block indices, thinking/text/tool_use lifecycle.
// Extracted from handleStreamingResponsesConversion.
// ---------------------------------------------------------------------------

type anthropicStreamEncoder struct {
	w     http.ResponseWriter
	debug *logging.DebugLog

	// state
	headersSent     bool
	msgID           string
	model           string
	messageStarted  bool
	nextBlockIndex  int
	thinkingOpen    bool
	thinkingIndex   int
	textOpen        bool
	textIndex       int
	activeToolCalls map[int]*streamingToolCall
	sawToolUse      bool
	inputTokens     int
	outputTokens    int
	stopReason      string
	streamFailed    bool
	errorMessage    string
}

func (e *anthropicStreamEncoder) Encode(event CanonicalEvent) error {
	// Parse the canonical (Responses) event and emit Anthropic SSE events
	return e.processResponsesEvent(event)
}

func (e *anthropicStreamEncoder) Close() error {
	if e.streamFailed {
		errMsg := e.errorMessage
		if errMsg == "" {
			errMsg = "upstream streaming failed"
		}
		e.writeSSE("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": errMsg,
			},
		})
		return nil
	}

	// Close any open blocks
	e.closeThinkingBlock()
	e.closeTextBlock()
	e.closeAllToolBlocks()

	// Determine stop_reason
	stopReason := e.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	if e.sawToolUse {
		stopReason = "tool_use"
	}

	// message_delta
	e.writeSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": e.outputTokens,
		},
	})

	// message_stop
	e.writeSSE("message_stop", map[string]any{"type": "message_stop"})

	return nil
}

func (e *anthropicStreamEncoder) processResponsesEvent(event CanonicalEvent) error {
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return nil // skip unparseable events
	}

	switch event.EventType {
	case "response.created", "response.in_progress":
		e.extractModelFromResponse(data)

	case "response.output_item.added":
		item, _ := data["item"].(map[string]any)
		if item == nil {
			break
		}
		e.ensureMessageStarted()
		itemType, _ := item["type"].(string)
		if itemType == "function_call" {
			e.sawToolUse = true
			e.closeThinkingBlock()
			e.closeTextBlock()
			idx := int(getFloat(data, "output_index"))
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			blockIdx := e.nextBlockIndex
			e.nextBlockIndex++
			if e.activeToolCalls == nil {
				e.activeToolCalls = make(map[int]*streamingToolCall)
			}
			e.activeToolCalls[idx] = &streamingToolCall{
				blockIndex: blockIdx,
				id:         callID,
				name:       name,
			}
			e.writeSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIdx,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    callID,
					"name":  name,
					"input": map[string]any{},
				},
			})
		}

	case "response.content_part.added":
		e.ensureMessageStarted()
		e.closeThinkingBlock()
		if !e.textOpen {
			e.textIndex = e.nextBlockIndex
			e.nextBlockIndex++
			e.textOpen = true
			e.writeSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": e.textIndex,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
		}

	case "response.output_text.delta":
		delta, _ := data["delta"].(string)
		if delta != "" && e.textOpen {
			e.writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.textIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": delta,
				},
			})
		}

	case "response.output_text.done":
		e.closeTextBlock()

	case "response.function_call_arguments.delta":
		delta, _ := data["delta"].(string)
		idx := int(getFloat(data, "output_index"))
		if tc, ok := e.activeToolCalls[idx]; ok && delta != "" {
			e.writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": tc.blockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": delta,
				},
			})
		}

	case "response.function_call_arguments.done":
		idx := int(getFloat(data, "output_index"))
		if tc, ok := e.activeToolCalls[idx]; ok {
			e.writeSSE("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": tc.blockIndex,
			})
			delete(e.activeToolCalls, idx)
		}

	case "response.reasoning_summary_part.added":
		e.ensureMessageStarted()
		if !e.thinkingOpen {
			e.thinkingIndex = e.nextBlockIndex
			e.nextBlockIndex++
			e.thinkingOpen = true
			e.writeSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": e.thinkingIndex,
				"content_block": map[string]any{
					"type":     "thinking",
					"thinking": "",
				},
			})
		}

	case "response.reasoning_summary_text.delta":
		delta, _ := data["delta"].(string)
		if delta != "" && e.thinkingOpen {
			e.writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.thinkingIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": delta,
				},
			})
		}

	case "response.reasoning_summary_text.done", "response.reasoning_summary_part.done":
		// Will be closed when text/tool starts or in Close()

	case "response.completed":
		e.extractCompletedInfo(data)

	case "response.failed", "error":
		e.streamFailed = true
		e.errorMessage = "upstream streaming failed" // default, may be overridden below
		if errObj, ok := data["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok && strings.TrimSpace(msg) != "" {
				e.errorMessage = msg
			}
		}
	}

	return nil
}

func (e *anthropicStreamEncoder) ensureMessageStarted() {
	if e.messageStarted {
		return
	}
	e.messageStarted = true
	e.writeSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      e.msgID,
			"type":    "message",
			"role":    "assistant",
			"model":   e.model,
			"content": []any{},
			"usage":   map[string]any{"input_tokens": e.inputTokens, "output_tokens": 0},
		},
	})
	e.writeSSE("ping", map[string]any{"type": "ping"})
}

func (e *anthropicStreamEncoder) closeThinkingBlock() {
	if !e.thinkingOpen {
		return
	}
	e.thinkingOpen = false
	e.writeSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": e.thinkingIndex,
	})
}

func (e *anthropicStreamEncoder) closeTextBlock() {
	if !e.textOpen {
		return
	}
	e.textOpen = false
	e.writeSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": e.textIndex,
	})
}

func (e *anthropicStreamEncoder) closeAllToolBlocks() {
	for idx, tc := range e.activeToolCalls {
		e.writeSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": tc.blockIndex,
		})
		delete(e.activeToolCalls, idx)
	}
}

func (e *anthropicStreamEncoder) extractModelFromResponse(data map[string]any) {
	resp := data
	if r, ok := data["response"].(map[string]any); ok {
		resp = r
	}
	if id, ok := resp["id"].(string); ok && id != "" {
		e.msgID = id
	}
	if m, ok := resp["model"].(string); ok && m != "" {
		e.model = m
	}
}

func (e *anthropicStreamEncoder) extractCompletedInfo(data map[string]any) {
	resp := data
	if r, ok := data["response"].(map[string]any); ok {
		resp = r
	}
	if m, ok := resp["model"].(string); ok && m != "" {
		e.model = m
	}
	if status, ok := resp["status"].(string); ok {
		e.stopReason = mapResponsesStatus(status)
	}
	if usage, ok := resp["usage"].(map[string]any); ok {
		e.inputTokens = int(getFloat(usage, "input_tokens"))
		e.outputTokens = int(getFloat(usage, "output_tokens"))
	}
	// Check for tool_use in output
	if output, ok := resp["output"].([]any); ok {
		for _, item := range output {
			if m, ok := item.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "function_call" {
					e.sawToolUse = true
				}
			}
		}
	}
}

func (e *anthropicStreamEncoder) writeSSE(event string, data any) {
	if !e.headersSent {
		e.headersSent = true
		e.w.Header().Set("Content-Type", "text/event-stream")
		e.w.Header().Set("Cache-Control", "no-cache")
		e.w.Header().Set("Connection", "keep-alive")
		e.w.WriteHeader(200)
	}
	writeSSEEvent(e.w, event, data)
}
