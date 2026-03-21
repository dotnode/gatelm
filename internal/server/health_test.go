package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

func newTestHealthManager() *HealthManager {
	return NewHealthManager(HealthManagerConfig{
		FailThreshold:       1,
		RecoveryTimeout:     30 * time.Second,
		HalfOpenMaxRequests: 1,
	}, http.DefaultClient, logging.NewDebugLog(false, ""))
}

func TestHealthManagerPassiveFailure(t *testing.T) {
	hm := newTestHealthManager()

	if !hm.IsHealthy("backend-a") {
		t.Fatal("unknown backend should be healthy")
	}

	justDown := hm.ReportFailure("backend-a")
	if !justDown {
		t.Fatal("should report just opened circuit")
	}

	if hm.IsHealthy("backend-a") {
		t.Fatal("should be unavailable after failure")
	}

	justDown = hm.ReportFailure("backend-a")
	if justDown {
		t.Fatal("should not report just opened on subsequent failure")
	}
}

func TestHealthManagerPassiveRecoveryBySuccess(t *testing.T) {
	hm := newTestHealthManager()

	hm.ReportFailure("backend-a")
	if hm.IsHealthy("backend-a") {
		t.Fatal("should be unavailable")
	}

	hm.ReportSuccess("backend-a")
	if !hm.IsHealthy("backend-a") {
		t.Fatal("should be available after success")
	}
	if hm.CircuitState("backend-a") != "healthy" {
		t.Fatalf("expected healthy state, got %s", hm.CircuitState("backend-a"))
	}
}

func TestHealthManagerPassiveRecoveryByTimeout(t *testing.T) {
	now := time.Now()
	hm := newTestHealthManager()
	hm.nowFunc = func() time.Time { return now }

	hm.ReportFailure("backend-a")
	if hm.IsHealthy("backend-a") {
		t.Fatal("should be unavailable immediately after failure")
	}

	hm.nowFunc = func() time.Time { return now.Add(31 * time.Second) }

	if !hm.IsHealthy("backend-a") {
		t.Fatal("should allow probe after recovery timeout")
	}
	if hm.CircuitState("backend-a") != "probing" {
		t.Fatalf("expected probing state, got %s", hm.CircuitState("backend-a"))
	}
}

func TestHealthManagerFailThreshold(t *testing.T) {
	hm := NewHealthManager(HealthManagerConfig{
		FailThreshold:       3,
		RecoveryTimeout:     30 * time.Second,
		HalfOpenMaxRequests: 1,
	}, http.DefaultClient, logging.NewDebugLog(false, ""))

	hm.ReportFailure("backend-a")
	if !hm.IsHealthy("backend-a") {
		t.Fatal("should still be available after 1 failure (threshold=3)")
	}

	hm.ReportFailure("backend-a")
	if !hm.IsHealthy("backend-a") {
		t.Fatal("should still be available after 2 failures (threshold=3)")
	}

	justDown := hm.ReportFailure("backend-a")
	if !justDown {
		t.Fatal("should report just opened circuit on 3rd failure")
	}
	if hm.IsHealthy("backend-a") {
		t.Fatal("should be unavailable after 3 failures")
	}
	if hm.CircuitState("backend-a") != "tripped" {
		t.Fatalf("expected tripped state, got %s", hm.CircuitState("backend-a"))
	}
}

func TestHealthManagerHalfOpenProbeLimit(t *testing.T) {
	now := time.Now()
	hm := newTestHealthManager()
	hm.nowFunc = func() time.Time { return now }

	hm.ReportFailure("backend-a")
	hm.nowFunc = func() time.Time { return now.Add(31 * time.Second) }

	if !hm.TryAcquireProbe("backend-a") {
		t.Fatal("first half-open probe should be allowed")
	}
	if hm.TryAcquireProbe("backend-a") {
		t.Fatal("second half-open probe should be blocked")
	}

	hm.ReleaseProbe("backend-a")
	if !hm.TryAcquireProbe("backend-a") {
		t.Fatal("probe should be allowed after release")
	}
}

func TestHealthManagerHalfOpenFailureReopensCircuit(t *testing.T) {
	now := time.Now()
	hm := newTestHealthManager()
	hm.nowFunc = func() time.Time { return now }

	hm.ReportFailure("backend-a")
	hm.nowFunc = func() time.Time { return now.Add(31 * time.Second) }

	if !hm.TryAcquireProbe("backend-a") {
		t.Fatal("probe should be allowed in half-open")
	}

	justDown := hm.ReportFailure("backend-a")
	if !justDown {
		t.Fatal("half-open failure should reopen circuit")
	}
	if hm.CircuitState("backend-a") != "tripped" {
		t.Fatalf("expected tripped state, got %s", hm.CircuitState("backend-a"))
	}
	if hm.IsHealthy("backend-a") {
		t.Fatal("backend should be unavailable after half-open failure")
	}
}

func TestHealthManagerActiveCheck(t *testing.T) {
	var checkCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkCount.Add(1)
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	hm := NewHealthManager(HealthManagerConfig{
		FailThreshold:       1,
		RecoveryTimeout:     30 * time.Second,
		HalfOpenMaxRequests: 1,
	}, ts.Client(), logging.NewDebugLog(false, ""))

	hm.ReportFailure("test-backend")

	backends := []config.Backend{{
		Name: "test-backend",
		URL:  ts.URL,
		HealthCheck: &config.HealthCheckConfig{
			Path:     "/healthz",
			Interval: "50ms",
		},
	}}

	hm.StartActiveChecks(backends)
	defer hm.Stop()

	time.Sleep(200 * time.Millisecond)

	if checkCount.Load() == 0 {
		t.Fatal("expected at least one health check")
	}

	if !hm.IsHealthy("test-backend") {
		t.Fatal("backend should be recovered by active health check")
	}
	if hm.CircuitState("test-backend") != "healthy" {
		t.Fatalf("expected healthy state after recovery, got %s", hm.CircuitState("test-backend"))
	}
}

func TestHealthManagerConcurrency(t *testing.T) {
	hm := newTestHealthManager()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			hm.ReportFailure("backend-a")
		}()
		go func() {
			defer wg.Done()
			hm.ReportSuccess("backend-a")
		}()
		go func() {
			defer wg.Done()
			hm.IsHealthy("backend-a")
		}()
		go func() {
			defer wg.Done()
			hm.TryAcquireProbe("backend-a")
			hm.ReleaseProbe("backend-a")
		}()
	}

	wg.Wait()
}
