package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

func needsProtocolConversion(clientProtocol, backendProtocol string) bool {
	return clientProtocol != "" && clientProtocol != backendProtocol
}

// isOpenAIFamily returns true if the protocol is any OpenAI variant (openai, openai-responses).
func isOpenAIFamily(protocol string) bool {
	return protocol == "openai" || protocol == "openai-responses"
}

func flattenContent(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	blocks, ok := v.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := block["text"].(string); ok {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

func mergeSystemPrompt(injectedPrompt string, reqSystem any) string {
	injectedPrompt = strings.TrimSpace(injectedPrompt)
	reqPrompt := strings.TrimSpace(flattenContent(reqSystem))

	switch {
	case injectedPrompt == "":
		return reqPrompt
	case reqPrompt == "":
		return injectedPrompt
	default:
		return injectedPrompt + "\n\n" + reqPrompt
	}
}

// injectSystemPromptIntoOpenAIChat injects a system prompt into an OpenAI
// Chat Completions request body. If the first message already has role "system",
// the injected prompt is prepended to its content; otherwise a new system
// message is inserted at the beginning of the messages array.
// Returns the original body unchanged on any parse error.
func injectSystemPromptIntoOpenAIChat(body []byte, systemPrompt string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}

	messages, _ := obj["messages"].([]any)
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok {
			if role, _ := first["role"].(string); role == "system" {
				existing, _ := first["content"].(string)
				first["content"] = mergeSystemPrompt(systemPrompt, existing)
				out, err := json.Marshal(obj)
				if err != nil {
					return body
				}
				return out
			}
		}
	}

	sysMsg := map[string]any{"role": "system", "content": systemPrompt}
	obj["messages"] = append([]any{sysMsg}, messages...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// injectSystemPromptIntoOpenAIResponses injects a system prompt into an OpenAI
// Responses API request body. If the first input item already has role "developer",
// the injected prompt is prepended to its content; otherwise a new developer
// message is inserted at the beginning of the input array.
// Returns the original body unchanged on any parse error.
func injectSystemPromptIntoOpenAIResponses(body []byte, systemPrompt string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}

	input, _ := obj["input"].([]any)
	if len(input) > 0 {
		if first, ok := input[0].(map[string]any); ok {
			if role, _ := first["role"].(string); role == "developer" {
				existing, _ := first["content"].(string)
				first["content"] = mergeSystemPrompt(systemPrompt, existing)
				out, err := json.Marshal(obj)
				if err != nil {
					return body
				}
				return out
			}
		}
	}

	devMsg := map[string]any{"role": "developer", "content": systemPrompt}
	obj["input"] = append([]any{devMsg}, input...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func normalizeReasoningEffort(defaultEffort string) string {
	effort := strings.ToLower(strings.TrimSpace(defaultEffort))
	switch effort {
	case "low", "medium", "high", "xhigh":
		return effort
	default:
		return ""
	}
}

func mapThinkingToReasoningEffort(thinking any, defaultEffort string) (string, bool) {
	if thinking == nil {
		if strings.TrimSpace(defaultEffort) == "" {
			return "", false
		}
		effort := normalizeReasoningEffort(defaultEffort)
		if effort == "" {
			return "", false
		}
		return effort, true
	}

	thinkingMap, ok := thinking.(map[string]any)
	if !ok {
		return "", false
	}

	if tp, _ := thinkingMap["type"].(string); tp != "" && tp != "enabled" && tp != "adaptive" {
		return "", false
	}

	// adaptive thinking: no budget_tokens, use default effort
	if tp, _ := thinkingMap["type"].(string); tp == "adaptive" {
		effort := normalizeReasoningEffort(defaultEffort)
		if effort == "" {
			return "", false
		}
		return effort, true
	}

	budget, ok := numberToInt(thinkingMap["budget_tokens"])
	if !ok {
		effort := normalizeReasoningEffort(defaultEffort)
		if effort == "" {
			return "", false
		}
		return effort, true
	}

	switch {
	case budget <= 4096:
		return "low", true
	case budget <= 12288:
		return "medium", true
	case budget <= 32768:
		return "high", true
	default:
		return "xhigh", true
	}
}

func numberToInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func convertAnthropicRequestToOpenAI(body []byte, defaultReasoningEffort, injectedSystemPrompt string) ([]byte, string, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, "", fmt.Errorf("unmarshal anthropic request: %w", err)
	}

	dst := make(map[string]any)

	for _, key := range []string{"model", "max_tokens", "temperature", "top_p", "stream"} {
		if v, ok := src[key]; ok {
			dst[key] = v
		}
	}

	if v, ok := src["stop_sequences"]; ok {
		dst["stop"] = v
	}

	if effort, ok := mapThinkingToReasoningEffort(src["thinking"], defaultReasoningEffort); ok {
		dst["reasoning_effort"] = effort
	}

	// Convert tools definitions: Anthropic input_schema -> OpenAI parameters
	if tools, ok := src["tools"].([]any); ok {
		var openAITools []map[string]any
		for _, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			fn := map[string]any{
				"name":       name,
				"parameters": tool["input_schema"],
			}
			if desc != "" {
				fn["description"] = desc
			}
			openAITools = append(openAITools, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		if len(openAITools) > 0 {
			dst["tools"] = openAITools
		}
	}

	// Convert tool_choice
	if tc, ok := src["tool_choice"]; ok {
		if tcMap, ok := tc.(map[string]any); ok {
			tcType, _ := tcMap["type"].(string)
			switch tcType {
			case "auto":
				dst["tool_choice"] = "auto"
			case "any":
				dst["tool_choice"] = "required"
			case "tool":
				tcName, _ := tcMap["name"].(string)
				dst["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": tcName},
				}
			case "disabled":
				dst["tool_choice"] = "none"
			}
		}
	}

	var messages []map[string]any

	if sysText := mergeSystemPrompt(injectedSystemPrompt, src["system"]); sysText != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": sysText,
		})
	}

	if msgs, ok := src["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			converted := convertAnthropicMessageToOpenAI(role, msg["content"])
			messages = append(messages, converted...)
		}
	}

	dst["messages"] = messages

	out, err := json.Marshal(dst)
	if err != nil {
		return nil, "", fmt.Errorf("marshal openai request: %w", err)
	}
	return out, "/v1/chat/completions", nil
}

// convertAnthropicRequestToOpenAIResponses converts an Anthropic /v1/messages request
// to an OpenAI /v1/responses request format.
func convertAnthropicRequestToOpenAIResponses(body []byte, defaultReasoningEffort, injectedSystemPrompt string) ([]byte, string, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, "", fmt.Errorf("unmarshal anthropic request: %w", err)
	}

	dst := make(map[string]any)

	// Model and stream pass through
	for _, key := range []string{"model", "stream", "temperature", "top_p"} {
		if v, ok := src[key]; ok {
			dst[key] = v
		}
	}

	// max_tokens → max_output_tokens
	if v, ok := src["max_tokens"]; ok {
		dst["max_output_tokens"] = v
	}

	// stop_sequences → stop
	if v, ok := src["stop_sequences"]; ok {
		dst["stop"] = v
	}

	// thinking → reasoning.effort
	effort, effortOk := mapThinkingToReasoningEffort(src["thinking"], defaultReasoningEffort)
	if effortOk {
		dst["reasoning"] = map[string]any{"effort": effort}
	}

	// Convert tools: Anthropic input_schema → Responses API flat format
	hasTools := false
	if tools, ok := src["tools"].([]any); ok {
		var respTools []map[string]any
		for _, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			ft := map[string]any{
				"type":       "function",
				"name":       name,
				"parameters": tool["input_schema"],
			}
			if desc != "" {
				ft["description"] = desc
			}
			respTools = append(respTools, ft)
		}
		if len(respTools) > 0 {
			dst["tools"] = respTools
			hasTools = true
		}
	}

	// Convert tool_choice
	_, hasExplicitToolChoice := src["tool_choice"]
	if tc, ok := src["tool_choice"]; ok {
		if tcMap, ok := tc.(map[string]any); ok {
			tcType, _ := tcMap["type"].(string)
			switch tcType {
			case "auto":
				dst["tool_choice"] = "auto"
			case "any":
				dst["tool_choice"] = "required"
			case "tool":
				tcName, _ := tcMap["name"].(string)
				dst["tool_choice"] = map[string]any{
					"type": "function",
					"name": tcName,
				}
			case "disabled":
				dst["tool_choice"] = "none"
			}
		}
	}
	if hasTools && !hasExplicitToolChoice {
		dst["tool_choice"] = "auto"
	}

	// Build input array
	var input []any

	// System → developer role
	if sysText := mergeSystemPrompt(injectedSystemPrompt, src["system"]); sysText != "" {
		input = append(input, map[string]any{
			"role":    "developer",
			"content": sysText,
		})
	}

	// Convert messages
	if msgs, ok := src["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			items := convertAnthropicMessageToResponses(role, msg["content"])
			input = append(input, items...)
		}
	}

	dst["input"] = input

	out, err := json.Marshal(dst)
	if err != nil {
		return nil, "", fmt.Errorf("marshal responses request: %w", err)
	}
	return out, "/v1/responses", nil
}

// convertAnthropicMessageToResponses converts a single Anthropic message to
// one or more Responses API input items.
func convertAnthropicMessageToResponses(role string, content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}

	blocks, ok := content.([]any)
	if !ok {
		return []any{map[string]any{"role": role, "content": ""}}
	}

	var result []any
	var textParts []string

	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, _ := block["text"].(string)
			textParts = append(textParts, text)
		case "tool_use":
			// Flush text parts before tool_use
			if len(textParts) > 0 {
				result = append(result, map[string]any{
					"role":    role,
					"content": strings.Join(textParts, ""),
				})
				textParts = nil
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			input := block["input"]
			argsBytes, _ := json.Marshal(input)
			result = append(result, map[string]any{
				"type":      "function_call",
				"call_id":   id,
				"name":      name,
				"arguments": string(argsBytes),
			})
		case "tool_result":
			toolUseID, _ := block["tool_use_id"].(string)
			trContent := extractToolResultContent(block["content"])
			result = append(result, map[string]any{
				"type":    "function_call_output",
				"call_id": toolUseID,
				"output":  trContent,
			})
		}
	}

	// Flush remaining text parts
	if len(textParts) > 0 {
		result = append(result, map[string]any{
			"role":    role,
			"content": strings.Join(textParts, ""),
		})
	}

	if len(result) == 0 {
		return []any{map[string]any{"role": role, "content": ""}}
	}

	return result
}

// convertAnthropicMessageToOpenAI converts an Anthropic message (with content
// blocks that may include text, tool_use, and tool_result) into one or more
// OpenAI-format messages.
func convertAnthropicMessageToOpenAI(role string, content any) []map[string]any {
	if s, ok := content.(string); ok {
		return []map[string]any{{"role": role, "content": s}}
	}

	blocks, ok := content.([]any)
	if !ok {
		return []map[string]any{{"role": role, "content": ""}}
	}

	var textParts []string
	var toolUses []map[string]any
	var toolResults []map[string]any

	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, _ := block["text"].(string)
			textParts = append(textParts, text)
		case "tool_use":
			toolUses = append(toolUses, block)
		case "tool_result":
			toolResults = append(toolResults, block)
		}
	}

	var result []map[string]any

	// Handle assistant messages with tool_use blocks
	if role == "assistant" {
		msg := map[string]any{"role": "assistant"}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "")
		}
		if len(toolUses) > 0 {
			var toolCalls []map[string]any
			for _, tu := range toolUses {
				id, _ := tu["id"].(string)
				name, _ := tu["name"].(string)
				input := tu["input"]
				argsBytes, _ := json.Marshal(input)
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(argsBytes),
					},
				})
			}
			msg["tool_calls"] = toolCalls
		}
		result = append(result, msg)
		return result
	}

	// Handle tool_result blocks -> OpenAI "tool" role messages
	for _, tr := range toolResults {
		toolUseID, _ := tr["tool_use_id"].(string)
		trContent := extractToolResultContent(tr["content"])
		result = append(result, map[string]any{
			"role":         "tool",
			"tool_call_id": toolUseID,
			"content":      trContent,
		})
	}

	// Handle text content in user/other messages
	if len(textParts) > 0 {
		result = append(result, map[string]any{
			"role":    role,
			"content": strings.Join(textParts, ""),
		})
	}

	if len(result) == 0 {
		return []map[string]any{{"role": role, "content": ""}}
	}

	return result
}

// extractToolResultContent extracts text from a tool_result content field,
// which can be a string or an array of content blocks.
func extractToolResultContent(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		return fmt.Sprintf("%v", content)
	}
	var parts []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := block["text"].(string); ok {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

func convertOpenAIResponseToAnthropic(body []byte, statusCode int) ([]byte, error) {
	if statusCode < 200 || statusCode >= 300 {
		return convertOpenAIErrorToAnthropic(body)
	}

	var src struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("unmarshal openai response: %w", err)
	}

	var contentBlocks []map[string]any
	finishReason := ""

	if len(src.Choices) > 0 {
		choice := src.Choices[0]
		finishReason = choice.FinishReason

		// Thinking block from reasoning_content (must come before text/tool_use)
		if choice.Message.ReasoningContent != nil && *choice.Message.ReasoningContent != "" {
			contentBlocks = append(contentBlocks, map[string]any{
				"type":     "thinking",
				"thinking": *choice.Message.ReasoningContent,
			})
		}

		if choice.Message.Content != nil && *choice.Message.Content != "" {
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": *choice.Message.Content,
			})
		}

		for _, tc := range choice.Message.ToolCalls {
			var input any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				input = map[string]any{}
			}
			contentBlocks = append(contentBlocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}

	if len(contentBlocks) == 0 {
		contentBlocks = []map[string]any{{"type": "text", "text": ""}}
	}

	stopReason := mapFinishReason(finishReason)
	// Defensive override: if tool_use blocks exist, force stop_reason to tool_use
	for _, block := range contentBlocks {
		if block["type"] == "tool_use" {
			stopReason = "tool_use"
			break
		}
	}

	resp := map[string]any{
		"id":            src.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         src.Model,
		"content":       contentBlocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  src.Usage.PromptTokens,
			"output_tokens": src.Usage.CompletionTokens,
		},
	}

	return json.Marshal(resp)
}

func convertOpenAIErrorToAnthropic(body []byte) ([]byte, error) {
	var src struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": string(body),
			},
		})
	}
	errType := src.Error.Type
	if errType == "" {
		errType = "api_error"
	}
	return json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": src.Error.Message,
		},
	})
}

// mapResponsesStatus maps Responses API status to Anthropic stop_reason.
func mapResponsesStatus(status string) string {
	switch status {
	case "completed":
		return "end_turn"
	case "incomplete":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// convertOpenAIResponsesResponseToAnthropic converts a non-streaming /v1/responses
// response to Anthropic /v1/messages format.
func convertOpenAIResponsesResponseToAnthropic(body []byte, statusCode int) ([]byte, error) {
	if statusCode < 200 || statusCode >= 300 {
		return convertOpenAIErrorToAnthropic(body)
	}

	var src struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			// reasoning fields
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			// function_call fields
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("unmarshal responses response: %w", err)
	}

	var contentBlocks []map[string]any
	var thinkingBlocks []map[string]any

	for _, item := range src.Output {
		switch item.Type {
		case "reasoning":
			for _, part := range item.Summary {
				if part.Type == "summary_text" && part.Text != "" {
					thinkingBlocks = append(thinkingBlocks, map[string]any{
						"type":     "thinking",
						"thinking": part.Text,
					})
				}
			}
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					contentBlocks = append(contentBlocks, map[string]any{
						"type": "text",
						"text": part.Text,
					})
				}
			}
		case "function_call":
			var input any
			if err := json.Unmarshal([]byte(item.Arguments), &input); err != nil {
				input = map[string]any{}
			}
			contentBlocks = append(contentBlocks, map[string]any{
				"type":  "tool_use",
				"id":    item.CallID,
				"name":  item.Name,
				"input": input,
			})
		}
	}

	// Thinking blocks must come before text/tool_use blocks
	if len(thinkingBlocks) > 0 {
		contentBlocks = append(thinkingBlocks, contentBlocks...)
	}

	if len(contentBlocks) == 0 {
		contentBlocks = []map[string]any{{"type": "text", "text": ""}}
	}

	inputTokens := 0
	outputTokens := 0
	if src.Usage != nil {
		inputTokens = src.Usage.InputTokens
		outputTokens = src.Usage.OutputTokens
	}

	stopReason := mapResponsesStatus(src.Status)
	// If there are tool_use blocks, stop_reason should be tool_use
	for _, block := range contentBlocks {
		if block["type"] == "tool_use" {
			stopReason = "tool_use"
			break
		}
	}

	resp := map[string]any{
		"id":            src.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         src.Model,
		"content":       contentBlocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	return json.Marshal(resp)
}

// convertResponsesRequestToAnthropic converts a canonical (Responses API) request
// to Anthropic /v1/messages format.
func convertResponsesRequestToAnthropic(body []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("unmarshal responses request: %w", err)
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

	// max_output_tokens → max_tokens
	if v, ok := src["max_output_tokens"]; ok {
		dst["max_tokens"] = v
	}

	// stop → stop_sequences
	if v, ok := src["stop"]; ok {
		dst["stop_sequences"] = v
	}

	// reasoning.effort → thinking (with budget_tokens)
	if reasoning, ok := src["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			switch effort {
			case "low":
				dst["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 4096}
			case "medium":
				dst["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 12288}
			case "high":
				dst["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 32768}
			case "xhigh":
				dst["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 65536}
			}
		}
	}

	// tools: Responses {type,name,description,parameters} → Anthropic {name,description,input_schema}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		var anthropicTools []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			at := map[string]any{}
			if v, ok := tm["name"].(string); ok {
				at["name"] = v
			}
			if v, ok := tm["description"].(string); ok {
				at["description"] = v
			}
			if v, ok := tm["parameters"]; ok {
				at["input_schema"] = v
			}
			anthropicTools = append(anthropicTools, at)
		}
		if len(anthropicTools) > 0 {
			dst["tools"] = anthropicTools
		}
	}

	// tool_choice conversion
	if tc, ok := src["tool_choice"]; ok {
		switch v := tc.(type) {
		case string:
			switch v {
			case "auto":
				dst["tool_choice"] = map[string]any{"type": "auto"}
			case "required":
				dst["tool_choice"] = map[string]any{"type": "any"}
			case "none":
				dst["tool_choice"] = map[string]any{"type": "disabled"}
			}
		case map[string]any:
			if v["type"] == "function" {
				if fn, ok := v["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok {
						dst["tool_choice"] = map[string]any{"type": "tool", "name": name}
					}
				}
			}
		}
	}

	// instructions → system
	if instructions, ok := src["instructions"].(string); ok && instructions != "" {
		dst["system"] = instructions
	}

	// input → messages
	if input, ok := src["input"].([]any); ok {
		var messages []any

		for _, item := range input {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := im["type"].(string)

			switch itemType {
			case "function_call":
				// function_call → assistant message with tool_use content block
				args, _ := im["arguments"].(string)
				var input any
				if args != "" {
					json.Unmarshal([]byte(args), &input)
				}
				messages = append(messages, map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{
							"type":  "tool_use",
							"id":    im["call_id"],
							"name":  im["name"],
							"input": input,
						},
					},
				})

			case "function_call_output":
				// function_call_output → user message with tool_result content block
				messages = append(messages, map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{
							"type":        "tool_result",
							"tool_use_id": im["call_id"],
							"content":     im["output"],
						},
					},
				})

			default:
				// Regular message (has role)
				role, _ := im["role"].(string)
				content := im["content"]

				// Map developer role to system for Anthropic
				if role == "developer" {
					existingSystem, _ := dst["system"].(string)
					contentStr := flattenContent(content)
					if existingSystem != "" {
						dst["system"] = existingSystem + "\n\n" + contentStr
					} else {
						dst["system"] = contentStr
					}
					continue
				}

				// Convert content to Anthropic format (string or content blocks)
				var anthropicContent any
				if s, ok := content.(string); ok {
					anthropicContent = s
				} else if content != nil {
					anthropicContent = content
				} else {
					anthropicContent = ""
				}

				messages = append(messages, map[string]any{
					"role":    role,
					"content": anthropicContent,
				})
			}
		}

		if len(messages) > 0 {
			dst["messages"] = messages
		}
	}

	out, err := json.Marshal(dst)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	return out, nil
}

// convertAnthropicResponseToResponses converts an Anthropic /v1/messages response
// to canonical (Responses API) format.
func convertAnthropicResponseToResponses(body []byte, statusCode int) ([]byte, error) {
	if statusCode < 200 || statusCode >= 300 {
		return convertAnthropicErrorToResponses(body)
	}

	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("unmarshal anthropic response: %w", err)
	}

	dst := map[string]any{
		"object": "response",
	}

	if v, ok := src["id"].(string); ok {
		dst["id"] = v
	}
	if v, ok := src["model"].(string); ok {
		dst["model"] = v
	}

	// Map stop_reason to status
	stopReason, _ := src["stop_reason"].(string)
	switch stopReason {
	case "end_turn":
		dst["status"] = "completed"
	case "max_tokens":
		dst["status"] = "incomplete"
	case "tool_use":
		dst["status"] = "completed"
	default:
		dst["status"] = "completed"
	}

	// Convert content blocks to output
	content, _ := src["content"].([]any)
	var output []any

	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := b["type"].(string)

		switch blockType {
		case "thinking":
			// thinking → reasoning output
			thinking, _ := b["thinking"].(string)
			if thinking != "" {
				output = append(output, map[string]any{
					"type": "reasoning",
					"summary": []any{
						map[string]any{"type": "summary_text", "text": thinking},
					},
				})
			}

		case "text":
			// text → message output
			text, _ := b["text"].(string)
			if text != "" {
				output = append(output, map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{"type": "output_text", "text": text},
					},
				})
			}

		case "tool_use":
			// tool_use → function_call output
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			input := b["input"]
			argsBytes, _ := json.Marshal(input)
			output = append(output, map[string]any{
				"type":      "function_call",
				"call_id":   id,
				"name":      name,
				"arguments": string(argsBytes),
			})
		}
	}

	if len(output) == 0 {
		output = []any{
			map[string]any{
				"type": "message",
				"content": []any{
					map[string]any{"type": "output_text", "text": ""},
				},
			},
		}
	}
	dst["output"] = output

	// usage
	if usage, ok := src["usage"].(map[string]any); ok {
		dst["usage"] = map[string]any{
			"input_tokens":  usage["input_tokens"],
			"output_tokens": usage["output_tokens"],
		}
	}

	return json.Marshal(dst)
}

// convertAnthropicErrorToResponses converts an Anthropic error response to Responses format.
func convertAnthropicErrorToResponses(body []byte) ([]byte, error) {
	var src struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &src); err != nil {
		return json.Marshal(map[string]any{
			"object": "response",
			"status": "failed",
			"error":  map[string]any{"message": string(body)},
		})
	}

	return json.Marshal(map[string]any{
		"object": "response",
		"status": "failed",
		"error": map[string]any{
			"type":    src.Error.Type,
			"message": src.Error.Message,
		},
	})
}

// responsesResponseToChat converts a canonical (Responses API) response
// to OpenAI Chat Completions format.
func responsesResponseToChat(body []byte, statusCode int) ([]byte, error) {
	if statusCode < 200 || statusCode >= 300 {
		return responsesErrorToChat(body)
	}

	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("unmarshal responses response: %w", err)
	}

	respID, _ := src["id"].(string)
	model, _ := src["model"].(string)
	status, _ := src["status"].(string)
	output, _ := src["output"].([]any)
	usage, _ := src["usage"].(map[string]any)

	var content string
	var toolCalls []map[string]any
	var reasoningContent string

	for _, item := range output {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := itemMap["type"].(string)

		switch itemType {
		case "message":
			if contentParts, ok := itemMap["content"].([]any); ok {
				for _, cp := range contentParts {
					cpMap, ok := cp.(map[string]any)
					if !ok {
						continue
					}
					if cpType, _ := cpMap["type"].(string); cpType == "output_text" {
						if text, ok := cpMap["text"].(string); ok {
							content += text
						}
					}
				}
			}

		case "reasoning":
			if summary, ok := itemMap["summary"].([]any); ok {
				for _, s := range summary {
					sMap, ok := s.(map[string]any)
					if !ok {
						continue
					}
					if sType, _ := sMap["type"].(string); sType == "summary_text" {
						if text, ok := sMap["text"].(string); ok {
							reasoningContent += text
						}
					}
				}
			}

		case "function_call":
			callID, _ := itemMap["call_id"].(string)
			name, _ := itemMap["name"].(string)
			args, _ := itemMap["arguments"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			})
		}
	}

	finishReason := "stop"
	if status == "incomplete" {
		finishReason = "length"
	}
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	message := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if reasoningContent != "" {
		message["reasoning_content"] = reasoningContent
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	promptTokens, _ := usage["input_tokens"].(float64)
	completionTokens, _ := usage["output_tokens"].(float64)

	chatResp := map[string]any{
		"id":      respID,
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     int(promptTokens),
			"completion_tokens": int(completionTokens),
			"total_tokens":      int(promptTokens + completionTokens),
		},
	}

	return json.Marshal(chatResp)
}

// responsesErrorToChat converts a Responses API error to Chat format.
func responsesErrorToChat(body []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"message": string(body),
				"type":    "api_error",
			},
		})
	}

	// Check for error in Responses format
	if errObj, ok := src["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		errType, _ := errObj["type"].(string)
		if errType == "" {
			errType = "api_error"
		}
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    errType,
			},
		})
	}

	// Check for status indicating failure
	if status, _ := src["status"].(string); status == "failed" {
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "request failed",
				"type":    "api_error",
			},
		})
	}

	return json.Marshal(map[string]any{
		"error": map[string]any{
			"message": string(body),
			"type":    "api_error",
		},
	})
}
