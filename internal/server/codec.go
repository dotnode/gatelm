package server

import (
	"bufio"
	"encoding/json"
	"net/http"

	"github.com/dotnode/gatelm/internal/logging"
)

// ---------------------------------------------------------------------------
// Canonical IR = OpenAI Responses API format (JSON + SSE events)
//
// Every protocol goes through this canonical representation:
//   Request:  clientBody → clientCodec.ToCanonical() → backendCodec.FromCanonical() → backendBody
//   Response: backendBody → backendCodec.ResponseToCanonical() → clientCodec.ResponseFromCanonical() → clientBody
//   Stream:   backendSSE → backendCodec.StreamDecoder → clientCodec.StreamEncoder → clientSSE
//
// Same-protocol passthrough skips the canonical layer for efficiency.
// ---------------------------------------------------------------------------

// ProtocolCodec encapsulates all encode/decode behavior for a single protocol.
// Canonical format = OpenAI Responses API JSON / SSE event format.
type ProtocolCodec interface {
	// Name returns the protocol identifier (e.g. "openai", "anthropic", "openai-responses").
	Name() string

	// DetectInbound returns true if the given request matches this protocol.
	DetectInbound(path string, headers http.Header) bool

	// ── Request direction ──

	// ToCanonical converts a request body from this protocol to canonical (Responses) format.
	// For the Responses codec this is a no-op.
	ToCanonical(body []byte, opts EncodeOpts) ([]byte, error)

	// FromCanonical converts a canonical request body to this protocol's outbound format.
	// Returns (body, pathOverride, error).
	FromCanonical(body []byte) ([]byte, string, error)

	// InjectDefaults performs lightweight default injection on a raw protocol body
	// (used in same-protocol passthrough to avoid full decode/encode).
	InjectDefaults(body []byte, opts DefaultOpts) []byte

	// InjectSystemPrompt injects a system prompt into a raw protocol body
	// (used in same-protocol passthrough).
	InjectSystemPrompt(body []byte, prompt string) []byte

	// ── Response direction ──

	// ResponseToCanonical converts a response body from this protocol to canonical format.
	ResponseToCanonical(body []byte, statusCode int) ([]byte, error)

	// ResponseFromCanonical converts a canonical response body to this protocol's format.
	ResponseFromCanonical(body []byte, statusCode int) ([]byte, error)

	// ── Streaming ──

	// NewStreamDecoder creates a decoder: this protocol's SSE → canonical (Responses) events.
	NewStreamDecoder() StreamDecoder

	// NewStreamEncoder creates an encoder: canonical events → this protocol's SSE written to w.
	NewStreamEncoder(w http.ResponseWriter) StreamEncoder

	// ── Utilities ──

	// ExtractUsage extracts usage info from a non-streaming response body in this protocol.
	ExtractUsage(body []byte) logging.UsageInfo
}

// EncodeOpts carries options for cross-protocol request conversion.
type EncodeOpts struct {
	ReasoningEffort string // default reasoning effort from config
	SystemPrompt    string // system prompt to inject
}

// DefaultOpts carries options for lightweight same-protocol default injection.
type DefaultOpts struct {
	MaxTokens      int      // inject max_tokens / max_output_tokens if client didn't set
	Temperature    *float64 // inject temperature if client didn't set
	NormalizeXHigh bool     // normalize xhigh → high for OpenAI family
}

// CanonicalEvent is a structured representation of a Responses SSE event.
type CanonicalEvent struct {
	EventType string          // SSE event name, e.g. "response.created", "response.output_text.delta"
	Data      json.RawMessage // raw JSON data payload
}

// StreamDecoder converts a backend protocol's SSE lines into canonical events.
type StreamDecoder interface {
	// Decode processes one SSE text line and returns zero or more canonical events.
	Decode(line string) ([]CanonicalEvent, error)
	// Flush returns any remaining events and accumulated usage info.
	Flush() ([]CanonicalEvent, logging.UsageInfo)
}

// StreamEncoder writes canonical events as client-protocol SSE.
type StreamEncoder interface {
	// Encode writes one canonical event to the client.
	Encode(event CanonicalEvent) error
	// Close writes any final/closing events.
	Close() error
}

// ---------------------------------------------------------------------------
// Generic streaming pipeline
// ---------------------------------------------------------------------------

// convertStream is a generic streaming conversion pipeline that replaces all
// protocol-specific streaming functions. It reads SSE from resp.Body, decodes
// via the backend's StreamDecoder, and encodes via the client's StreamEncoder.
func convertStream(
	w http.ResponseWriter,
	resp *http.Response,
	decoder StreamDecoder,
	encoder StreamEncoder,
	debug *logging.DebugLog,
	reqID string,
) (logging.UsageInfo, error) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		events, err := decoder.Decode(line)
		if err != nil {
			debug.Printf("[%s] stream decode error: %v", reqID, err)
			continue
		}
		for _, evt := range events {
			if err := encoder.Encode(evt); err != nil {
				_, usage := decoder.Flush()
				return usage, err
			}
		}
	}

	finalEvents, usage := decoder.Flush()
	for _, evt := range finalEvents {
		if err := encoder.Encode(evt); err != nil {
			return usage, err
		}
	}
	encoder.Close()

	if err := scanner.Err(); err != nil {
		debug.Printf("[%s] stream scanner error: %v", reqID, err)
	}

	return usage, nil
}

// ---------------------------------------------------------------------------
// Shared request preparation (eliminates duplication across HTTP/WS/WS-SSE)
// ---------------------------------------------------------------------------

// prepareRequestBody applies model rewriting and protocol-native default injection.
// Used for the same-protocol passthrough path (no full canonical decode/encode).
func prepareRequestBody(body []byte, requestModel, backendModel string, codec ProtocolCodec, mc resolvedModelConfig) []byte {
	if backendModel != "" && backendModel != requestModel {
		_, body = replaceModelInBody(body, backendModel)
	}
	body = codec.InjectDefaults(body, DefaultOpts{
		MaxTokens:      mc.defaultMaxTokens,
		Temperature:    mc.defaultTemperature,
		NormalizeXHigh: mc.normalizeXHighReasoningEffort,
	})
	return body
}

// applyCanonicalDefaults injects defaults into a canonical (Responses format) body.
// Used for the cross-protocol conversion path after ToCanonical().
func applyCanonicalDefaults(body []byte, mc resolvedModelConfig, systemPrompt string) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	changed := false

	// max_output_tokens (Responses format)
	if mc.defaultMaxTokens > 0 {
		if _, exists := payload["max_output_tokens"]; !exists {
			payload["max_output_tokens"], _ = json.Marshal(mc.defaultMaxTokens)
			changed = true
		}
	}

	// temperature
	if mc.defaultTemperature != nil {
		if _, exists := payload["temperature"]; !exists {
			payload["temperature"], _ = json.Marshal(*mc.defaultTemperature)
			changed = true
		}
	}

	// reasoning.effort (normalize xhigh if configured)
	if mc.normalizeXHighReasoningEffort {
		if raw, exists := payload["reasoning"]; exists {
			var reasoning map[string]any
			if err := json.Unmarshal(raw, &reasoning); err == nil {
				if effort, _ := reasoning["effort"].(string); effort == "xhigh" {
					reasoning["effort"] = "high"
					payload["reasoning"], _ = json.Marshal(reasoning)
					changed = true
				}
			}
		}
	}

	// system prompt injection (Responses: instructions field or input[0] developer role)
	if systemPrompt != "" {
		body = injectCanonicalSystemPrompt(body, payload, systemPrompt)
		return body
	}

	if !changed {
		return body
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// injectCanonicalSystemPrompt injects system prompt into canonical (Responses) body.
// Uses the "instructions" field (merge if existing).
func injectCanonicalSystemPrompt(origBody []byte, payload map[string]json.RawMessage, prompt string) []byte {
	if prompt == "" {
		return origBody
	}

	// Use instructions field for Responses format
	if raw, exists := payload["instructions"]; exists {
		var existing string
		if json.Unmarshal(raw, &existing) == nil && existing != "" {
			prompt = prompt + "\n\n" + existing
		}
	}
	payload["instructions"], _ = json.Marshal(prompt)

	out, err := json.Marshal(payload)
	if err != nil {
		return origBody
	}
	return out
}

// ---------------------------------------------------------------------------
// Codec registry helpers
// ---------------------------------------------------------------------------

// buildCodecs creates all protocol codec instances.
func buildCodecs(debug *logging.DebugLog) map[string]ProtocolCodec {
	return map[string]ProtocolCodec{
		"openai-responses": newResponsesCodec(debug),
		"anthropic":        newAnthropicCodec(debug),
		"openai":           newOpenAIChatCodec(debug),
	}
}
