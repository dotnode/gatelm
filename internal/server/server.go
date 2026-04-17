package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

const maxRequestBodySize = 32 << 20   // 32 MB
const maxStreamLogSize = 32 * 1024    // 32 KB
const maxResponseBodySize = 100 << 20 // 100 MB – caps fallback (non-flusher) response reads

type Server struct {
	mu              sync.RWMutex
	Cfg             config.Config
	ConfigPath      string
	ModelIndex      *ModelIndex
	TokenLog        *logging.TokenLogger
	Debug           *logging.DebugLog
	Client          *http.Client
	Health          *HealthManager
	Selector        *BackendSelector
	Observer        Observer
	Codecs          map[string]ProtocolCodec // protocol name → codec
	loginAttempts   sync.Map                 // ip -> *loginAttempt (for console rate limiting)
	sessions        sync.Map                 // token -> *sessionEntry (for console sessions)
	cleanupStop     chan struct{}            // signals the cleanup goroutine to exit
	cleanupDone     chan struct{}            // closed when the cleanup goroutine exits
	cleanupStopOnce sync.Once                // guards close(cleanupStop) against double-close
}

func shouldNormalizeReasoningEffortAlias(protocol string, normalize bool, body []byte) []byte {
	if !normalize || !isOpenAIFamily(protocol) {
		return body
	}
	return normalizeReasoningEffortAliasInBody(body, protocol)
}

func debugConvertedRequestSummary(body []byte) string {
	if len(body) == 0 {
		return "empty body"
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Sprintf("invalid json: %v", err)
	}

	parts := make([]string, 0, 8)
	if model, _ := payload["model"].(string); model != "" {
		parts = append(parts, fmt.Sprintf("model=%s", model))
	}
	if stream, ok := payload["stream"].(bool); ok {
		parts = append(parts, fmt.Sprintf("stream=%t", stream))
	}
	if effort, _ := payload["reasoning_effort"].(string); effort != "" {
		parts = append(parts, fmt.Sprintf("reasoning_effort=%s", effort))
	} else if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if effort, _ := reasoning["effort"].(string); effort != "" {
			parts = append(parts, fmt.Sprintf("reasoning_effort=%s", effort))
		}
	}
	if toolChoice, ok := payload["tool_choice"]; ok {
		parts = append(parts, fmt.Sprintf("tool_choice=%s", debugToolChoiceSummary(toolChoice)))
	} else {
		parts = append(parts, "tool_choice=<unset>")
	}
	toolCount, toolNames := debugToolsSummary(payload["tools"])
	parts = append(parts, fmt.Sprintf("tools_count=%d", toolCount))
	if len(toolNames) > 0 {
		parts = append(parts, fmt.Sprintf("tools=%s", strings.Join(toolNames, ",")))
	}
	return strings.Join(parts, " ")
}

func debugToolChoiceSummary(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		typeName, _ := v["type"].(string)
		name, _ := v["name"].(string)
		if name == "" {
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		if typeName == "" && name == "" {
			return "object"
		}
		if name != "" {
			return fmt.Sprintf("%s:%s", typeName, name)
		}
		return typeName
	default:
		return fmt.Sprintf("%T", value)
	}
}

func debugToolsSummary(value any) (int, []string) {
	tools, ok := value.([]any)
	if !ok {
		return 0, nil
	}
	if len(tools) == 0 {
		return 0, nil
	}

	nameSet := make(map[string]struct{})
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		if strings.TrimSpace(name) != "" {
			nameSet[name] = struct{}{}
		}
	}

	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 3 {
		names = names[:3]
	}
	return len(tools), names
}

func New(cfg config.Config, tokenLog *logging.TokenLogger, debug *logging.DebugLog, client *http.Client, health *HealthManager, observer Observer) *Server {
	if observer == nil {
		observer = NewNoopObserver()
	}
	s := &Server{
		Cfg:         cfg,
		ModelIndex:  BuildModelIndex(cfg.Backends),
		TokenLog:    tokenLog,
		Debug:       debug,
		Client:      client,
		Health:      health,
		Selector:    NewBackendSelector(health),
		Observer:    observer,
		Codecs:      buildCodecs(debug),
		cleanupStop: make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

func (s *Server) SetConfigPath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ConfigPath = path
}

func (s *Server) snapshot() serverSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return serverSnapshot{
		cfg:        s.Cfg,
		modelIndex: s.ModelIndex,
		tokenLog:   s.TokenLog,
		client:     s.Client,
		health:     s.Health,
		selector:   s.Selector,
		observer:   s.Observer,
		configPath: s.ConfigPath,
	}
}

type serverSnapshot struct {
	cfg        config.Config
	modelIndex *ModelIndex
	tokenLog   *logging.TokenLogger
	client     *http.Client
	health     *HealthManager
	selector   *BackendSelector
	observer   Observer
	configPath string
}

func generateRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// readRequestBody reads the request body with a size limit and handles errors.
// Returns the body bytes, or writes an error response and returns nil.
func (s *Server) readRequestBody(w http.ResponseWriter, r *http.Request, backendName, backendProtocol, clientProtocol, keyLabel, reqID string, start time.Time) []byte {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			s.logUsage(logging.UsageLog{
				Backend:         backendName,
				ClientProtocol:  clientProtocol,
				BackendProtocol: backendProtocol,
				ClientKey:       keyLabel,
				StatusCode:      http.StatusRequestEntityTooLarge,
				Error:           "request body too large",
			}, start, reqID, 0, "request_too_large")
			return nil
		}
		http.Error(w, "read request body failed", http.StatusBadRequest)
		s.logUsage(logging.UsageLog{
			Backend:         backendName,
			ClientProtocol:  clientProtocol,
			BackendProtocol: backendProtocol,
			ClientKey:       keyLabel,
			StatusCode:      http.StatusBadRequest,
			Error:           err.Error(),
		}, start, reqID, 0, "read_request_error")
		return nil
	}
	_ = r.Body.Close()
	s.Debug.Headers("request", r.Header)
	s.Debug.Body("request", bodyBytes)
	return bodyBytes
}

// resolvedModelConfig holds resolved per-model configuration after merging
// model_defaults and per-backend model settings.
type resolvedModelConfig struct {
	reasoningEffort               string
	systemPrompt                  string
	defaultMaxTokens              int
	normalizeXHighReasoningEffort bool
	defaultTemperature            *float64
}

// applyModelDefaultConfig applies non-zero fields from a ModelDefaultConfig onto
// a resolvedModelConfig. The special value "none" for SystemPrompt explicitly
// clears the system prompt (prevents inheriting from upper layers).
func applyModelDefaultConfig(res *resolvedModelConfig, mc config.ModelDefaultConfig) {
	if mc.ReasoningEffort != "" {
		res.reasoningEffort = mc.ReasoningEffort
	}
	if mc.SystemPrompt != "" {
		if mc.SystemPrompt == "none" {
			res.systemPrompt = ""
		} else {
			res.systemPrompt = mc.SystemPrompt
		}
	}
	if mc.MaxTokens > 0 {
		res.defaultMaxTokens = mc.MaxTokens
	}
	if mc.NormalizeXHighReasoningEffort {
		res.normalizeXHighReasoningEffort = true
	}
	if mc.DefaultTemperature != nil {
		res.defaultTemperature = mc.DefaultTemperature
	}
}

// resolveModelDefaults merges model_defaults (by backend model name) with
// candidate-level settings. Lookup order (later overrides earlier):
//  1. candidate-level defaults
//  2. model_defaults["backendModel"] — generic key
//  3. model_defaults["backendModel@clientProtocol"] — protocol-specific key
func resolveModelDefaults(cfg config.Config, cand candidateResult, clientProtocol string) resolvedModelConfig {
	res := resolvedModelConfig{
		reasoningEffort:               cand.reasoningDefaultEffort,
		systemPrompt:                  cand.systemPrompt,
		defaultMaxTokens:              cand.defaultMaxTokens,
		normalizeXHighReasoningEffort: cand.normalizeXHighReasoningEffort,
		defaultTemperature:            cand.defaultTemperature,
	}

	// Generic key: model_defaults["backendModel"]
	if modelCfg, ok := cfg.ModelDefaults[cand.backendModel]; ok {
		applyModelDefaultConfig(&res, modelCfg)
	}

	// Protocol-specific key: model_defaults["backendModel@clientProtocol"]
	if clientProtocol != "" {
		protocolKey := cand.backendModel + "@" + clientProtocol
		if modelCfg, ok := cfg.ModelDefaults[protocolKey]; ok {
			applyModelDefaultConfig(&res, modelCfg)
		}
	}

	return res
}

func (s *Server) observeAttempt(backend, reason string) {
	snap := s.snapshot()
	if snap.observer == nil {
		return
	}
	outcome := "failure"
	if reason == "success" {
		outcome = "success"
	}
	snap.observer.ObserveAttempt(AttemptMetric{
		Backend:       backend,
		Outcome:       outcome,
		ErrorCategory: reason,
	})
}

func requestResult(statusCode int, errorCategory string) string {
	if statusCode >= 400 {
		return "failure"
	}
	if strings.TrimSpace(errorCategory) == "" || errorCategory == "success" {
		return "success"
	}
	return "failure"
}

func (s *Server) logUsage(entry logging.UsageLog, start time.Time, reqID string, retryCount int, errorCategory string) {
	snap := s.snapshot()
	if entry.Time == "" {
		entry.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if retryCount < 0 {
		retryCount = 0
	}
	durationMs := time.Since(start).Milliseconds()
	if durationMs < 0 {
		durationMs = 0
	}

	entry.RequestID = reqID
	entry.DurationMs = durationMs
	entry.RetryCount = retryCount
	if strings.TrimSpace(errorCategory) == "" {
		if entry.StatusCode >= 400 || entry.Error != "" {
			errorCategory = "unknown"
		} else {
			errorCategory = "success"
		}
	}
	entry.ErrorCategory = errorCategory
	if entry.Backend == "" {
		entry.Backend = "none"
	}
	if entry.BackendProtocol == "" && entry.Backend != "none" {
		for _, backend := range snap.cfg.Backends {
			if backend.Name == entry.Backend {
				entry.BackendProtocol = backend.Protocol
				break
			}
		}
	}

	if snap.tokenLog != nil {
		snap.tokenLog.Log(entry)
	}
	if snap.observer != nil {
		snap.observer.ObserveRequest(RequestMetric{
			ClientProtocol: entry.ClientProtocol,
			Backend:        entry.Backend,
			Result:         requestResult(entry.StatusCode, entry.ErrorCategory),
			StatusCode:     entry.StatusCode,
			ErrorCategory:  entry.ErrorCategory,
			Duration:       time.Duration(durationMs) * time.Millisecond,
			RetryCount:     retryCount,
		})
	}
}

func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	start := time.Now()
	reqID := generateRequestID()
	s.Debug.Printf("[%s] request: %s %s from %s", reqID, r.Method, r.URL.String(), r.RemoteAddr)

	w.Header().Set("X-Request-Id", reqID)

	clientProtocol := detectProtocolByRequest(r.URL.Path, r.Header)

	// WebSocket upgrade detection — must be before body reading
	if isWebSocketUpgrade(r) {
		s.Debug.Printf("[%s] websocket upgrade detected for %s", reqID, r.URL.Path)
		s.handleWebSocket(w, r, snap, clientProtocol, reqID, start)
		return
	}

	// Intercept /v1/models for Anthropic clients: return all model aliases
	if r.URL.Path == "/v1/models" && isAnthropicClient(r.URL.Path, r.Header) {
		s.Debug.Printf("[%s] serving model list for anthropic client", reqID)
		s.serveModelList(w)
		return
	}

	// 1. Try path prefix match first
	if backend, ok := snap.modelIndex.MatchByPrefix(r.URL.Path); ok {
		s.Debug.Printf("[%s] matched backend by prefix: %s (prefix=%s)", reqID, backend.Name, backend.PathPrefix)
		s.handleWithBackend(w, r, backend, clientProtocol, reqID, start)
		return
	}

	// 2. Read body and resolve backend by model name
	clientKey := extractClientKey(r.Header)
	keyLabel := normalizeKeyLabel(clientKey, snap.cfg.APIKeys)

	bodyBytes := s.readRequestBody(w, r, "none", "", clientProtocol, keyLabel, reqID, start)
	if bodyBytes == nil {
		return
	}

	requestModel := extractModel(bodyBytes)

	var candidates []modelEntry

	if requestModel != "" {
		entries, found := snap.modelIndex.ResolveCandidates(requestModel)
		if found {
			candidates = entries
		} else if snap.modelIndex.DefaultBackend() != nil {
			candidates = []modelEntry{{
				backend:      snap.modelIndex.DefaultBackend(),
				backendModel: requestModel,
			}}
		}
	} else {
		if snap.modelIndex.DefaultBackend() != nil {
			candidates = []modelEntry{{
				backend:      snap.modelIndex.DefaultBackend(),
				backendModel: "",
			}}
		}
	}

	if len(candidates) == 0 {
		s.Debug.Printf("[%s] no backend found for model %q", reqID, requestModel)
		http.Error(w, "no backend found for model: "+requestModel, http.StatusNotFound)
		s.logUsage(logging.UsageLog{
			Backend:        "none",
			ClientProtocol: clientProtocol,
			ClientKey:      keyLabel,
			RequestModel:   requestModel,
			StatusCode:     http.StatusNotFound,
			Error:          "no backend found for model",
		}, start, reqID, 0, "model_not_found")
		return
	}

	s.forwardWithRetry(w, r, candidates, clientProtocol, reqID, start, keyLabel, requestModel, bodyBytes)
}

// handleWithBackend handles requests matched by path prefix.
func (s *Server) handleWithBackend(w http.ResponseWriter, r *http.Request, backend *config.Backend, clientProtocol, reqID string, start time.Time) {
	snap := s.snapshot()
	clientKey := extractClientKey(r.Header)
	keyLabel := normalizeKeyLabel(clientKey, snap.cfg.APIKeys)

	bodyBytes := s.readRequestBody(w, r, backend.Name, backend.Protocol, clientProtocol, keyLabel, reqID, start)
	if bodyBytes == nil {
		return
	}

	requestModel := extractModel(bodyBytes)

	// For path-prefix matched backends, use single candidate (no retry across backends)
	candidate := modelEntry{backend: backend, backendModel: requestModel}
	if requestModel != "" {
		if entries, found := snap.modelIndex.ResolveWithinBackend(requestModel, backend); found && len(entries) > 0 {
			if entries[0].backendModel != "" {
				candidate.backendModel = entries[0].backendModel
			}
			candidate.reasoningDefaultEffort = entries[0].reasoningDefaultEffort
			candidate.systemPrompt = entries[0].systemPrompt
			candidate.defaultMaxTokens = entries[0].defaultMaxTokens
			candidate.normalizeXHighReasoningEffort = entries[0].normalizeXHighReasoningEffort
			candidate.defaultTemperature = entries[0].defaultTemperature
		}
	}
	if candidate.backendModel != "" && candidate.backendModel != requestModel {
		_, bodyBytes = replaceModelInBody(bodyBytes, candidate.backendModel)
	}

	candidates := []modelEntry{candidate}
	s.forwardWithRetry(w, r, candidates, clientProtocol, reqID, start, keyLabel, requestModel, bodyBytes)
}

// forwardWithRetry tries each healthy candidate backend in priority order.
// On connection errors or 5xx responses, it retries the next candidate.
// No bytes are written to the client until a usable response is obtained.
func (s *Server) forwardWithRetry(
	w http.ResponseWriter,
	r *http.Request,
	candidates []modelEntry,
	clientProtocol, reqID string,
	start time.Time,
	keyLabel, requestModel string,
	bodyBytes []byte,
) {
	snap := s.snapshot()
	ordered := snap.selector.SelectOrdered(candidates)
	fallbackMode := false
	if len(ordered) == 0 {
		// All backends circuit-broken; try fallback (ignore health status)
		// to avoid complete service blackout when there is no healthy alternative.
		ordered = snap.selector.SelectFallback(candidates)
		if len(ordered) == 0 {
			s.Debug.Printf("[%s] all backends unavailable for model %q", reqID, requestModel)
			http.Error(w, "all backends unavailable for model: "+requestModel, http.StatusServiceUnavailable)
			s.logUsage(logging.UsageLog{
				Backend:        "none",
				ClientProtocol: clientProtocol,
				ClientKey:      keyLabel,
				RequestModel:   requestModel,
				StatusCode:     http.StatusServiceUnavailable,
				Error:          "all backends unavailable",
			}, start, reqID, 0, "no_backend")
			return
		}
		fallbackMode = true
		s.Debug.Printf("[%s] all backends circuit-broken for model %q, trying fallback", reqID, requestModel)
	}

	type retryPlan struct {
		nextCandidate bool
		backoff       time.Duration
		reason        string
		reportableErr string
	}

	max429Backoff := 2 * time.Second

	classify := func(resp *http.Response, err error) retryPlan {
		if err != nil {
			if isRetriableNetworkError(err) {
				return retryPlan{nextCandidate: true, reason: "network_error", reportableErr: err.Error()}
			}
			return retryPlan{nextCandidate: false, reason: "request_error", reportableErr: err.Error()}
		}

		status := resp.StatusCode
		switch {
		case status == http.StatusTooManyRequests:
			backoff := parseRetryAfter(resp.Header.Get("Retry-After"))
			if backoff <= 0 {
				backoff = 200 * time.Millisecond
			}
			if backoff > max429Backoff {
				backoff = max429Backoff
			}
			return retryPlan{nextCandidate: true, backoff: backoff, reason: "http_429", reportableErr: fmt.Sprintf("backend returned %d", status)}
		case status >= 500:
			return retryPlan{nextCandidate: true, reason: "http_5xx", reportableErr: fmt.Sprintf("backend returned %d", status)}
		case status >= 400:
			return retryPlan{nextCandidate: false, reason: "http_4xx", reportableErr: fmt.Sprintf("backend returned %d", status)}
		default:
			return retryPlan{nextCandidate: false, reason: "success"}
		}
	}

	var lastErr error
	attempts := 0
	for i, cand := range ordered {
		backend := cand.backend
		backendModel := cand.backendModel

		if !fallbackMode {
			probeAcquired := snap.health.TryAcquireProbe(backend.Name)
			if !probeAcquired {
				s.Debug.Printf("[%s] skip backend %s (circuit=%s)", reqID, backend.Name, snap.health.CircuitState(backend.Name))
				continue
			}
		}

		forwardedModel := backendModel
		mc := resolveModelDefaults(snap.cfg, cand, clientProtocol)
		backendCodec := s.Codecs[backend.Protocol]
		rewrittenBody := prepareRequestBody(bodyBytes, requestModel, backendModel, backendCodec, mc)

		s.Debug.Printf("[%s] trying backend %s (attempt %d/%d, priority=%d, circuit=%s)",
			reqID, backend.Name, i+1, len(ordered), backend.Priority, snap.health.CircuitState(backend.Name))

		attempts++

		resp, err := s.tryForward(r, backend, clientProtocol, rewrittenBody, reqID, mc.reasoningEffort, mc.systemPrompt, mc.normalizeXHighReasoningEffort)
		plan := classify(resp, err)

		if err != nil {
			lastErr = err
			s.observeAttempt(backend.Name, plan.reason)
			if plan.nextCandidate {
				justDown := snap.health.ReportFailure(backend.Name)
				if justDown {
					s.Debug.Printf("[%s] backend %s circuit tripped", reqID, backend.Name)
				}
				s.Debug.Printf("[%s] backend %s failed (%s): %v", reqID, backend.Name, plan.reason, err)
				continue
			}
			snap.health.ReleaseProbe(backend.Name)
			http.Error(w, "upstream request failed", http.StatusBadGateway)
			s.logUsage(logging.UsageLog{
				Backend:         backend.Name,
				ClientProtocol:  clientProtocol,
				BackendProtocol: backend.Protocol,
				ClientKey:       keyLabel,
				RequestModel:    requestModel,
				StatusCode:      http.StatusBadGateway,
				Error:           plan.reportableErr,
			}, start, reqID, attempts-1, plan.reason)
			return
		}

		s.observeAttempt(backend.Name, plan.reason)
		if plan.nextCandidate {
			if i < len(ordered)-1 {
				resp.Body.Close()
				lastErr = errors.New(plan.reportableErr)
				justDown := snap.health.ReportFailure(backend.Name)
				if justDown {
					s.Debug.Printf("[%s] backend %s circuit tripped", reqID, backend.Name)
				}
				s.Debug.Printf("[%s] backend %s returned %d (%s), trying next", reqID, backend.Name, resp.StatusCode, plan.reason)
				if plan.backoff > 0 {
					s.Debug.Printf("[%s] backoff before next candidate: %s", reqID, plan.backoff)
					if !sleepWithContext(r.Context(), plan.backoff) {
						snap.health.ReleaseProbe(backend.Name)
						http.Error(w, "request canceled", http.StatusGatewayTimeout)
						s.logUsage(logging.UsageLog{
							Backend:         backend.Name,
							ClientProtocol:  clientProtocol,
							BackendProtocol: backend.Protocol,
							ClientKey:       keyLabel,
							RequestModel:    requestModel,
							ForwardedModel:  forwardedModel,
							StatusCode:      http.StatusGatewayTimeout,
							Error:           "request canceled",
						}, start, reqID, attempts-1, "request_canceled")
						return
					}
				}
				continue
			}
			justDown := snap.health.ReportFailure(backend.Name)
			if justDown {
				s.Debug.Printf("[%s] backend %s circuit tripped", reqID, backend.Name)
			}
			s.Debug.Printf("[%s] backend %s returned %d (%s), no more candidates", reqID, backend.Name, resp.StatusCode, plan.reason)
			s.writeResponse(w, r, resp, backend, clientProtocol, reqID, start, keyLabel, requestModel, forwardedModel, rewrittenBody, attempts-1, plan.reason)
			return
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			snap.health.ReleaseProbe(backend.Name)
		}
		snap.health.ReportSuccess(backend.Name)
		s.Debug.Printf("[%s] resolved backend: %s (protocol=%s, url=%s)", reqID, backend.Name, backend.Protocol, backend.URL)
		if forwardedModel != "" && forwardedModel != requestModel {
			s.Debug.Printf("[%s] model rewrite: %s -> %s", reqID, requestModel, forwardedModel)
		}
		s.writeResponse(w, r, resp, backend, clientProtocol, reqID, start, keyLabel, requestModel, forwardedModel, rewrittenBody, attempts-1, plan.reason)
		return
	}

	errMsg := "all backends failed"
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	category := "upstream_failed"
	if attempts == 0 {
		category = "circuit_tripped"
	}
	http.Error(w, "upstream request failed", http.StatusBadGateway)
	s.logUsage(logging.UsageLog{
		Backend:         ordered[len(ordered)-1].backend.Name,
		ClientProtocol:  clientProtocol,
		BackendProtocol: ordered[len(ordered)-1].backend.Protocol,
		ClientKey:       keyLabel,
		RequestModel:    requestModel,
		StatusCode:      http.StatusBadGateway,
		Error:           errMsg,
	}, start, reqID, attempts-1, category)
}

// tryForward sends the request to a single backend and returns the response.
// It does NOT write anything to the client's ResponseWriter.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		return d
	}
	return 0
}

func isRetriableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() || netErr.Temporary() {
			return true
		}
	}
	errMsg := strings.ToLower(err.Error())
	if strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "no such host") {
		return true
	}
	return false
}

func (s *Server) tryForward(
	r *http.Request,
	backend *config.Backend,
	clientProtocol string,
	rewrittenBody []byte,
	reqID string,
	reasoningDefaultEffort string,
	injectedSystemPrompt string,
	normalizeXHigh bool,
) (*http.Response, error) {
	isStreaming := detectStreamRequest(rewrittenBody)

	var convertedPath string
	currentBody := rewrittenBody

	clientCodec := s.Codecs[clientProtocol]
	backendCodec := s.Codecs[backend.Protocol]

	if needsProtocolConversion(clientProtocol, backend.Protocol) && clientCodec != nil && backendCodec != nil {
		// Cross-protocol: client → canonical (Responses) → backend
		canonical, err := clientCodec.ToCanonical(currentBody, EncodeOpts{
			ReasoningEffort: reasoningDefaultEffort,
			SystemPrompt:    injectedSystemPrompt,
		})
		if err != nil {
			return nil, fmt.Errorf("protocol conversion failed: %v", err)
		}
		converted, pathOverride, err := backendCodec.FromCanonical(canonical)
		if err != nil {
			return nil, fmt.Errorf("protocol conversion failed: %v", err)
		}
		currentBody = converted
		convertedPath = pathOverride
		if normalizeXHigh && isOpenAIFamily(backend.Protocol) {
			currentBody = normalizeReasoningEffortAliasInBody(currentBody, backend.Protocol)
		}
		if isStreaming {
			switch backend.Protocol {
			case "openai":
				currentBody = ensureStreamOptions(currentBody)
			case "openai-responses":
				currentBody = ensureResponsesStream(currentBody)
			}
		}
	} else if injectedSystemPrompt != "" && backendCodec != nil {
		// Same protocol passthrough — inject system prompt via codec
		currentBody = backendCodec.InjectSystemPrompt(currentBody, injectedSystemPrompt)
	}

	incomingURL := r.URL
	if convertedPath != "" {
		u := *r.URL
		u.Path = convertedPath
		incomingURL = &u
	}
	targetURL, err := composeTargetURL(backend, incomingURL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend url: %v", err)
	}

	// Apply per-backend timeout if configured
	reqCtx := r.Context()
	if backend.Timeout != "" {
		if d, err := time.ParseDuration(backend.Timeout); err == nil && d > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(reqCtx, d)
			defer cancel()
		}
	}

	req, err := http.NewRequestWithContext(reqCtx, r.Method, targetURL, bytes.NewReader(currentBody))
	if err != nil {
		return nil, fmt.Errorf("build upstream request failed: %v", err)
	}

	copyHeaders(req.Header, r.Header)
	removeHopByHopHeaders(req.Header)
	applyBackendHeaders(req.Header, backend)
	if needsProtocolConversion(clientProtocol, backend.Protocol) && clientProtocol == "anthropic" {
		req.Header.Del("anthropic-version")
		if req.Header.Get("Authorization") != "" {
			req.Header.Del("x-api-key")
		}
	}
	req.Header.Del("Host")
	req.Header.Set("X-Request-Id", reqID)
	req.ContentLength = int64(len(currentBody))
	if req.Header.Get("Content-Type") == "" && len(currentBody) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.ContentLength >= 0 {
		req.Header.Set("Content-Length", fmt.Sprintf("%d", req.ContentLength))
	}

	if s.Debug.IsEnabled() && len(currentBody) > 0 {
		s.Debug.Printf("[%s] upstream request summary: protocol=%s path=%s %s", reqID, backend.Protocol, incomingURL.Path, debugConvertedRequestSummary(currentBody))
	}
	s.Debug.Printf("[%s] target URL: %s", reqID, targetURL)
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// writeResponse handles writing a successful upstream response back to the client.
// This replaces the old forwardRequest's response-writing portion.
func readDecodedResponseBody(resp *http.Response) ([]byte, error) {
	encoding := strings.TrimSpace(strings.ToLower(resp.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return io.ReadAll(resp.Body)
	}

	var reader io.ReadCloser
	switch encoding {
	case "gzip":
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}
		reader = gzipReader
	case "br":
		reader = io.NopCloser(brotli.NewReader(resp.Body))
	default:
		return io.ReadAll(resp.Body)
	}
	defer reader.Close()

	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read %s response body: %w", encoding, err)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return body, nil
}

func (s *Server) writeResponse(
	w http.ResponseWriter,
	r *http.Request,
	resp *http.Response,
	backend *config.Backend,
	clientProtocol, reqID string,
	start time.Time,
	keyLabel, requestModel, forwardedModel string,
	rewrittenBody []byte,
	retryCount int,
	errorCategory string,
) {
	defer resp.Body.Close()

	isStreaming := detectStreamRequest(rewrittenBody)
	convertResponse := needsProtocolConversion(clientProtocol, backend.Protocol)
	clientCodec := s.Codecs[clientProtocol]
	backendCodec := s.Codecs[backend.Protocol]
	// Only convert if we have both codecs
	if clientCodec == nil || backendCodec == nil {
		convertResponse = false
	}

	s.Debug.Printf("[%s] upstream response: status=%d (duration=%s)", reqID, resp.StatusCode, time.Since(start))

	if isStreaming && convertResponse {
		if resp.StatusCode == 200 {
			s.Debug.Printf("[%s] streaming with protocol conversion: %s -> %s", reqID, backend.Protocol, clientProtocol)
			decoder := backendCodec.NewStreamDecoder()
			encoder := clientCodec.NewStreamEncoder(w)
			usage, _ := convertStream(w, resp, decoder, encoder, s.Debug, reqID)
			s.Debug.Printf("[%s] streaming conversion completed: model=%s duration=%s", reqID, usage.ResponseModel, time.Since(start))
			s.logUsage(logging.UsageLog{
				Backend:          backend.Name,
				ClientProtocol:   clientProtocol,
				BackendProtocol:  backend.Protocol,
				ClientKey:        keyLabel,
				RequestModel:     requestModel,
				ForwardedModel:   forwardedModel,
				ResponseModel:    usage.ResponseModel,
				StatusCode:       resp.StatusCode,
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				TotalTokens:      usage.TotalTokens,
				InputTokens:      usage.InputTokens,
				OutputTokens:     usage.OutputTokens,
			}, start, reqID, retryCount, errorCategory)
			return
		}
		isStreaming = false
	}

	if isStreaming && resp.StatusCode >= 400 {
		// Error responses from upstream are never streamed — fall through to non-streaming path.
		isStreaming = false
	}

	if isStreaming {
		s.Debug.Printf("[%s] streaming response (passthrough)", reqID)
		usage := s.handleStreamingResponse(w, resp, backend.Protocol, reqID)
		s.Debug.Printf("[%s] streaming completed: model=%s duration=%s", reqID, usage.ResponseModel, time.Since(start))
		s.logUsage(logging.UsageLog{
			Backend:          backend.Name,
			ClientProtocol:   clientProtocol,
			BackendProtocol:  backend.Protocol,
			ClientKey:        keyLabel,
			RequestModel:     requestModel,
			ForwardedModel:   forwardedModel,
			ResponseModel:    usage.ResponseModel,
			StatusCode:       resp.StatusCode,
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			InputTokens:      usage.InputTokens,
			OutputTokens:     usage.OutputTokens,
		}, start, reqID, retryCount, errorCategory)
		return
	}

	var (
		respBody []byte
		err      error
		usage    logging.UsageInfo
	)
	respBody, err = readDecodedResponseBody(resp)
	if err == nil {
		usage = logging.ExtractUsage(backend.Protocol, respBody)
	}
	if err != nil {
		http.Error(w, "read upstream response failed", http.StatusBadGateway)
		s.logUsage(logging.UsageLog{
			Backend:         backend.Name,
			ClientProtocol:  clientProtocol,
			BackendProtocol: backend.Protocol,
			ClientKey:       keyLabel,
			RequestModel:    requestModel,
			ForwardedModel:  forwardedModel,
			StatusCode:      http.StatusBadGateway,
			Error:           err.Error(),
		}, start, reqID, retryCount, "read_upstream_error")
		return
	}
	s.Debug.Headers("response", resp.Header)
	s.Debug.Body("response", respBody)

	respBodyToSend := respBody
	if convertResponse {
		// Cross-protocol: backend response → canonical → client format
		canonical, toErr := backendCodec.ResponseToCanonical(respBody, resp.StatusCode)
		if toErr != nil {
			log.Printf("response protocol conversion (to canonical) failed: %v", toErr)
		} else {
			converted, fromErr := clientCodec.ResponseFromCanonical(canonical, resp.StatusCode)
			if fromErr != nil {
				log.Printf("response protocol conversion (from canonical) failed: %v", fromErr)
			} else {
				respBodyToSend = converted
			}
		}
	}

	copyHeaders(w.Header(), resp.Header)
	removeHopByHopHeaders(w.Header())
	w.Header().Del("Content-Encoding")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(respBodyToSend)))
	if convertResponse {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	if _, writeErr := w.Write(respBodyToSend); writeErr != nil {
		s.logUsage(logging.UsageLog{
			Backend:         backend.Name,
			ClientProtocol:  clientProtocol,
			BackendProtocol: backend.Protocol,
			ClientKey:       keyLabel,
			RequestModel:    requestModel,
			ForwardedModel:  forwardedModel,
			StatusCode:      http.StatusBadGateway,
			Error:           writeErr.Error(),
		}, start, reqID, retryCount, "write_response_error")
		return
	}

	s.Debug.Printf("[%s] completed: backend=%s client=%s model=%s status=%d response_size=%d duration=%s",
		reqID, backend.Name, keyLabel, usage.ResponseModel, resp.StatusCode, len(respBodyToSend), time.Since(start))

	s.logUsage(logging.UsageLog{
		Backend:          backend.Name,
		ClientProtocol:   clientProtocol,
		BackendProtocol:  backend.Protocol,
		ClientKey:        keyLabel,
		RequestModel:     requestModel,
		ForwardedModel:   forwardedModel,
		ResponseModel:    usage.ResponseModel,
		StatusCode:       resp.StatusCode,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
	}, start, reqID, retryCount, errorCategory)
}

func (s *Server) handleStreamingResponse(
	w http.ResponseWriter,
	resp *http.Response,
	backendProtocol string,
	reqID string,
) logging.UsageInfo {
	s.Debug.Headers("streaming response", resp.Header)
	flusher, ok := w.(http.Flusher)
	if !ok {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		s.Debug.Body("streaming response (no flusher)", body)
		copyHeaders(w.Header(), resp.Header)
		removeHopByHopHeaders(w.Header())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return logging.ExtractUsage(backendProtocol, body)
	}

	copyHeaders(w.Header(), resp.Header)
	removeHopByHopHeaders(w.Header())
	w.WriteHeader(resp.StatusCode)

	var usage logging.UsageInfo
	var streamLog strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if s.Debug.IsEnabled() && streamLog.Len() < maxStreamLogSize {
			streamLog.WriteString(line)
			streamLog.WriteByte('\n')
		}

		if strings.HasPrefix(line, "data: ") {
			payload := line[6:]
			if payload != "[DONE]" {
				if u, found := logging.ExtractStreamingUsage(backendProtocol, []byte(payload)); found {
					usage = u
				} else if u.ResponseModel != "" && usage.ResponseModel == "" {
					usage.ResponseModel = u.ResponseModel
				}
			}
		}

		_, err := fmt.Fprintf(w, "%s\n", line)
		if err != nil {
			break
		}

		if line == "" {
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		s.Debug.Printf("[%s] streaming scanner error: %v", reqID, err)
	}

	flusher.Flush()
	if s.Debug.IsEnabled() {
		s.Debug.Printf("[%s] streaming response body:\n%s", reqID, streamLog.String())
	}
	return usage
}

func (s *Server) handleStreamingConversion(
	w http.ResponseWriter,
	resp *http.Response,
	reqID string,
) logging.UsageInfo {
	s.Debug.Headers("streaming conversion response", resp.Header)
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback: read entire body and convert as non-streaming
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		s.Debug.Body("streaming conversion response (no flusher)", body)
		converted, err := convertOpenAIResponseToAnthropic(body, resp.StatusCode)
		if err != nil {
			converted = body
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(converted)
		return logging.ExtractUsage("openai", body)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	var (
		msgID          string
		model          string
		messageStarted bool
		textBlockOpen  bool
		textBlockIndex int
		stopReason     string
		inputTokens    int
		outputTokens   int
		streamLog      strings.Builder

		// Thinking block tracking (reasoning_content from OpenAI)
		thinkingBlockOpen  bool
		thinkingBlockIndex int

		// Tool call tracking
		activeToolCalls map[int]*streamingToolCall
		nextBlockIndex  int
	)

	activeToolCalls = make(map[int]*streamingToolCall)

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if s.Debug.IsEnabled() && streamLog.Len() < maxStreamLogSize {
			streamLog.WriteString(line)
			streamLog.WriteByte('\n')
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		if payload == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			s.Debug.Printf("[%s] malformed SSE chunk: %v", reqID, err)
			continue
		}

		if chunk.ID != "" && msgID == "" {
			msgID = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}

		// Emit message_start on first meaningful chunk
		if !messageStarted && len(chunk.Choices) > 0 {
			if err := writeSSEEvent(w, "message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            "msg_" + msgID,
					"type":          "message",
					"role":          "assistant",
					"content":       []any{},
					"model":         model,
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage": map[string]any{
						"input_tokens":  0,
						"output_tokens": 0,
					},
				},
			}); err != nil {
				break
			}
			if err := writeSSEEvent(w, "ping", map[string]any{"type": "ping"}); err != nil {
				break
			}
			flusher.Flush()
			messageStarted = true
		}

		// Handle reasoning content → thinking block (must come before text)
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.ReasoningContent != "" {
			if !thinkingBlockOpen {
				thinkingBlockIndex = nextBlockIndex
				nextBlockIndex++
				if err := writeSSEEvent(w, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": thinkingBlockIndex,
					"content_block": map[string]any{
						"type":     "thinking",
						"thinking": "",
					},
				}); err != nil {
					break
				}
				thinkingBlockOpen = true
			}
			if err := writeSSEEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": thinkingBlockIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": chunk.Choices[0].Delta.ReasoningContent,
				},
			}); err != nil {
				break
			}
			flusher.Flush()
		}

		// Handle text content
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			// Close thinking block if open before starting text
			if thinkingBlockOpen {
				if err := writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": thinkingBlockIndex,
				}); err != nil {
					break
				}
				thinkingBlockOpen = false
				flusher.Flush()
			}
			if !textBlockOpen {
				textBlockIndex = nextBlockIndex
				nextBlockIndex++
				if err := writeSSEEvent(w, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": textBlockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				}); err != nil {
					break
				}
				textBlockOpen = true
			}
			if err := writeSSEEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": chunk.Choices[0].Delta.Content,
				},
			}); err != nil {
				break
			}
			flusher.Flush()
		}

		// Handle tool calls
		if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			// Close thinking block if open before starting tool blocks
			if thinkingBlockOpen {
				if err := writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": thinkingBlockIndex,
				}); err != nil {
					break
				}
				thinkingBlockOpen = false
				flusher.Flush()
			}
			// Close text block if open before starting tool blocks
			if textBlockOpen {
				if err := writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": textBlockIndex,
				}); err != nil {
					break
				}
				textBlockOpen = false
				flusher.Flush()
			}

			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				existing, exists := activeToolCalls[tc.Index]
				if !exists {
					// New tool call
					existing = &streamingToolCall{
						blockIndex: nextBlockIndex,
						id:         tc.ID,
						name:       tc.Function.Name,
					}
					activeToolCalls[tc.Index] = existing
					nextBlockIndex++

					if err := writeSSEEvent(w, "content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": existing.blockIndex,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  tc.Function.Name,
							"input": map[string]any{},
						},
					}); err != nil {
						break
					}
					flusher.Flush()
				}

				// Emit argument deltas
				if tc.Function.Arguments != "" {
					existing.arguments.WriteString(tc.Function.Arguments)
					if err := writeSSEEvent(w, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": existing.blockIndex,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": tc.Function.Arguments,
						},
					}); err != nil {
						break
					}
					flusher.Flush()
				}
			}
		}

		// Finish reason
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			stopReason = mapFinishReason(*chunk.Choices[0].FinishReason)
		}

		// Usage (from final chunk with stream_options.include_usage)
		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
	}

	// Close any open thinking block
	if thinkingBlockOpen {
		_ = writeSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": thinkingBlockIndex,
		})
	}

	// Close any open text block
	if textBlockOpen {
		_ = writeSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		})
	}

	// Close tool call blocks
	for _, tc := range activeToolCalls {
		_ = writeSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": tc.blockIndex,
		})
	}

	// Emit closing events
	if messageStarted {
		// Defensive override: if tool calls exist, force stop_reason to tool_use
		if len(activeToolCalls) > 0 {
			stopReason = "tool_use"
		} else if stopReason == "" {
			stopReason = "end_turn"
		}
		_ = writeSSEEvent(w, "message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"output_tokens": outputTokens,
			},
		})
		_ = writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	}

	if err := scanner.Err(); err != nil {
		s.Debug.Printf("[%s] streaming conversion scanner error: %v", reqID, err)
	}

	flusher.Flush()
	if s.Debug.IsEnabled() {
		s.Debug.Printf("[%s] streaming conversion upstream body:\n%s", reqID, streamLog.String())
	}

	return logging.UsageInfo{
		ResponseModel:    model,
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
	}
}

// handleStreamingResponsesConversion handles streaming from an OpenAI Responses API
// backend and converts SSE events to Anthropic format.
// Responses API uses named events like "response.output_text.delta".
func (s *Server) handleStreamingResponsesConversion(
	w http.ResponseWriter,
	resp *http.Response,
	reqID string,
) logging.UsageInfo {
	s.Debug.Headers("streaming responses conversion", resp.Header)
	flusher, ok := w.(http.Flusher)
	if !ok {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		s.Debug.Body("streaming responses (no flusher)", body)
		converted, err := convertOpenAIResponsesResponseToAnthropic(body, resp.StatusCode)
		if err != nil {
			converted = body
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(converted)
		return logging.ExtractUsage("openai-responses", body)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	var (
		msgID          string
		model          string
		messageStarted bool
		textBlockOpen  bool
		textBlockIndex int
		stopReason     string
		inputTokens    int
		outputTokens   int
		streamLog      strings.Builder
		streamFailed   bool
		streamErrMsg   string

		// Thinking block tracking (reasoning summary from Responses API)
		thinkingBlockOpen  bool
		thinkingBlockIndex int

		// Tool call tracking: outputIndex -> tool call state
		activeToolCalls map[int]*streamingToolCall
		nextBlockIndex  int
		sawToolUse      bool

		// SSE parsing state
		currentEvent string
	)

	activeToolCalls = make(map[int]*streamingToolCall)

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if s.Debug.IsEnabled() && streamLog.Len() < maxStreamLogSize {
			streamLog.WriteString(line)
			streamLog.WriteByte('\n')
		}

		// Parse SSE event type
		if strings.HasPrefix(line, "event: ") {
			currentEvent = line[7:]
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		eventType := currentEvent
		currentEvent = "" // reset for next event

		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			s.Debug.Printf("[%s] malformed Responses SSE event (%s): %v", reqID, eventType, err)
			continue
		}

		switch eventType {
		case "response.created":
			created := data
			if responseObj, ok := data["response"].(map[string]any); ok {
				created = responseObj
			}
			if id, ok := created["id"].(string); ok && id != "" {
				msgID = id
			}
			if m, ok := created["model"].(string); ok && m != "" {
				model = m
			}

		case "response.in_progress":
			if responseObj, ok := data["response"].(map[string]any); ok {
				if id, ok := responseObj["id"].(string); ok && id != "" && msgID == "" {
					msgID = id
				}
				if m, ok := responseObj["model"].(string); ok && m != "" {
					model = m
				}
			}

		case "response.output_item.added":
			item, _ := data["item"].(map[string]any)
			if item == nil {
				continue
			}
			itemType, _ := item["type"].(string)
			outputIdx := int(getFloat(data, "output_index"))

			// Emit message_start on first item
			if !messageStarted {
				if err := writeSSEEvent(w, "message_start", map[string]any{
					"type": "message_start",
					"message": map[string]any{
						"id":            "msg_" + msgID,
						"type":          "message",
						"role":          "assistant",
						"content":       []any{},
						"model":         model,
						"stop_reason":   nil,
						"stop_sequence": nil,
						"usage": map[string]any{
							"input_tokens":  0,
							"output_tokens": 0,
						},
					},
				}); err != nil {
					break
				}
				if err := writeSSEEvent(w, "ping", map[string]any{"type": "ping"}); err != nil {
					break
				}
				flusher.Flush()
				messageStarted = true
			}

			if itemType == "function_call" {
				sawToolUse = true
				// Close thinking block if open
				if thinkingBlockOpen {
					if err := writeSSEEvent(w, "content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": thinkingBlockIndex,
					}); err != nil {
						break
					}
					thinkingBlockOpen = false
					flusher.Flush()
				}
				// Close text block if open
				if textBlockOpen {
					if err := writeSSEEvent(w, "content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": textBlockIndex,
					}); err != nil {
						break
					}
					textBlockOpen = false
					flusher.Flush()
				}

				callID, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				tc := &streamingToolCall{
					blockIndex: nextBlockIndex,
					id:         callID,
					name:       name,
				}
				activeToolCalls[outputIdx] = tc
				nextBlockIndex++

				if err := writeSSEEvent(w, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": tc.blockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    callID,
						"name":  name,
						"input": map[string]any{},
					},
				}); err != nil {
					break
				}
				flusher.Flush()
			}

		case "response.content_part.added":
			// Close thinking block if open before text content starts
			if thinkingBlockOpen {
				if err := writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": thinkingBlockIndex,
				}); err != nil {
					break
				}
				thinkingBlockOpen = false
				flusher.Flush()
			}
			// Start of text content block
			if !textBlockOpen {
				if !messageStarted {
					if err := writeSSEEvent(w, "message_start", map[string]any{
						"type": "message_start",
						"message": map[string]any{
							"id":            "msg_" + msgID,
							"type":          "message",
							"role":          "assistant",
							"content":       []any{},
							"model":         model,
							"stop_reason":   nil,
							"stop_sequence": nil,
							"usage": map[string]any{
								"input_tokens":  0,
								"output_tokens": 0,
							},
						},
					}); err != nil {
						break
					}
					if err := writeSSEEvent(w, "ping", map[string]any{"type": "ping"}); err != nil {
						break
					}
					flusher.Flush()
					messageStarted = true
				}
				textBlockIndex = nextBlockIndex
				nextBlockIndex++
				if err := writeSSEEvent(w, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": textBlockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				}); err != nil {
					break
				}
				textBlockOpen = true
				flusher.Flush()
			}

		case "response.output_text.delta":
			delta, _ := data["delta"].(string)
			if delta != "" && textBlockOpen {
				if err := writeSSEEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": textBlockIndex,
					"delta": map[string]any{
						"type": "text_delta",
						"text": delta,
					},
				}); err != nil {
					break
				}
				flusher.Flush()
			}

		case "response.function_call_arguments.delta":
			outputIdx := int(getFloat(data, "output_index"))
			delta, _ := data["delta"].(string)
			if tc, ok := activeToolCalls[outputIdx]; ok && delta != "" {
				tc.arguments.WriteString(delta)
				if err := writeSSEEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": tc.blockIndex,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": delta,
					},
				}); err != nil {
					break
				}
				flusher.Flush()
			}

		case "response.reasoning_summary_part.added":
			// Start a thinking content block for reasoning summary
			if !messageStarted {
				if err := writeSSEEvent(w, "message_start", map[string]any{
					"type": "message_start",
					"message": map[string]any{
						"id":            "msg_" + msgID,
						"type":          "message",
						"role":          "assistant",
						"content":       []any{},
						"model":         model,
						"stop_reason":   nil,
						"stop_sequence": nil,
						"usage": map[string]any{
							"input_tokens":  0,
							"output_tokens": 0,
						},
					},
				}); err != nil {
					break
				}
				if err := writeSSEEvent(w, "ping", map[string]any{"type": "ping"}); err != nil {
					break
				}
				flusher.Flush()
				messageStarted = true
			}
			if thinkingBlockOpen {
				_ = writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": thinkingBlockIndex,
				})
				thinkingBlockOpen = false
			}
			thinkingBlockIndex = nextBlockIndex
			nextBlockIndex++
			if err := writeSSEEvent(w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": thinkingBlockIndex,
				"content_block": map[string]any{
					"type":     "thinking",
					"thinking": "",
				},
			}); err != nil {
				break
			}
			thinkingBlockOpen = true
			flusher.Flush()

		case "response.reasoning_summary_text.delta":
			delta, _ := data["delta"].(string)
			if delta != "" && thinkingBlockOpen {
				if err := writeSSEEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": thinkingBlockIndex,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": delta,
					},
				}); err != nil {
					break
				}
				flusher.Flush()
			}

		case "response.reasoning_summary_text.done", "response.reasoning_summary_part.done":
			if thinkingBlockOpen {
				if err := writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": thinkingBlockIndex,
				}); err != nil {
					break
				}
				thinkingBlockOpen = false
				flusher.Flush()
			}

		case "response.output_text.done":
			// Text content complete, close block
			if textBlockOpen {
				if err := writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": textBlockIndex,
				}); err != nil {
					break
				}
				textBlockOpen = false
				flusher.Flush()
			}

		case "response.function_call_arguments.done":
			outputIdx := int(getFloat(data, "output_index"))
			if tc, ok := activeToolCalls[outputIdx]; ok {
				if err := writeSSEEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": tc.blockIndex,
				}); err != nil {
					break
				}
				flusher.Flush()
				delete(activeToolCalls, outputIdx)
			}

		case "response.completed":
			completed := data
			if responseObj, ok := data["response"].(map[string]any); ok {
				completed = responseObj
			}
			if usage, ok := completed["usage"].(map[string]any); ok {
				inputTokens = int(getFloat(usage, "input_tokens"))
				outputTokens = int(getFloat(usage, "output_tokens"))
			}
			if m, ok := completed["model"].(string); ok && m != "" {
				model = m
			}
			if status, ok := completed["status"].(string); ok {
				stopReason = mapResponsesStatus(status)
			}
			hasCompletedFunctionCall := hasResponsesFunctionCallOutput(completed["output"])
			if hasCompletedFunctionCall {
				sawToolUse = true
			}
			if sawToolUse || len(activeToolCalls) > 0 {
				stopReason = "tool_use"
			}
			if s.Debug.IsEnabled() {
				s.Debug.Printf("[%s] responses conversion decision: saw_tool_use=%t active_tool_calls=%d completed_has_function_call=%t stop_reason=%s stream_failed=%t", reqID, sawToolUse, len(activeToolCalls), hasCompletedFunctionCall, stopReason, streamFailed)
			}

		case "response.failed", "error":
			streamFailed = true
			streamErrMsg = "upstream streaming failed"
			if errObj, ok := data["error"].(map[string]any); ok {
				if msg, ok := errObj["message"].(string); ok && strings.TrimSpace(msg) != "" {
					streamErrMsg = msg
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		s.Debug.Printf("[%s] streaming responses conversion scanner error: %v", reqID, err)
	}

	// Close any remaining open blocks
	if thinkingBlockOpen {
		_ = writeSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": thinkingBlockIndex,
		})
	}
	if textBlockOpen {
		_ = writeSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		})
	}
	for _, tc := range activeToolCalls {
		_ = writeSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": tc.blockIndex,
		})
	}

	// Emit closing events
	if streamFailed {
		_ = writeSSEEvent(w, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": streamErrMsg,
			},
		})
	} else if messageStarted {
		if stopReason == "" {
			stopReason = "end_turn"
		}
		_ = writeSSEEvent(w, "message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"output_tokens": outputTokens,
			},
		})
		_ = writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	}

	flusher.Flush()
	if s.Debug.IsEnabled() {
		s.Debug.Printf("[%s] streaming responses conversion upstream body:\n%s", reqID, streamLog.String())
	}

	return logging.UsageInfo{
		ResponseModel:    model,
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
	}
}

func hasResponsesFunctionCallOutput(output any) bool {
	items, ok := output.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if itemType, _ := itemMap["type"].(string); itemType == "function_call" {
			return true
		}
	}
	return false
}

func (s *Server) serveModelList(w http.ResponseWriter) {
	snap := s.snapshot()
	seen := make(map[string]bool)
	var models []map[string]any

	for _, b := range snap.cfg.Backends {
		for _, m := range b.Models {
			// Add model name
			if !seen[m.Name] {
				seen[m.Name] = true
				models = append(models, map[string]any{
					"id":           m.Name,
					"display_name": m.Name,
					"created_at":   "2025-01-01T00:00:00Z",
					"type":         "model",
				})
			}
			// Add aliases
			for _, alias := range m.Aliases {
				if !seen[alias] {
					seen[alias] = true
					models = append(models, map[string]any{
						"id":           alias,
						"display_name": alias,
						"created_at":   "2025-01-01T00:00:00Z",
						"type":         "model",
					})
				}
			}
		}
	}

	resp := map[string]any{
		"data":     models,
		"has_more": false,
		"first_id": "",
		"last_id":  "",
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(b)))
	_, _ = w.Write(b)
}
