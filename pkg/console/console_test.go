package console

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dotnode/gatelm/pkg/gatelm"
)

func TestMountCustomBasePath(t *testing.T) {
	gw, err := gatelm.New(gatelm.Options{
		Config: gatelm.Config{
			Console: gatelm.ConsoleConfig{Enabled: true, Password: "secret-pass"},
			Backends: []gatelm.Backend{{
				Name:     "b1",
				URL:      "http://example.com",
				Protocol: "openai",
				Default:  true,
				Models: []gatelm.Model{{
					Name: "gpt-4o",
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer gw.Close()

	mux := http.NewServeMux()
	Mount(mux, gw, Options{BasePath: "/admin/ai"})

	req := httptest.NewRequest(http.MethodGet, "/admin/ai/api/auth/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}
