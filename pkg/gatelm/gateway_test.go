package gatelm

import (
	"net/http/httptest"
	"testing"
)

func minimalConfig() Config {
	return Config{
		Backends: []Backend{{
			Name:     "b1",
			URL:      "http://example.com",
			Protocol: "openai",
			Default:  true,
			Models: []Model{{
				Name: "gpt-4o",
			}},
		}},
	}
}

func TestNewValidatesAndNormalizesConfig(t *testing.T) {
	gw, err := New(Options{
		Config: Config{
			Backends: []Backend{{
				Name:    "b1",
				URL:     "http://example.com",
				Default: true,
				Models: []Model{{
					Name: "gpt-4o",
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer gw.Close()

	if got := gw.CurrentConfig().Backends[0].Protocol; got != "openai" {
		t.Fatalf("backend protocol = %q, want openai", got)
	}
}

func TestReloadValidatesAndUpdatesConcurrencyLimit(t *testing.T) {
	gw, err := New(Options{Config: minimalConfig()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer gw.Close()

	next := minimalConfig()
	next.MaxConcurrentRequests = 3
	next.Backends[0].Protocol = ""
	if err := gw.Reload(next); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	if got := cap(gw.currentConcurrencySem()); got != 3 {
		t.Fatalf("concurrency cap = %d, want 3", got)
	}
	if got := gw.CurrentConfig().Backends[0].Protocol; got != "openai" {
		t.Fatalf("backend protocol = %q, want openai", got)
	}
}

func TestServeHTTPHealthEndpoints(t *testing.T) {
	gw, err := New(Options{Config: minimalConfig()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer gw.Close()

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("/healthz status = %d, want 200", w.Code)
	}

	req = httptest.NewRequest("GET", "/healthz/detail", nil)
	w = httptest.NewRecorder()
	gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("/healthz/detail status = %d, want 200", w.Code)
	}
}
