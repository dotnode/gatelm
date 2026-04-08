package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type AttemptMetric struct {
	Backend       string
	Outcome       string
	ErrorCategory string
}

type RequestMetric struct {
	ClientProtocol string
	Backend        string
	Result         string
	StatusCode     int
	ErrorCategory  string
	Duration       time.Duration
	RetryCount     int
}

type Observer interface {
	ObserveAttempt(metric AttemptMetric)
	ObserveRequest(metric RequestMetric)
}

type noopObserver struct{}

func NewNoopObserver() Observer {
	return noopObserver{}
}

func (noopObserver) ObserveAttempt(AttemptMetric) {}
func (noopObserver) ObserveRequest(RequestMetric) {}

type promObserver struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	backendAttempts *prometheus.CounterVec
	requestRetries  prometheus.Histogram
}

func NewPrometheusObserver() (Observer, http.Handler, error) {
	obs := &promObserver{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gatelm_requests_total",
				Help: "Total number of proxied requests by final result.",
			},
			[]string{"client_protocol", "backend", "result", "status_class", "error_category"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "gatelm_request_duration_seconds",
				Help:    "End-to-end proxied request duration in seconds.",
				Buckets: []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10},
			},
			[]string{"client_protocol", "backend", "result"},
		),
		backendAttempts: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gatelm_backend_attempts_total",
				Help: "Total backend forward attempts grouped by outcome and error category.",
			},
			[]string{"backend", "outcome", "error_category"},
		),
		requestRetries: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "gatelm_request_retries",
				Help:    "Number of retries per request.",
				Buckets: []float64{0, 1, 2, 3, 5, 8},
			},
		),
	}

	registry := prometheus.NewRegistry()
	collectors := []prometheus.Collector{
		obs.requestsTotal,
		obs.requestDuration,
		obs.backendAttempts,
		obs.requestRetries,
	}
	for _, collector := range collectors {
		if err := registry.Register(collector); err != nil {
			return nil, nil, err
		}
	}

	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return obs, handler, nil
}

func (o *promObserver) ObserveAttempt(metric AttemptMetric) {
	backend := sanitizeBackend(metric.Backend)
	outcome := sanitizeOutcome(metric.Outcome)
	category := sanitizeErrorCategory(metric.ErrorCategory)
	if outcome == "success" {
		category = "success"
	}
	o.backendAttempts.WithLabelValues(backend, outcome, category).Inc()
}

func (o *promObserver) ObserveRequest(metric RequestMetric) {
	protocol := sanitizeProtocol(metric.ClientProtocol)
	backend := sanitizeBackend(metric.Backend)
	result := sanitizeResult(metric.Result)
	category := sanitizeErrorCategory(metric.ErrorCategory)
	if result == "success" {
		category = "success"
	}

	o.requestsTotal.WithLabelValues(
		protocol,
		backend,
		result,
		statusClass(metric.StatusCode),
		category,
	).Inc()

	duration := metric.Duration.Seconds()
	if duration < 0 {
		duration = 0
	}
	o.requestDuration.WithLabelValues(protocol, backend, result).Observe(duration)

	retries := metric.RetryCount
	if retries < 0 {
		retries = 0
	}
	o.requestRetries.Observe(float64(retries))
}

func sanitizeProtocol(v string) string {
	p := strings.ToLower(strings.TrimSpace(v))
	switch p {
	case "openai", "openai-responses", "anthropic":
		return p
	default:
		return "unknown"
	}
}

func sanitizeBackend(v string) string {
	b := strings.TrimSpace(v)
	if b == "" {
		return "none"
	}
	return b
}

func sanitizeOutcome(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "success") {
		return "success"
	}
	return "failure"
}

func sanitizeResult(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "success") {
		return "success"
	}
	return "failure"
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	case code >= 100:
		return "1xx"
	default:
		return "none"
	}
}

var allowedErrorCategories = map[string]struct{}{
	"success":              {},
	"network_error":        {},
	"http_429":             {},
	"http_5xx":             {},
	"http_4xx":             {},
	"request_error":        {},
	"no_backend":           {},
	"upstream_failed":      {},
	"request_too_large":    {},
	"read_request_error":   {},
	"read_upstream_error":  {},
	"circuit_tripped":      {},
	"request_canceled":     {},
	"write_response_error": {},
	"model_not_found":      {},
}

func sanitizeErrorCategory(v string) string {
	category := strings.ToLower(strings.TrimSpace(v))
	if category == "" {
		return "unknown"
	}
	if _, ok := allowedErrorCategories[category]; ok {
		return category
	}
	return "unknown"
}
