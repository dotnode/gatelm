package server

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/dotnode/gatelm/internal/config"
)

func detectProtocolByRequest(requestPath string, headers http.Header) string {
	if isAnthropicPath(requestPath) {
		return "anthropic"
	}
	if isOpenAIResponsesPath(requestPath) {
		return "openai-responses"
	}
	if isOpenAIPath(requestPath) {
		return "openai"
	}
	if strings.TrimSpace(headers.Get("anthropic-version")) != "" {
		return "anthropic"
	}
	return ""
}

func isOpenAIResponsesPath(p string) bool {
	return strings.HasPrefix(p, "/v1/responses")
}

func isOpenAIPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "/v1/chat/completions"):
		return true
	case strings.HasPrefix(p, "/v1/completions"):
		return true
	case strings.HasPrefix(p, "/v1/responses"):
		return true
	case strings.HasPrefix(p, "/v1/embeddings"):
		return true
	case strings.HasPrefix(p, "/v1/images"):
		return true
	case strings.HasPrefix(p, "/v1/audio/transcriptions"):
		return true
	case strings.HasPrefix(p, "/v1/audio/translations"):
		return true
	case strings.HasPrefix(p, "/v1/audio/speech"):
		return true
	case strings.HasPrefix(p, "/v1/moderations"):
		return true
	case strings.HasPrefix(p, "/v1/models"):
		return true
	default:
		return false
	}
}

func isAnthropicPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "/v1/messages"):
		return true
	case strings.HasPrefix(p, "/v1/complete"):
		return true
	default:
		return false
	}
}

func composeTargetURL(backend *config.Backend, incoming *url.URL) (string, error) {
	base, err := url.Parse(backend.URL)
	if err != nil {
		return "", err
	}
	pathPart := incoming.Path
	if backend.StripPrefix && strings.HasPrefix(pathPart, backend.PathPrefix) {
		pathPart = strings.TrimPrefix(pathPart, backend.PathPrefix)
		if pathPart == "" {
			pathPart = "/"
		}
	}
	base.Path = joinURLPath(base.Path, pathPart)
	base.RawQuery = incoming.RawQuery
	return base.String(), nil
}

func joinURLPath(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if strings.HasSuffix(a, "/") && strings.HasPrefix(b, "/") {
		return a + strings.TrimPrefix(b, "/")
	}
	if strings.HasSuffix(a, "/") || strings.HasPrefix(b, "/") {
		return a + b
	}
	return path.Clean(a + "/" + b)
}

func isAnthropicClient(h http.Header) bool {
	if strings.TrimSpace(h.Get("anthropic-version")) != "" {
		return true
	}
	if strings.TrimSpace(h.Get("x-api-key")) != "" {
		auth := strings.TrimSpace(h.Get("Authorization"))
		if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return true
		}
	}
	return false
}
