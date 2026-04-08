package server

import (
	"net/http"
	"strings"

	"github.com/dotnode/gatelm/internal/config"
)

const defaultAnthropicVersion = "2023-06-01"

func applyBackendHeaders(dst http.Header, backend *config.Backend) {
	if dst == nil || backend == nil {
		return
	}
	applyBackendAuthHeaders(dst, backend)
	for k, v := range backend.Headers {
		dst.Set(k, v)
	}
}

func applyBackendAuthHeaders(dst http.Header, backend *config.Backend) {
	if dst == nil || backend == nil {
		return
	}
	apiKey := strings.TrimSpace(backend.APIKey)
	switch backend.Protocol {
	case "openai", "openai-responses":
		if apiKey != "" {
			dst.Set("Authorization", "Bearer "+apiKey)
		}
	case "anthropic":
		if apiKey != "" {
			dst.Set("x-api-key", apiKey)
		}
		version := strings.TrimSpace(backend.AnthropicVersion)
		if version == "" {
			version = defaultAnthropicVersion
		}
		dst.Set("anthropic-version", version)
	}
}
