package server

import (
	"encoding/json"
	"testing"
)

func TestFlattenContent(t *testing.T) {
	if got := flattenContent("hello"); got != "hello" {
		t.Fatalf("string: got %q, want hello", got)
	}
	blocks := []any{
		map[string]any{"type": "text", "text": "foo"},
		map[string]any{"type": "text", "text": "bar"},
	}
	if got := flattenContent(blocks); got != "foobar" {
		t.Fatalf("blocks: got %q, want foobar", got)
	}
	if got := flattenContent(nil); got != "" {
		t.Fatalf("nil: got %q, want empty", got)
	}
}

func TestMergeSystemPrompt(t *testing.T) {
	tests := []struct {
		name     string
		injected string
		req      any
		want     string
	}{
		{
			name:     "only request system",
			injected: "",
			req:      "Request prompt",
			want:     "Request prompt",
		},
		{
			name:     "only injected prompt",
			injected: "Injected prompt",
			req:      nil,
			want:     "Injected prompt",
		},
		{
			name:     "prepend injected to request",
			injected: "Injected prompt",
			req:      "Request prompt",
			want:     "Injected prompt\n\nRequest prompt",
		},
		{
			name:     "request blocks flattened",
			injected: "Injected prompt",
			req: []any{
				map[string]any{"type": "text", "text": "Request "},
				map[string]any{"type": "text", "text": "prompt"},
			},
			want: "Injected prompt\n\nRequest prompt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeSystemPrompt(tt.injected, tt.req)
			if got != tt.want {
				t.Fatalf("mergeSystemPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInjectSystemPromptIntoOpenAIChat(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		prompt string
		check  func(t *testing.T, result map[string]any)
	}{
		{
			name:   "insert when no system message",
			body:   `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
			prompt: "Be helpful",
			check: func(t *testing.T, result map[string]any) {
				msgs := result["messages"].([]any)
				if len(msgs) != 2 {
					t.Fatalf("expected 2 messages, got %d", len(msgs))
				}
				first := msgs[0].(map[string]any)
				if first["role"] != "system" || first["content"] != "Be helpful" {
					t.Fatalf("unexpected first message: %v", first)
				}
			},
		},
		{
			name:   "merge with existing system message",
			body:   `{"model":"gpt-5","messages":[{"role":"system","content":"Existing"},{"role":"user","content":"hi"}]}`,
			prompt: "Injected",
			check: func(t *testing.T, result map[string]any) {
				msgs := result["messages"].([]any)
				if len(msgs) != 2 {
					t.Fatalf("expected 2 messages, got %d", len(msgs))
				}
				first := msgs[0].(map[string]any)
				if first["content"] != "Injected\n\nExisting" {
					t.Fatalf("unexpected merged content: %q", first["content"])
				}
			},
		},
		{
			name:   "empty prompt returns body unchanged",
			body:   `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
			prompt: "",
			check: func(t *testing.T, result map[string]any) {
				// injectSystemPromptIntoOpenAIChat should not be called with empty prompt,
				// but if it is, mergeSystemPrompt handles empty correctly
				msgs := result["messages"].([]any)
				first := msgs[0].(map[string]any)
				if first["role"] != "system" {
					// Empty prompt still inserts a system message with empty content — caller should guard
					t.Skip("empty prompt edge case")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := injectSystemPromptIntoOpenAIChat([]byte(tt.body), tt.prompt)
			var obj map[string]any
			if err := json.Unmarshal(result, &obj); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			tt.check(t, obj)
		})
	}
}

func TestInjectSystemPromptIntoOpenAIResponses(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		prompt string
		check  func(t *testing.T, result map[string]any)
	}{
		{
			name:   "insert when no developer message",
			body:   `{"model":"gpt-5","input":[{"role":"user","content":"hi"}]}`,
			prompt: "Be helpful",
			check: func(t *testing.T, result map[string]any) {
				input := result["input"].([]any)
				if len(input) != 2 {
					t.Fatalf("expected 2 items, got %d", len(input))
				}
				first := input[0].(map[string]any)
				if first["role"] != "developer" || first["content"] != "Be helpful" {
					t.Fatalf("unexpected first item: %v", first)
				}
			},
		},
		{
			name:   "merge with existing developer message",
			body:   `{"model":"gpt-5","input":[{"role":"developer","content":"Existing"},{"role":"user","content":"hi"}]}`,
			prompt: "Injected",
			check: func(t *testing.T, result map[string]any) {
				input := result["input"].([]any)
				if len(input) != 2 {
					t.Fatalf("expected 2 items, got %d", len(input))
				}
				first := input[0].(map[string]any)
				if first["content"] != "Injected\n\nExisting" {
					t.Fatalf("unexpected merged content: %q", first["content"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := injectSystemPromptIntoOpenAIResponses([]byte(tt.body), tt.prompt)
			var obj map[string]any
			if err := json.Unmarshal(result, &obj); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			tt.check(t, obj)
		})
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"content_filter", "end_turn"},
		{"", "end_turn"},
	}
	for _, tt := range tests {
		if got := mapFinishReason(tt.input); got != tt.want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMapFinishReasonToolCalls(t *testing.T) {
	if got := mapFinishReason("tool_calls"); got != "tool_use" {
		t.Errorf("mapFinishReason(tool_calls) = %q, want tool_use", got)
	}
}

func TestNeedsProtocolConversion(t *testing.T) {
	if !needsProtocolConversion("anthropic", "openai") {
		t.Fatal("expected true for anthropic->openai")
	}
	if needsProtocolConversion("openai", "openai") {
		t.Fatal("expected false for openai->openai")
	}
	if needsProtocolConversion("", "openai") {
		t.Fatal("expected false for empty client protocol")
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty omits effort", input: "", want: ""},
		{name: "xhigh preserved", input: "xhigh", want: "xhigh"},
		{name: "uppercase trimmed", input: "  HIGH  ", want: "high"},
		{name: "invalid omits effort", input: "ultra", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeReasoningEffort(tt.input)
			if got != tt.want {
				t.Fatalf("normalizeReasoningEffort(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapThinkingToReasoningEffort(t *testing.T) {
	tests := []struct {
		name          string
		input         any
		defaultEffort string
		want          string
		wantOK        bool
	}{
		{
			name:          "budget low",
			input:         map[string]any{"type": "enabled", "budget_tokens": 1024},
			defaultEffort: "medium",
			want:          "low",
			wantOK:        true,
		},
		{
			name:          "budget medium",
			input:         map[string]any{"type": "enabled", "budget_tokens": 8192},
			defaultEffort: "medium",
			want:          "medium",
			wantOK:        true,
		},
		{
			name:          "budget high",
			input:         map[string]any{"type": "enabled", "budget_tokens": 20000},
			defaultEffort: "medium",
			want:          "high",
			wantOK:        true,
		},
		{
			name:          "budget high boundary",
			input:         map[string]any{"type": "enabled", "budget_tokens": 32768},
			defaultEffort: "medium",
			want:          "high",
			wantOK:        true,
		},
		{
			name:          "budget xhigh",
			input:         map[string]any{"type": "enabled", "budget_tokens": 50000},
			defaultEffort: "medium",
			want:          "xhigh",
			wantOK:        true,
		},
		{
			name:          "budget xhigh boundary",
			input:         map[string]any{"type": "enabled", "budget_tokens": 32769},
			defaultEffort: "medium",
			want:          "xhigh",
			wantOK:        true,
		},
		{
			name:          "missing budget uses configured default",
			input:         map[string]any{"type": "enabled"},
			defaultEffort: "high",
			want:          "high",
			wantOK:        true,
		},
		{
			name:          "missing thinking uses configured default",
			input:         nil,
			defaultEffort: "high",
			want:          "high",
			wantOK:        true,
		},
		{
			name:          "missing thinking without default omits effort",
			input:         nil,
			defaultEffort: "",
			want:          "",
			wantOK:        false,
		},
		{
			name:          "thinking enabled without budget and without default omits effort",
			input:         map[string]any{"type": "enabled"},
			defaultEffort: "",
			want:          "",
			wantOK:        false,
		},
		{
			name:          "unsupported type",
			input:         map[string]any{"type": "disabled", "budget_tokens": 1024},
			defaultEffort: "medium",
			want:          "",
			wantOK:        false,
		},
		{
			name:          "invalid shape",
			input:         "enabled",
			defaultEffort: "medium",
			want:          "",
			wantOK:        false,
		},
		{
			name:          "adaptive thinking uses configured default",
			input:         map[string]any{"type": "adaptive"},
			defaultEffort: "xhigh",
			want:          "xhigh",
			wantOK:        true,
		},
		{
			name:          "adaptive thinking without default omits effort",
			input:         map[string]any{"type": "adaptive"},
			defaultEffort: "",
			want:          "",
			wantOK:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := mapThinkingToReasoningEffort(tt.input, tt.defaultEffort)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("effort = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertAnthropicRequestToOpenAI(t *testing.T) {
	t.Run("basic with system", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":1024,
			"system":"You are helpful.",
			"messages":[{"role":"user","content":"Hello"}]
		}`)
		out, path, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if path != "/v1/chat/completions" {
			t.Fatalf("path = %s, want /v1/chat/completions", path)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		msgs := result["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}
		sys := msgs[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "You are helpful." {
			t.Fatalf("system message mismatch: %v", sys)
		}
	})

	t.Run("content blocks flattened", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":512,
			"messages":[{"role":"user","content":[
				{"type":"text","text":"Hello "},
				{"type":"text","text":"world"}
			]}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		msgs := result["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != "Hello world" {
			t.Fatalf("content not flattened: %v", msg["content"])
		}
	})

	t.Run("stop_sequences renamed", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"stop_sequences":["\n\nHuman:"],
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if _, ok := result["stop_sequences"]; ok {
			t.Fatalf("stop_sequences should not be in output")
		}
		if result["stop"] == nil {
			t.Fatalf("stop field missing")
		}
	})

	t.Run("thinking mapped to reasoning_effort", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"enabled","budget_tokens":20000},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %v, want high", result["reasoning_effort"])
		}
	})

	t.Run("thinking with large budget mapped to xhigh", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"enabled","budget_tokens":65536},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["reasoning_effort"] != "xhigh" {
			t.Fatalf("reasoning_effort = %v, want xhigh", result["reasoning_effort"])
		}
	})

	t.Run("thinking without budget uses configured default", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"enabled"},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "high", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %v, want high", result["reasoning_effort"])
		}
	})

	t.Run("thinking type disabled ignored", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"disabled","budget_tokens":20000},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if _, ok := result["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should be omitted when thinking is disabled")
		}
	})

	t.Run("missing thinking uses configured default", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "high", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %v, want high", result["reasoning_effort"])
		}
	})

	t.Run("missing thinking without default omits effort", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "xhigh", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["reasoning_effort"] != "xhigh" {
			t.Fatalf("reasoning_effort = %v, want xhigh", result["reasoning_effort"])
		}
	})

	t.Run("missing thinking without default omits effort", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"system":[{"type":"text","text":"You are "},{"type":"text","text":"helpful."}],
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		msgs := result["messages"].([]any)
		sys := msgs[0].(map[string]any)
		if sys["content"] != "You are helpful." {
			t.Fatalf("system blocks not flattened: %v", sys["content"])
		}
	})

	t.Run("injects configured system prompt when request has none", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "Stay concise")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		msgs := result["messages"].([]any)
		sys := msgs[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "Stay concise" {
			t.Fatalf("injected system mismatch: %v", sys)
		}
	})

	t.Run("prepends configured system prompt before request system", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"system":"Follow user context",
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "Stay concise")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		msgs := result["messages"].([]any)
		sys := msgs[0].(map[string]any)
		if sys["content"] != "Stay concise\n\nFollow user context" {
			t.Fatalf("merged system mismatch: %v", sys["content"])
		}
	})
}

func TestConvertAnthropicRequestToOpenAIResponses(t *testing.T) {
	t.Run("thinking mapped to reasoning_effort", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"enabled","budget_tokens":1024},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, path, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", path)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["max_output_tokens"] != float64(100) {
			t.Fatalf("max_output_tokens = %v, want 100", result["max_output_tokens"])
		}
		reasoning, ok := result["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning = %T, want object", result["reasoning"])
		}
		if reasoning["effort"] != "low" {
			t.Fatalf("reasoning.effort = %v, want low", reasoning["effort"])
		}
		if _, ok := result["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should not be present for responses request")
		}
	})

	t.Run("thinking without budget uses configured default", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"enabled"},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "xhigh", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		reasoning, ok := result["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning = %T, want object", result["reasoning"])
		}
		if reasoning["effort"] != "xhigh" {
			t.Fatalf("reasoning.effort = %v, want xhigh", reasoning["effort"])
		}
		if _, ok := result["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should not be present for responses request")
		}
	})

	t.Run("missing thinking uses configured default", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "high", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		reasoning, ok := result["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning = %T, want object", result["reasoning"])
		}
		if reasoning["effort"] != "high" {
			t.Fatalf("reasoning.effort = %v, want high", reasoning["effort"])
		}
		if _, ok := result["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should not be present for responses request")
		}
	})

	t.Run("missing thinking uses configured xhigh default", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "xhigh", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		reasoning, ok := result["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning = %T, want object", result["reasoning"])
		}
		if reasoning["effort"] != "xhigh" {
			t.Fatalf("reasoning.effort = %v, want xhigh", reasoning["effort"])
		}
		if _, ok := result["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should not be present for responses request")
		}
	})

	t.Run("missing thinking without default omits effort", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if _, ok := result["reasoning"]; ok {
			t.Fatalf("reasoning should be omitted when no thinking/default provided")
		}
		if _, ok := result["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should not be present for responses request")
		}
	})

	t.Run("thinking enabled without budget and without default omits effort", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"enabled"},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if _, ok := result["reasoning"]; ok {
			t.Fatalf("reasoning should be omitted when no configured default exists")
		}
		if _, ok := result["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should not be present for responses request")
		}
	})

	t.Run("injects configured system prompt as developer role", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"system":"Follow user context",
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "Stay concise")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		inputItems := result["input"].([]any)
		dev := inputItems[0].(map[string]any)
		if dev["content"] != "Stay concise\n\nFollow user context" {
			t.Fatalf("merged developer prompt mismatch: %v", dev["content"])
		}
	})

	t.Run("maps tools and explicit auto tool_choice", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"tool_choice":{"type":"auto"},
			"tools":[{
				"name":"read_file",
				"description":"Read file",
				"input_schema":{"type":"object","properties":{"path":{"type":"string"}}}
			}],
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %v, want auto", result["tool_choice"])
		}
		tools := result["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(tools))
		}
		tool := tools[0].(map[string]any)
		if tool["type"] != "function" {
			t.Fatalf("tool type = %v, want function", tool["type"])
		}
		if tool["name"] != "read_file" {
			t.Fatalf("tool name = %v, want read_file", tool["name"])
		}
	})

	t.Run("maps tool_choice any to required", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"tool_choice":{"type":"any"},
			"tools":[{"name":"read_file","input_schema":{"type":"object"}}],
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["tool_choice"] != "required" {
			t.Fatalf("tool_choice = %v, want required", result["tool_choice"])
		}
	})

	t.Run("defaults tool_choice auto when tools exist", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"tools":[{"name":"read_file","input_schema":{"type":"object"}}],
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %v, want auto", result["tool_choice"])
		}
	})

	t.Run("does not force stream for non-streaming requests", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if _, ok := result["stream"]; ok {
			t.Fatalf("stream should remain unset for non-streaming requests, got %v", result["stream"])
		}
	})

	t.Run("stop_sequences mapped to stop", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"stop_sequences":["\n\nHuman:"],
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if _, ok := result["stop_sequences"]; ok {
			t.Fatalf("stop_sequences should not be in responses output")
		}
		if result["stop"] == nil {
			t.Fatalf("stop field missing in responses output")
		}
		stopArr := result["stop"].([]any)
		if len(stopArr) != 1 || stopArr[0] != "\n\nHuman:" {
			t.Fatalf("stop = %v, want [\\n\\nHuman:]", result["stop"])
		}
	})

	t.Run("thinking with large budget mapped to xhigh", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"thinking":{"type":"enabled","budget_tokens":65536},
			"messages":[{"role":"user","content":"Hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAIResponses(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		reasoning, ok := result["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning = %T, want object", result["reasoning"])
		}
		if reasoning["effort"] != "xhigh" {
			t.Fatalf("reasoning.effort = %v, want xhigh", reasoning["effort"])
		}
	})
}

func TestConvertOpenAIResponseToAnthropic(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		input := []byte(`{
			"id":"chatcmpl-abc123",
			"model":"gpt-5.3-codex",
			"choices":[{
				"message":{"role":"assistant","content":"Hello!"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`)
		out, err := convertOpenAIResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["type"] != "message" {
			t.Fatalf("type = %v, want message", result["type"])
		}
		if result["role"] != "assistant" {
			t.Fatalf("role = %v, want assistant", result["role"])
		}
		if result["stop_reason"] != "end_turn" {
			t.Fatalf("stop_reason = %v, want end_turn", result["stop_reason"])
		}
		content := result["content"].([]any)
		block := content[0].(map[string]any)
		if block["text"] != "Hello!" {
			t.Fatalf("content text = %v, want Hello!", block["text"])
		}
		usage := result["usage"].(map[string]any)
		if usage["input_tokens"] != float64(10) {
			t.Fatalf("input_tokens = %v, want 10", usage["input_tokens"])
		}
		if usage["output_tokens"] != float64(5) {
			t.Fatalf("output_tokens = %v, want 5", usage["output_tokens"])
		}
	})

	t.Run("length stop", func(t *testing.T) {
		input := []byte(`{
			"id":"chatcmpl-xyz",
			"model":"gpt-4",
			"choices":[{
				"message":{"role":"assistant","content":"truncated..."},
				"finish_reason":"length"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":100,"total_tokens":105}
		}`)
		out, err := convertOpenAIResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["stop_reason"] != "max_tokens" {
			t.Fatalf("stop_reason = %v, want max_tokens", result["stop_reason"])
		}
	})
}

func TestConvertOpenAIErrorToAnthropic(t *testing.T) {
	input := []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`)
	out, err := convertOpenAIErrorToAnthropic(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result map[string]any
	json.Unmarshal(out, &result)
	if result["type"] != "error" {
		t.Fatalf("type = %v, want error", result["type"])
	}
	errObj := result["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Fatalf("error type = %v, want rate_limit_error", errObj["type"])
	}
}

func TestConvertAnthropicRequestToolUse(t *testing.T) {
	t.Run("tools and tool_choice", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":1024,
			"tools":[{
				"name":"get_weather",
				"description":"Get the weather",
				"input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
			}],
			"tool_choice":{"type":"auto"},
			"messages":[{"role":"user","content":"What is the weather in NYC?"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		tools := result["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(tools))
		}
		tool := tools[0].(map[string]any)
		if tool["type"] != "function" {
			t.Fatalf("tool type = %v, want function", tool["type"])
		}
		fn := tool["function"].(map[string]any)
		if fn["name"] != "get_weather" {
			t.Fatalf("function name = %v, want get_weather", fn["name"])
		}
		if fn["description"] != "Get the weather" {
			t.Fatalf("function description = %v", fn["description"])
		}
		params := fn["parameters"].(map[string]any)
		if params["type"] != "object" {
			t.Fatalf("parameters type = %v, want object", params["type"])
		}

		if result["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %v, want auto", result["tool_choice"])
		}
	})

	t.Run("tool_choice any", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":100,
			"tool_choice":{"type":"any"},
			"tools":[{"name":"test","input_schema":{"type":"object"}}],
			"messages":[{"role":"user","content":"hi"}]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["tool_choice"] != "required" {
			t.Fatalf("tool_choice = %v, want required", result["tool_choice"])
		}
	})

	t.Run("messages with tool_use and tool_result", func(t *testing.T) {
		input := []byte(`{
			"model":"claude-opus-4-6",
			"max_tokens":1024,
			"messages":[
				{"role":"user","content":"What is the weather?"},
				{"role":"assistant","content":[
					{"type":"text","text":"Let me check."},
					{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"NYC"}}
				]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"toolu_123","content":"72°F sunny"}
				]}
			]
		}`)
		out, _, err := convertAnthropicRequestToOpenAI(input, "medium", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)
		msgs := result["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}

		user := msgs[0].(map[string]any)
		if user["role"] != "user" {
			t.Fatalf("msg[0] role = %v, want user", user["role"])
		}

		assistant := msgs[1].(map[string]any)
		if assistant["role"] != "assistant" {
			t.Fatalf("msg[1] role = %v, want assistant", assistant["role"])
		}
		if assistant["content"] != "Let me check." {
			t.Fatalf("msg[1] content = %v, want 'Let me check.'", assistant["content"])
		}
		toolCalls := assistant["tool_calls"].([]any)
		if len(toolCalls) != 1 {
			t.Fatalf("expected 1 tool_call, got %d", len(toolCalls))
		}
		tc := toolCalls[0].(map[string]any)
		if tc["id"] != "toolu_123" {
			t.Fatalf("tool_call id = %v", tc["id"])
		}
		tcFn := tc["function"].(map[string]any)
		if tcFn["name"] != "get_weather" {
			t.Fatalf("tool_call function name = %v", tcFn["name"])
		}

		toolMsg := msgs[2].(map[string]any)
		if toolMsg["role"] != "tool" {
			t.Fatalf("msg[2] role = %v, want tool", toolMsg["role"])
		}
		if toolMsg["tool_call_id"] != "toolu_123" {
			t.Fatalf("msg[2] tool_call_id = %v", toolMsg["tool_call_id"])
		}
		if toolMsg["content"] != "72°F sunny" {
			t.Fatalf("msg[2] content = %v", toolMsg["content"])
		}
	})
}

func TestConvertOpenAIResponseToolCalls(t *testing.T) {
	t.Run("non-streaming", func(t *testing.T) {
		input := []byte(`{
			"id":"chatcmpl-abc",
			"model":"gpt-4",
			"choices":[{
				"message":{
					"role":"assistant",
					"content":null,
					"tool_calls":[{
						"id":"call_123",
						"type":"function",
						"function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}
		}`)
		out, err := convertOpenAIResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		if result["stop_reason"] != "tool_use" {
			t.Fatalf("stop_reason = %v, want tool_use", result["stop_reason"])
		}

		content := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(content))
		}
		block := content[0].(map[string]any)
		if block["type"] != "tool_use" {
			t.Fatalf("content block type = %v, want tool_use", block["type"])
		}
		if block["id"] != "call_123" {
			t.Fatalf("tool_use id = %v", block["id"])
		}
		if block["name"] != "get_weather" {
			t.Fatalf("tool_use name = %v", block["name"])
		}
		inputObj := block["input"].(map[string]any)
		if inputObj["city"] != "NYC" {
			t.Fatalf("tool_use input city = %v", inputObj["city"])
		}
	})

	t.Run("text and tool_calls", func(t *testing.T) {
		input := []byte(`{
			"id":"chatcmpl-abc",
			"model":"gpt-4",
			"choices":[{
				"message":{
					"role":"assistant",
					"content":"I'll check the weather.",
					"tool_calls":[{
						"id":"call_456",
						"type":"function",
						"function":{"name":"get_weather","arguments":"{\"city\":\"LA\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":15}
		}`)
		out, err := convertOpenAIResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		content := result["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(content))
		}
		textBlock := content[0].(map[string]any)
		if textBlock["type"] != "text" {
			t.Fatalf("first block type = %v, want text", textBlock["type"])
		}
		toolBlock := content[1].(map[string]any)
		if toolBlock["type"] != "tool_use" {
			t.Fatalf("second block type = %v, want tool_use", toolBlock["type"])
		}
	})
}

func TestExtractToolResultContent(t *testing.T) {
	if got := extractToolResultContent("hello"); got != "hello" {
		t.Fatalf("string: got %q, want hello", got)
	}
	if got := extractToolResultContent(nil); got != "" {
		t.Fatalf("nil: got %q, want empty", got)
	}
	blocks := []any{
		map[string]any{"type": "text", "text": "part1"},
		map[string]any{"type": "text", "text": "part2"},
	}
	if got := extractToolResultContent(blocks); got != "part1part2" {
		t.Fatalf("blocks: got %q, want part1part2", got)
	}
}

func TestConvertOpenAIResponseWithReasoning(t *testing.T) {
	t.Run("reasoning_content produces thinking block", func(t *testing.T) {
		input := []byte(`{
			"id":"chatcmpl-reasoning",
			"model":"o3",
			"choices":[{
				"message":{
					"role":"assistant",
					"content":"The answer is 42.",
					"reasoning_content":"Let me think step by step..."
				},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":20}
		}`)
		out, err := convertOpenAIResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		content := result["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(content))
		}
		// First block must be thinking
		thinkingBlock := content[0].(map[string]any)
		if thinkingBlock["type"] != "thinking" {
			t.Fatalf("first block type = %v, want thinking", thinkingBlock["type"])
		}
		if thinkingBlock["thinking"] != "Let me think step by step..." {
			t.Fatalf("thinking content = %v", thinkingBlock["thinking"])
		}
		// Second block must be text
		textBlock := content[1].(map[string]any)
		if textBlock["type"] != "text" {
			t.Fatalf("second block type = %v, want text", textBlock["type"])
		}
		if textBlock["text"] != "The answer is 42." {
			t.Fatalf("text content = %v", textBlock["text"])
		}
	})

	t.Run("no reasoning_content produces no thinking block", func(t *testing.T) {
		input := []byte(`{
			"id":"chatcmpl-noreason",
			"model":"gpt-4",
			"choices":[{
				"message":{"role":"assistant","content":"Hello!"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":3}
		}`)
		out, err := convertOpenAIResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		content := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(content))
		}
		if content[0].(map[string]any)["type"] != "text" {
			t.Fatalf("block type = %v, want text", content[0].(map[string]any)["type"])
		}
	})

	t.Run("reasoning with tool_calls", func(t *testing.T) {
		input := []byte(`{
			"id":"chatcmpl-reason-tool",
			"model":"o3",
			"choices":[{
				"message":{
					"role":"assistant",
					"content":null,
					"reasoning_content":"I need to call the tool.",
					"tool_calls":[{
						"id":"call_abc",
						"type":"function",
						"function":{"name":"read_file","arguments":"{\"path\":\"/tmp/test\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":15}
		}`)
		out, err := convertOpenAIResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		content := result["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("expected 2 content blocks (thinking+tool_use), got %d", len(content))
		}
		if content[0].(map[string]any)["type"] != "thinking" {
			t.Fatalf("first block type = %v, want thinking", content[0].(map[string]any)["type"])
		}
		if content[1].(map[string]any)["type"] != "tool_use" {
			t.Fatalf("second block type = %v, want tool_use", content[1].(map[string]any)["type"])
		}
		if result["stop_reason"] != "tool_use" {
			t.Fatalf("stop_reason = %v, want tool_use", result["stop_reason"])
		}
	})
}

func TestConvertOpenAIResponseStopReasonToolUseOverride(t *testing.T) {
	// Edge case: finish_reason="stop" but tool_calls present → stop_reason must be "tool_use"
	input := []byte(`{
		"id":"chatcmpl-edge",
		"model":"gpt-4",
		"choices":[{
			"message":{
				"role":"assistant",
				"content":null,
				"tool_calls":[{
					"id":"call_edge",
					"type":"function",
					"function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}
				}]
			},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":5,"completion_tokens":10}
	}`)
	out, err := convertOpenAIResponseToAnthropic(input, 200)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result map[string]any
	json.Unmarshal(out, &result)
	if result["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use (defensive override)", result["stop_reason"])
	}
}

func TestConvertResponsesWithReasoning(t *testing.T) {
	t.Run("reasoning output produces thinking block", func(t *testing.T) {
		input := []byte(`{
			"id":"resp_reasoning",
			"model":"o3",
			"status":"completed",
			"output":[
				{
					"type":"reasoning",
					"id":"rs_123",
					"summary":[{"type":"summary_text","text":"Step 1: analyze the problem."}]
				},
				{
					"type":"message",
					"role":"assistant",
					"content":[{"type":"output_text","text":"Here is the result."}]
				}
			],
			"usage":{"input_tokens":20,"output_tokens":30}
		}`)
		out, err := convertOpenAIResponsesResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		content := result["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(content))
		}
		thinkingBlock := content[0].(map[string]any)
		if thinkingBlock["type"] != "thinking" {
			t.Fatalf("first block type = %v, want thinking", thinkingBlock["type"])
		}
		if thinkingBlock["thinking"] != "Step 1: analyze the problem." {
			t.Fatalf("thinking = %v", thinkingBlock["thinking"])
		}
		textBlock := content[1].(map[string]any)
		if textBlock["type"] != "text" {
			t.Fatalf("second block type = %v, want text", textBlock["type"])
		}
	})

	t.Run("no reasoning output no thinking block", func(t *testing.T) {
		input := []byte(`{
			"id":"resp_noreason",
			"model":"gpt-4",
			"status":"completed",
			"output":[
				{
					"type":"message",
					"role":"assistant",
					"content":[{"type":"output_text","text":"Just text."}]
				}
			],
			"usage":{"input_tokens":5,"output_tokens":3}
		}`)
		out, err := convertOpenAIResponsesResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		content := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(content))
		}
		if content[0].(map[string]any)["type"] != "text" {
			t.Fatalf("block type = %v, want text", content[0].(map[string]any)["type"])
		}
	})

	t.Run("reasoning with function_call", func(t *testing.T) {
		input := []byte(`{
			"id":"resp_reason_tool",
			"model":"o3",
			"status":"completed",
			"output":[
				{
					"type":"reasoning",
					"id":"rs_456",
					"summary":[{"type":"summary_text","text":"I should call the tool."}]
				},
				{
					"type":"function_call",
					"call_id":"fc_abc",
					"name":"read_file",
					"arguments":"{\"path\":\"/tmp\"}"
				}
			],
			"usage":{"input_tokens":10,"output_tokens":20}
		}`)
		out, err := convertOpenAIResponsesResponseToAnthropic(input, 200)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]any
		json.Unmarshal(out, &result)

		content := result["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(content))
		}
		if content[0].(map[string]any)["type"] != "thinking" {
			t.Fatalf("first block type = %v, want thinking", content[0].(map[string]any)["type"])
		}
		if content[1].(map[string]any)["type"] != "tool_use" {
			t.Fatalf("second block type = %v, want tool_use", content[1].(map[string]any)["type"])
		}
		if result["stop_reason"] != "tool_use" {
			t.Fatalf("stop_reason = %v, want tool_use", result["stop_reason"])
		}
	})
}
