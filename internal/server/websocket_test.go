package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{
			name: "valid upgrade",
			header: http.Header{
				"Connection": []string{"Upgrade"},
				"Upgrade":    []string{"websocket"},
			},
			want: true,
		},
		{
			name: "case insensitive",
			header: http.Header{
				"Connection": []string{"upgrade"},
				"Upgrade":    []string{"WebSocket"},
			},
			want: true,
		},
		{
			name: "connection with multiple values",
			header: http.Header{
				"Connection": []string{"keep-alive, Upgrade"},
				"Upgrade":    []string{"websocket"},
			},
			want: true,
		},
		{
			name: "missing upgrade header",
			header: http.Header{
				"Connection": []string{"Upgrade"},
			},
			want: false,
		},
		{
			name: "missing connection header",
			header: http.Header{
				"Upgrade": []string{"websocket"},
			},
			want: false,
		},
		{
			name:   "empty headers",
			header: http.Header{},
			want:   false,
		},
		{
			name: "wrong upgrade value",
			header: http.Header{
				"Connection": []string{"Upgrade"},
				"Upgrade":    []string{"h2c"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Header: tt.header}
			if got := isWebSocketUpgrade(r); got != tt.want {
				t.Errorf("isWebSocketUpgrade() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComposeWSURL(t *testing.T) {
	tests := []struct {
		name       string
		backendURL string
		path       string
		want       string
	}{
		{
			name:       "http to ws",
			backendURL: "http://localhost:8080",
			path:       "/v1/responses",
			want:       "ws://localhost:8080/v1/responses",
		},
		{
			name:       "https to wss",
			backendURL: "https://api.example.com",
			path:       "/v1/responses",
			want:       "wss://api.example.com/v1/responses",
		},
		{
			name:       "with base path",
			backendURL: "https://api.example.com/proxy",
			path:       "/v1/responses",
			want:       "wss://api.example.com/proxy/v1/responses",
		},
		{
			name:       "preserves query string",
			backendURL: "http://localhost:8080",
			path:       "/v1/responses?foo=bar",
			want:       "ws://localhost:8080/v1/responses?foo=bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &config.Backend{URL: tt.backendURL}
			u, _ := url.Parse("http://proxy" + tt.path)
			got, err := composeWSURL(backend, u)
			if err != nil {
				t.Fatalf("composeWSURL() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("composeWSURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractWSUsage(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		wantOK  bool
		wantIn  int
		wantOut int
		wantMod string
	}{
		{
			name: "response.completed with usage",
			msg: `{
				"type": "response.completed",
				"response": {
					"model": "gpt-4o",
					"usage": {"input_tokens": 100, "output_tokens": 50}
				}
			}`,
			wantOK:  true,
			wantIn:  100,
			wantOut: 50,
			wantMod: "gpt-4o",
		},
		{
			name: "response.done with usage",
			msg: `{
				"type": "response.done",
				"response": {
					"model": "gpt-5",
					"usage": {"input_tokens": 200, "output_tokens": 100}
				}
			}`,
			wantOK:  true,
			wantIn:  200,
			wantOut: 100,
			wantMod: "gpt-5",
		},
		{
			name:   "text delta event",
			msg:    `{"type": "response.output_text.delta", "delta": "hello"}`,
			wantOK: false,
		},
		{
			name:   "invalid json",
			msg:    `not json`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := extractWSUsage([]byte(tt.msg))
			if ok != tt.wantOK {
				t.Fatalf("extractWSUsage() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if info.InputTokens != tt.wantIn {
				t.Errorf("InputTokens = %d, want %d", info.InputTokens, tt.wantIn)
			}
			if info.OutputTokens != tt.wantOut {
				t.Errorf("OutputTokens = %d, want %d", info.OutputTokens, tt.wantOut)
			}
			if info.ResponseModel != tt.wantMod {
				t.Errorf("ResponseModel = %q, want %q", info.ResponseModel, tt.wantMod)
			}
		})
	}
}

func TestWebSocketBasicRelay(t *testing.T) {
	// Create a mock backend WS server
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("backend upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Read first message (the request)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}

		// Parse request to get model
		var req map[string]any
		json.Unmarshal(msg, &req)

		// Send a text delta
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))

		// Send response.completed with usage
		completed := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"model":  "test-model",
				"status": "completed",
				"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5},
			},
		}
		completedJSON, _ := json.Marshal(completed)
		conn.WriteMessage(websocket.TextMessage, completedJSON)

		// Close
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
	}))
	defer backendServer.Close()

	// Setup proxy server with backend
	backendURL := strings.Replace(backendServer.URL, "http://", "", 1)
	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test-backend",
			URL:      "http://" + backendURL,
			Protocol: "openai-responses",
			Default:  true,
			Models: []config.Model{{
				Name:    "test-model",
				Aliases: []string{"my-alias"},
			}},
		}},
	}

	debugLog := logging.NewDebugLog(false, "")
	health := NewHealthManager(HealthManagerConfig{
		FailThreshold:       3,
		RecoveryTimeout:     30 * time.Second,
		HalfOpenMaxRequests: 1,
	}, http.DefaultClient, debugLog)

	srv := New(cfg, &logging.TokenLogger{}, debugLog, http.DefaultClient, health, NewNoopObserver())

	// Start proxy WS server
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	// Connect to proxy via WS
	wsURL := strings.Replace(proxyServer.URL, "http://", "ws://", 1) + "/v1/responses"
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}
	defer clientConn.Close()

	// Send first message with model
	request := map[string]any{
		"model": "my-alias",
		"input": "test",
	}
	reqJSON, _ := json.Marshal(request)
	if err := clientConn.WriteMessage(websocket.TextMessage, reqJSON); err != nil {
		t.Fatalf("write first message failed: %v", err)
	}

	// Read responses
	var messages []map[string]any
	for {
		clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := clientConn.ReadMessage()
		if err != nil {
			break // connection closed or timeout
		}
		var parsed map[string]any
		if json.Unmarshal(msg, &parsed) == nil {
			messages = append(messages, parsed)
		}
	}

	if len(messages) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(messages))
	}

	// Verify text delta
	if messages[0]["type"] != "response.output_text.delta" {
		t.Errorf("first message type = %v, want response.output_text.delta", messages[0]["type"])
	}

	// Verify response.completed
	if messages[1]["type"] != "response.completed" {
		t.Errorf("second message type = %v, want response.completed", messages[1]["type"])
	}
}

func TestWebSocketModelRouting(t *testing.T) {
	// Track which backend received the request
	var receivedModel string

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req map[string]any
		json.Unmarshal(msg, &req)
		receivedModel, _ = req["model"].(string)

		// Send completion and close
		completed, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"model":  receivedModel,
				"status": "completed",
				"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1},
			},
		})
		conn.WriteMessage(websocket.TextMessage, completed)
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
	}))
	defer backendServer.Close()

	backendURL := strings.Replace(backendServer.URL, "http://", "", 1)
	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      "http://" + backendURL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "backend-model-name",
				Aliases: []string{"client-alias"},
			}},
		}},
	}

	debugLog := logging.NewDebugLog(false, "")
	health := NewHealthManager(HealthManagerConfig{
		FailThreshold:       3,
		RecoveryTimeout:     30 * time.Second,
		HalfOpenMaxRequests: 1,
	}, http.DefaultClient, debugLog)

	srv := New(cfg, &logging.TokenLogger{}, debugLog, http.DefaultClient, health, NewNoopObserver())

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	wsURL := strings.Replace(proxyServer.URL, "http://", "ws://", 1) + "/v1/responses"
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	// Send with alias
	reqJSON, _ := json.Marshal(map[string]any{"model": "client-alias", "input": "test"})
	clientConn.WriteMessage(websocket.TextMessage, reqJSON)

	// Read until close
	for {
		clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := clientConn.ReadMessage()
		if err != nil {
			break
		}
	}

	// Verify model was rewritten
	if receivedModel != "backend-model-name" {
		t.Errorf("backend received model = %q, want %q", receivedModel, "backend-model-name")
	}
}

func TestRelaySSEToWS(t *testing.T) {
	// Create a real WS server/client pair for testing
	var received []string
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Read messages until close
		for {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			received = append(received, string(msg))
		}
	}))
	defer wsServer.Close()

	// Connect as WS client
	wsURL := strings.Replace(wsServer.URL, "http://", "ws://", 1)
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	// Simulate SSE stream
	sseData := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n\n" +
		"data: [DONE]\n\n"

	debugLog := logging.NewDebugLog(false, "")
	srv := &Server{Debug: debugLog}

	usage := srv.relaySSEToWS(clientConn, strings.NewReader(sseData), "test-req")

	// Close WS so server collects messages
	clientConn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	time.Sleep(100 * time.Millisecond)

	// Verify usage extraction
	if usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", usage.OutputTokens)
	}
	if usage.ResponseModel != "gpt-5" {
		t.Errorf("ResponseModel = %q, want %q", usage.ResponseModel, "gpt-5")
	}

	// Verify WS messages received by server (3 data events, [DONE] skipped)
	if len(received) != 3 {
		t.Fatalf("expected 3 WS messages, got %d: %v", len(received), received)
	}

	// Verify first message is response.created
	var msg0 map[string]any
	json.Unmarshal([]byte(received[0]), &msg0)
	if msg0["type"] != "response.created" {
		t.Errorf("message[0] type = %v, want response.created", msg0["type"])
	}

	// Verify second message is text delta
	var msg1 map[string]any
	json.Unmarshal([]byte(received[1]), &msg1)
	if msg1["type"] != "response.output_text.delta" {
		t.Errorf("message[1] type = %v, want response.output_text.delta", msg1["type"])
	}
}

func TestUnwrapWSEnvelope(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]any
	}{
		{
			name: "flat format strips type and WS fields",
			body: `{"type":"response.create","model":"gpt-5","input":"hello","generate":false,"client_metadata":{"foo":"bar"}}`,
			want: map[string]any{"model": "gpt-5", "input": "hello"},
		},
		{
			name: "nested format extracts response object",
			body: `{"type":"response.create","response":{"model":"gpt-5","input":"hello"}}`,
			want: map[string]any{"model": "gpt-5", "input": "hello"},
		},
		{
			name: "no type field returns as-is",
			body: `{"model":"gpt-5","input":"hello"}`,
			want: map[string]any{"model": "gpt-5", "input": "hello"},
		},
		{
			name: "non response.create type returns as-is",
			body: `{"type":"conversation.item.create","item":{"role":"user"}}`,
			want: map[string]any{"type": "conversation.item.create", "item": map[string]any{"role": "user"}},
		},
		{
			name: "flat format preserves API fields",
			body: `{"type":"response.create","model":"gpt-5","tools":[{"type":"function"}],"reasoning":{"effort":"high"},"stream":true}`,
			want: map[string]any{"model": "gpt-5", "tools": []any{map[string]any{"type": "function"}}, "reasoning": map[string]any{"effort": "high"}, "stream": true},
		},
		{
			name: "strips event_id",
			body: `{"type":"response.create","event_id":"evt_123","model":"gpt-5"}`,
			want: map[string]any{"model": "gpt-5"},
		},
		{
			name: "invalid json returns as-is",
			body: `not json`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := unwrapWSEnvelope([]byte(tt.body))
			if tt.want == nil {
				if string(result) != tt.body {
					t.Errorf("expected body unchanged, got %s", string(result))
				}
				return
			}

			var got map[string]any
			if err := json.Unmarshal(result, &got); err != nil {
				t.Fatalf("unmarshal result failed: %v, body=%s", err, string(result))
			}

			// Check no WS-specific fields remain (for response.create)
			for _, wsField := range []string{"type", "generate", "client_metadata", "event_id"} {
				if _, exists := got[wsField]; exists {
					if tt.want[wsField] == nil {
						t.Errorf("WS field %q should be stripped, but found in result", wsField)
					}
				}
			}

			// Check expected fields exist
			for k, v := range tt.want {
				gotVal, exists := got[k]
				if !exists {
					t.Errorf("expected field %q missing from result", k)
					continue
				}
				wantJSON, _ := json.Marshal(v)
				gotJSON, _ := json.Marshal(gotVal)
				if string(wantJSON) != string(gotJSON) {
					t.Errorf("field %q = %s, want %s", k, string(gotJSON), string(wantJSON))
				}
			}
		})
	}
}

func TestBuildHTTPFallbackRequest(t *testing.T) {
	original := &http.Request{
		Method: "GET",
		URL:    &url.URL{Path: "/v1/responses", RawQuery: "beta=true"},
		Header: http.Header{
			"Connection":              []string{"Upgrade"},
			"Upgrade":                 []string{"websocket"},
			"Sec-Websocket-Key":       []string{"test-key"},
			"Sec-Websocket-Version":   []string{"13"},
			"Authorization":           []string{"Bearer sk-test"},
			"X-Custom":                []string{"keep-me"},
		},
	}

	result := buildHTTPFallbackRequest(original)

	// Method should be POST
	if result.Method != "POST" {
		t.Errorf("Method = %q, want POST", result.Method)
	}

	// Path should be preserved
	if result.URL.Path != "/v1/responses" {
		t.Errorf("Path = %q, want /v1/responses", result.URL.Path)
	}

	// Query should be preserved
	if result.URL.RawQuery != "beta=true" {
		t.Errorf("RawQuery = %q, want beta=true", result.URL.RawQuery)
	}

	// WS headers should be removed
	wsHeaders := []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version"}
	for _, h := range wsHeaders {
		if result.Header.Get(h) != "" {
			t.Errorf("WS header %q should be removed, got %q", h, result.Header.Get(h))
		}
	}

	// Non-WS headers should be preserved
	if result.Header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("Authorization header should be preserved")
	}
	if result.Header.Get("X-Custom") != "keep-me" {
		t.Errorf("X-Custom header should be preserved")
	}

	// Original request should not be modified
	if original.Method != "GET" {
		t.Errorf("Original method was modified: %q", original.Method)
	}
	if original.Header.Get("Connection") != "Upgrade" {
		t.Errorf("Original Connection header was modified")
	}
}

func TestWSHTTPFallback(t *testing.T) {
	// Backend that fails WebSocket but succeeds on HTTP
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			// Reject WS connections
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			// Immediately close with error
			conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(1013, "no available account"),
				time.Now().Add(time.Second))
			conn.Close()
			return
		}

		// HTTP POST succeeds with SSE stream
		if r.Method == "POST" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)

			flusher, ok := w.(http.Flusher)
			if !ok {
				return
			}

			events := []string{
				`event: response.created` + "\n" + `data: {"type":"response.created","response":{"id":"resp_fb"}}` + "\n\n",
				`event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","delta":"fallback works"}` + "\n\n",
				`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"model":"test-model","status":"completed","usage":{"input_tokens":20,"output_tokens":10}}}` + "\n\n",
				"data: [DONE]\n\n",
			}

			for _, event := range events {
				w.Write([]byte(event))
				flusher.Flush()
			}
			return
		}
	}))
	defer backendServer.Close()

	backendURL := strings.Replace(backendServer.URL, "http://", "", 1)
	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "fallback-test",
			URL:      "http://" + backendURL,
			Protocol: "openai-responses",
			Default:  true,
			Models: []config.Model{{
				Name:    "test-model",
				Aliases: []string{"my-model"},
			}},
		}},
	}

	debugLog := logging.NewDebugLog(false, "")
	health := NewHealthManager(HealthManagerConfig{
		FailThreshold:       10, // High threshold to avoid circuit breaking during test
		RecoveryTimeout:     30 * time.Second,
		HalfOpenMaxRequests: 1,
	}, http.DefaultClient, debugLog)

	srv := New(cfg, &logging.TokenLogger{}, debugLog, http.DefaultClient, health, NewNoopObserver())

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	// Connect via WebSocket — should fail WS, then succeed via HTTP fallback
	wsURL := strings.Replace(proxyServer.URL, "http://", "ws://", 1) + "/v1/responses"
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}
	defer clientConn.Close()

	// Send request
	reqJSON, _ := json.Marshal(map[string]any{"model": "my-model", "input": "test"})
	if err := clientConn.WriteMessage(websocket.TextMessage, reqJSON); err != nil {
		t.Fatalf("write first message failed: %v", err)
	}

	// Read responses — should get events from HTTP fallback
	var messages []map[string]any
	for {
		clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := clientConn.ReadMessage()
		if err != nil {
			break
		}
		var parsed map[string]any
		if json.Unmarshal(msg, &parsed) == nil {
			messages = append(messages, parsed)
		}
	}

	if len(messages) < 2 {
		t.Fatalf("expected at least 2 messages from HTTP fallback, got %d", len(messages))
	}

	// Verify we got the expected events
	if messages[0]["type"] != "response.created" {
		t.Errorf("first message type = %v, want response.created", messages[0]["type"])
	}

	// Verify text delta contains fallback content
	if messages[1]["type"] != "response.output_text.delta" {
		t.Errorf("second message type = %v, want response.output_text.delta", messages[1]["type"])
	}
	if delta, ok := messages[1]["delta"].(string); ok && delta != "fallback works" {
		t.Errorf("delta = %q, want %q", delta, "fallback works")
	}
}
