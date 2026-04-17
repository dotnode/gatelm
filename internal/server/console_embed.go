package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed consoleui/index.html
var consoleAssets embed.FS

type consoleUIAssetSource struct {
	fsys   fs.FS
	source string
	path   string
}

func (s *Server) consoleStaticHandler(basePath string) http.Handler {
	basePath = normalizeConsoleBasePath(basePath)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only serve index.html for the console root. Subtree paths that are
		// not explicit API routes should 404 so mistyped API calls aren't
		// silently swallowed by the SPA fallback and returned as HTML 200.
		if r.URL.Path != basePath && r.URL.Path != basePath+"/" {
			http.NotFound(w, r)
			return
		}
		assets, err := resolveConsoleUIAssets()
		if err != nil {
			http.Error(w, "console ui not built", http.StatusServiceUnavailable)
			return
		}
		serveConsoleIndex(w, r, assets.fsys, basePath)
	})
}

func resolveConsoleUIAssets() (consoleUIAssetSource, error) {
	for _, dir := range consoleUICandidateDirs() {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if source, ok := tryConsoleUIDir(dir); ok {
			return source, nil
		}
	}
	sub, err := fs.Sub(consoleAssets, "consoleui")
	if err != nil {
		return consoleUIAssetSource{}, err
	}
	return consoleUIAssetSource{fsys: sub, source: "embed"}, nil
}

func consoleUICandidateDirs() []string {
	candidates := make([]string, 0, 4)
	if envDir := strings.TrimSpace(os.Getenv("AI_PROXY_CONSOLE_UI_DIR")); envDir != "" {
		candidates = append(candidates, envDir)
	}
	candidates = append(candidates,
		filepath.Join("internal", "server", "consoleui"),
		filepath.Join(".", "internal", "server", "consoleui"),
	)
	return candidates
}

func tryConsoleUIDir(dir string) (consoleUIAssetSource, bool) {
	cleanDir := filepath.Clean(dir)
	if _, err := os.Stat(filepath.Join(cleanDir, "index.html")); err != nil {
		return consoleUIAssetSource{}, false
	}
	return consoleUIAssetSource{fsys: os.DirFS(cleanDir), source: "disk", path: cleanDir}, true
}

func currentConsoleUIAssetSource() consoleUIAssetSource {
	assets, err := resolveConsoleUIAssets()
	if err != nil {
		return consoleUIAssetSource{source: "unavailable"}
	}
	return assets
}

func serveConsoleIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS, basePath string) {
	content, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	content = injectConsoleBasePath(content, basePath)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(content)))
}

func injectConsoleBasePath(content []byte, basePath string) []byte {
	basePath = normalizeConsoleBasePath(basePath)
	encodedBasePath, err := json.Marshal(basePath)
	if err != nil {
		return content
	}
	snippet := []byte("<script>window.__GATELM_CONSOLE_BASE_PATH__=" + string(encodedBasePath) + ";</script>")
	if bytes.Contains(content, snippet) {
		return content
	}
	if bytes.Contains(content, []byte("<head>")) {
		return bytes.Replace(content, []byte("<head>"), append([]byte("<head>"), snippet...), 1)
	}
	return append(snippet, content...)
}
