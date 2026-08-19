package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen                string                        `yaml:"listen"`
	Debug                 bool                          `yaml:"debug"`
	MaxConcurrentRequests int                           `yaml:"max_concurrent_requests"` // 0 = unlimited
	Backends              []Backend                     `yaml:"backends"`
	TokenLog              TokenLogConfig                `yaml:"token_log"`
	APIKeys               map[string]string             `yaml:"api_keys"`
	ModelDefaults         map[string]ModelDefaultConfig `yaml:"model_defaults"`
	CircuitBreaker        CircuitBreakerConfig          `yaml:"circuit_breaker"`
	Console               ConsoleConfig                 `yaml:"console"`
	TrustedProxies        []string                      `yaml:"trusted_proxies"` // IPs or CIDRs; empty = only use RemoteAddr
}

type TokenLogConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	File          string `yaml:"file" json:"file"`
	RetentionDays int    `yaml:"retention_days" json:"retention_days"` // 0 = no cleanup
}

type ModelDefaultConfig struct {
	ReasoningEffort               string   `yaml:"reasoning_effort" json:"reasoning_effort"`
	MaxTokens                     int      `yaml:"max_tokens" json:"max_tokens"`
	SystemPrompt                  string   `yaml:"system_prompt" json:"system_prompt"`
	NormalizeXHighReasoningEffort bool     `yaml:"normalize_xhigh_reasoning_effort" json:"normalize_xhigh_reasoning_effort"`
	DefaultTemperature            *float64 `yaml:"default_temperature,omitempty" json:"default_temperature,omitempty"`
}

type CircuitBreakerConfig struct {
	FailThreshold       int    `yaml:"fail_threshold" json:"fail_threshold"`
	RecoveryTimeout     string `yaml:"recovery_timeout" json:"recovery_timeout"`
	HalfOpenMaxRequests int    `yaml:"half_open_max_requests" json:"half_open_max_requests"`
}

type ConsoleConfig struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Password string `yaml:"password" json:"password"`
	// RawPassword holds the pre-expansion password (e.g. "${CONSOLE_PASSWORD}")
	// as written in the YAML file, before NormalizeAndValidate expands env vars
	// into Password. It's kept out of YAML/JSON so it never round-trips through
	// the console UI; SaveAndReloadConfig uses it to avoid baking the expanded
	// plaintext password back onto disk when the admin didn't intend to change it.
	RawPassword string `yaml:"-" json:"-"`
}

type Backend struct {
	Name             string             `yaml:"name" json:"name"`
	Enabled          *bool              `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	URL              string             `yaml:"url" json:"url"`
	Protocol         string             `yaml:"protocol" json:"protocol"`
	APIKey           string             `yaml:"api_key" json:"api_key"`
	AnthropicVersion string             `yaml:"anthropic_version" json:"anthropic_version"`
	Headers          map[string]string  `yaml:"headers" json:"headers"`
	Models           []Model            `yaml:"models" json:"models"`
	PathPrefix       string             `yaml:"path_prefix" json:"path_prefix"`
	StripPrefix      bool               `yaml:"strip_prefix" json:"strip_prefix"`
	Default          bool               `yaml:"default" json:"default"`
	Priority         int                `yaml:"priority" json:"priority"`
	Weight           int                `yaml:"weight" json:"weight"`
	Timeout          string             `yaml:"timeout" json:"timeout"`   // per-backend request timeout, e.g. "120s"; empty = use global default
	SSEOnly          bool               `yaml:"sse_only" json:"sse_only"` // when true, WS clients use HTTP SSE directly (skip WS dial)
	HealthCheck      *HealthCheckConfig `yaml:"health_check" json:"health_check"`
}

func BackendEnabled(b *Backend) bool {
	return b != nil && (b.Enabled == nil || *b.Enabled)
}

type HealthCheckConfig struct {
	Path     string `yaml:"path" json:"path"`
	Interval string `yaml:"interval" json:"interval"`
}

type Model struct {
	Name                          string   `yaml:"name" json:"name"`
	Aliases                       []string `yaml:"aliases" json:"aliases"`
	ReasoningDefaultEffort        string   `yaml:"reasoning_default_effort" json:"reasoning_default_effort"`
	SystemPrompt                  string   `yaml:"system_prompt" json:"system_prompt"`
	DefaultMaxTokens              int      `yaml:"default_max_tokens" json:"default_max_tokens"`
	NormalizeXHighReasoningEffort bool     `yaml:"normalize_xhigh_reasoning_effort" json:"normalize_xhigh_reasoning_effort"`
	DefaultTemperature            *float64 `yaml:"default_temperature,omitempty" json:"default_temperature,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	return NormalizeAndValidate(cfg)
}

func NormalizeAndValidate(cfg Config) (Config, error) {
	if len(cfg.Backends) == 0 {
		return Config{}, errors.New("backends is empty")
	}

	cfg.Console.RawPassword = cfg.Console.Password
	cfg.Console.Password = strings.TrimSpace(os.Expand(cfg.Console.Password, os.Getenv))
	if cfg.Console.Enabled && cfg.Console.Password == "" {
		return Config{}, errors.New("console.password is required when console.enabled=true")
	}
	for modelName, modelCfg := range cfg.ModelDefaults {
		modelCfg.SystemPrompt = strings.TrimSpace(modelCfg.SystemPrompt)
		modelCfg.ReasoningEffort = strings.ToLower(strings.TrimSpace(modelCfg.ReasoningEffort))
		if modelCfg.ReasoningEffort != "" {
			switch modelCfg.ReasoningEffort {
			case "low", "medium", "high", "xhigh":
			default:
				return Config{}, fmt.Errorf("invalid model_defaults[%q].reasoning_effort %q: must be low/medium/high/xhigh", modelName, modelCfg.ReasoningEffort)
			}
		}
		if modelCfg.MaxTokens < 0 {
			return Config{}, fmt.Errorf("model_defaults[%q].max_tokens must be non-negative", modelName)
		}
		if modelCfg.DefaultTemperature != nil {
			t := *modelCfg.DefaultTemperature
			if t < 0 || t > 2 {
				return Config{}, fmt.Errorf("model_defaults[%q].default_temperature must be in [0, 2]", modelName)
			}
		}
		cfg.ModelDefaults[modelName] = modelCfg
	}

	// Validate circuit breaker config
	if cfg.CircuitBreaker.RecoveryTimeout != "" {
		if _, err := time.ParseDuration(cfg.CircuitBreaker.RecoveryTimeout); err != nil {
			return Config{}, fmt.Errorf("invalid circuit_breaker.recovery_timeout %q: %w", cfg.CircuitBreaker.RecoveryTimeout, err)
		}
	}

	seenNames := make(map[string]bool)
	defaultCount := 0

	for i := range cfg.Backends {
		b := &cfg.Backends[i]

		// Validate backend name uniqueness
		if b.Name != "" {
			if seenNames[b.Name] {
				return Config{}, fmt.Errorf("duplicate backend name: %s", b.Name)
			}
			seenNames[b.Name] = true
		}

		// Validate URL
		if b.URL != "" {
			if _, err := url.Parse(b.URL); err != nil {
				return Config{}, fmt.Errorf("backend %q: invalid url: %w", b.Name, err)
			}
		}

		// Protocol default and validation
		b.Protocol = strings.ToLower(strings.TrimSpace(b.Protocol))
		b.APIKey = strings.TrimSpace(b.APIKey)
		b.AnthropicVersion = strings.TrimSpace(b.AnthropicVersion)
		if b.Protocol == "" {
			b.Protocol = "openai"
		}
		switch b.Protocol {
		case "openai", "anthropic", "openai-responses":
			// valid
		default:
			return Config{}, fmt.Errorf("backend %q: invalid protocol %q: must be openai/anthropic/openai-responses", b.Name, b.Protocol)
		}

		// Validate default count
		if b.Default {
			defaultCount++
			if defaultCount > 1 {
				return Config{}, fmt.Errorf("backend %q: only one backend can be marked as default", b.Name)
			}
		}

		// Weight default
		if b.Weight <= 0 {
			b.Weight = 1
		}

		// Validate backend timeout
		if b.Timeout != "" {
			if _, err := time.ParseDuration(b.Timeout); err != nil {
				return Config{}, fmt.Errorf("backend %q: invalid timeout %q: %w", b.Name, b.Timeout, err)
			}
		}

		// Validate health check config
		if b.HealthCheck != nil {
			if b.HealthCheck.Path == "" {
				b.HealthCheck.Path = "/healthz"
			}
			if b.HealthCheck.Interval != "" {
				if _, err := time.ParseDuration(b.HealthCheck.Interval); err != nil {
					return Config{}, fmt.Errorf("backend %q: invalid health_check interval %q: %w", b.Name, b.HealthCheck.Interval, err)
				}
			}
		}

		// Validate model aliases — unique within same backend only
		seenInBackend := make(map[string]bool)
		for j := range b.Models {
			m := &b.Models[j]

			m.SystemPrompt = strings.TrimSpace(m.SystemPrompt)
			m.ReasoningDefaultEffort = strings.ToLower(strings.TrimSpace(m.ReasoningDefaultEffort))
			if m.ReasoningDefaultEffort != "" {
				switch m.ReasoningDefaultEffort {
				case "low", "medium", "high", "xhigh":
				default:
					return Config{}, fmt.Errorf("backend %q model %q: invalid reasoning_default_effort %q: must be low/medium/high/xhigh", b.Name, m.Name, m.ReasoningDefaultEffort)
				}
			}

			if m.DefaultMaxTokens < 0 {
				return Config{}, fmt.Errorf("backend %q model %q: default_max_tokens must be non-negative", b.Name, m.Name)
			}

			if m.DefaultTemperature != nil {
				t := *m.DefaultTemperature
				if t < 0 || t > 2 {
					return Config{}, fmt.Errorf("backend %q model %q: default_temperature must be in [0, 2]", b.Name, m.Name)
				}
			}

			if m.Name != "" {
				if seenInBackend[m.Name] {
					return Config{}, fmt.Errorf("backend %q: duplicate model name %q", b.Name, m.Name)
				}
				seenInBackend[m.Name] = true
			}

			for _, alias := range m.Aliases {
				alias = strings.TrimSpace(alias)
				if alias == "" {
					continue
				}
				if alias == m.Name {
					continue // alias same as model name within same backend is ok
				}
				if seenInBackend[alias] {
					return Config{}, fmt.Errorf("backend %q: duplicate alias %q", b.Name, alias)
				}
				seenInBackend[alias] = true
			}
		}
	}
	return cfg, nil
}
