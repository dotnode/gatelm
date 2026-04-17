package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

// loginConsole performs a login and returns the session cookie.
func loginConsole(t *testing.T, mux *http.ServeMux, basePath, password string) *http.Cookie {
	t.Helper()
	body := bytes.NewReader([]byte(`{"password":"` + password + `"}`))
	req := httptest.NewRequest(http.MethodPost, basePath+"/api/login", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: status=%d body=%s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie after login")
	}
	return cookies[0]
}

func TestConsoleConfigGetAndStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	logger, err := logging.NewTokenLogger(config.TokenLogConfig{Enabled: true, File: dbPath})
	if err != nil {
		t.Fatalf("new token logger: %v", err)
	}
	defer logger.Close()

	cfg := config.Config{
		Listen:   ":8080",
		TokenLog: config.TokenLogConfig{Enabled: true, File: dbPath},
		Console:  config.ConsoleConfig{Enabled: true, Password: "test-pass"},
		Backends: []config.Backend{{
			Name:     "b1",
			URL:      "http://example.com",
			Protocol: "openai",
			Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet-4"}}},
		}},
	}
	hm := NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, ""))
	srv := New(cfg, logger, logging.NewDebugLog(false, ""), http.DefaultClient, hm, NewNoopObserver())
	srv.SetConfigPath("/tmp/config.yaml")
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)

	cookie := loginConsole(t, mux, "/console", "test-pass")

	req := httptest.NewRequest(http.MethodGet, "/console/api/config", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("config status = %d", w.Code)
	}

	var cfgResp consoleConfigPayload
	if err := json.Unmarshal(w.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("decode config response: %v body=%s", err, w.Body.String())
	}
	if len(cfgResp.Backends) != 1 {
		t.Fatalf("expected one backend, got %d", len(cfgResp.Backends))
	}
	if !config.BackendEnabled(&cfgResp.Backends[0]) {
		t.Fatal("expected backend without enabled field to default to enabled")
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/console/api/status", nil)
	statusReq.AddCookie(cookie)
	statusW := httptest.NewRecorder()
	mux.ServeHTTP(statusW, statusReq)
	if statusW.Code != 200 {
		t.Fatalf("status code = %d", statusW.Code)
	}
}

func TestConsoleTestRejectsDisabledBackend(t *testing.T) {
	disabled := false
	cfg := config.Config{
		Console: config.ConsoleConfig{Enabled: true, Password: "secret-pass"},
		Backends: []config.Backend{{
			Name: "b1", URL: "http://example.com", Protocol: "openai", Enabled: &disabled, Models: []config.Model{{Name: "gpt-4o"}},
		}},
	}
	srv := New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)
	cookie := loginConsole(t, mux, "/console", "secret-pass")

	body := bytes.NewReader([]byte(`{"backend":"b1","model":"gpt-4o","path":"/v1/chat/completions","prompt":"ping"}`))
	req := httptest.NewRequest(http.MethodPost, "/console/api/test", body)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for disabled backend, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestConsoleLoginSecureCookieTrustsOnlyTrustedProxy(t *testing.T) {
	cfg := config.Config{
		Console: config.ConsoleConfig{Enabled: true, Password: "secret-pass"},
		Backends: []config.Backend{{
			Name: "b1", URL: "http://example.com", Protocol: "openai", Models: []config.Model{{Name: "gpt-4o"}},
		}},
	}
	srv := New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)

	body := bytes.NewReader([]byte(`{"password":"secret-pass"}`))
	req := httptest.NewRequest(http.MethodPost, "/console/api/login", body)
	req.RemoteAddr = "198.51.100.10:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie")
	}
	if cookies[0].Secure {
		t.Fatal("expected insecure cookie for untrusted proxy header")
	}

	cfg.TrustedProxies = []string{"198.51.100.10"}
	srv = New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	mux = http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)

	body = bytes.NewReader([]byte(`{"password":"secret-pass"}`))
	req = httptest.NewRequest(http.MethodPost, "/console/api/login", body)
	req.RemoteAddr = "198.51.100.10:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected trusted proxy login 200, got %d", w.Code)
	}
	cookies = w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected trusted proxy session cookie")
	}
	if !cookies[0].Secure {
		t.Fatal("expected secure cookie for trusted proxy https header")
	}
}

func TestConsoleAuthFlow(t *testing.T) {
	cfg := config.Config{
		Console: config.ConsoleConfig{Enabled: true, Password: "secret-pass"},
		Backends: []config.Backend{{
			Name: "b1", URL: "http://example.com", Protocol: "openai", Models: []config.Model{{Name: "gpt-4o"}},
		}},
	}
	srv := New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)

	unauthReq := httptest.NewRequest(http.MethodGet, "/console/api/config", nil)
	unauthW := httptest.NewRecorder()
	mux.ServeHTTP(unauthW, unauthReq)
	if unauthW.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthW.Code)
	}

	badBody := bytes.NewReader([]byte(`{"password":"wrong"}`))
	badReq := httptest.NewRequest(http.MethodPost, "/console/api/login", badBody)
	badW := httptest.NewRecorder()
	mux.ServeHTTP(badW, badReq)
	if badW.Code != http.StatusUnauthorized {
		t.Fatalf("expected bad login 401, got %d", badW.Code)
	}

	goodBody := bytes.NewReader([]byte(`{"password":"secret-pass"}`))
	goodReq := httptest.NewRequest(http.MethodPost, "/console/api/login", goodBody)
	goodW := httptest.NewRecorder()
	mux.ServeHTTP(goodW, goodReq)
	if goodW.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d", goodW.Code)
	}
	cookies := goodW.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie")
	}
	if cookies[0].Path != "/console" {
		t.Fatalf("expected cookie path /console, got %q", cookies[0].Path)
	}

	authReq := httptest.NewRequest(http.MethodGet, "/console/api/config", nil)
	authReq.AddCookie(cookies[0])
	authW := httptest.NewRecorder()
	mux.ServeHTTP(authW, authReq)
	if authW.Code != http.StatusOK {
		t.Fatalf("expected authed config 200, got %d", authW.Code)
	}
}

func TestConsoleCustomBasePath(t *testing.T) {
	cfg := config.Config{
		Console: config.ConsoleConfig{Enabled: true, Password: "secret-pass"},
		Backends: []config.Backend{{
			Name: "b1", URL: "http://example.com", Protocol: "openai", Models: []config.Model{{Name: "gpt-4o"}},
		}},
	}
	srv := New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutesWithOptions(mux, ConsoleRouteOptions{BasePath: "/admin/ai"})

	goodBody := bytes.NewReader([]byte(`{"password":"secret-pass"}`))
	goodReq := httptest.NewRequest(http.MethodPost, "/admin/ai/api/login", goodBody)
	goodW := httptest.NewRecorder()
	mux.ServeHTTP(goodW, goodReq)
	if goodW.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d", goodW.Code)
	}
	cookies := goodW.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie")
	}
	if cookies[0].Path != "/admin/ai" {
		t.Fatalf("expected cookie path /admin/ai, got %q", cookies[0].Path)
	}

	pageReq := httptest.NewRequest(http.MethodGet, "/admin/ai", nil)
	pageW := httptest.NewRecorder()
	mux.ServeHTTP(pageW, pageReq)
	if pageW.Code != http.StatusOK {
		t.Fatalf("expected page 200, got %d", pageW.Code)
	}
	if !bytes.Contains(pageW.Body.Bytes(), []byte(`window.__GATELM_CONSOLE_BASE_PATH__="/admin/ai"`)) {
		t.Fatalf("expected injected base path, got body=%s", pageW.Body.String())
	}
}

func TestConsoleLogsAndSaveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	dbPath := filepath.Join(tmpDir, "usage.db")
	original := []byte("# top comment\nlisten: ':8080'\n\n# console comment\nconsole:\n  enabled: true\n  password: test-pass\n\n# model defaults comment\nmodel_defaults:\n  gpt-4o: # model default comment\n    reasoning_effort: high\n\n# backends comment\nbackends:\n  - name: b1 # backend comment\n    url: http://example.com\n    protocol: openai\n    headers:\n      Authorization: Bearer a # header comment\n    health_check: # health check comment\n      path: /healthz # health path comment\n      interval: 30s # health interval comment\n    models:\n      - name: gpt-4o # model comment\n        aliases: [claude-sonnet-4]\n")
	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	logger, err := logging.NewTokenLogger(config.TokenLogConfig{Enabled: true, File: dbPath})
	if err != nil {
		t.Fatalf("new token logger: %v", err)
	}
	logger.Log(logging.UsageLog{
		Time:            "2026-01-01T00:00:00Z",
		Backend:         "b1",
		ClientProtocol:  "anthropic",
		BackendProtocol: "openai",
		ClientKey:       "k1",
		StatusCode:      200,
		TotalTokens:     12,
	})
	logger.Log(logging.UsageLog{
		Time:            "2026-01-01T00:01:00Z",
		Backend:         "b1",
		ClientProtocol:  "openai",
		BackendProtocol: "openai",
		ClientKey:       "k2",
		StatusCode:      500,
		TotalTokens:     34,
	})
	logger.Close()
	logger, err = logging.NewTokenLogger(config.TokenLogConfig{Enabled: true, File: dbPath})
	if err != nil {
		t.Fatalf("reopen logger: %v", err)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	hm := NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, ""))
	srv := New(cfg, logger, logging.NewDebugLog(false, ""), http.DefaultClient, hm, NewNoopObserver())
	srv.SetConfigPath(configPath)
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)

	cookie := loginConsole(t, mux, "/console", "test-pass")

	logsReq := httptest.NewRequest(http.MethodGet, "/console/api/logs?backend=b1&limit=1&offset=1", nil)
	logsReq.AddCookie(cookie)
	logsW := httptest.NewRecorder()
	mux.ServeHTTP(logsW, logsReq)
	if logsW.Code != 200 {
		t.Fatalf("logs code = %d", logsW.Code)
	}
	var logsResp consoleLogsResponse
	if err := json.Unmarshal(logsW.Body.Bytes(), &logsResp); err != nil {
		t.Fatalf("decode logs response: %v body=%s", err, logsW.Body.String())
	}
	if len(logsResp.Items) != 1 {
		t.Fatalf("logs items len = %d, want 1", len(logsResp.Items))
	}
	if logsResp.Items[0].ClientProtocol != "anthropic" {
		t.Fatalf("client_protocol = %q, want anthropic", logsResp.Items[0].ClientProtocol)
	}
	if logsResp.Items[0].BackendProtocol != "openai" {
		t.Fatalf("backend_protocol = %q, want openai", logsResp.Items[0].BackendProtocol)
	}
	if logsResp.Summary.TotalRequests != 2 {
		t.Fatalf("summary total_requests = %d, want 2", logsResp.Summary.TotalRequests)
	}
	if logsResp.Pagination.Page != 2 || logsResp.Pagination.PageSize != 1 || logsResp.Pagination.Total != 2 || logsResp.Pagination.TotalPages != 2 {
		t.Fatalf("unexpected pagination: %+v", logsResp.Pagination)
	}
	if !logsResp.Pagination.HasPrev || logsResp.Pagination.HasNext {
		t.Fatalf("unexpected pagination prev/next: %+v", logsResp.Pagination)
	}

	payload := consoleConfigPayload{
		Listen: ":9090",
		Backends: []config.Backend{{
			Name:             "b1",
			URL:              "http://example.org",
			Protocol:         "openai",
			APIKey:           "sk-openai-updated",
			AnthropicVersion: "2024-10-22",
			Headers:          map[string]string{"Authorization": "Bearer b"},
			HealthCheck: &config.HealthCheckConfig{
				Path:     "/readyz",
				Interval: "45s",
			},
			Models: []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-opus-4.6"}}},
		}},
		TokenLog:      config.TokenLogConfig{Enabled: true, File: dbPath},
		ModelDefaults: map[string]config.ModelDefaultConfig{"gpt-4o": {ReasoningEffort: "medium"}},
		Console:       config.ConsoleConfig{Enabled: true, Password: "test-pass"},
	}
	body, _ := json.Marshal(payload)
	saveReq := httptest.NewRequest(http.MethodPut, "/console/api/config", bytes.NewReader(body))
	saveReq.AddCookie(cookie)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != 200 {
		t.Fatalf("save code = %d body=%s", saveW.Code, saveW.Body.String())
	}
	if srv.snapshot().cfg.Listen != ":9090" {
		t.Fatalf("expected reloaded listen, got %s", srv.snapshot().cfg.Listen)
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	updatedText := string(updated)
	if !bytes.Contains(updated, []byte("# top comment")) {
		t.Fatalf("expected top comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# console comment")) {
		t.Fatalf("expected console comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# backends comment")) {
		t.Fatalf("expected backends comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# backend comment")) {
		t.Fatalf("expected backend comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# model comment")) {
		t.Fatalf("expected model comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# header comment")) {
		t.Fatalf("expected header comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# health check comment")) {
		t.Fatalf("expected health check comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# health path comment")) {
		t.Fatalf("expected health path comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# health interval comment")) {
		t.Fatalf("expected health interval comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("# model defaults comment")) {
		t.Fatalf("expected model defaults comment to be preserved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("api_key: sk-openai-updated")) {
		t.Fatalf("expected api_key to be saved, got:\n%s", updatedText)
	}
	if !bytes.Contains(updated, []byte("anthropic_version: \"2024-10-22\"")) {
		t.Fatalf("expected anthropic_version to be saved, got:\n%s", updatedText)
	}
}

func TestSaveAndReloadConfigRejectsInvalidConfigWithoutOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	original := []byte("# keep me\nlisten: ':8080'\nconsole:\n  enabled: true\n  password: test-pass\nbackends:\n  - name: b1\n    url: http://example.com\n    protocol: openai\n    models:\n      - name: gpt-4o\n")
	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	srv := New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	srv.SetConfigPath(configPath)

	invalid := cfg
	invalid.Backends = nil
	if err := srv.SaveAndReloadConfig(configPath, invalid); err == nil {
		t.Fatal("expected invalid config error")
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after failed save: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("expected config file unchanged after failed save\nwant:\n%s\n\ngot:\n%s", string(original), string(after))
	}
}

// extractIP must take the rightmost XFF entry when the direct remote address
// is a trusted proxy, so a client-supplied leftmost hop cannot forge a fresh
// IP and bypass the per-IP login rate limit.
func TestExtractIPTakesRightmostXFF(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	if got := extractIP(req, []string{"10.0.0.1"}); got != "5.6.7.8" {
		t.Fatalf("expected rightmost XFF entry, got %q", got)
	}

	// With no trusted proxies, XFF must be ignored entirely.
	if got := extractIP(req, nil); got != "10.0.0.1" {
		t.Fatalf("expected remote host when proxy untrusted, got %q", got)
	}
}

// GET /console/api/config must not leak the admin password back to the UI.
func TestConsoleConfigGetRedactsPassword(t *testing.T) {
	cfg := config.Config{
		Console: config.ConsoleConfig{Enabled: true, Password: "super-secret"},
		Backends: []config.Backend{{
			Name: "b1", URL: "http://example.com", Protocol: "openai", Models: []config.Model{{Name: "gpt-4o"}},
		}},
	}
	srv := New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)
	cookie := loginConsole(t, mux, "/console", "super-secret")

	req := httptest.NewRequest(http.MethodGet, "/console/api/config", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get config code = %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("super-secret")) {
		t.Fatalf("password leaked in GET response: %s", w.Body.String())
	}
	var payload consoleConfigPayload
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Console.Password != "" {
		t.Fatalf("expected empty console.password in GET, got %q", payload.Console.Password)
	}
	if !payload.Console.Enabled {
		t.Fatal("expected console.enabled=true to still be reported")
	}
}

// PUT with a blank password must preserve the existing password rather than
// erase it — the UI never receives the plaintext (see TestConsoleConfigGetRedactsPassword),
// so an unchanged password must round-trip as an empty string in the payload.
func TestConsoleConfigPutPreservesPasswordWhenBlank(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	original := []byte("listen: ':8080'\nconsole:\n  enabled: true\n  password: original-secret\nbackends:\n  - name: b1\n    url: http://example.com\n    protocol: openai\n    models:\n      - name: gpt-4o\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := New(cfg, nil, logging.NewDebugLog(false, ""), http.DefaultClient, NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, "")), NewNoopObserver())
	srv.SetConfigPath(configPath)
	mux := http.NewServeMux()
	srv.RegisterConsoleRoutes(mux)
	cookie := loginConsole(t, mux, "/console", "original-secret")

	payload := consoleConfigPayload{
		Listen:   ":9090",
		Console:  config.ConsoleConfig{Enabled: true, Password: ""},
		Backends: cfg.Backends,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPut, "/console/api/config", bytes.NewReader(body))
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT code = %d body=%s", w.Code, w.Body.String())
	}

	// Existing session must remain valid — an unchanged password should not
	// trigger session revocation.
	authReq := httptest.NewRequest(http.MethodGet, "/console/api/config", nil)
	authReq.AddCookie(cookie)
	authW := httptest.NewRecorder()
	mux.ServeHTTP(authW, authReq)
	if authW.Code != http.StatusOK {
		t.Fatalf("config after blank-password PUT = %d; expected session to remain valid", authW.Code)
	}

	// Login with the preserved password must still succeed.
	loginBody := bytes.NewReader([]byte(`{"password":"original-secret"}`))
	loginReq := httptest.NewRequest(http.MethodPost, "/console/api/login", loginBody)
	loginW := httptest.NewRecorder()
	mux.ServeHTTP(loginW, loginReq)
	if loginW.Code != http.StatusOK {
		t.Fatalf("re-login with preserved password = %d body=%s", loginW.Code, loginW.Body.String())
	}

	// And the on-disk YAML still carries the original password.
	persisted, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if !bytes.Contains(persisted, []byte("password: original-secret")) {
		t.Fatalf("expected original password persisted, got:\n%s", string(persisted))
	}
}
