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

	statusReq := httptest.NewRequest(http.MethodGet, "/console/api/status", nil)
	statusReq.AddCookie(cookie)
	statusW := httptest.NewRecorder()
	mux.ServeHTTP(statusW, statusReq)
	if statusW.Code != 200 {
		t.Fatalf("status code = %d", statusW.Code)
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
	original := []byte("listen: ':8080'\nconsole:\n  enabled: true\n  password: test-pass\nbackends:\n  - name: b1\n    url: http://example.com\n    protocol: openai\n    models:\n      - name: gpt-4o\n")
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
			Name:     "b2",
			URL:      "http://example.org",
			Protocol: "openai",
			Models:   []config.Model{{Name: "gpt-4.1", Aliases: []string{"claude-opus-4.6"}}},
		}},
		TokenLog: config.TokenLogConfig{Enabled: true, File: dbPath},
		Console:  config.ConsoleConfig{Enabled: true, Password: "test-pass"},
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
}
