package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/dotnode/gatelm/internal/config"
)

func TestJoinURLPathPreservesPathSemantics(t *testing.T) {
	got := joinURLPath("/proxy", "v1//chat/../completions")
	want := "/proxy/v1//chat/../completions"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestComposeTargetURL(t *testing.T) {
	backend := &config.Backend{
		Name:        "test",
		URL:         "https://api.openai.com",
		PathPrefix:  "/openai",
		StripPrefix: true,
	}
	in := &url.URL{Path: "/openai/v1/chat/completions", RawQuery: "a=1"}
	got, err := composeTargetURL(backend, in)
	if err != nil {
		t.Fatalf("composeTargetURL err: %v", err)
	}
	want := "https://api.openai.com/v1/chat/completions?a=1"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestPathPrefixMatches(t *testing.T) {
	tests := []struct {
		path   string
		prefix string
		want   bool
	}{
		{path: "/openai", prefix: "/openai", want: true},
		{path: "/openai/v1/chat/completions", prefix: "/openai", want: true},
		{path: "/openai2", prefix: "/openai", want: false},
		{path: "/openai-backend", prefix: "/openai", want: false},
		{path: "/other/path", prefix: "/", want: true},
	}
	for _, tt := range tests {
		if got := pathPrefixMatches(tt.path, tt.prefix); got != tt.want {
			t.Fatalf("pathPrefixMatches(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestTrimPathPrefix(t *testing.T) {
	tests := []struct {
		path   string
		prefix string
		want   string
	}{
		{path: "/openai", prefix: "/openai", want: "/"},
		{path: "/openai/v1/chat/completions", prefix: "/openai", want: "/v1/chat/completions"},
		{path: "/openai2/v1/chat/completions", prefix: "/openai", want: "/openai2/v1/chat/completions"},
		{path: "/other/path", prefix: "/", want: "/other/path"},
	}
	for _, tt := range tests {
		if got := trimPathPrefix(tt.path, tt.prefix); got != tt.want {
			t.Fatalf("trimPathPrefix(%q, %q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestModelIndexResolve(t *testing.T) {
	backends := []config.Backend{
		{
			Name:     "backend-a",
			URL:      "http://localhost:8001",
			Protocol: "openai",
			Models: []config.Model{
				{Name: "gpt-4", Aliases: []string{"my-gpt4", "fast-model"}},
				{Name: "gpt-3.5", Aliases: []string{"cheap-model"}},
			},
		},
		{
			Name:     "backend-b",
			URL:      "http://localhost:8002",
			Protocol: "anthropic",
			Models: []config.Model{
				{Name: "claude-3", Aliases: []string{"my-claude"}},
			},
		},
	}

	idx := BuildModelIndex(backends)

	// Resolve by alias
	b, model, found := idx.Resolve("my-gpt4")
	if !found || b.Name != "backend-a" || model != "gpt-4" {
		t.Fatalf("resolve my-gpt4: found=%v backend=%v model=%v", found, b, model)
	}

	// Resolve by model name directly
	b, model, found = idx.Resolve("gpt-4")
	if !found || b.Name != "backend-a" || model != "gpt-4" {
		t.Fatalf("resolve gpt-4: found=%v backend=%v model=%v", found, b, model)
	}

	// Resolve other backend
	b, model, found = idx.Resolve("my-claude")
	if !found || b.Name != "backend-b" || model != "claude-3" {
		t.Fatalf("resolve my-claude: found=%v backend=%v model=%v", found, b, model)
	}

	// Unknown model
	_, _, found = idx.Resolve("unknown-model")
	if found {
		t.Fatal("expected not found for unknown model")
	}
}

func TestModelIndexDefaultBackend(t *testing.T) {
	t.Run("single backend is default", func(t *testing.T) {
		backends := []config.Backend{
			{Name: "only", URL: "http://localhost:8001", Protocol: "openai"},
		}
		idx := BuildModelIndex(backends)
		def := idx.DefaultBackend()
		if def == nil || def.Name != "only" {
			t.Fatalf("expected single backend as default, got %v", def)
		}
	})

	t.Run("explicit default", func(t *testing.T) {
		backends := []config.Backend{
			{Name: "a", URL: "http://localhost:8001", Protocol: "openai"},
			{Name: "b", URL: "http://localhost:8002", Protocol: "openai", Default: true},
		}
		idx := BuildModelIndex(backends)
		def := idx.DefaultBackend()
		if def == nil || def.Name != "b" {
			t.Fatalf("expected 'b' as default, got %v", def)
		}
	})

	t.Run("no default with multiple backends", func(t *testing.T) {
		backends := []config.Backend{
			{Name: "a", URL: "http://localhost:8001", Protocol: "openai"},
			{Name: "b", URL: "http://localhost:8002", Protocol: "openai"},
		}
		idx := BuildModelIndex(backends)
		def := idx.DefaultBackend()
		if def != nil {
			t.Fatalf("expected no default, got %v", def.Name)
		}
	})
}

func TestModelIndexMatchByPrefix(t *testing.T) {
	backends := []config.Backend{
		{Name: "root", URL: "http://localhost:8001", Protocol: "openai", PathPrefix: "/"},
		{Name: "openai-prefixed", URL: "http://localhost:8002", Protocol: "openai", PathPrefix: "/openai"},
	}
	idx := BuildModelIndex(backends)

	// Longest prefix match
	b, ok := idx.MatchByPrefix("/openai/v1/chat/completions")
	if !ok || b.Name != "openai-prefixed" {
		t.Fatalf("expected openai-prefixed, got ok=%v name=%v", ok, b)
	}

	// Similar prefix must not match
	b, ok = idx.MatchByPrefix("/openai2/v1/chat/completions")
	if !ok || b.Name != "root" {
		t.Fatalf("expected root for similar prefix, got ok=%v name=%v", ok, b)
	}

	// Shorter prefix
	b, ok = idx.MatchByPrefix("/other/path")
	if !ok || b.Name != "root" {
		t.Fatalf("expected root, got ok=%v name=%v", ok, b)
	}
}

func TestDetectProtocolByRequest(t *testing.T) {
	// OpenAI path
	if got := detectProtocolByRequest("/v1/chat/completions", http.Header{}); got != "openai" {
		t.Fatalf("expected openai, got %s", got)
	}

	// Anthropic path
	if got := detectProtocolByRequest("/v1/messages", http.Header{}); got != "anthropic" {
		t.Fatalf("expected anthropic, got %s", got)
	}

	// Anthropic header
	h := http.Header{}
	h.Set("anthropic-version", "2023-06-01")
	if got := detectProtocolByRequest("/custom/path", h); got != "anthropic" {
		t.Fatalf("expected anthropic by header, got %s", got)
	}

	// Unknown
	if got := detectProtocolByRequest("/unknown", http.Header{}); got != "" {
		t.Fatalf("expected empty, got %s", got)
	}
}

func TestIsAnthropicClient(t *testing.T) {
	if !isAnthropicClient("/v1/messages", http.Header{}) {
		t.Fatal("expected anthropic client by path")
	}
	h := http.Header{}
	h.Set("anthropic-version", "2023-06-01")
	if !isAnthropicClient("/v1/models", h) {
		t.Fatal("expected anthropic client by anthropic-version header")
	}
	h = http.Header{}
	h.Set("x-api-key", "sk-test")
	if isAnthropicClient("/v1/models", h) {
		t.Fatal("expected x-api-key only not to imply anthropic client")
	}
}

func TestResolveCandidatesMultiBackend(t *testing.T) {
	backends := []config.Backend{
		{
			Name:     "secondary",
			URL:      "http://localhost:8002",
			Protocol: "openai",
			Priority: 2,
			Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
		},
		{
			Name:     "primary",
			URL:      "http://localhost:8001",
			Protocol: "openai",
			Priority: 1,
			Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
		},
	}

	idx := BuildModelIndex(backends)

	// ResolveCandidates should return both, sorted by priority
	candidates, found := idx.ResolveCandidates("claude-sonnet")
	if !found {
		t.Fatal("expected candidates for claude-sonnet")
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].backend.Name != "primary" {
		t.Fatalf("expected primary first, got %s", candidates[0].backend.Name)
	}
	if candidates[1].backend.Name != "secondary" {
		t.Fatalf("expected secondary second, got %s", candidates[1].backend.Name)
	}

	// Resolve returns the highest priority
	b, model, found := idx.Resolve("claude-sonnet")
	if !found || b.Name != "primary" || model != "gpt-4o" {
		t.Fatalf("Resolve should return primary, got %v %s", b.Name, model)
	}

	// Also works for model name
	candidates, found = idx.ResolveCandidates("gpt-4o")
	if !found || len(candidates) != 2 {
		t.Fatalf("expected 2 candidates for gpt-4o, got %d found=%v", len(candidates), found)
	}
}

func TestResolveWithinBackendModelReasoningEffort(t *testing.T) {
	backends := []config.Backend{
		{
			Name:     "primary",
			URL:      "http://localhost:8001",
			Protocol: "openai",
			Priority: 1,
			Models: []config.Model{{
				Name:                   "gpt-4o",
				Aliases:                []string{"claude-sonnet"},
				ReasoningDefaultEffort: "high",
			}},
		},
		{
			Name:     "secondary",
			URL:      "http://localhost:8002",
			Protocol: "openai",
			Priority: 2,
			Models: []config.Model{{
				Name:                   "gpt-4o",
				Aliases:                []string{"claude-sonnet"},
				ReasoningDefaultEffort: "low",
			}},
		},
	}

	idx := BuildModelIndex(backends)
	entries, found := idx.ResolveWithinBackend("claude-sonnet", &backends[0])
	if !found || len(entries) != 1 {
		t.Fatalf("expected one matched entry, got found=%v len=%d", found, len(entries))
	}
	if entries[0].reasoningDefaultEffort != "high" {
		t.Fatalf("reasoningDefaultEffort = %q, want high", entries[0].reasoningDefaultEffort)
	}
}

func TestResolveWithinBackendModelSystemPrompt(t *testing.T) {
	backends := []config.Backend{
		{
			Name:     "primary",
			URL:      "http://localhost:8001",
			Protocol: "openai",
			Priority: 1,
			Models: []config.Model{{
				Name:         "gpt-4o",
				Aliases:      []string{"claude-sonnet"},
				SystemPrompt: "model style",
			}},
		},
	}

	idx := BuildModelIndex(backends)
	entries, found := idx.ResolveWithinBackend("claude-sonnet", &backends[0])
	if !found || len(entries) != 1 {
		t.Fatalf("expected one matched entry, got found=%v len=%d", found, len(entries))
	}
	if entries[0].systemPrompt != "model style" {
		t.Fatalf("systemPrompt = %q, want model style", entries[0].systemPrompt)
	}
}

func TestResolveCandidatesSingleBackend(t *testing.T) {
	backends := []config.Backend{
		{
			Name:     "only",
			URL:      "http://localhost:8001",
			Protocol: "openai",
			Models:   []config.Model{{Name: "gpt-4", Aliases: []string{"my-model"}}},
		},
	}

	idx := BuildModelIndex(backends)

	candidates, found := idx.ResolveCandidates("my-model")
	if !found || len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d found=%v", len(candidates), found)
	}
	if candidates[0].backend.Name != "only" {
		t.Fatalf("expected 'only', got %s", candidates[0].backend.Name)
	}
}

func TestModelIndexDefaultMaxTokens(t *testing.T) {
	backends := []config.Backend{{
		Name:     "test",
		URL:      "http://localhost:8001",
		Protocol: "openai",
		Models: []config.Model{
			{Name: "gpt-4", Aliases: []string{"my-model"}, DefaultMaxTokens: 8192},
		},
	}}
	idx := BuildModelIndex(backends)
	candidates, found := idx.ResolveCandidates("my-model")
	if !found || len(candidates) == 0 {
		t.Fatal("expected candidates")
	}
	if candidates[0].defaultMaxTokens != 8192 {
		t.Fatalf("defaultMaxTokens = %d, want 8192", candidates[0].defaultMaxTokens)
	}

	// Also verify via ResolveWithinBackend
	entries, found := idx.ResolveWithinBackend("my-model", &backends[0])
	if !found || len(entries) == 0 {
		t.Fatal("expected entries")
	}
	if entries[0].defaultMaxTokens != 8192 {
		t.Fatalf("ResolveWithinBackend defaultMaxTokens = %d, want 8192", entries[0].defaultMaxTokens)
	}
}

func TestModelIndexSkipsDisabledBackends(t *testing.T) {
	disabled := false
	backends := []config.Backend{
		{
			Name:     "disabled-primary",
			URL:      "http://localhost:8001",
			Protocol: "openai",
			Default:  true,
			Enabled:  &disabled,
			Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"shared-model"}}},
		},
		{
			Name:       "disabled-prefix",
			URL:        "http://localhost:8002",
			Protocol:   "openai",
			PathPrefix: "/openai",
			Enabled:    &disabled,
			Models:     []config.Model{{Name: "gpt-4o"}},
		},
		{
			Name:     "enabled-secondary",
			URL:      "http://localhost:8003",
			Protocol: "openai",
			Priority: 2,
			Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"shared-model"}}},
		},
	}

	idx := BuildModelIndex(backends)

	candidates, found := idx.ResolveCandidates("shared-model")
	if !found || len(candidates) != 1 {
		t.Fatalf("expected one enabled candidate, got found=%v len=%d", found, len(candidates))
	}
	if candidates[0].backend.Name != "enabled-secondary" {
		t.Fatalf("expected enabled-secondary, got %s", candidates[0].backend.Name)
	}
	if def := idx.DefaultBackend(); def == nil || def.Name != "enabled-secondary" {
		t.Fatalf("expected single enabled backend to become default, got %v", def)
	}
	if matched, ok := idx.MatchByPrefix("/openai/v1/chat/completions"); ok {
		t.Fatalf("expected disabled prefix backend to be ignored, got %s", matched.Name)
	}
}

func TestModelIndexSingleEnabledBackendBecomesDefault(t *testing.T) {
	disabled := false
	backends := []config.Backend{
		{Name: "disabled", URL: "http://localhost:8001", Protocol: "openai", Enabled: &disabled},
		{Name: "enabled", URL: "http://localhost:8002", Protocol: "openai"},
	}

	idx := BuildModelIndex(backends)
	def := idx.DefaultBackend()
	if def == nil || def.Name != "enabled" {
		t.Fatalf("expected enabled backend as default, got %v", def)
	}
}
