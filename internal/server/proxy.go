package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func removeHopByHopHeaders(h http.Header) {
	h.Del("Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Authenticate")
	h.Del("Proxy-Authorization")
	h.Del("Te")
	h.Del("Trailer")
	h.Del("Transfer-Encoding")
	h.Del("Upgrade")
}

// modifyJSONBody unmarshals body as a JSON object, calls modifier, and re-marshals.
// Returns original body unchanged if unmarshal fails or body is empty.
func modifyJSONBody(body []byte, modifier func(map[string]any) bool) []byte {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if !modifier(payload) {
		return body
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// renameJSONKey renames a top-level JSON key from oldKey to newKey if oldKey
// exists and newKey does not. This is a no-op if oldKey is absent.
func renameJSONKey(body []byte, oldKey, newKey string) []byte {
	return modifyJSONBody(body, func(p map[string]any) bool {
		v, ok := p[oldKey]
		if !ok {
			return false
		}
		if _, exists := p[newKey]; exists {
			return false
		}
		p[newKey] = v
		delete(p, oldKey)
		return true
	})
}

// replaceModelInBody replaces the model field in the JSON body with newModel.
// Returns the original model name found in the body and the rewritten body.
func replaceModelInBody(body []byte, newModel string) (requestModel string, newBody []byte) {
	if len(body) == 0 {
		return "", body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", body
	}
	modelVal, ok := payload["model"].(string)
	if !ok || modelVal == "" {
		return "", body
	}
	requestModel = modelVal
	if newModel != "" && newModel != modelVal {
		payload["model"] = newModel
		b, err := json.Marshal(payload)
		if err == nil {
			return requestModel, b
		}
	}
	return requestModel, body
}

// ensureMaxTokens injects max_tokens into the JSON body if the field is absent
// and defaultMaxTokens > 0. Checks both max_tokens and max_completion_tokens
// to avoid overriding any client-specified token limit.
func ensureMaxTokens(body []byte, defaultMaxTokens int, backendProtocol string) []byte {
	if defaultMaxTokens <= 0 {
		return body
	}
	return modifyJSONBody(body, func(p map[string]any) bool {
		if _, ok := p["max_tokens"]; ok {
			return false
		}
		if _, ok := p["max_completion_tokens"]; ok {
			return false
		}
		if _, ok := p["max_output_tokens"]; ok {
			return false
		}
		if backendProtocol == "openai-responses" {
			p["max_output_tokens"] = defaultMaxTokens
		} else {
			p["max_tokens"] = defaultMaxTokens
		}
		return true
	})
}

// ensureTemperature injects temperature into the JSON body if the field is absent
// and defaultTemperature is non-nil. Does not override client-specified temperature.
func ensureTemperature(body []byte, defaultTemperature *float64) []byte {
	if defaultTemperature == nil {
		return body
	}
	return modifyJSONBody(body, func(p map[string]any) bool {
		if _, ok := p["temperature"]; ok {
			return false
		}
		p["temperature"] = *defaultTemperature
		return true
	})
}

// normalizeReasoningEffortAliasInBody rewrites the compatibility alias xhigh
// to high for OpenAI-family upstream requests. Other values are preserved.
func normalizeReasoningEffortAliasInBody(body []byte, protocol string) []byte {
	return modifyJSONBody(body, func(p map[string]any) bool {
		if protocol == "openai-responses" {
			reasoning, ok := p["reasoning"].(map[string]any)
			if !ok {
				return false
			}
			effort, ok := reasoning["effort"].(string)
			if !ok || strings.ToLower(strings.TrimSpace(effort)) != "xhigh" {
				return false
			}
			reasoning["effort"] = "high"
			p["reasoning"] = reasoning
			return true
		}
		effort, ok := p["reasoning_effort"].(string)
		if !ok || strings.ToLower(strings.TrimSpace(effort)) != "xhigh" {
			return false
		}
		p["reasoning_effort"] = "high"
		return true
	})
}

func extractModel(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	m, _ := payload["model"].(string)
	return m
}

func detectStreamRequest(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var partial struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return false
	}
	return partial.Stream
}

func stripStreamField(body []byte) []byte {
	return modifyJSONBody(body, func(p map[string]any) bool {
		delete(p, "stream")
		delete(p, "stream_options")
		return true
	})
}

func ensureStreamOptions(body []byte) []byte {
	return modifyJSONBody(body, func(p map[string]any) bool {
		p["stream_options"] = map[string]any{"include_usage": true}
		return true
	})
}

func ensureResponsesStream(body []byte) []byte {
	return modifyJSONBody(body, func(p map[string]any) bool {
		p["stream"] = true
		return true
	})
}

func extractClientKey(h http.Header) string {
	auth := strings.TrimSpace(h.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if k := strings.TrimSpace(h.Get("x-api-key")); k != "" {
		return k
	}
	return ""
}

func normalizeKeyLabel(key string, mapping map[string]string) string {
	if key == "" {
		return "anonymous"
	}
	if name, ok := mapping[key]; ok && name != "" {
		return name
	}
	if len(key) <= 8 {
		return "key:" + key
	}
	sum := sha256.Sum256([]byte(key))
	return "key:" + key[:4] + "..." + key[len(key)-4:] + ":" + hex.EncodeToString(sum[:4])
}

func writeSSEEvent(w io.Writer, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

// streamingToolCall tracks state of a single tool call being streamed from OpenAI.
type streamingToolCall struct {
	blockIndex int
	id         string
	name       string
	arguments  strings.Builder
}

// getFloat extracts a float64 from a map and returns it, defaulting to 0.
func getFloat(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return f
}

// openAIStreamChunk represents a single chunk from OpenAI streaming response.
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}
