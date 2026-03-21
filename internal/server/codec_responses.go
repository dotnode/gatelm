package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/dotnode/gatelm/internal/logging"
)

// responsesCodec handles the OpenAI Responses API protocol.
// Since Responses IS the canonical format, most methods are identity/no-op.
type responsesCodec struct {
	debug *logging.DebugLog
}

func newResponsesCodec(debug *logging.DebugLog) *responsesCodec {
	return &responsesCodec{debug: debug}
}

func (c *responsesCodec) Name() string { return "openai-responses" }

func (c *responsesCodec) DetectInbound(path string, _ http.Header) bool {
	return strings.HasPrefix(path, "/v1/responses")
}

// ── Request direction ──

// ToCanonical is a no-op: Responses format IS canonical.
func (c *responsesCodec) ToCanonical(body []byte, _ EncodeOpts) ([]byte, error) {
	return body, nil
}

// FromCanonical is a no-op: canonical IS Responses format.
func (c *responsesCodec) FromCanonical(body []byte) ([]byte, string, error) {
	return body, "/v1/responses", nil
}

// InjectDefaults performs lightweight default injection on a Responses body.
func (c *responsesCodec) InjectDefaults(body []byte, opts DefaultOpts) []byte {
	body = ensureMaxTokens(body, opts.MaxTokens, "openai-responses")
	body = ensureTemperature(body, opts.Temperature)
	body = shouldNormalizeReasoningEffortAlias("openai-responses", opts.NormalizeXHigh, body)
	return body
}

// InjectSystemPrompt injects a system prompt into a Responses body.
func (c *responsesCodec) InjectSystemPrompt(body []byte, prompt string) []byte {
	if prompt == "" {
		return body
	}
	return injectSystemPromptIntoOpenAIResponses(body, prompt)
}

// ── Response direction ──

// ResponseToCanonical is a no-op: Responses response IS canonical.
func (c *responsesCodec) ResponseToCanonical(body []byte, _ int) ([]byte, error) {
	return body, nil
}

// ResponseFromCanonical is a no-op: canonical IS Responses format.
func (c *responsesCodec) ResponseFromCanonical(body []byte, _ int) ([]byte, error) {
	return body, nil
}

// ── Streaming ──

// NewStreamDecoder returns a passthrough decoder (Responses SSE = canonical events).
func (c *responsesCodec) NewStreamDecoder() StreamDecoder {
	return &responsesStreamDecoder{}
}

// NewStreamEncoder returns a passthrough encoder (canonical events = Responses SSE).
func (c *responsesCodec) NewStreamEncoder(w http.ResponseWriter) StreamEncoder {
	return &responsesStreamEncoder{w: w}
}

// ── Utilities ──

func (c *responsesCodec) ExtractUsage(body []byte) logging.UsageInfo {
	return logging.ExtractUsage("openai-responses", body)
}

// ---------------------------------------------------------------------------
// Responses passthrough stream decoder
// ---------------------------------------------------------------------------

type responsesStreamDecoder struct {
	usage     logging.UsageInfo
	lastEvent string
}

func (d *responsesStreamDecoder) Decode(line string) ([]CanonicalEvent, error) {
	// Handle "event: <type>" lines by storing the event type
	if strings.HasPrefix(line, "event: ") {
		d.lastEvent = strings.TrimPrefix(line, "event: ")
		return nil, nil
	}

	if !strings.HasPrefix(line, "data: ") {
		return nil, nil
	}

	data := line[6:]
	if data == "[DONE]" {
		return nil, nil
	}

	eventType := d.lastEvent
	d.lastEvent = ""

	// Extract usage from response.completed
	if eventType == "response.completed" || eventType == "response.done" {
		d.extractUsage([]byte(data))
	}

	return []CanonicalEvent{{
		EventType: eventType,
		Data:      json.RawMessage(data),
	}}, nil
}

func (d *responsesStreamDecoder) Flush() ([]CanonicalEvent, logging.UsageInfo) {
	return nil, d.usage
}

func (d *responsesStreamDecoder) extractUsage(data []byte) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return
	}

	// Handle nested structure: data.response
	response := obj
	if r, ok := obj["response"].(map[string]any); ok {
		response = r
	}

	if usage, ok := response["usage"].(map[string]any); ok {
		d.usage.InputTokens = int(getFloat(usage, "input_tokens"))
		d.usage.OutputTokens = int(getFloat(usage, "output_tokens"))
		d.usage.PromptTokens = d.usage.InputTokens
		d.usage.CompletionTokens = d.usage.OutputTokens
		d.usage.TotalTokens = d.usage.InputTokens + d.usage.OutputTokens
	}
	if m, ok := response["model"].(string); ok && m != "" {
		d.usage.ResponseModel = m
	}
}

// ---------------------------------------------------------------------------
// Responses passthrough stream encoder
// ---------------------------------------------------------------------------

type responsesStreamEncoder struct {
	w http.ResponseWriter
}

func (e *responsesStreamEncoder) Encode(event CanonicalEvent) error {
	var line string
	if event.EventType != "" {
		line = "event: " + event.EventType + "\ndata: " + string(event.Data) + "\n\n"
	} else {
		line = "data: " + string(event.Data) + "\n\n"
	}

	_, err := e.w.Write([]byte(line))
	if err != nil {
		return err
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (e *responsesStreamEncoder) Close() error {
	return nil
}
