package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadConfigWithoutReasoningSection(t *testing.T) {
	path := writeTempConfig(t, `
backends:
  - name: b1
    url: "http://127.0.0.1:8080"
    protocol: "openai"
    models:
      - name: gpt-4o
        aliases: [claude-sonnet-4]
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
}

func TestLoadConfigModelDefaults(t *testing.T) {
	path := writeTempConfig(t, `
model_defaults:
  gpt-5.4:
    reasoning_effort: xhigh
    max_tokens: 3276
  openai/gpt-oss-120b:
    reasoning_effort: medium
    normalize_xhigh_reasoning_effort: true
backends:
  - name: b1
    url: "http://127.0.0.1:8080"
    protocol: "openai"
    models:
      - name: gpt-5.4
        aliases: [claude-opus-4-6]
      - name: openai/gpt-oss-120b
        aliases: [claude-haiku-4-5]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := cfg.ModelDefaults["gpt-5.4"].ReasoningEffort; got != "xhigh" {
		t.Fatalf("model_defaults[gpt-5.4].reasoning_effort = %q, want xhigh", got)
	}
	if got := cfg.ModelDefaults["gpt-5.4"].MaxTokens; got != 3276 {
		t.Fatalf("model_defaults[gpt-5.4].max_tokens = %d, want 3276", got)
	}
	if got := cfg.ModelDefaults["openai/gpt-oss-120b"].ReasoningEffort; got != "medium" {
		t.Fatalf("model_defaults[openai/gpt-oss-120b].reasoning_effort = %q, want medium", got)
	}
	if !cfg.ModelDefaults["openai/gpt-oss-120b"].NormalizeXHighReasoningEffort {
		t.Fatal("expected model_defaults[openai/gpt-oss-120b].normalize_xhigh_reasoning_effort = true")
	}
}

func TestLoadConfigModelDefaultsReasoningEffortInvalid(t *testing.T) {
	path := writeTempConfig(t, `
model_defaults:
  gpt-5.4:
    reasoning_effort: ultra
backends:
  - name: b1
    url: "http://127.0.0.1:8080"
    protocol: "openai"
    models:
      - name: gpt-5.4
        aliases: [claude-opus-4-6]
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid model_defaults[\"gpt-5.4\"].reasoning_effort") {
		t.Fatalf("error = %v, want contains invalid model_defaults[\"gpt-5.4\"].reasoning_effort", err)
	}
}

func TestLoadConfigBackendAuthFields(t *testing.T) {
	path := writeTempConfig(t, `
backends:
  - name: openai
    url: "http://127.0.0.1:8080"
    protocol: "openai"
    api_key: "  sk-openai  "
    models:
      - name: gpt-4o
  - name: anthropic
    url: "http://127.0.0.1:8081"
    protocol: "anthropic"
    api_key: " sk-ant "
    anthropic_version: " 2024-10-22 "
    models:
      - name: claude-3-5-sonnet
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := cfg.Backends[0].APIKey; got != "sk-openai" {
		t.Fatalf("backend[0].api_key = %q, want sk-openai", got)
	}
	if got := cfg.Backends[1].APIKey; got != "sk-ant" {
		t.Fatalf("backend[1].api_key = %q, want sk-ant", got)
	}
	if got := cfg.Backends[1].AnthropicVersion; got != "2024-10-22" {
		t.Fatalf("backend[1].anthropic_version = %q, want 2024-10-22", got)
	}
}

func TestLoadConfigModelSystemPromptTrim(t *testing.T) {
	path := writeTempConfig(t, `
backends:
  - name: b1
    url: "http://127.0.0.1:8080"
    protocol: "openai"
    models:
      - name: gpt-4o
        system_prompt: "  model style  "
        aliases: [claude-sonnet-4]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Backends[0].Models[0].SystemPrompt != "model style" {
		t.Fatalf("model.system_prompt = %q, want %q", cfg.Backends[0].Models[0].SystemPrompt, "model style")
	}
}
