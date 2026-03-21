package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

type loginAttempt struct {
	mu          sync.Mutex
	count       int
	windowStart time.Time
}

const (
	consoleSessionCookieName = "console_session"
	consoleSessionMaxAge     = 24 * time.Hour
)

type sessionEntry struct {
	expiresAt time.Time
}

type consoleConfigPayload struct {
	Listen                string                               `json:"listen" yaml:"listen"`
	Debug                 bool                                 `json:"debug" yaml:"debug"`
	MaxConcurrentRequests int                                  `json:"max_concurrent_requests" yaml:"max_concurrent_requests"`
	Backends              []config.Backend                     `json:"backends" yaml:"backends"`
	TokenLog              config.TokenLogConfig                `json:"token_log" yaml:"token_log"`
	APIKeys               map[string]string                    `json:"api_keys" yaml:"api_keys"`
	ModelDefaults         map[string]config.ModelDefaultConfig `json:"model_defaults" yaml:"model_defaults"`
	SystemPrompt          string                               `json:"system_prompt" yaml:"system_prompt"`
	CircuitBreaker        config.CircuitBreakerConfig          `json:"circuit_breaker" yaml:"circuit_breaker"`
	Console               config.ConsoleConfig                 `json:"console" yaml:"console"`
	TrustedProxies        []string                             `json:"trusted_proxies" yaml:"trusted_proxies"`
}

type consoleUIStatus struct {
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
}

type consoleStatusResponse struct {
	ConfigPath   string                  `json:"config_path"`
	Listen       string                  `json:"listen"`
	BackendCount int                     `json:"backend_count"`
	ModelCount   int                     `json:"model_count"`
	AliasCount   int                     `json:"alias_count"`
	TokenLog     consoleTokenLogStatus   `json:"token_log"`
	ConsoleUI    consoleUIStatus         `json:"console_ui"`
	Backends     []BackendHealthSnapshot `json:"backends"`
}

type consoleTokenLogStatus struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"`
	Path           string `json:"path"`
	DroppedEntries int64  `json:"dropped_entries"`
}

type consoleLogsResponse struct {
	Items      []logging.UsageLog      `json:"items"`
	Summary    logging.UsageLogSummary `json:"summary"`
	Pagination consoleLogsPagination   `json:"pagination"`
}

type consoleLogsPagination struct {
	Page       int  `json:"page"`
	PageSize   int  `json:"page_size"`
	Offset     int  `json:"offset"`
	Total      int  `json:"total"`
	TotalPages int  `json:"total_pages"`
	HasPrev    bool `json:"has_prev"`
	HasNext    bool `json:"has_next"`
}

type consoleTestRequest struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
	Path    string `json:"path"`
	Prompt  string `json:"prompt"`
}

type consoleTestResponse struct {
	Backend      string `json:"backend"`
	TargetURL    string `json:"target_url"`
	StatusCode   int    `json:"status_code"`
	DurationMs   int64  `json:"duration_ms"`
	ResponseBody string `json:"response_body,omitempty"`
	Error        string `json:"error,omitempty"`
}

type consoleLoginRequest struct {
	Password string `json:"password"`
}

type consoleAuthStatusResponse struct {
	Enabled       bool `json:"enabled"`
	Authenticated bool `json:"authenticated"`
}

type ConsoleRouteOptions struct {
	BasePath string
}

func (s *Server) RegisterConsoleRoutes(mux *http.ServeMux) {
	s.RegisterConsoleRoutesWithOptions(mux, ConsoleRouteOptions{})
}

func (s *Server) RegisterConsoleRoutesWithOptions(mux *http.ServeMux, opts ConsoleRouteOptions) {
	basePath := normalizeConsoleBasePath(opts.BasePath)
	mux.Handle(basePath, s.consoleStaticHandler(basePath))
	mux.Handle(basePath+"/", s.consoleStaticHandler(basePath))
	mux.HandleFunc(basePath+"/api/login", s.handleConsoleLogin(basePath))
	mux.HandleFunc(basePath+"/api/logout", s.handleConsoleLogout(basePath))
	mux.HandleFunc(basePath+"/api/auth/status", s.handleConsoleAuthStatus)
	mux.HandleFunc(basePath+"/api/config", s.requireConsoleAuth(s.handleConsoleConfig))
	mux.HandleFunc(basePath+"/api/status", s.requireConsoleAuth(s.handleConsoleStatus))
	mux.HandleFunc(basePath+"/api/logs", s.requireConsoleAuth(s.handleConsoleLogs))
	mux.HandleFunc(basePath+"/api/test", s.requireConsoleAuth(s.handleConsoleTest))
}

func (s *Server) handleConsoleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleConsoleConfigGet(w, r)
	case http.MethodPut:
		s.handleConsoleConfigPut(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleConsoleConfigGet(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	writeJSON(w, http.StatusOK, consoleConfigPayload{
		Listen:                snap.cfg.Listen,
		Debug:                 snap.cfg.Debug,
		MaxConcurrentRequests: snap.cfg.MaxConcurrentRequests,
		Backends:              snap.cfg.Backends,
		TokenLog:              snap.cfg.TokenLog,
		APIKeys:               snap.cfg.APIKeys,
		ModelDefaults:         snap.cfg.ModelDefaults,
		SystemPrompt:          snap.cfg.SystemPrompt,
		CircuitBreaker:        snap.cfg.CircuitBreaker,
		Console:               snap.cfg.Console,
		TrustedProxies:        snap.cfg.TrustedProxies,
	})
}

func (s *Server) handleConsoleConfigPut(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	if snap.configPath == "" {
		http.Error(w, "config path is not set", http.StatusBadRequest)
		return
	}
	var payload consoleConfigPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	cfg := config.Config{
		Listen:                payload.Listen,
		Debug:                 payload.Debug,
		MaxConcurrentRequests: payload.MaxConcurrentRequests,
		Backends:              payload.Backends,
		TokenLog:              payload.TokenLog,
		APIKeys:               payload.APIKeys,
		ModelDefaults:         payload.ModelDefaults,
		SystemPrompt:          payload.SystemPrompt,
		CircuitBreaker:        payload.CircuitBreaker,
		Console:               payload.Console,
		TrustedProxies:        payload.TrustedProxies,
	}
	if err := s.SaveAndReloadConfig(snap.configPath, cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) SaveAndReloadConfig(path string, cfg config.Config) error {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config failed: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir failed: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp config failed: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config failed: %w", err)
	}
	validated, err := config.LoadConfig(tmpPath)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config failed: %w", err)
	}
	return s.ReloadConfig(validated)
}

func (s *Server) ReloadConfig(newCfg config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Invalidate all sessions when password changes
	if newCfg.Console.Password != s.Cfg.Console.Password {
		s.revokeAllSessions()
	}

	newHealth := NewHealthManager(HealthManagerConfig{
		FailThreshold:       newCfg.CircuitBreaker.FailThreshold,
		RecoveryTimeout:     parseRecoveryTimeoutOrDefault(newCfg.CircuitBreaker.RecoveryTimeout, 30*time.Second),
		HalfOpenMaxRequests: newCfg.CircuitBreaker.HalfOpenMaxRequests,
	}, s.Client, s.Debug)
	newHealth.StartActiveChecks(newCfg.Backends)

	newTokenLog := s.TokenLog
	oldTokenLog := s.TokenLog
	if tokenLogNeedsRebuild(s.Cfg.TokenLog, newCfg.TokenLog) {
		created, err := logging.NewTokenLogger(newCfg.TokenLog)
		if err != nil {
			newHealth.Stop()
			return fmt.Errorf("rebuild token logger failed: %w", err)
		}
		newTokenLog = created
	}

	oldHealth := s.Health
	s.Cfg = newCfg
	s.ModelIndex = BuildModelIndex(newCfg.Backends)
	s.TokenLog = newTokenLog
	s.Health = newHealth
	s.Selector = NewBackendSelector(newHealth)

	if oldHealth != nil {
		oldHealth.Stop()
	}
	if oldTokenLog != nil && oldTokenLog != newTokenLog {
		oldTokenLog.Close()
	}
	return nil
}

func tokenLogNeedsRebuild(oldCfg, newCfg config.TokenLogConfig) bool {
	return oldCfg.Enabled != newCfg.Enabled || oldCfg.File != newCfg.File || oldCfg.RetentionDays != newCfg.RetentionDays
}

func (s *Server) handleConsoleStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	modelCount := 0
	aliasCount := 0
	for _, backend := range snap.cfg.Backends {
		modelCount += len(backend.Models)
		for _, model := range backend.Models {
			aliasCount += len(model.Aliases)
		}
	}
	consoleUI := currentConsoleUIAssetSource()
	writeJSON(w, http.StatusOK, consoleStatusResponse{
		ConfigPath:   snap.configPath,
		Listen:       snap.cfg.Listen,
		BackendCount: len(snap.cfg.Backends),
		ModelCount:   modelCount,
		AliasCount:   aliasCount,
		TokenLog: consoleTokenLogStatus{
			Enabled:        snap.cfg.TokenLog.Enabled,
			Mode:           snap.tokenLog.Mode(),
			Path:           snap.tokenLog.Path(),
			DroppedEntries: snap.tokenLog.DroppedEntries(),
		},
		ConsoleUI: consoleUIStatus{
			Source: consoleUI.source,
			Path:   consoleUI.path,
		},
		Backends: snap.health.Snapshot(snap.cfg.Backends),
	})
}

func (s *Server) handleConsoleLogs(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	limit := parseIntQuery(r, "limit", 50)
	offset := parseIntQuery(r, "offset", 0)
	logs, summary, err := snap.tokenLog.QueryUsageLogs(logging.UsageLogFilter{
		Limit:         limit,
		Offset:        offset,
		ClientKey:     strings.TrimSpace(r.URL.Query().Get("client_key")),
		Backend:       strings.TrimSpace(r.URL.Query().Get("backend")),
		StatusCode:    parseIntQuery(r, "status_code", 0),
		ErrorCategory: strings.TrimSpace(r.URL.Query().Get("error_category")),
		StartTime:     strings.TrimSpace(r.URL.Query().Get("start_time")),
		EndTime:       strings.TrimSpace(r.URL.Query().Get("end_time")),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pageSize, normalizedOffset := normalizePagination(limit, offset)
	writeJSON(w, http.StatusOK, consoleLogsResponse{
		Items:      logs,
		Summary:    summary,
		Pagination: buildConsoleLogsPagination(summary.TotalRequests, pageSize, normalizedOffset),
	})
}

func buildConsoleLogsPagination(total, pageSize, offset int) consoleLogsPagination {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}
	if offset < 0 {
		offset = 0
	}
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	page := 1
	if totalPages > 0 {
		page = offset/pageSize + 1
		if page > totalPages {
			page = totalPages
		}
	}
	return consoleLogsPagination{
		Page:       page,
		PageSize:   pageSize,
		Offset:     offset,
		Total:      total,
		TotalPages: totalPages,
		HasPrev:    offset > 0,
		HasNext:    offset+pageSize < total,
	}
}

func normalizePagination(limit, offset int) (pageSize int, normalizedOffset int) {
	pageSize = limit
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}
	normalizedOffset = offset
	if normalizedOffset < 0 {
		normalizedOffset = 0
	}
	return pageSize, normalizedOffset
}

func (s *Server) handleConsoleTest(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	var req consoleTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	backend := findBackendByName(snap.cfg.Backends, req.Backend)
	if backend == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/v1/chat/completions"
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "ping"
	}
	model := strings.TrimSpace(req.Model)
	if model == "" && len(backend.Models) > 0 {
		model = backend.Models[0].Name
	}
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{{
			"role":    "user",
			"content": prompt,
		}},
		"stream": false,
	}
	body, _ := json.Marshal(payload)
	incomingURL := &url.URL{Path: path}
	targetURL, err := composeTargetURL(backend, incomingURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range backend.Headers {
		httpReq.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := snap.client.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusOK, consoleTestResponse{Backend: backend.Name, TargetURL: targetURL, Error: err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	writeJSON(w, http.StatusOK, consoleTestResponse{
		Backend:      backend.Name,
		TargetURL:    targetURL,
		StatusCode:   resp.StatusCode,
		DurationMs:   time.Since(start).Milliseconds(),
		ResponseBody: string(respBody),
	})
}

func (s *Server) requireConsoleAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := s.snapshot()
		if !snap.cfg.Console.Enabled {
			http.NotFound(w, r)
			return
		}
		if !s.isConsoleAuthenticated(r, snap.cfg) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleConsoleAuthStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	writeJSON(w, http.StatusOK, consoleAuthStatusResponse{
		Enabled:       snap.cfg.Console.Enabled,
		Authenticated: s.isConsoleAuthenticated(r, snap.cfg),
	})
}

func (s *Server) handleConsoleLogin(basePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap := s.snapshot()
		if !snap.cfg.Console.Enabled {
			writeJSON(w, http.StatusOK, consoleAuthStatusResponse{Enabled: false, Authenticated: false})
			return
		}

		// Rate limiting: 10 attempts per minute per IP
		ip := extractIP(r, snap.cfg.TrustedProxies)
		if !s.checkLoginRateLimit(ip) {
			http.Error(w, "too many login attempts", http.StatusTooManyRequests)
			return
		}

		var req consoleLoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Password) != snap.cfg.Console.Password {
			http.Error(w, "invalid password", http.StatusUnauthorized)
			return
		}
		s.issueConsoleSessionCookie(w, r, basePath)
		writeJSON(w, http.StatusOK, consoleAuthStatusResponse{Enabled: true, Authenticated: true})
	}
}

func (s *Server) handleConsoleLogout(basePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.clearConsoleSessionCookie(w, r, basePath)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (s *Server) issueConsoleSessionCookie(w http.ResponseWriter, r *http.Request, basePath string) {
	token := generateSessionToken()
	s.sessions.Store(token, &sessionEntry{expiresAt: time.Now().Add(consoleSessionMaxAge)})
	maxAge := int(consoleSessionMaxAge.Seconds())
	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookieName,
		Value:    token,
		Path:     normalizeConsoleBasePath(basePath),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		MaxAge:   maxAge,
	})
}

func (s *Server) clearConsoleSessionCookie(w http.ResponseWriter, r *http.Request, basePath string) {
	if cookie, err := r.Cookie(consoleSessionCookieName); err == nil {
		s.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookieName,
		Value:    "",
		Path:     normalizeConsoleBasePath(basePath),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		MaxAge:   -1,
	})
}

func (s *Server) isConsoleAuthenticated(r *http.Request, cfg config.Config) bool {
	if !cfg.Console.Enabled {
		return false
	}
	cookie, err := r.Cookie(consoleSessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	val, ok := s.sessions.Load(cookie.Value)
	if !ok {
		return false
	}
	entry := val.(*sessionEntry)
	if time.Now().After(entry.expiresAt) {
		s.sessions.Delete(cookie.Value)
		return false
	}
	return true
}

func generateSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func (s *Server) revokeAllSessions() {
	s.sessions.Range(func(key, _ any) bool {
		s.sessions.Delete(key)
		return true
	})
}

func findBackendByName(backends []config.Backend, name string) *config.Backend {
	for i := range backends {
		if backends[i].Name == name {
			return &backends[i]
		}
	}
	return nil
}

func parseIntQuery(r *http.Request, key string, def int) int {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return def
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return def
	}
	return n
}

func parseRecoveryTimeoutOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func normalizeConsoleBasePath(basePath string) string {
	basePath = strings.TrimSpace(basePath)
	if basePath == "" {
		return "/console"
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		return "/console"
	}
	return basePath
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func stableSortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// checkLoginRateLimit returns true if the IP is allowed to attempt login (max 10/min).
func (s *Server) checkLoginRateLimit(ip string) bool {
	const maxAttempts = 10
	const window = time.Minute

	now := time.Now()
	val, _ := s.loginAttempts.LoadOrStore(ip, &loginAttempt{windowStart: now})
	attempt := val.(*loginAttempt)

	attempt.mu.Lock()
	defer attempt.mu.Unlock()

	// Reset window if expired
	if now.Sub(attempt.windowStart) >= window {
		attempt.count = 0
		attempt.windowStart = now
	}

	if attempt.count >= maxAttempts {
		return false
	}

	attempt.count++
	return true
}

// extractIP extracts the client IP from the request.
// Proxy headers (X-Forwarded-For, X-Real-IP) are only trusted when the
// direct remote address matches one of the configured trusted proxies.
func extractIP(r *http.Request, trustedProxies []string) string {
	remoteHost, _, _ := net.SplitHostPort(r.RemoteAddr)
	if remoteHost == "" {
		remoteHost = r.RemoteAddr
	}

	if isTrustedProxy(remoteHost, trustedProxies) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if idx := strings.Index(xff, ","); idx > 0 {
				return strings.TrimSpace(xff[:idx])
			}
			return strings.TrimSpace(xff)
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}

	return remoteHost
}

// isTrustedProxy checks whether remoteIP is in the trusted proxy list.
// Supports exact IPs, CIDRs, and the wildcard "*" (trust all).
func isTrustedProxy(remoteIP string, trustedProxies []string) bool {
	if len(trustedProxies) == 0 {
		return false
	}
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	for _, tp := range trustedProxies {
		if tp == "*" {
			return true
		}
		if strings.Contains(tp, "/") {
			if _, cidr, err := net.ParseCIDR(tp); err == nil && cidr.Contains(ip) {
				return true
			}
		} else {
			if tpIP := net.ParseIP(tp); tpIP != nil && tpIP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// cleanupLoop periodically purges expired sessions and stale login-attempt records.
func (s *Server) cleanupLoop() {
	defer close(s.cleanupDone)
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.cleanupStop:
			return
		case <-ticker.C:
			s.purgeExpiredSessions()
			s.purgeStaleLoginAttempts()
		}
	}
}

func (s *Server) stopCleanup() {
	close(s.cleanupStop)
	<-s.cleanupDone
}

func (s *Server) purgeExpiredSessions() {
	now := time.Now()
	s.sessions.Range(func(key, val any) bool {
		entry := val.(*sessionEntry)
		if now.After(entry.expiresAt) {
			s.sessions.Delete(key)
		}
		return true
	})
}

func (s *Server) purgeStaleLoginAttempts() {
	now := time.Now()
	s.loginAttempts.Range(func(key, val any) bool {
		attempt := val.(*loginAttempt)
		attempt.mu.Lock()
		stale := now.Sub(attempt.windowStart) >= 5*time.Minute
		attempt.mu.Unlock()
		if stale {
			s.loginAttempts.Delete(key)
		}
		return true
	})
}
