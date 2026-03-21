package server

import "sync"

type requestEvent struct {
	ClientProtocol string
	Backend        string
	Result         string
	StatusCode     int
	ErrorCategory  string
	RetryCount     int
}

type attemptEvent struct {
	Backend       string
	Outcome       string
	ErrorCategory string
}

type captureObserver struct {
	mu       sync.Mutex
	requests []requestEvent
	attempts []attemptEvent
}

func newCaptureObserver() *captureObserver {
	return &captureObserver{}
}

func (o *captureObserver) ObserveAttempt(metric AttemptMetric) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts = append(o.attempts, attemptEvent{
		Backend:       metric.Backend,
		Outcome:       metric.Outcome,
		ErrorCategory: metric.ErrorCategory,
	})
}

func (o *captureObserver) ObserveRequest(metric RequestMetric) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, requestEvent{
		ClientProtocol: metric.ClientProtocol,
		Backend:        metric.Backend,
		Result:         metric.Result,
		StatusCode:     metric.StatusCode,
		ErrorCategory:  metric.ErrorCategory,
		RetryCount:     metric.RetryCount,
	})
}

func (o *captureObserver) latestRequest() (requestEvent, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.requests) == 0 {
		return requestEvent{}, false
	}
	return o.requests[len(o.requests)-1], true
}

func (o *captureObserver) allAttempts() []attemptEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	items := make([]attemptEvent, len(o.attempts))
	copy(items, o.attempts)
	return items
}
