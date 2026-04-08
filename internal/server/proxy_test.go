package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dotnode/gatelm/internal/config"
)

func TestReplaceModelInBody(t *testing.T) {
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	requestModel, rewritten := replaceModelInBody(body, "gpt-4.1-mini")
	if requestModel != "gpt-4o-mini" {
		t.Fatalf("unexpected request model: %s", requestModel)
	}
	if string(rewritten) == string(body) {
		t.Fatalf("expected rewritten body")
	}

	var result map[string]any
	json.Unmarshal(rewritten, &result)
	if result["model"] != "gpt-4.1-mini" {
		t.Fatalf("model not rewritten: %v", result["model"])
	}
}

func TestReplaceModelInBodySameModel(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	requestModel, rewritten := replaceModelInBody(body, "gpt-4")
	if requestModel != "gpt-4" {
		t.Fatalf("unexpected request model: %s", requestModel)
	}
	// Same model, body should be unchanged
	if string(rewritten) != string(body) {
		t.Fatalf("body should not change when model is the same")
	}
}

func TestReplaceModelInBodyEmpty(t *testing.T) {
	requestModel, rewritten := replaceModelInBody(nil, "gpt-4")
	if requestModel != "" {
		t.Fatalf("expected empty request model for nil body")
	}
	if rewritten != nil {
		t.Fatalf("expected nil body returned")
	}
}

func TestDetectStreamRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"stream true", `{"stream":true,"model":"gpt-4"}`, true},
		{"stream false", `{"stream":false,"model":"gpt-4"}`, false},
		{"no stream field", `{"model":"gpt-4","messages":[]}`, false},
		{"empty object", `{}`, false},
		{"empty body", ``, false},
		{"invalid json", `not json`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectStreamRequest([]byte(tt.body))
			if got != tt.want {
				t.Errorf("detectStreamRequest(%s) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestStripStreamField(t *testing.T) {
	t.Run("removes stream and stream_options", func(t *testing.T) {
		input := []byte(`{"model":"gpt-4","stream":true,"stream_options":{"include_usage":true}}`)
		out := stripStreamField(input)
		var result map[string]any
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if _, ok := result["stream"]; ok {
			t.Fatal("stream field should be removed")
		}
		if _, ok := result["stream_options"]; ok {
			t.Fatal("stream_options field should be removed")
		}
		if result["model"] != "gpt-4" {
			t.Fatalf("model should be preserved, got %v", result["model"])
		}
	})

	t.Run("no stream field", func(t *testing.T) {
		input := []byte(`{"model":"gpt-4"}`)
		out := stripStreamField(input)
		var result map[string]any
		json.Unmarshal(out, &result)
		if result["model"] != "gpt-4" {
			t.Fatalf("model should be preserved")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		input := []byte(`not json`)
		out := stripStreamField(input)
		if string(out) != "not json" {
			t.Fatalf("should return original body for invalid json")
		}
	})
}

func TestEnsureResponsesStream(t *testing.T) {
	t.Run("adds stream true when missing", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hi"}]}`)
		out := ensureResponsesStream(input)
		var result map[string]any
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if result["stream"] != true {
			t.Fatalf("stream = %v, want true", result["stream"])
		}
		if result["model"] != "gpt-5" {
			t.Fatalf("model should be preserved, got %v", result["model"])
		}
	})

	t.Run("overrides stream false to true", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5","stream":false,"tool_choice":"auto"}`)
		out := ensureResponsesStream(input)
		var result map[string]any
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if result["stream"] != true {
			t.Fatalf("stream = %v, want true", result["stream"])
		}
		if result["tool_choice"] != "auto" {
			t.Fatalf("tool_choice should be preserved, got %v", result["tool_choice"])
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		input := []byte(`not json`)
		out := ensureResponsesStream(input)
		if string(out) != "not json" {
			t.Fatalf("should return original body for invalid json")
		}
	})
}

func TestEnsureMaxTokens(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		defaultMax    int
		wantMaxTokens float64
		wantInjected  bool
	}{
		{
			name:          "injects when absent",
			body:          `{"model":"gpt-4","messages":[]}`,
			defaultMax:    4096,
			wantMaxTokens: 4096,
			wantInjected:  true,
		},
		{
			name:          "does not override existing max_tokens",
			body:          `{"model":"gpt-4","max_tokens":1024,"messages":[]}`,
			defaultMax:    4096,
			wantMaxTokens: 1024,
			wantInjected:  true,
		},
		{
			name:         "does not inject when max_completion_tokens present",
			body:         `{"model":"gpt-4","max_completion_tokens":2048,"messages":[]}`,
			defaultMax:   4096,
			wantInjected: false,
		},
		{
			name:         "zero default does nothing",
			body:         `{"model":"gpt-4","messages":[]}`,
			defaultMax:   0,
			wantInjected: false,
		},
		{
			name:         "negative default does nothing",
			body:         `{"model":"gpt-4","messages":[]}`,
			defaultMax:   -1,
			wantInjected: false,
		},
		{
			name:         "empty body returns unchanged",
			body:         ``,
			defaultMax:   4096,
			wantInjected: false,
		},
		{
			name:         "invalid json returns unchanged",
			body:         `not json`,
			defaultMax:   4096,
			wantInjected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ensureMaxTokens([]byte(tt.body), tt.defaultMax, "openai")
			if tt.body == "" || tt.body == "not json" {
				if string(result) != tt.body {
					t.Fatalf("expected unchanged body, got %s", result)
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(result, &m); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			v, ok := m["max_tokens"].(float64)
			if tt.wantInjected {
				if !ok {
					t.Fatalf("expected max_tokens in result")
				}
				if v != tt.wantMaxTokens {
					t.Fatalf("max_tokens = %v, want %v", v, tt.wantMaxTokens)
				}
			} else if !ok {
				// max_tokens not present, expected
			}
		})
	}
}

func TestNormalizeReasoningEffortAliasInBody(t *testing.T) {
	t.Run("chat completions", func(t *testing.T) {
		tests := []struct {
			name       string
			body       string
			wantEffort string
			unchanged  bool
		}{
			{
				name:       "rewrites xhigh to high",
				body:       `{"model":"gpt-5","reasoning_effort":"xhigh","messages":[]}`,
				wantEffort: "high",
			},
			{
				name:       "rewrites uppercase xhigh to high",
				body:       `{"model":"gpt-5","reasoning_effort":"XHIGH","messages":[]}`,
				wantEffort: "high",
			},
			{
				name:       "preserves valid high",
				body:       `{"model":"gpt-5","reasoning_effort":"high","messages":[]}`,
				wantEffort: "high",
				unchanged:  true,
			},
			{
				name:      "ignores missing field",
				body:      `{"model":"gpt-5","messages":[]}`,
				unchanged: true,
			},
			{
				name:      "invalid json returns unchanged",
				body:      `not json`,
				unchanged: true,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				result := normalizeReasoningEffortAliasInBody([]byte(tt.body), "openai")
				if tt.unchanged && string(result) == tt.body {
					return
				}
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					t.Fatalf("unmarshal failed: %v", err)
				}
				if got, _ := m["reasoning_effort"].(string); got != tt.wantEffort {
					t.Fatalf("reasoning_effort = %q, want %q", got, tt.wantEffort)
				}
			})
		}
	})

	t.Run("responses", func(t *testing.T) {
		result := normalizeReasoningEffortAliasInBody([]byte(`{"model":"gpt-5","reasoning":{"effort":"xhigh"},"input":[]}`), "openai-responses")
		var m map[string]any
		if err := json.Unmarshal(result, &m); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		reasoning, ok := m["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning = %T, want object", m["reasoning"])
		}
		if got, _ := reasoning["effort"].(string); got != "high" {
			t.Fatalf("reasoning.effort = %q, want high", got)
		}
		if _, ok := m["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should not be present for responses payload")
		}
	})
}

func TestApplyBackendAuthHeaders(t *testing.T) {
	t.Run("openai injects bearer", func(t *testing.T) {
		h := http.Header{}
		applyBackendHeaders(h, &config.Backend{Protocol: "openai", APIKey: "sk-openai"})
		if got := h.Get("Authorization"); got != "Bearer sk-openai" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer sk-openai")
		}
	})

	t.Run("openai responses injects bearer", func(t *testing.T) {
		h := http.Header{}
		applyBackendHeaders(h, &config.Backend{Protocol: "openai-responses", APIKey: "sk-responses"})
		if got := h.Get("Authorization"); got != "Bearer sk-responses" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer sk-responses")
		}
	})

	t.Run("anthropic injects api key and default version", func(t *testing.T) {
		h := http.Header{}
		applyBackendHeaders(h, &config.Backend{Protocol: "anthropic", APIKey: "sk-ant"})
		if got := h.Get("x-api-key"); got != "sk-ant" {
			t.Fatalf("x-api-key = %q, want %q", got, "sk-ant")
		}
		if got := h.Get("anthropic-version"); got != defaultAnthropicVersion {
			t.Fatalf("anthropic-version = %q, want %q", got, defaultAnthropicVersion)
		}
	})

	t.Run("anthropic uses custom version", func(t *testing.T) {
		h := http.Header{}
		applyBackendHeaders(h, &config.Backend{Protocol: "anthropic", APIKey: "sk-ant", AnthropicVersion: "2024-10-22"})
		if got := h.Get("anthropic-version"); got != "2024-10-22" {
			t.Fatalf("anthropic-version = %q, want %q", got, "2024-10-22")
		}
	})

	t.Run("explicit headers override generated auth", func(t *testing.T) {
		h := http.Header{}
		applyBackendHeaders(h, &config.Backend{Protocol: "openai", APIKey: "sk-openai", Headers: map[string]string{"Authorization": "Bearer custom"}})
		if got := h.Get("Authorization"); got != "Bearer custom" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer custom")
		}
	})
}

func TestEnsureTemperature(t *testing.T) {
	temp := func(v float64) *float64 { return &v }

	tests := []struct {
		name            string
		body            string
		defaultTemp     *float64
		wantTemperature float64
		wantInjected    bool
	}{
		{
			name:            "injects when absent",
			body:            `{"model":"gpt-4","messages":[]}`,
			defaultTemp:     temp(0.6),
			wantTemperature: 0.6,
			wantInjected:    true,
		},
		{
			name:            "does not override existing temperature",
			body:            `{"model":"gpt-4","temperature":1.0,"messages":[]}`,
			defaultTemp:     temp(0.6),
			wantTemperature: 1.0,
			wantInjected:    true,
		},
		{
			name:            "injects zero temperature",
			body:            `{"model":"gpt-4","messages":[]}`,
			defaultTemp:     temp(0.0),
			wantTemperature: 0.0,
			wantInjected:    true,
		},
		{
			name:         "nil default does nothing",
			body:         `{"model":"gpt-4","messages":[]}`,
			defaultTemp:  nil,
			wantInjected: false,
		},
		{
			name:         "empty body returns unchanged",
			body:         ``,
			defaultTemp:  temp(0.6),
			wantInjected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ensureTemperature([]byte(tt.body), tt.defaultTemp)
			if tt.body == "" {
				if string(result) != tt.body {
					t.Fatalf("expected unchanged body, got %s", result)
				}
				return
			}
			if !tt.wantInjected {
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					return
				}
				if _, ok := m["temperature"]; ok {
					t.Fatalf("temperature should not be injected when default is nil")
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(result, &m); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			v, ok := m["temperature"].(float64)
			if !ok {
				t.Fatalf("expected temperature in result, got body: %s", result)
			}
			if v != tt.wantTemperature {
				t.Fatalf("temperature = %v, want %v", v, tt.wantTemperature)
			}
		})
	}
}
