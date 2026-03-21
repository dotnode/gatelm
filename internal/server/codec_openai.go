package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/dotnode/gatelm/internal/logging"
)

// openaiChatCodec handles the OpenAI Chat Completions API protocol.
type openaiChatCodec struct {
	debug *logging.DebugLog
}

func newOpenAIChatCodec(debug *logging.DebugLog) *openaiChatCodec {
	return &openaiChatCodec{debug: debug}
}

func (c *openaiChatCodec) Name() string { return "openai" }

func (c *openaiChatCodec) DetectInbound(path string, _ http.Header) bool {
	return strings.HasPrefix(path, "/v1/chat/completions") ||
		strings.HasPrefix(path, "/v1/completions") ||
		strings.HasPrefix(path, "/v1/embeddings") ||
		strings.HasPrefix(path, "/v1/images") ||
		strings.HasPrefix(path, "/v1/audio/") ||
		strings.HasPrefix(path, "/v1/moderations") ||
		strings.HasPrefix(path, "/v1/models")
}

// ── Request direction ──

// ToCanonical converts OpenAI Chat request → canonical (Responses) format.
func (c *openaiChatCodec) ToCanonical(body []byte, opts EncodeOpts) ([]byte, error) {
	return chatRequestToResponses(body, opts)
}

// FromCanonical converts canonical (Responses) request → OpenAI Chat format.
func (c *openaiChatCodec) FromCanonical(body []byte) ([]byte, string, error) {
	converted, err := responsesRequestToChat(body)
	if err != nil {
		return nil, "", err
	}
	return converted, "/v1/chat/completions", nil
}

// InjectDefaults performs lightweight injection on an OpenAI Chat body.
func (c *openaiChatCodec) InjectDefaults(body []byte, opts DefaultOpts) []byte {
	// Normalize max_output_tokens → max_tokens for Chat Completions format.
	// Some clients (e.g. Codex) send max_output_tokens in Chat requests.
	body = renameJSONKey(body, "max_output_tokens", "max_tokens")
	body = ensureMaxTokens(body, opts.MaxTokens, "openai")
	body = ensureTemperature(body, opts.Temperature)
	body = shouldNormalizeReasoningEffortAlias("openai", opts.NormalizeXHigh, body)
	return body
}

// InjectSystemPrompt injects system prompt into OpenAI Chat body.
func (c *openaiChatCodec) InjectSystemPrompt(body []byte, prompt string) []byte {
	if prompt == "" {
		return body
	}
	return injectSystemPromptIntoOpenAIChat(body, prompt)
}

// ── Response direction ──

// ResponseToCanonical converts Chat response → canonical (Responses) format.
func (c *openaiChatCodec) ResponseToCanonical(body []byte, statusCode int) ([]byte, error) {
	return chatResponseToResponsesResponse(body, statusCode)
}

// ResponseFromCanonical converts canonical (Responses) response → Chat format.
func (c *openaiChatCodec) ResponseFromCanonical(body []byte, statusCode int) ([]byte, error) {
	// TODO: implement when OpenAI Chat clients need Responses backend responses
	return body, nil
}

// ── Streaming ──

// NewStreamDecoder creates a decoder: Chat SSE chunks → canonical (Responses) events.
func (c *openaiChatCodec) NewStreamDecoder() StreamDecoder {
	return &openaiChatStreamDecoder{debug: c.debug}
}

// NewStreamEncoder creates an encoder: canonical events → Chat SSE chunks.
func (c *openaiChatCodec) NewStreamEncoder(w http.ResponseWriter) StreamEncoder {
	// Chat passthrough encoder — writes canonical events as Chat-format SSE
	return &openaiChatStreamEncoder{w: w, debug: c.debug}
}

// ── Utilities ──

func (c *openaiChatCodec) ExtractUsage(body []byte) logging.UsageInfo {
	return logging.ExtractUsage("openai", body)
}

// ---------------------------------------------------------------------------
// chatRequestToResponses converts an OpenAI Chat Completions request body
// to canonical (Responses API) format.
// ---------------------------------------------------------------------------

func chatRequestToResponses(body []byte, opts EncodeOpts) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}

	dst := make(map[string]any)

	// Direct field copies
	if v, ok := src["model"]; ok {
		dst["model"] = v
	}
	if v, ok := src["stream"]; ok {
		dst["stream"] = v
	}
	if v, ok := src["temperature"]; ok {
		dst["temperature"] = v
	}
	if v, ok := src["top_p"]; ok {
		dst["top_p"] = v
	}

	// max_tokens / max_completion_tokens / max_output_tokens → max_output_tokens
	if v, ok := src["max_completion_tokens"]; ok {
		dst["max_output_tokens"] = v
	} else if v, ok := src["max_tokens"]; ok {
		dst["max_output_tokens"] = v
	} else if v, ok := src["max_output_tokens"]; ok {
		dst["max_output_tokens"] = v
	}

	// stop → stop (same name in Responses)
	if v, ok := src["stop"]; ok {
		dst["stop"] = v
	}

	// reasoning_effort → reasoning.effort
	if effort, ok := src["reasoning_effort"].(string); ok && effort != "" {
		dst["reasoning"] = map[string]any{"effort": effort}
	} else if opts.ReasoningEffort != "" {
		dst["reasoning"] = map[string]any{"effort": opts.ReasoningEffort}
	}

	// tools: Chat {type,function:{name,description,parameters}} → Responses {type,name,description,parameters}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		var responsesTools []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tm["function"].(map[string]any)
			if fn == nil {
				responsesTools = append(responsesTools, t)
				continue
			}
			rt := map[string]any{"type": "function"}
			if v, ok := fn["name"]; ok {
				rt["name"] = v
			}
			if v, ok := fn["description"]; ok {
				rt["description"] = v
			}
			if v, ok := fn["parameters"]; ok {
				rt["parameters"] = v
			}
			responsesTools = append(responsesTools, rt)
		}
		dst["tools"] = responsesTools
	}

	// tool_choice: passthrough (same format or string)
	if v, ok := src["tool_choice"]; ok {
		dst["tool_choice"] = v
	}

	// messages → instructions + input
	if messages, ok := src["messages"].([]any); ok {
		var instructions string
		var input []any

		for _, m := range messages {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)

			switch role {
			case "system", "developer":
				text := flattenContent(msg["content"])
				if opts.SystemPrompt != "" {
					text = mergeSystemPrompt(opts.SystemPrompt, text)
					opts.SystemPrompt = "" // only inject once
				}
				if instructions == "" {
					instructions = text
				} else {
					instructions = instructions + "\n\n" + text
				}

			case "tool":
				// tool result → function_call_output
				callID, _ := msg["tool_call_id"].(string)
				content := flattenContent(msg["content"])
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  content,
				})

			case "assistant":
				// Check for tool_calls
				if toolCalls, ok := msg["tool_calls"].([]any); ok && len(toolCalls) > 0 {
					// Add text first if present
					if content := flattenContent(msg["content"]); content != "" {
						input = append(input, map[string]any{
							"role":    "assistant",
							"content": content,
						})
					}
					for _, tc := range toolCalls {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tcm["function"].(map[string]any)
						if fn == nil {
							continue
						}
						input = append(input, map[string]any{
							"type":      "function_call",
							"call_id":   tcm["id"],
							"name":      fn["name"],
							"arguments": fn["arguments"],
						})
					}
				} else {
					content := flattenContent(msg["content"])
					input = append(input, map[string]any{
						"role":    "assistant",
						"content": content,
					})
				}

			default: // "user" and others
				content := flattenContent(msg["content"])
				input = append(input, map[string]any{
					"role":    role,
					"content": content,
				})
			}
		}

		// Inject system prompt if not consumed by a system message
		if opts.SystemPrompt != "" {
			if instructions == "" {
				instructions = opts.SystemPrompt
			} else {
				instructions = opts.SystemPrompt + "\n\n" + instructions
			}
		}

		if instructions != "" {
			dst["instructions"] = instructions
		}
		if len(input) > 0 {
			dst["input"] = input
		}
	}

	return json.Marshal(dst)
}

// ---------------------------------------------------------------------------
// responsesRequestToChat converts a canonical (Responses API) request body
// to OpenAI Chat Completions format.
// ---------------------------------------------------------------------------

func responsesRequestToChat(body []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}

	dst := make(map[string]any)

	// Direct copies
	if v, ok := src["model"]; ok {
		dst["model"] = v
	}
	if v, ok := src["stream"]; ok {
		dst["stream"] = v
	}
	if v, ok := src["temperature"]; ok {
		dst["temperature"] = v
	}
	if v, ok := src["top_p"]; ok {
		dst["top_p"] = v
	}
	if v, ok := src["stop"]; ok {
		dst["stop"] = v
	}

	// max_output_tokens → max_tokens
	if v, ok := src["max_output_tokens"]; ok {
		dst["max_tokens"] = v
	}

	// reasoning.effort → reasoning_effort
	if reasoning, ok := src["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			dst["reasoning_effort"] = effort
		}
	}

	// tools: Responses {type,name,description,parameters} → Chat {type,function:{name,description,parameters}}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		var chatTools []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			// Check if already in Chat format (has "function" key)
			if _, hasFn := tm["function"]; hasFn {
				chatTools = append(chatTools, t)
				continue
			}
			fn := map[string]any{}
			if v, ok := tm["name"]; ok {
				fn["name"] = v
			}
			if v, ok := tm["description"]; ok {
				fn["description"] = v
			}
			if v, ok := tm["parameters"]; ok {
				fn["parameters"] = v
			}
			chatTools = append(chatTools, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		dst["tools"] = chatTools
	}

	if v, ok := src["tool_choice"]; ok {
		dst["tool_choice"] = v
	}

	// instructions → system message; input → messages
	var messages []any

	if instructions, ok := src["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	if input, ok := src["input"].([]any); ok {
		for _, item := range input {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := im["type"].(string)

			switch itemType {
			case "function_call":
				// function_call → assistant message with tool_calls
				args, _ := im["arguments"].(string)
				if args == "" {
					if rawArgs, ok := im["arguments"]; ok {
						b, _ := json.Marshal(rawArgs)
						args = string(b)
					}
				}
				messages = append(messages, map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"id":   im["call_id"],
							"type": "function",
							"function": map[string]any{
								"name":      im["name"],
								"arguments": args,
							},
						},
					},
				})

			case "function_call_output":
				// function_call_output → tool role message
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": im["call_id"],
					"content":      im["output"],
				})

			default:
				// Regular message (has role)
				msg := map[string]any{}
				if v, ok := im["role"]; ok {
					// Map "developer" role to "system" for Chat format
					if v == "developer" {
						v = "system"
					}
					msg["role"] = v
				}
				if v, ok := im["content"]; ok {
					msg["content"] = v
				}
				messages = append(messages, msg)
			}
		}
	}

	if len(messages) > 0 {
		dst["messages"] = messages
	}

	return json.Marshal(dst)
}

// ---------------------------------------------------------------------------
// chatResponseToResponsesResponse converts an OpenAI Chat response body
// to canonical (Responses) format.
// ---------------------------------------------------------------------------

func chatResponseToResponsesResponse(body []byte, statusCode int) ([]byte, error) {
	if statusCode >= 400 {
		// Pass through errors — they'll be handled by the client codec
		return body, nil
	}

	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}

	dst := map[string]any{
		"object": "response",
	}
	if v, ok := src["id"]; ok {
		dst["id"] = v
	}
	if v, ok := src["model"]; ok {
		dst["model"] = v
	}

	// Build output from choices
	var output []any
	choices, _ := src["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if choice != nil {
			msg, _ := choice["message"].(map[string]any)
			if msg != nil {
				// reasoning_content → reasoning output
				if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
					output = append(output, map[string]any{
						"type": "reasoning",
						"summary": []any{
							map[string]any{"type": "summary_text", "text": rc},
						},
					})
				}

				// content → message output
				if content, ok := msg["content"].(string); ok && content != "" {
					output = append(output, map[string]any{
						"type": "message",
						"content": []any{
							map[string]any{"type": "output_text", "text": content},
						},
					})
				}

				// tool_calls → function_call outputs
				if toolCalls, ok := msg["tool_calls"].([]any); ok {
					for _, tc := range toolCalls {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tcm["function"].(map[string]any)
						if fn == nil {
							continue
						}
						output = append(output, map[string]any{
							"type":      "function_call",
							"call_id":   tcm["id"],
							"name":      fn["name"],
							"arguments": fn["arguments"],
						})
					}
				}
			}

			// finish_reason → status
			fr, _ := choice["finish_reason"].(string)
			switch fr {
			case "stop":
				dst["status"] = "completed"
			case "length":
				dst["status"] = "incomplete"
			case "tool_calls":
				dst["status"] = "completed"
			default:
				dst["status"] = "completed"
			}
		}
	}
	dst["output"] = output

	// usage
	if usage, ok := src["usage"].(map[string]any); ok {
		dst["usage"] = map[string]any{
			"input_tokens":  getFloat(usage, "prompt_tokens"),
			"output_tokens": getFloat(usage, "completion_tokens"),
		}
	}

	return json.Marshal(dst)
}

// ---------------------------------------------------------------------------
// OpenAI Chat stream decoder: Chat SSE chunks → canonical (Responses) events
//
// Tracks state across chunks and emits Responses-format events with explicit
// lifecycle boundaries (output_item.added, delta, done, completed).
// ---------------------------------------------------------------------------

type openaiChatStreamDecoder struct {
	debug *logging.DebugLog
	usage logging.UsageInfo

	// state
	model            string
	id               string
	reasoningStarted bool
	textStarted      bool
	toolCalls        map[int]*chatStreamToolCall // OpenAI index → state
	finishReason     string
}

type chatStreamToolCall struct {
	id   string
	name string
	args strings.Builder
}

func (d *openaiChatStreamDecoder) Decode(line string) ([]CanonicalEvent, error) {
	if !strings.HasPrefix(line, "data: ") {
		return nil, nil
	}

	data := line[6:]
	if data == "[DONE]" {
		return nil, nil
	}

	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
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
		} `json:"usage"`
	}

	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, nil
	}

	if chunk.ID != "" {
		d.id = chunk.ID
	}
	if chunk.Model != "" {
		d.model = chunk.Model
	}
	if chunk.Usage != nil {
		d.usage.PromptTokens = chunk.Usage.PromptTokens
		d.usage.CompletionTokens = chunk.Usage.CompletionTokens
		d.usage.InputTokens = chunk.Usage.PromptTokens
		d.usage.OutputTokens = chunk.Usage.CompletionTokens
		d.usage.TotalTokens = chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens
	}

	if len(chunk.Choices) == 0 {
		return nil, nil
	}
	choice := chunk.Choices[0]
	delta := choice.Delta

	var events []CanonicalEvent

	// Emit response.created on first meaningful chunk
	if d.model != "" && !d.reasoningStarted && !d.textStarted && len(d.toolCalls) == 0 {
		if delta.ReasoningContent != "" || delta.Content != "" || len(delta.ToolCalls) > 0 {
			events = append(events, d.makeEvent("response.created", map[string]any{
				"response": map[string]any{
					"id":     d.id,
					"model":  d.model,
					"status": "in_progress",
				},
			}))
		}
	}

	// reasoning_content → reasoning summary events
	if delta.ReasoningContent != "" {
		if !d.reasoningStarted {
			d.reasoningStarted = true
			events = append(events, d.makeEvent("response.reasoning_summary_part.added", map[string]any{}))
		}
		events = append(events, d.makeEvent("response.reasoning_summary_text.delta", map[string]any{
			"delta": delta.ReasoningContent,
		}))
	}

	// content → output text events
	if delta.Content != "" {
		if !d.textStarted {
			if d.reasoningStarted {
				events = append(events, d.makeEvent("response.reasoning_summary_text.done", map[string]any{}))
				events = append(events, d.makeEvent("response.reasoning_summary_part.done", map[string]any{}))
				d.reasoningStarted = false // closed
			}
			d.textStarted = true
			events = append(events, d.makeEvent("response.output_item.added", map[string]any{
				"output_index": 0,
				"item":         map[string]any{"type": "message"},
			}))
			events = append(events, d.makeEvent("response.content_part.added", map[string]any{
				"output_index": 0,
			}))
		}
		events = append(events, d.makeEvent("response.output_text.delta", map[string]any{
			"output_index": 0,
			"delta":        delta.Content,
		}))
	}

	// tool_calls → function call events
	for _, tc := range delta.ToolCalls {
		if d.toolCalls == nil {
			d.toolCalls = make(map[int]*chatStreamToolCall)
		}

		existing, exists := d.toolCalls[tc.Index]
		if !exists {
			// Close reasoning/text if open
			if d.reasoningStarted {
				events = append(events, d.makeEvent("response.reasoning_summary_text.done", map[string]any{}))
				events = append(events, d.makeEvent("response.reasoning_summary_part.done", map[string]any{}))
				d.reasoningStarted = false
			}
			if d.textStarted {
				events = append(events, d.makeEvent("response.output_text.done", map[string]any{
					"output_index": 0,
				}))
				d.textStarted = false
			}

			existing = &chatStreamToolCall{id: tc.ID, name: tc.Function.Name}
			d.toolCalls[tc.Index] = existing
			events = append(events, d.makeEvent("response.output_item.added", map[string]any{
				"output_index": tc.Index + 1, // offset by 1 for message output at 0
				"item": map[string]any{
					"type":    "function_call",
					"call_id": tc.ID,
					"name":    tc.Function.Name,
				},
			}))
		}

		if tc.Function.Arguments != "" {
			existing.args.WriteString(tc.Function.Arguments)
			events = append(events, d.makeEvent("response.function_call_arguments.delta", map[string]any{
				"output_index": tc.Index + 1,
				"delta":        tc.Function.Arguments,
			}))
		}
	}

	// finish_reason
	if choice.FinishReason != nil {
		d.finishReason = *choice.FinishReason
	}

	return events, nil
}

func (d *openaiChatStreamDecoder) Flush() ([]CanonicalEvent, logging.UsageInfo) {
	d.usage.ResponseModel = d.model

	var events []CanonicalEvent

	// Close open reasoning
	if d.reasoningStarted {
		events = append(events, d.makeEvent("response.reasoning_summary_text.done", map[string]any{}))
		events = append(events, d.makeEvent("response.reasoning_summary_part.done", map[string]any{}))
	}

	// Close open text
	if d.textStarted {
		events = append(events, d.makeEvent("response.output_text.done", map[string]any{
			"output_index": 0,
		}))
	}

	// Close open tool calls
	for idx := range d.toolCalls {
		events = append(events, d.makeEvent("response.function_call_arguments.done", map[string]any{
			"output_index": idx + 1,
		}))
	}

	// response.completed
	status := "completed"
	if d.finishReason == "length" {
		status = "incomplete"
	}

	var output []any
	// Reconstruct output for completed event
	events = append(events, d.makeEvent("response.completed", map[string]any{
		"response": map[string]any{
			"id":     d.id,
			"model":  d.model,
			"status": status,
			"output": output,
			"usage": map[string]any{
				"input_tokens":  d.usage.InputTokens,
				"output_tokens": d.usage.OutputTokens,
			},
		},
	}))

	return events, d.usage
}

func (d *openaiChatStreamDecoder) makeEvent(eventType string, data any) CanonicalEvent {
	jsonData, _ := json.Marshal(data)
	return CanonicalEvent{
		EventType: eventType,
		Data:      json.RawMessage(jsonData),
	}
}

// ---------------------------------------------------------------------------
// OpenAI Chat stream encoder: canonical events → Chat SSE chunks
// (for passthrough or future Responses→Chat streaming)
// ---------------------------------------------------------------------------

type openaiChatStreamEncoder struct {
	w     http.ResponseWriter
	debug *logging.DebugLog
}

func (e *openaiChatStreamEncoder) Encode(event CanonicalEvent) error {
	// For passthrough, write as-is in Chat SSE format
	line := "data: " + string(event.Data) + "\n\n"
	_, err := e.w.Write([]byte(line))
	if err != nil {
		return err
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (e *openaiChatStreamEncoder) Close() error {
	_, err := e.w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return err
}
