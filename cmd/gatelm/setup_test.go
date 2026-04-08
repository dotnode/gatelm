package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dotnode/gatelm/pkg/gatelm"
)

func TestDefaultAnthropicVersion(t *testing.T) {
	if got := defaultAnthropicVersion("anthropic"); got != "2023-06-01" {
		t.Fatalf("defaultAnthropicVersion(anthropic) = %q, want 2023-06-01", got)
	}
	if got := defaultAnthropicVersion("openai"); got != "" {
		t.Fatalf("defaultAnthropicVersion(openai) = %q, want empty", got)
	}
}

func TestWriteConfigFileUsesAPIKeyFields(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	cfg := testConfig()
	if err := writeConfigFile(path, &cfg); err != nil {
		t.Fatalf("writeConfigFile() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "api_key: sk-test") {
		t.Fatalf("expected api_key in generated config, got:\n%s", text)
	}
	if strings.Contains(text, "Authorization:") || strings.Contains(text, "x-api-key:") {
		t.Fatalf("expected no auth headers in generated config, got:\n%s", text)
	}
}

func testConfig() gatelm.Config {
	return gatelm.Config{
		Listen: ":18765",
		Backends: []gatelm.Backend{{
			Name:             "default",
			URL:              "https://api.openai.com",
			Protocol:         "anthropic",
			APIKey:           "sk-test",
			AnthropicVersion: defaultAnthropicVersion("anthropic"),
			Default:          true,
			Weight:           1,
		}},
	}
}
