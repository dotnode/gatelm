package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestPromObserverSanitizeHelpers(t *testing.T) {
	if got := sanitizeProtocol("OPENAI"); got != "openai" {
		t.Fatalf("sanitizeProtocol openai = %s", got)
	}
	if got := sanitizeProtocol("openai-responses"); got != "openai-responses" {
		t.Fatalf("sanitizeProtocol openai-responses = %s", got)
	}
	if got := sanitizeProtocol("something-else"); got != "unknown" {
		t.Fatalf("sanitizeProtocol unknown = %s", got)
	}
	if got := sanitizeBackend(" "); got != "none" {
		t.Fatalf("sanitizeBackend empty = %s", got)
	}
	if got := sanitizeOutcome("ok"); got != "failure" {
		t.Fatalf("sanitizeOutcome fallback = %s", got)
	}
	if got := sanitizeResult("success"); got != "success" {
		t.Fatalf("sanitizeResult success = %s", got)
	}
	if got := statusClass(503); got != "5xx" {
		t.Fatalf("statusClass 503 = %s", got)
	}
	if got := sanitizeErrorCategory("HTTP_429"); got != "http_429" {
		t.Fatalf("sanitizeErrorCategory known = %s", got)
	}
	if got := sanitizeErrorCategory("dynamic-error"); got != "unknown" {
		t.Fatalf("sanitizeErrorCategory unknown = %s", got)
	}
}

func TestPromObserverObserve(t *testing.T) {
	observer, _, err := NewPrometheusObserver()
	if err != nil {
		t.Fatalf("NewPrometheusObserver failed: %v", err)
	}
	po, ok := observer.(*promObserver)
	if !ok {
		t.Fatal("observer type assertion failed")
	}

	po.ObserveAttempt(AttemptMetric{Backend: "b1", Outcome: "failure", ErrorCategory: "http_5xx"})
	po.ObserveAttempt(AttemptMetric{Backend: "b1", Outcome: "success", ErrorCategory: ""})

	po.ObserveRequest(RequestMetric{
		ClientProtocol: "openai",
		Backend:        "b1",
		Result:         "success",
		StatusCode:     200,
		ErrorCategory:  "success",
		Duration:       120 * time.Millisecond,
		RetryCount:     1,
	})
	po.ObserveRequest(RequestMetric{
		ClientProtocol: "openai-responses",
		Backend:        "b2",
		Result:         "success",
		StatusCode:     200,
		ErrorCategory:  "success",
		Duration:       5 * time.Millisecond,
		RetryCount:     0,
	})

	if got := counterValue(po.backendAttempts.WithLabelValues("b1", "failure", "http_5xx")); got != 1 {
		t.Fatalf("backendAttempts failure = %v", got)
	}
	if got := counterValue(po.backendAttempts.WithLabelValues("b1", "success", "success")); got != 1 {
		t.Fatalf("backendAttempts success = %v", got)
	}
	if got := counterValue(po.requestsTotal.WithLabelValues("openai", "b1", "success", "2xx", "success")); got != 1 {
		t.Fatalf("requestsTotal success = %v", got)
	}
	if got := counterValue(po.requestsTotal.WithLabelValues("openai-responses", "b2", "success", "2xx", "success")); got != 1 {
		t.Fatalf("requestsTotal openai-responses = %v", got)
	}
	if got := histogramCount(po.requestRetries); got != 2 {
		t.Fatalf("requestRetries count = %d", got)
	}
}

func TestNewPrometheusObserverHandler(t *testing.T) {
	observer, handler, err := NewPrometheusObserver()
	if err != nil {
		t.Fatalf("NewPrometheusObserver failed: %v", err)
	}
	if handler == nil {
		t.Fatal("metrics handler should not be nil")
	}

	observer.ObserveRequest(RequestMetric{
		ClientProtocol: "openai",
		Backend:        "b1",
		Result:         "success",
		StatusCode:     200,
		ErrorCategory:  "success",
		Duration:       10 * time.Millisecond,
		RetryCount:     0,
	})

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "gatelm_requests_total") {
		t.Fatal("metrics output should contain gatelm_requests_total")
	}
	if !strings.Contains(body, "gatelm_request_duration_seconds") {
		t.Fatal("metrics output should contain gatelm_request_duration_seconds")
	}
}

func counterValue(counter prometheus.Counter) float64 {
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		return -1
	}
	return metric.GetCounter().GetValue()
}

func histogramCount(histogram prometheus.Histogram) uint64 {
	metric := &dto.Metric{}
	if err := histogram.Write(metric); err != nil {
		return 0
	}
	return metric.GetHistogram().GetSampleCount()
}
