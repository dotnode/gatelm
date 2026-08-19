package server

import (
	"net/http"
	"net/http/httptest"
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

// Stop() must be safe to call more than once — a caller closing the server
// through more than one path (e.g. direct Server.Close() plus a wrapper that
// already stopped it) must not panic on a double close(stopCh).
func TestHealthManagerStopIsIdempotent(t *testing.T) {
	hm := newTestHealthManager()
	hm.Stop()
	hm.Stop()
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

func TestActiveHealthChecksSkipDisabledBackend(t *testing.T) {
	var checkCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkCount.Add(1)
		w.WriteHeader(200)
	}))
	defer server.Close()

	disabled := false
	hm := NewHealthManager(HealthManagerConfig{}, server.Client(), logging.NewDebugLog(false, ""))
	hm.StartActiveChecks([]config.Backend{{
		Name:    "disabled-backend",
		URL:     server.URL,
		Enabled: &disabled,
		HealthCheck: &config.HealthCheckConfig{
			Path:     "/healthz",
			Interval: "20ms",
		},
	}})
	defer hm.Stop()

	time.Sleep(80 * time.Millisecond)
	if checkCount.Load() != 0 {
		t.Fatalf("expected disabled backend to skip active health checks, got %d", checkCount.Load())
	}

	snapshots := hm.Snapshot([]config.Backend{{Name: "disabled-backend", URL: server.URL, Enabled: &disabled}})
	if len(snapshots) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Enabled {
		t.Fatal("expected disabled snapshot")
	}
	if snapshots[0].State != "disabled" {
		t.Fatalf("expected disabled state, got %s", snapshots[0].State)
	}
	if snapshots[0].ActiveHealthCheck {
		t.Fatal("expected active health check to be false for disabled backend")
	}
}

// StartActiveChecks must seed health state for every enabled backend, even
// those without an explicit health_check block. Otherwise AllBackendsDown
// would only consider backends that happen to be in the map (e.g. one that
// reported a failure), and report all-down while other enabled backends are
// actually fine.
func TestStartActiveChecksSeedsHealthStateForAllEnabled(t *testing.T) {
	hm := NewHealthManager(HealthManagerConfig{}, http.DefaultClient, logging.NewDebugLog(false, ""))
	backends := []config.Backend{
		{Name: "b1", URL: "http://a", Protocol: "openai"},
		{Name: "b2", URL: "http://b", Protocol: "openai"},
	}
	hm.StartActiveChecks(backends)
	defer hm.Stop()

	// Only b1 has ever failed; b2 was never touched by request traffic.
	hm.ReportFailure("b1")

	if hm.AllBackendsDown() {
		t.Fatal("expected AllBackendsDown=false when b2 is still healthy")
	}

	hm.ReportFailure("b2")
	if !hm.AllBackendsDown() {
		t.Fatal("expected AllBackendsDown=true after both backends tripped")
	}
}
