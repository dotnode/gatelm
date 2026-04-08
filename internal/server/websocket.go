package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

const (
	wsReadLimit     = 32 << 20 // 32 MB, same as maxRequestBodySize
	wsFirstMsgWait  = 30 * time.Second
	wsPongWait      = 5 * time.Minute
	wsPingInterval  = 30 * time.Second
	wsWriteDeadline = 10 * time.Second
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: checkWSOrigin,
}

// checkWSOrigin validates that the WebSocket Origin header matches the request Host.
func checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients may omit Origin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := u.Hostname()
	requestHost := r.Host
	if h, _, err := net.SplitHostPort(requestHost); err == nil {
		requestHost = h
	}
	return strings.EqualFold(originHost, requestHost)
}

// isWebSocketUpgrade detects an HTTP WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		containsTokenIgnoreCase(r.Header.Get("Connection"), "upgrade")
}

// containsTokenIgnoreCase checks if a comma-separated header value contains a token.
func containsTokenIgnoreCase(headerVal, token string) bool {
	for _, part := range strings.Split(headerVal, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// handleWebSocket handles a WebSocket upgrade request.
// It accepts the client WS, reads the first message to extract the model,
// resolves the backend, dials the backend WS with retry across candidates,
// and relays messages bidirectionally.
func (s *Server) handleWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	snap serverSnapshot,
	clientProtocol, reqID string,
	start time.Time,
) {
	clientKey := extractClientKey(r.Header)
	keyLabel := normalizeKeyLabel(clientKey, snap.cfg.APIKeys)

	// Accept WebSocket from client
	respHeaders := http.Header{}
	respHeaders.Set("X-Request-Id", reqID)
	clientConn, err := wsUpgrader.Upgrade(w, r, respHeaders)
	if err != nil {
		s.Debug.Printf("[%s] websocket upgrade failed: %v", reqID, err)
		return // Upgrade already wrote the error response
	}
	defer clientConn.Close()
	clientConn.SetReadLimit(wsReadLimit)

	// Read first message with timeout
	clientConn.SetReadDeadline(time.Now().Add(wsFirstMsgWait))
	msgType, firstMsg, err := clientConn.ReadMessage()
	if err != nil {
		s.Debug.Printf("[%s] websocket read first message failed: %v", reqID, err)
		writeWSClose(clientConn, websocket.CloseInternalServerErr, "failed to read first message")
		return
	}
	clientConn.SetReadDeadline(time.Time{}) // reset

	s.Debug.Body("websocket first message", firstMsg)

	// Extract model from first message
	requestModel := extractModel(firstMsg)

	// Resolve backend candidates
	candidates := resolveWSCandidates(snap, requestModel)
	if len(candidates) == 0 {
		s.Debug.Printf("[%s] websocket no backend for model %q", reqID, requestModel)
		writeWSClose(clientConn, websocket.CloseInternalServerErr, "no backend found for model: "+requestModel)
		s.logUsage(logging.UsageLog{
			Backend:         "none",
			ClientProtocol:  clientProtocol,
			BackendProtocol: "",
			ClientKey:       keyLabel,
			RequestModel:    requestModel,
			StatusCode:      http.StatusNotFound,
			Error:           "no backend found for model",
		}, start, reqID, 0, "model_not_found")
		return
	}

	// Get ordered candidate list (like HTTP forwardWithRetry)
	ordered := snap.selector.SelectOrdered(candidates)
	fallbackMode := false
	if len(ordered) == 0 {
		ordered = snap.selector.SelectFallback(candidates)
		if len(ordered) == 0 {
			s.Debug.Printf("[%s] websocket all backends unavailable for model %q", reqID, requestModel)
			writeWSClose(clientConn, websocket.CloseInternalServerErr, "all backends unavailable")
			s.logUsage(logging.UsageLog{
				Backend:         "none",
				ClientProtocol:  clientProtocol,
				BackendProtocol: "",
				ClientKey:       keyLabel,
				RequestModel:    requestModel,
				StatusCode:      http.StatusServiceUnavailable,
				Error:           "all backends unavailable",
			}, start, reqID, 0, "no_backend")
			return
		}
		fallbackMode = true
		s.Debug.Printf("[%s] websocket all backends circuit-broken for model %q, trying fallback", reqID, requestModel)
	}

	// Try each candidate in priority order
	var lastErr string
	for _, cand := range ordered {
		backend := cand.backend

		if !fallbackMode {
			if !snap.health.TryAcquireProbe(backend.Name) {
				s.Debug.Printf("[%s] websocket skip backend %s (circuit=%s)", reqID, backend.Name, snap.health.CircuitState(backend.Name))
				continue
			}
		}

		backendModel := cand.backendModel
		forwardedModel := backendModel

		// Apply model defaults and rewrite
		mc := resolveModelDefaults(snap.cfg, cand, clientProtocol)
		backendCodec := s.Codecs[backend.Protocol]
		rewrittenMsg := prepareRequestBody(firstMsg, requestModel, backendModel, backendCodec, mc)

		// Protocol conversion or system prompt injection via codec
		clientCodec := s.Codecs[clientProtocol]
		if needsProtocolConversion(clientProtocol, backend.Protocol) && clientCodec != nil && backendCodec != nil {
			canonical, err := clientCodec.ToCanonical(rewrittenMsg, EncodeOpts{
				ReasoningEffort: mc.reasoningEffort,
				SystemPrompt:    mc.systemPrompt,
			})
			if err != nil {
				s.Debug.Printf("[%s] websocket protocol conversion failed: %v", reqID, err)
				lastErr = fmt.Sprintf("protocol conversion failed: %v", err)
				continue
			}
			converted, _, convErr := backendCodec.FromCanonical(canonical)
			if convErr != nil {
				s.Debug.Printf("[%s] websocket protocol conversion failed: %v", reqID, convErr)
				lastErr = fmt.Sprintf("protocol conversion failed: %v", convErr)
				continue
			}
			rewrittenMsg = converted
		} else if mc.systemPrompt != "" && backendCodec != nil {
			rewrittenMsg = backendCodec.InjectSystemPrompt(rewrittenMsg, mc.systemPrompt)
		}

		s.Debug.Body("websocket rewritten first message", rewrittenMsg)

		// If backend prefers SSE, skip WS dial and go directly to HTTP SSE
		if backend.SSEOnly {
			s.Debug.Printf("[%s] backend %s configured sse_only, using SSE directly", reqID, backend.Name)
			if s.wsPerBackendSSEFallback(clientConn, r, snap, firstMsg, cand, clientProtocol, keyLabel, requestModel, reqID, start) {
				return
			}
			lastErr = fmt.Sprintf("backend %s: SSE failed", backend.Name)
			continue
		}

		// Compose backend WebSocket URL
		wsURL, urlErr := composeWSURL(backend, r.URL)
		if urlErr != nil {
			s.Debug.Printf("[%s] websocket compose URL failed for %s: %v", reqID, backend.Name, urlErr)
			lastErr = fmt.Sprintf("invalid backend url: %v", urlErr)
			continue
		}

		s.Debug.Printf("[%s] websocket dialing backend: %s → %s (model=%s)", reqID, backend.Name, wsURL, forwardedModel)

		// Try to connect and relay with this backend
		relayErr := s.wsDialAndRelay(r, clientConn, msgType, rewrittenMsg, backend, wsURL, snap, reqID, start, clientProtocol, keyLabel, requestModel, forwardedModel)
		if relayErr == nil {
			// Successful relay completed (connection closed normally)
			return
		}

		// Check if it's a dial error (can retry next backend) vs relay error (already sent data to client, can't retry)
		if relayErr.dataRelayed {
			// Already sent data to client, can't switch backends
			s.Debug.Printf("[%s] websocket relay error after data sent to client on %s: %v", reqID, backend.Name, relayErr.err)
			s.logUsage(logging.UsageLog{
				Backend:         backend.Name,
				ClientProtocol:  clientProtocol,
				BackendProtocol: backend.Protocol,
				ClientKey:       keyLabel,
				RequestModel:    requestModel,
				ForwardedModel:  forwardedModel,
				StatusCode:      502,
				Error:           relayErr.err.Error(),
			}, start, reqID, 0, "network_error")
			return
		}

		// WS failed without relaying data — try SSE for this same backend
		if isWSNotSupported(relayErr) {
			// WS not supported is not a backend error — no health penalty
			s.Debug.Printf("[%s] backend %s does not support WebSocket, trying SSE", reqID, backend.Name)
		} else {
			// Real WS error (e.g. close 1013) — penalise health but still try SSE
			s.Debug.Printf("[%s] websocket backend %s failed: %v, trying SSE fallback", reqID, backend.Name, relayErr.err)
			snap.health.ReportFailure(backend.Name)
		}
		clientConn.SetReadDeadline(time.Time{})
		if s.wsPerBackendSSEFallback(clientConn, r, snap, firstMsg, cand, clientProtocol, keyLabel, requestModel, reqID, start) {
			return
		}
		lastErr = fmt.Sprintf("backend %s: WS and SSE both failed", backend.Name)
	}

	// All backends exhausted (WS + per-backend SSE)
	s.Debug.Printf("[%s] websocket all backends failed for model %q", reqID, requestModel)
	writeWSClose(clientConn, websocket.CloseInternalServerErr, "all backends failed: "+lastErr)
	s.logUsage(logging.UsageLog{
		Backend:         "none",
		ClientProtocol:  clientProtocol,
		BackendProtocol: "",
		ClientKey:       keyLabel,
		RequestModel:    requestModel,
		StatusCode:      http.StatusBadGateway,
		Error:           "all backends failed: " + lastErr,
	}, start, reqID, len(ordered), "all_backends_failed")
}

// wsPerBackendSSEFallback attempts HTTP SSE for a single backend when its WS
// connection is not supported. Returns true if the SSE path succeeded.
func (s *Server) wsPerBackendSSEFallback(
	clientConn *websocket.Conn,
	r *http.Request,
	snap serverSnapshot,
	firstMsg []byte,
	cand candidateResult,
	clientProtocol, keyLabel, requestModel, reqID string,
	start time.Time,
) bool {
	backend := cand.backend
	backendModel := cand.backendModel
	forwardedModel := backendModel

	// Apply model defaults and rewrite using codec
	mc := resolveModelDefaults(snap.cfg, cand, clientProtocol)
	backendCodec := s.Codecs[backend.Protocol]
	rewrittenBody := prepareRequestBody(firstMsg, requestModel, backendModel, backendCodec, mc)

	// Strip WebSocket-only fields before sending as HTTP POST
	rewrittenBody = unwrapWSEnvelope(rewrittenBody)

	// Ensure stream=true for HTTP SSE
	rewrittenBody = ensureResponsesStream(rewrittenBody)

	s.Debug.Printf("[%s] ws→sse fallback: trying backend %s (protocol=%s)", reqID, backend.Name, backend.Protocol)

	fallbackReq := buildHTTPFallbackRequest(r)
	resp, err := s.tryForward(fallbackReq, backend, clientProtocol, rewrittenBody, reqID,
		mc.reasoningEffort, mc.systemPrompt, mc.normalizeXHighReasoningEffort)
	if err != nil {
		snap.health.ReportFailure(backend.Name)
		s.Debug.Printf("[%s] ws→sse fallback: backend %s request failed: %v", reqID, backend.Name, err)
		return false
	}

	if resp.StatusCode >= 400 {
		body := make([]byte, 1024)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		snap.health.ReportFailure(backend.Name)
		s.Debug.Printf("[%s] ws→sse fallback: backend %s returned %d: %s", reqID, backend.Name, resp.StatusCode, string(body[:n]))
		return false
	}

	// Success — relay SSE stream to WS client
	snap.health.ReportSuccess(backend.Name)
	s.Debug.Printf("[%s] ws→sse fallback: backend %s connected (status=%d), relaying SSE→WS", reqID, backend.Name, resp.StatusCode)

	usage := s.relaySSEToWS(clientConn, resp.Body, reqID)
	resp.Body.Close()

	writeWSClose(clientConn, websocket.CloseNormalClosure, "")

	s.logUsage(logging.UsageLog{
		Backend:          backend.Name,
		ClientProtocol:   clientProtocol,
		BackendProtocol:  backend.Protocol,
		ClientKey:        keyLabel,
		RequestModel:     requestModel,
		ForwardedModel:   forwardedModel,
		ResponseModel:    usage.ResponseModel,
		StatusCode:       200,
		Transport:        "ws_sse",
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
	}, start, reqID, 0, "success")

	s.Debug.Printf("[%s] ws→sse fallback: completed via %s, duration=%s usage=%+v", reqID, backend.Name, time.Since(start), usage)
	return true
}

// relaySSEToWS reads an HTTP SSE stream and forwards each data event as a
// WebSocket text message. Returns extracted usage info from completion events.
func (s *Server) relaySSEToWS(clientConn *websocket.Conn, body interface{ Read([]byte) (int, error) }, reqID string) logging.UsageInfo {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var usage logging.UsageInfo

	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: "event: <type>\ndata: <json>\n\n"
		// We only care about data: lines
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := line[6:] // strip "data: " prefix
		if data == "[DONE]" {
			break
		}

		// Extract usage from response.completed / response.done events
		if u, ok := extractWSUsage([]byte(data)); ok {
			usage = u
		}

		// Send as WS text message
		clientConn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
		if err := clientConn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
			s.Debug.Printf("[%s] ws-http-fallback: client write error: %v", reqID, err)
			break
		}
	}

	if err := scanner.Err(); err != nil {
		s.Debug.Printf("[%s] ws-http-fallback: SSE read error: %v", reqID, err)
	}

	return usage
}

// unwrapWSEnvelope strips WebSocket-specific fields from a WS message body
// so it can be used as an HTTP POST body for the Responses API.
// Handles two formats:
//   - Flat: {"type":"response.create","model":"...","input":[...], ...}
//     → removes "type", "event_id", "generate", "client_metadata"
//   - Nested: {"type":"response.create","response":{"model":"...","input":[...]}}
//     → extracts the inner "response" object
func unwrapWSEnvelope(body []byte) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	// Check if this is a WS envelope (has "type" field)
	typeRaw, hasType := payload["type"]
	if !hasType {
		return body
	}

	var msgType string
	if err := json.Unmarshal(typeRaw, &msgType); err != nil {
		return body
	}

	// Only unwrap response.create messages
	if msgType != "response.create" {
		return body
	}

	// Check for nested format: {"type":"response.create","response":{...}}
	if responseRaw, ok := payload["response"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(responseRaw, &inner); err == nil {
			// Use the inner response object as the HTTP body
			out, err := json.Marshal(inner)
			if err != nil {
				return body
			}
			return out
		}
	}

	// Flat format: strip WS-specific fields
	delete(payload, "type")
	delete(payload, "event_id")
	delete(payload, "generate")
	delete(payload, "client_metadata")

	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// buildHTTPFallbackRequest creates a POST request from a WS upgrade request,
// removing WebSocket-specific headers while preserving the original path and
// other headers.
func buildHTTPFallbackRequest(r *http.Request) *http.Request {
	fallback := r.Clone(r.Context())
	fallback.Method = "POST"
	fallback.Body = nil
	fallback.ContentLength = 0

	// Remove WebSocket upgrade headers
	fallback.Header.Del("Connection")
	fallback.Header.Del("Upgrade")
	fallback.Header.Del("Sec-Websocket-Key")
	fallback.Header.Del("Sec-Websocket-Version")
	fallback.Header.Del("Sec-Websocket-Extensions")
	fallback.Header.Del("Sec-Websocket-Protocol")

	return fallback
}

// isWSNotSupported returns true when a WS relay error indicates the backend
// does not support WebSocket (as opposed to a real network / business error).
// This lets the caller fall back to HTTP SSE for the same backend without
// penalising its health state.
func isWSNotSupported(re *wsRelayError) bool {
	if re == nil || re.dataRelayed {
		return false
	}
	msg := strings.ToLower(re.err.Error())
	// Backend accepted the TCP handshake but immediately closed the connection
	if strings.Contains(msg, "use of closed network connection") {
		return true
	}
	// Dial-phase HTTP rejection (e.g. 400 Bad Request, 405 Method Not Allowed)
	if strings.Contains(msg, "bad handshake") {
		return true
	}
	return false
}

// wsRelayError represents an error during WebSocket relay, indicating whether
// data was already relayed to the client (making retry impossible).
type wsRelayError struct {
	err         error
	dataRelayed bool
}

// wsDialAndRelay connects to a backend WS, sends the first message, and runs
// bidirectional relay. Returns nil on normal completion, or a wsRelayError.
func (s *Server) wsDialAndRelay(
	r *http.Request,
	clientConn *websocket.Conn,
	msgType int,
	rewrittenMsg []byte,
	backend *config.Backend,
	wsURL string,
	snap serverSnapshot,
	reqID string,
	start time.Time,
	clientProtocol, keyLabel, requestModel, forwardedModel string,
) *wsRelayError {
	// Dial backend WS
	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		TLSClientConfig:  &tls.Config{},
	}

	backendHeaders := http.Header{}
	applyBackendHeaders(backendHeaders, backend)
	backendHeaders.Set("X-Request-Id", reqID)

	dialCtx := r.Context()
	if backend.Timeout != "" {
		if d, parseErr := time.ParseDuration(backend.Timeout); parseErr == nil && d > 0 {
			var dialCancel context.CancelFunc
			dialCtx, dialCancel = context.WithTimeout(dialCtx, d)
			defer dialCancel()
		}
	}

	backendConn, _, dialErr := dialer.DialContext(dialCtx, wsURL, backendHeaders)
	if dialErr != nil {
		return &wsRelayError{err: fmt.Errorf("dial failed: %v", dialErr), dataRelayed: false}
	}
	defer backendConn.Close()
	backendConn.SetReadLimit(wsReadLimit)

	snap.health.ReportSuccess(backend.Name)
	s.Debug.Printf("[%s] websocket backend connected: %s", reqID, backend.Name)

	// Send first (rewritten) message to backend
	if err := backendConn.WriteMessage(msgType, rewrittenMsg); err != nil {
		return &wsRelayError{err: fmt.Errorf("send first message failed: %v", err), dataRelayed: false}
	}

	// Bidirectional relay
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var usage logging.UsageInfo
	var usageMu sync.Mutex
	var wg sync.WaitGroup
	var backendCloseCode int
	var backendCloseText string
	var backendReadErr error
	var dataRelayed bool

	// Setup ping/pong on both connections
	setupPingPong(clientConn)
	setupPingPong(backendConn)

	// Backend → Client relay
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			mt, msg, readErr := backendConn.ReadMessage()
			if readErr != nil {
				// Extract close code/text from the error for forwarding
				if ce, ok := readErr.(*websocket.CloseError); ok {
					backendCloseCode = ce.Code
					backendCloseText = ce.Text
				}
				if !isNormalWSClose(readErr) {
					s.Debug.Printf("[%s] websocket backend read error: %v", reqID, readErr)
					backendReadErr = readErr
				}
				return
			}

			dataRelayed = true

			// Extract usage from response.completed events
			if u, ok := extractWSUsage(msg); ok {
				usageMu.Lock()
				usage = u
				usageMu.Unlock()
			}

			clientConn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			if writeErr := clientConn.WriteMessage(mt, msg); writeErr != nil {
				s.Debug.Printf("[%s] websocket client write error: %v", reqID, writeErr)
				return
			}
		}
	}()

	// Client → Backend relay
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			mt, msg, readErr := clientConn.ReadMessage()
			if readErr != nil {
				if !isNormalWSClose(readErr) {
					s.Debug.Printf("[%s] websocket client read error: %v", reqID, readErr)
				}
				return
			}

			backendConn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			if writeErr := backendConn.WriteMessage(mt, msg); writeErr != nil {
				s.Debug.Printf("[%s] websocket backend write error: %v", reqID, writeErr)
				return
			}
		}
	}()

	// Start ping tickers
	go wsPingTicker(ctx, clientConn)
	go wsPingTicker(ctx, backendConn)

	// When one side exits, close the backend conn and set a short read deadline
	// on the client conn to unblock both goroutines.
	// We must NOT close clientConn here — it may be reused for the next backend retry.
	go func() {
		<-ctx.Done()
		backendConn.Close()
		// Set an immediate read deadline on clientConn to unblock ReadMessage
		clientConn.SetReadDeadline(time.Now())
	}()

	// Wait for both relay goroutines to finish
	wg.Wait()

	// Forward backend close frame to client only when data was already relayed
	// (i.e. we can't retry another backend). When no data was relayed, keep
	// the client WS open so handleWebSocket can retry the next backend or
	// fall back to HTTP streaming.
	if dataRelayed {
		if backendCloseCode != 0 && backendCloseCode != websocket.CloseNormalClosure {
			s.Debug.Printf("[%s] websocket forwarding backend close to client: code=%d text=%s", reqID, backendCloseCode, backendCloseText)
			writeWSClose(clientConn, backendCloseCode, backendCloseText)
		} else {
			writeWSClose(clientConn, websocket.CloseNormalClosure, "")
		}
	}

	// Report health based on how connection ended
	// Skip health penalty for WS-not-supported (caller handles SSE fallback)
	wsNotSupErr := backendReadErr != nil && !dataRelayed &&
		isWSNotSupported(&wsRelayError{err: backendReadErr, dataRelayed: false})
	if backendReadErr != nil && !wsNotSupErr {
		snap.health.ReportFailure(backend.Name)
	}

	// Log usage
	usageMu.Lock()
	finalUsage := usage
	usageMu.Unlock()

	errCat := "success"
	statusCode := 200
	errMsg := ""
	if backendReadErr != nil && !dataRelayed {
		if wsNotSupErr {
			// WS not supported — not a real error, caller will try SSE
			errCat = "ws_not_supported"
		} else {
			errCat = "network_error"
		}
		statusCode = 502
		errMsg = fmt.Sprintf("websocket backend error: %v", backendReadErr)
	} else if backendReadErr != nil {
		// Backend failed after sending some data
		errCat = "network_error"
		statusCode = 502
		errMsg = fmt.Sprintf("websocket backend error: %v", backendReadErr)
	}

	s.logUsage(logging.UsageLog{
		Backend:          backend.Name,
		ClientProtocol:   clientProtocol,
		BackendProtocol:  backend.Protocol,
		ClientKey:        keyLabel,
		RequestModel:     requestModel,
		ForwardedModel:   forwardedModel,
		ResponseModel:    finalUsage.ResponseModel,
		StatusCode:       statusCode,
		Transport:        "ws",
		PromptTokens:     finalUsage.PromptTokens,
		CompletionTokens: finalUsage.CompletionTokens,
		TotalTokens:      finalUsage.TotalTokens,
		InputTokens:      finalUsage.InputTokens,
		OutputTokens:     finalUsage.OutputTokens,
		Error:            errMsg,
	}, start, reqID, 0, errCat)

	s.Debug.Printf("[%s] websocket connection closed: backend=%s duration=%s usage=%+v", reqID, backend.Name, time.Since(start), finalUsage)

	// If backend immediately closed without relaying data, it's a retriable error
	if backendReadErr != nil && !dataRelayed {
		return &wsRelayError{err: backendReadErr, dataRelayed: false}
	}

	return nil
}

// resolveWSCandidates resolves backend candidates for a WebSocket request.
func resolveWSCandidates(snap serverSnapshot, requestModel string) []modelEntry {
	if requestModel != "" {
		entries, found := snap.modelIndex.ResolveCandidates(requestModel)
		if found {
			return entries
		}
		if snap.modelIndex.DefaultBackend() != nil {
			return []modelEntry{{
				backend:      snap.modelIndex.DefaultBackend(),
				backendModel: requestModel,
			}}
		}
	} else {
		if snap.modelIndex.DefaultBackend() != nil {
			return []modelEntry{{
				backend:      snap.modelIndex.DefaultBackend(),
				backendModel: "",
			}}
		}
	}
	return nil
}

// composeWSURL converts a backend's HTTP URL to WebSocket URL for the given path.
func composeWSURL(backend *config.Backend, incomingURL *url.URL) (string, error) {
	httpURL, err := composeTargetURL(backend, incomingURL)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	}
	return parsed.String(), nil
}

// extractWSUsage extracts usage info from a WebSocket message if it's a response.completed event.
func extractWSUsage(msg []byte) (logging.UsageInfo, bool) {
	var data map[string]any
	if err := json.Unmarshal(msg, &data); err != nil {
		return logging.UsageInfo{}, false
	}

	eventType, _ := data["type"].(string)
	if eventType != "response.completed" && eventType != "response.done" {
		return logging.UsageInfo{}, false
	}

	// response.completed may have the response nested under "response" key
	completed := data
	if responseObj, ok := data["response"].(map[string]any); ok {
		completed = responseObj
	}

	var info logging.UsageInfo
	if usage, ok := completed["usage"].(map[string]any); ok {
		info.InputTokens = int(getFloat(usage, "input_tokens"))
		info.OutputTokens = int(getFloat(usage, "output_tokens"))
		info.PromptTokens = info.InputTokens
		info.CompletionTokens = info.OutputTokens
		info.TotalTokens = info.InputTokens + info.OutputTokens
	}
	if m, ok := completed["model"].(string); ok && m != "" {
		info.ResponseModel = m
	}

	return info, true
}

// writeWSClose sends a close frame with a message.
func writeWSClose(conn *websocket.Conn, code int, msg string) {
	closeMsg := websocket.FormatCloseMessage(code, msg)
	_ = conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(5*time.Second))
}

// isNormalWSClose returns true if the error indicates a normal WebSocket closure.
func isNormalWSClose(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	)
}

// setupPingPong configures pong handler to keep connections alive.
func setupPingPong(conn *websocket.Conn) {
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
}

// wsPingTicker sends periodic pings to keep the connection alive.
func wsPingTicker(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteDeadline)); err != nil {
				return
			}
		}
	}
}
