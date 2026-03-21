package gatelm

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/dotnode/gatelm/internal/logging"
	"github.com/dotnode/gatelm/internal/server"
)

type Options struct {
	Config     Config
	ConfigPath string
	Debug      bool
	DebugDir   string
	HTTPClient *http.Client
}

type Gateway struct {
	mu             sync.RWMutex
	server         *server.Server
	debugLog       *logging.DebugLog
	metricsHandler http.Handler
	concurrencySem chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

func New(opts Options) (*Gateway, error) {
	cfg := opts.Config
	if len(cfg.Backends) == 0 {
		if opts.ConfigPath == "" {
			return nil, errNoConfig
		}
		loaded, err := LoadConfig(opts.ConfigPath)
		if err != nil {
			return nil, err
		}
		cfg = loaded
	}
	validated, err := ValidateConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg = validated

	debugEnabled := opts.Debug || cfg.Debug
	debugDir := opts.DebugDir
	if debugDir == "" {
		debugDir = "logs"
	}
	if debugEnabled {
		if err := os.MkdirAll(debugDir, 0o755); err != nil {
			return nil, err
		}
	}

	debugLog := logging.NewDebugLog(debugEnabled, debugDir)
	tokenLogger, err := logging.NewTokenLogger(cfg.TokenLog)
	if err != nil {
		debugLog.Close()
		return nil, err
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = newDefaultHTTPClient()
	}

	healthMgr := server.NewHealthManager(server.HealthManagerConfig{
		FailThreshold:       cfg.CircuitBreaker.FailThreshold,
		RecoveryTimeout:     parseDurationOrDefault(cfg.CircuitBreaker.RecoveryTimeout, 30*time.Second),
		HalfOpenMaxRequests: cfg.CircuitBreaker.HalfOpenMaxRequests,
	}, httpClient, debugLog)
	healthMgr.StartActiveChecks(cfg.Backends)

	observer, metricsHandler, err := server.NewPrometheusObserver()
	if err != nil {
		observer = server.NewNoopObserver()
		metricsHandler = nil
	}

	srv := server.New(cfg, tokenLogger, debugLog, httpClient, healthMgr, observer)
	if opts.ConfigPath != "" {
		srv.SetConfigPath(opts.ConfigPath)
	}

	return &Gateway{
		server:         srv,
		debugLog:       debugLog,
		metricsHandler: metricsHandler,
		concurrencySem: makeConcurrencySem(cfg.MaxConcurrentRequests),
	}, nil
}

func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.ServeHTTP)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	case "/healthz/detail":
		w.Header().Set("Content-Type", "application/json")
		if g.server.AllBackendsDown() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "degraded", "reason": "all backends down"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	case "/metrics":
		if g.metricsHandler != nil {
			g.metricsHandler.ServeHTTP(w, r)
			return
		}
	}

	sem := g.currentConcurrencySem()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-r.Context().Done():
			http.Error(w, "service overloaded", http.StatusServiceUnavailable)
			return
		}
	}

	g.server.Handle(w, r)
}

func (g *Gateway) RegisterConsoleRoutes(mux *http.ServeMux, basePath string) {
	g.server.RegisterConsoleRoutesWithOptions(mux, server.ConsoleRouteOptions{BasePath: basePath})
}

func (g *Gateway) Reload(cfg Config) error {
	validated, err := ValidateConfig(cfg)
	if err != nil {
		return err
	}
	if err := g.server.ReloadConfig(validated); err != nil {
		return err
	}
	g.mu.Lock()
	g.concurrencySem = makeConcurrencySem(validated.MaxConcurrentRequests)
	g.mu.Unlock()
	return nil
}

func (g *Gateway) CurrentConfig() Config {
	return g.server.CurrentConfig()
}

func (g *Gateway) Close() error {
	g.closeOnce.Do(func() {
		if g.server != nil {
			g.closeErr = g.server.Close()
		}
		if g.debugLog != nil {
			g.debugLog.Close()
		}
	})
	return g.closeErr
}

func (g *Gateway) Shutdown(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		done <- g.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errNoConfig = &gatewayError{message: "gatelm: either Config or ConfigPath is required"}

type gatewayError struct {
	message string
}

func (e *gatewayError) Error() string {
	return e.message
}

func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func makeConcurrencySem(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

func (g *Gateway) currentConcurrencySem() chan struct{} {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.concurrencySem
}

func newDefaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
		},
	}
}
