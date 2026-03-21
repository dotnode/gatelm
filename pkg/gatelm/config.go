package gatelm

import internalconfig "github.com/dotnode/gatelm/internal/config"

type Config = internalconfig.Config
type TokenLogConfig = internalconfig.TokenLogConfig
type ModelDefaultConfig = internalconfig.ModelDefaultConfig
type CircuitBreakerConfig = internalconfig.CircuitBreakerConfig
type ConsoleConfig = internalconfig.ConsoleConfig
type Backend = internalconfig.Backend
type HealthCheckConfig = internalconfig.HealthCheckConfig
type Model = internalconfig.Model

func LoadConfig(path string) (Config, error) {
	return internalconfig.LoadConfig(path)
}

func ValidateConfig(cfg Config) (Config, error) {
	return internalconfig.NormalizeAndValidate(cfg)
}
