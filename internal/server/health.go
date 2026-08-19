package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

type circuitState string

const (
	circuitStateHealthy circuitState = "healthy"
	circuitStateTripped circuitState = "tripped"
	circuitStateProbing circuitState = "probing"
)

type backendHealth struct {
	mu               sync.RWMutex
	state            circuitState
	consecutiveFails int
	lastFailTime     time.Time
	halfOpenInFlight int
}

type HealthManagerConfig struct {
	FailThreshold       int
	RecoveryTimeout     time.Duration
	HalfOpenMaxRequests int
}

type HealthManager struct {
	mu       sync.RWMutex
	backends map[string]*backendHealth
	cfg      HealthManagerConfig
	client   *http.Client
	debug    *logging.DebugLog
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	nowFunc  func() time.Time // for testing
}

type BackendHealthSnapshot struct {
	Name              string    `json:"name"`
	Enabled           bool      `json:"enabled"`
	State             string    `json:"state"`
	ConsecutiveFails  int       `json:"consecutive_fails"`
	LastFailTime      time.Time `json:"last_fail_time,omitempty"`
	HalfOpenInFlight  int       `json:"half_open_in_flight"`
	Priority          int       `json:"priority"`
	Weight            int       `json:"weight"`
	Protocol          string    `json:"protocol"`
	URL               string    `json:"url"`
	PathPrefix        string    `json:"path_prefix,omitempty"`
	ActiveHealthCheck bool      `json:"active_health_check"`
}

func NewHealthManager(cfg HealthManagerConfig, client *http.Client, debug *logging.DebugLog) *HealthManager {
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = 1
	}
	if cfg.RecoveryTimeout <= 0 {
		cfg.RecoveryTimeout = 30 * time.Second
	}
	if cfg.HalfOpenMaxRequests <= 0 {
		cfg.HalfOpenMaxRequests = 1
	}
	return &HealthManager{
		backends: make(map[string]*backendHealth),
		cfg:      cfg,
		client:   client,
		debug:    debug,
		stopCh:   make(chan struct{}),
		nowFunc:  time.Now,
	}
}

// adoptState copies circuit-breaker state (state, consecutiveFails,
// lastFailTime, halfOpenInFlight) from old into hm for every backend name old
// knows about. Used on config reload: a fresh HealthManager otherwise starts
// every backend as healthy, which would silently un-trip a circuit breaker
// for a backend that's still actively failing just because an admin saved an
// unrelated config change.
func (hm *HealthManager) adoptState(old *HealthManager) {
	if old == nil {
		return
	}
	old.mu.RLock()
	names := make([]string, 0, len(old.backends))
	for name := range old.backends {
		names = append(names, name)
	}
	old.mu.RUnlock()

	for _, name := range names {
		old.mu.RLock()
		oldBH := old.backends[name]
		old.mu.RUnlock()
		if oldBH == nil {
			continue
		}
		oldBH.mu.RLock()
		state := oldBH.state
		consecutiveFails := oldBH.consecutiveFails
		lastFailTime := oldBH.lastFailTime
		halfOpenInFlight := oldBH.halfOpenInFlight
		oldBH.mu.RUnlock()

		newBH := hm.getOrCreate(name)
		newBH.mu.Lock()
		newBH.state = state
		newBH.consecutiveFails = consecutiveFails
		newBH.lastFailTime = lastFailTime
		newBH.halfOpenInFlight = halfOpenInFlight
		newBH.mu.Unlock()
	}
}

// getOrCreate returns the backendHealth for the given backend name, creating it if needed.
// Uses double-check locking pattern: RLock for fast path, Lock for creation path with second check.
func (hm *HealthManager) getOrCreate(name string) *backendHealth {
	hm.mu.RLock()
	bh, exists := hm.backends[name]
	hm.mu.RUnlock()
	if exists {
		return bh
	}

	hm.mu.Lock()
	defer hm.mu.Unlock()
	// Double check after write lock
	if bh, exists = hm.backends[name]; exists {
		return bh
	}
	bh = &backendHealth{state: circuitStateHealthy}
	hm.backends[name] = bh
	return bh
}

func (hm *HealthManager) now() time.Time {
	if hm.nowFunc != nil {
		return hm.nowFunc()
	}
	return time.Now()
}

func (hm *HealthManager) maybeMoveToHalfOpenLocked(bh *backendHealth, now time.Time) {
	if bh.state != circuitStateTripped {
		return
	}
	if now.Sub(bh.lastFailTime) < hm.cfg.RecoveryTimeout {
		return
	}
	bh.state = circuitStateProbing
	bh.consecutiveFails = 0
	bh.halfOpenInFlight = 0
}

// IsHealthy returns whether the backend can currently receive traffic.
// - healthy: healthy
// - tripped: unhealthy until RecoveryTimeout passes
// - probing: healthy only while probe slots are available
func (hm *HealthManager) IsHealthy(name string) bool {
	bh := hm.getOrCreate(name)

	bh.mu.Lock()
	defer bh.mu.Unlock()

	hm.maybeMoveToHalfOpenLocked(bh, hm.now())
	switch bh.state {
	case circuitStateHealthy:
		return true
	case circuitStateProbing:
		return bh.halfOpenInFlight < hm.cfg.HalfOpenMaxRequests
	default:
		return false
	}
}

// TryAcquireProbe reserves one probe slot for probing backends.
// Healthy backends always allow requests.
func (hm *HealthManager) TryAcquireProbe(name string) bool {
	bh := hm.getOrCreate(name)

	bh.mu.Lock()
	defer bh.mu.Unlock()

	hm.maybeMoveToHalfOpenLocked(bh, hm.now())
	switch bh.state {
	case circuitStateHealthy:
		return true
	case circuitStateProbing:
		if bh.halfOpenInFlight >= hm.cfg.HalfOpenMaxRequests {
			return false
		}
		bh.halfOpenInFlight++
		return true
	default:
		return false
	}
}

// ReleaseProbe releases one probing slot when the request result should not
// affect circuit state (for example client-side 4xx or canceled requests).
func (hm *HealthManager) ReleaseProbe(name string) {
	bh := hm.getOrCreate(name)

	bh.mu.Lock()
	defer bh.mu.Unlock()

	if bh.state == circuitStateProbing && bh.halfOpenInFlight > 0 {
		bh.halfOpenInFlight--
	}
}

// CircuitState returns current circuit breaker state for a backend.
func (hm *HealthManager) CircuitState(name string) string {
	bh := hm.getOrCreate(name)

	bh.mu.Lock()
	defer bh.mu.Unlock()

	hm.maybeMoveToHalfOpenLocked(bh, hm.now())
	return string(bh.state)
}

// ReportSuccess marks a backend as healthy and resets any tripped circuit.
func (hm *HealthManager) ReportSuccess(name string) {
	bh := hm.getOrCreate(name)
	bh.mu.Lock()
	defer bh.mu.Unlock()

	bh.state = circuitStateHealthy
	bh.consecutiveFails = 0
	bh.halfOpenInFlight = 0
	bh.lastFailTime = time.Time{}
}

// ReportFailure records a failure. Returns true if the backend just transitioned to tripped.
func (hm *HealthManager) ReportFailure(name string) bool {
	bh := hm.getOrCreate(name)
	bh.mu.Lock()
	defer bh.mu.Unlock()

	now := hm.now()
	prevState := bh.state

	switch bh.state {
	case circuitStateHealthy:
		bh.consecutiveFails++
		bh.lastFailTime = now
		if bh.consecutiveFails >= hm.cfg.FailThreshold {
			bh.state = circuitStateTripped
			bh.halfOpenInFlight = 0
		}
	case circuitStateProbing:
		bh.state = circuitStateTripped
		bh.lastFailTime = now
		bh.consecutiveFails = hm.cfg.FailThreshold
		bh.halfOpenInFlight = 0
	case circuitStateTripped:
		// Keep tripped and extend cooldown window.
		bh.lastFailTime = now
	}

	return prevState != circuitStateTripped && bh.state == circuitStateTripped
}

// StartActiveChecks launches goroutines for backends with health_check configured.
// It also initializes health state for every enabled backend so that AllBackendsDown
// reports accurately even when some backends have not served any traffic yet.
func (hm *HealthManager) StartActiveChecks(backends []config.Backend) {
	for i := range backends {
		b := &backends[i]
		if !config.BackendEnabled(b) {
			continue
		}

		// Ensure health state exists for every enabled backend, even without active checks.
		hm.getOrCreate(b.Name)

		if b.HealthCheck == nil || b.HealthCheck.Path == "" {
			continue
		}

		interval := 30 * time.Second
		if b.HealthCheck.Interval != "" {
			if d, err := time.ParseDuration(b.HealthCheck.Interval); err == nil && d > 0 {
				interval = d
			}
		}

		hm.wg.Add(1)
		go hm.activeCheckLoop(b, interval)
	}
}

func (hm *HealthManager) activeCheckLoop(backend *config.Backend, interval time.Duration) {
	defer hm.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	checkURL := strings.TrimRight(backend.URL, "/") + backend.HealthCheck.Path

	for {
		select {
		case <-hm.stopCh:
			return
		case <-ticker.C:
			hm.doActiveCheck(backend, checkURL)
		}
	}
}

func (hm *HealthManager) doActiveCheck(backend *config.Backend, checkURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", checkURL, nil)
	if err != nil {
		return
	}

	applyBackendHeaders(req.Header, backend)

	resp, err := hm.client.Do(req)
	if err != nil {
		justDown := hm.ReportFailure(backend.Name)
		if justDown {
			hm.debug.Printf("health check: %s marked unhealthy: %v", backend.Name, err)
		}
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		wasDegraded := hm.CircuitState(backend.Name) != string(circuitStateHealthy)
		hm.ReportSuccess(backend.Name)
		if wasDegraded {
			hm.debug.Printf("health check: %s recovered (status=%d)", backend.Name, resp.StatusCode)
		}
	} else {
		justDown := hm.ReportFailure(backend.Name)
		if justDown {
			hm.debug.Printf("health check: %s marked unhealthy (status=%d)", backend.Name, resp.StatusCode)
		}
	}
}

func (hm *HealthManager) Snapshot(backends []config.Backend) []BackendHealthSnapshot {
	result := make([]BackendHealthSnapshot, 0, len(backends))
	for i := range backends {
		b := backends[i]
		enabled := config.BackendEnabled(&b)
		bh := hm.getOrCreate(b.Name)
		bh.mu.Lock()
		hm.maybeMoveToHalfOpenLocked(bh, hm.now())
		state := string(bh.state)
		if !enabled {
			state = "disabled"
		}
		snapshot := BackendHealthSnapshot{
			Name:              b.Name,
			Enabled:           enabled,
			State:             state,
			ConsecutiveFails:  bh.consecutiveFails,
			LastFailTime:      bh.lastFailTime,
			HalfOpenInFlight:  bh.halfOpenInFlight,
			Priority:          b.Priority,
			Weight:            b.Weight,
			Protocol:          b.Protocol,
			URL:               b.URL,
			PathPrefix:        b.PathPrefix,
			ActiveHealthCheck: enabled && b.HealthCheck != nil,
		}
		bh.mu.Unlock()
		result = append(result, snapshot)
	}
	return result
}

// Stop shuts down all active check goroutines. Safe to call more than once
// (e.g. if a caller Close()s the server directly as well as through a
// wrapper that already stopped it) — only the first call closes stopCh.
func (hm *HealthManager) Stop() {
	hm.stopOnce.Do(func() {
		close(hm.stopCh)
		hm.wg.Wait()
	})
}

// AllBackendsDown returns true if all known backends are in tripped state.
func (hm *HealthManager) AllBackendsDown() bool {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	if len(hm.backends) == 0 {
		return false
	}
	now := hm.now()
	for _, bh := range hm.backends {
		bh.mu.Lock()
		hm.maybeMoveToHalfOpenLocked(bh, now)
		state := bh.state
		bh.mu.Unlock()
		if state != circuitStateTripped {
			return false
		}
	}
	return true
}
