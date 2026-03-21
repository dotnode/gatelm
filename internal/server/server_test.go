package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

func newTestServerWithObserver(cfg config.Config, observer Observer) *Server {
	hm := NewHealthManager(HealthManagerConfig{
		FailThreshold:   1,
		RecoveryTimeout: 30 * time.Second,
	}, http.DefaultClient, logging.NewDebugLog(false, ""))
	return New(cfg, &logging.TokenLogger{}, logging.NewDebugLog(false, ""), http.DefaultClient, hm, observer)
}

func newTestServer(cfg config.Config) *Server {
	return newTestServerWithObserver(cfg, NewNoopObserver())
}

func gzipBytes(t *testing.T, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(src); err != nil {
		t.Fatalf("gzip write failed: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}
	return buf.Bytes()
}

func brotliBytes(t *testing.T, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := brotli.NewWriter(&buf)
	if _, err := zw.Write(src); err != nil {
		t.Fatalf("brotli write failed: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("brotli close failed: %v", err)
	}
	return buf.Bytes()
}

func TestStreamingPassthrough(t *testing.T) {
	events := []string{
		`data: {"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hello"}}],"model":"gpt-4"}`,
		``,
		`data: {"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"content":" world"}}],"model":"gpt-4"}`,
		``,
		`data: {"id":"1","object":"chat.completion.chunk","choices":[],"model":"gpt-4","usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected Content-Type text/event-stream, got %s", ct)
	}

	respBody, _ := io.ReadAll(resp.Body)
	scanner := bufio.NewScanner(bytes.NewReader(respBody))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if len(lines) < 4 {
		t.Fatalf("expected at least 4 lines, got %d: %v", len(lines), lines)
	}

	found := false
	for _, l := range lines {
		if l == events[0] {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("first event not found in response")
	}
}

func TestNonStreamingUnchanged(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"chatcmpl-1","model":"gpt-4","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["model"] != "gpt-4" {
		t.Fatalf("unexpected model: %v", result["model"])
	}
}

func TestProtocolConversionStreaming(t *testing.T) {
	events := []string{
		`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: [DONE]`,
		``,
	}

	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test-backend",
			URL:      backend.URL,
			Protocol: "openai",
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-3","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected Content-Type text/event-stream, got %s", ct)
	}

	if !bytes.Contains(receivedBody, []byte(`"stream":true`)) {
		t.Fatalf("backend should receive stream:true, got: %s", receivedBody)
	}
	if !bytes.Contains(receivedBody, []byte(`"stream_options"`)) {
		t.Fatalf("backend should receive stream_options, got: %s", receivedBody)
	}

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	requiredEvents := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	for _, ev := range requiredEvents {
		if !strings.Contains(respStr, ev) {
			t.Errorf("missing event %q in response:\n%s", ev, respStr)
		}
	}

	if !strings.Contains(respStr, `"text":"hello"`) {
		t.Errorf("missing 'hello' content delta in response")
	}
	if !strings.Contains(respStr, `"text":" world"`) {
		t.Errorf("missing ' world' content delta in response")
	}

	if !strings.Contains(respStr, `"stop_reason":"end_turn"`) {
		t.Errorf("missing stop_reason end_turn in response")
	}
}

func TestStreamingConversionEvents(t *testing.T) {
	events := []string{
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`,
		``,
		`data: [DONE]`,
		``,
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-3","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"test"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var sseEvents []struct {
		event string
		data  map[string]any
	}
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = line[7:]
		} else if strings.HasPrefix(line, "data: ") {
			var data map[string]any
			json.Unmarshal([]byte(line[6:]), &data)
			sseEvents = append(sseEvents, struct {
				event string
				data  map[string]any
			}{currentEvent, data})
			currentEvent = ""
		}
	}

	if len(sseEvents) < 6 {
		t.Fatalf("expected at least 6 SSE events, got %d", len(sseEvents))
	}

	expectedOrder := []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	for i, want := range expectedOrder {
		if i >= len(sseEvents) {
			t.Fatalf("missing event at index %d: want %s", i, want)
		}
		if sseEvents[i].event != want {
			t.Errorf("event[%d] = %q, want %q", i, sseEvents[i].event, want)
		}
	}

	msgStart := sseEvents[0].data
	if msgStart["type"] != "message_start" {
		t.Errorf("message_start type = %v", msgStart["type"])
	}
	msg := msgStart["message"].(map[string]any)
	if msg["model"] != "gpt-4" {
		t.Errorf("model = %v, want gpt-4", msg["model"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}

	delta := sseEvents[3].data
	if delta["type"] != "content_block_delta" {
		t.Errorf("delta type = %v", delta["type"])
	}
	deltaInner := delta["delta"].(map[string]any)
	if deltaInner["text"] != "Hi" {
		t.Errorf("delta text = %v, want Hi", deltaInner["text"])
	}

	msgDelta := sseEvents[5].data
	deltaObj := msgDelta["delta"].(map[string]any)
	if deltaObj["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", deltaObj["stop_reason"])
	}
	usageObj := msgDelta["usage"].(map[string]any)
	if usageObj["output_tokens"] != float64(3) {
		t.Errorf("output_tokens = %v, want 3", usageObj["output_tokens"])
	}
}

func TestStreamingConversionErrorFallback(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-3","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["type"] != "error" {
		t.Fatalf("expected error type, got %v", result["type"])
	}
	errObj := result["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Fatalf("error type = %v, want rate_limit_error", errObj["type"])
	}
}

func TestStreamingConversionToolCalls(t *testing.T) {
	events := []string{
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"NYC\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`,
		``,
		`data: [DONE]`,
		``,
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-3","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	type sseEvent struct {
		event string
		data  map[string]any
	}
	var sseEvents []sseEvent
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = line[7:]
		} else if strings.HasPrefix(line, "data: ") {
			var data map[string]any
			json.Unmarshal([]byte(line[6:]), &data)
			sseEvents = append(sseEvents, sseEvent{currentEvent, data})
			currentEvent = ""
		}
	}

	hasToolUseStart := false
	hasInputJsonDelta := false
	hasToolUseStop := false

	for _, e := range sseEvents {
		if e.event == "content_block_start" {
			cb, ok := e.data["content_block"].(map[string]any)
			if ok && cb["type"] == "tool_use" {
				hasToolUseStart = true
				if cb["id"] != "call_abc" {
					t.Errorf("tool_use id = %v, want call_abc", cb["id"])
				}
				if cb["name"] != "get_weather" {
					t.Errorf("tool_use name = %v, want get_weather", cb["name"])
				}
			}
		}
		if e.event == "content_block_delta" {
			delta, ok := e.data["delta"].(map[string]any)
			if ok && delta["type"] == "input_json_delta" {
				hasInputJsonDelta = true
			}
		}
		if e.event == "content_block_stop" {
			hasToolUseStop = true
		}
		if e.event == "message_delta" {
			delta, ok := e.data["delta"].(map[string]any)
			if ok && delta["stop_reason"] != "tool_use" {
				t.Errorf("stop_reason = %v, want tool_use", delta["stop_reason"])
			}
		}
	}

	if !hasToolUseStart {
		t.Error("missing content_block_start with type tool_use")
	}
	if !hasInputJsonDelta {
		t.Error("missing content_block_delta with type input_json_delta")
	}
	if !hasToolUseStop {
		t.Error("missing content_block_stop for tool_use")
	}
}

func TestStreamingResponsesConversionUpstreamFailedEmitsError(t *testing.T) {
	events := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_fail","model":"gpt-5","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"msg_1","type":"message","status":"in_progress","content":[],"role":"assistant"},"output_index":0,"sequence_number":1}`,
		``,
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","content_index":0,"item_id":"msg_1","output_index":0,"part":{"type":"output_text","text":""},"sequence_number":2}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","content_index":0,"delta":"hi","item_id":"msg_1","output_index":0,"sequence_number":3}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"server_error","message":"upstream stream failed"},"sequence_number":4}`,
		``,
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_fail","status":"failed","error":{"code":"server_error","message":"upstream stream failed"}}}`,
		``,
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "responses-backend",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5",
				Aliases: []string{"claude-opus"},
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	if !strings.Contains(respStr, `event: error`) {
		t.Fatalf("expected error event in response, got:\n%s", respStr)
	}
	if !strings.Contains(respStr, `"upstream streaming failed"`) {
		t.Fatalf("expected upstream error message in response, got:\n%s", respStr)
	}
	if strings.Contains(respStr, `"stop_reason":"end_turn"`) {
		t.Fatalf("unexpected end_turn on upstream failure, got:\n%s", respStr)
	}
}

func TestSystemPromptInjectionForOpenAIChat(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"chatcmpl-1","model":"gpt-4","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		SystemPrompt: "Global style",
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:         "gpt-4",
				Aliases:      []string{"claude-sonnet"},
				SystemPrompt: "Model style",
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-sonnet","max_tokens":100,"system":"User context","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if !bytes.Contains(receivedBody, []byte(`"role":"system"`)) {
		t.Fatalf("expected system role in upstream request, got: %s", receivedBody)
	}
	if !bytes.Contains(receivedBody, []byte(`"content":"Model style\n\nUser context"`)) {
		t.Fatalf("expected merged model-level system prompt, got: %s", receivedBody)
	}
}

func TestSystemPromptInjectionForOpenAIResponses(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"resp_1","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		SystemPrompt: "Global style",
		Backends: []config.Backend{{
			Name:     "responses",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5",
				Aliases: []string{"claude-opus"},
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"system":"User context","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if !bytes.Contains(receivedBody, []byte(`"role":"developer"`)) {
		t.Fatalf("expected developer role in upstream request, got: %s", receivedBody)
	}
	if !bytes.Contains(receivedBody, []byte(`"content":"Global style\n\nUser context"`)) {
		t.Fatalf("expected merged global system prompt in upstream request, got: %s", receivedBody)
	}
}

func TestStreamingResponsesConversionNestedCompletedToolUseStopReason(t *testing.T) {
	events := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_test","model":"gpt-5","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_abc","name":"Read"},"output_index":0,"sequence_number":2}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"file_path\":\"/tmp/a\"}","item_id":"fc_1","output_index":0,"sequence_number":3}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","arguments":"{\"file_path\":\"/tmp/a\"}","item_id":"fc_1","output_index":0,"sequence_number":4}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_test","model":"gpt-5","status":"completed","usage":{"input_tokens":12,"output_tokens":7},"output":[{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"file_path\":\"/tmp/a\"}","call_id":"call_abc","name":"Read"}]}}`,
		``,
	}

	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "responses-backend",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:                   "gpt-5",
				Aliases:                []string{"claude-opus"},
				ReasoningDefaultEffort: "high",
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"stream":true,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"read"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	if !strings.Contains(respStr, `"type":"tool_use"`) {
		t.Fatal("missing tool_use content block in response")
	}
	if !strings.Contains(respStr, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason tool_use, got response:\n%s", respStr)
	}
	if !strings.Contains(respStr, `"output_tokens":7`) {
		t.Fatalf("expected output_tokens 7, got response:\n%s", respStr)
	}
	if !bytes.Contains(receivedBody, []byte(`"reasoning":{"effort":"high"}`)) {
		t.Fatalf("expected model-level responses reasoning high in upstream request, got: %s", receivedBody)
	}
	if bytes.Contains(receivedBody, []byte(`"reasoning_effort":`)) {
		t.Fatalf("responses upstream request should not contain reasoning_effort, got: %s", receivedBody)
	}
}

func TestStreamingResponsesConversionForcesResponsesStreamFlag(t *testing.T) {
	events := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_test","model":"gpt-5","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"msg_1","type":"message","status":"in_progress","content":[],"role":"assistant"},"output_index":0,"sequence_number":1}`,
		``,
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","content_index":0,"item_id":"msg_1","output_index":0,"part":{"type":"output_text","text":""},"sequence_number":2}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","content_index":0,"delta":"hi","item_id":"msg_1","output_index":0,"sequence_number":3}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_test","model":"gpt-5","status":"completed","usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}}`,
		``,
	}

	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "responses-backend",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5",
				Aliases: []string{"claude-opus"},
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream request failed: %v", err)
	}
	if upstream["stream"] != true {
		t.Fatalf("expected upstream stream=true, got %v", upstream["stream"])
	}
}

func TestStreamingResponsesConversionSendsAutoToolChoiceWhenToolsPresent(t *testing.T) {
	events := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_test","model":"gpt-5","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_read","name":"Read"},"output_index":0,"sequence_number":2}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"file_path\":\"/tmp/a\"}","item_id":"fc_1","output_index":0,"sequence_number":3}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","arguments":"{\"file_path\":\"/tmp/a\"}","item_id":"fc_1","output_index":0,"sequence_number":4}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_test","model":"gpt-5","status":"completed","usage":{"input_tokens":12,"output_tokens":7},"output":[{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"file_path\":\"/tmp/a\"}","call_id":"call_read","name":"Read"}]}}`,
		``,
	}

	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "responses-backend",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5",
				Aliases: []string{"claude-opus"},
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{
		"model":"claude-opus",
		"max_tokens":100,
		"stream":true,
		"tools":[{"name":"Read","description":"Read file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
		"messages":[{"role":"user","content":"read"}]
	}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)
	if !strings.Contains(respStr, `"type":"tool_use"`) {
		t.Fatal("missing tool_use content block in response")
	}
	if !strings.Contains(respStr, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason tool_use, got response:\n%s", respStr)
	}
	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream request failed: %v", err)
	}
	if upstream["tool_choice"] != "auto" {
		t.Fatalf("expected default tool_choice auto in upstream request, got: %v", upstream["tool_choice"])
	}
	tools, ok := upstream["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 upstream tool, got: %v", upstream["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected upstream tool payload: %T", tools[0])
	}
	if tool["type"] != "function" || tool["name"] != "Read" {
		t.Fatalf("expected Responses flat tool definition, got: %v", tool)
	}
}

func TestNonStreamingResponsesConversionKeepsNonStreamMode(t *testing.T) {
	payload := []byte(`{"id":"resp_1","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		compressed := gzipBytes(t, payload)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		w.WriteHeader(200)
		_, _ = w.Write(compressed)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "responses-backend",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models:   []config.Model{{Name: "gpt-5", Aliases: []string{"claude-opus"}}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)
	if bytes.Contains(receivedBody, []byte(`"stream":true`)) {
		t.Fatalf("expected upstream request to keep non-stream mode, got: %s", receivedBody)
	}
	if !strings.Contains(respStr, `"text":"ok"`) {
		t.Fatalf("expected converted text content, got: %s", respStr)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("expected no content-encoding after decode, got %q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(respBody)) {
		t.Fatalf("expected content-length %d, got %q", len(respBody), got)
	}
}

func TestNonStreamingResponsesConversionDecodesBrotli(t *testing.T) {
	payload := []byte(`{"id":"resp_2","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"brotli ok"}]}],"usage":{"input_tokens":4,"output_tokens":3}}`)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compressed := brotliBytes(t, payload)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		w.WriteHeader(200)
		_, _ = w.Write(compressed)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "responses-backend",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models:   []config.Model{{Name: "gpt-5", Aliases: []string{"claude-opus"}}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)
	if !strings.Contains(respStr, `"text":"brotli ok"`) {
		t.Fatalf("expected converted brotli text content, got: %s", respStr)
	}
	if !strings.Contains(respStr, `"output_tokens":3`) {
		t.Fatalf("expected converted usage, got: %s", respStr)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("expected no content-encoding after decode, got %q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(respBody)) {
		t.Fatalf("expected content-length %d, got %q", len(respBody), got)
	}
}

func TestStreamingConversionTextThenToolCalls(t *testing.T) {
	events := []string{
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"content":"I'll check."},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"content":null,"tool_calls":[{"index":0,"id":"call_xyz","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/tmp\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range events {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-3","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"test"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	if !strings.Contains(respStr, `"text_delta"`) {
		t.Error("missing text_delta in response")
	}
	if !strings.Contains(respStr, `"tool_use"`) {
		t.Error("missing tool_use content block in response")
	}
	if !strings.Contains(respStr, `"input_json_delta"`) {
		t.Error("missing input_json_delta in response")
	}
	if !strings.Contains(respStr, `"stop_reason":"tool_use"`) {
		t.Error("missing stop_reason tool_use in response")
	}
}

func TestRetryOnBackendFailure(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int32

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(502)
		fmt.Fprint(w, `{"error":"bad gateway"}`)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4o","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer secondary.Close()

	cfg := config.Config{
		Backends: []config.Backend{
			{
				Name:     "primary",
				URL:      primary.URL,
				Protocol: "openai",
				Priority: 1,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
			},
			{
				Name:     "secondary",
				URL:      secondary.URL,
				Protocol: "openai",
				Priority: 2,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
			},
		},
	}
	observer := newCaptureObserver()
	srv := newTestServerWithObserver(cfg, observer)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 (retried to secondary), got %d", resp.StatusCode)
	}

	if primaryHits.Load() != 1 {
		t.Errorf("expected primary to be hit once, got %d", primaryHits.Load())
	}
	if secondaryHits.Load() != 1 {
		t.Errorf("expected secondary to be hit once, got %d", secondaryHits.Load())
	}

	reqEvent, ok := observer.latestRequest()
	if !ok {
		t.Fatal("expected request metric event")
	}
	if reqEvent.RetryCount != 1 {
		t.Fatalf("expected retry_count=1, got %d", reqEvent.RetryCount)
	}
	if reqEvent.ErrorCategory != "success" {
		t.Fatalf("expected error_category=success, got %s", reqEvent.ErrorCategory)
	}
	if reqEvent.Result != "success" {
		t.Fatalf("expected result=success, got %s", reqEvent.Result)
	}
	if reqEvent.Backend != "secondary" {
		t.Fatalf("expected backend=secondary, got %s", reqEvent.Backend)
	}

	attempts := observer.allAttempts()
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempt events, got %d", len(attempts))
	}
	if attempts[0].Backend != "primary" || attempts[0].Outcome != "failure" || attempts[0].ErrorCategory != "http_5xx" {
		t.Fatalf("unexpected first attempt: %+v", attempts[0])
	}
	if attempts[1].Backend != "secondary" || attempts[1].Outcome != "success" || attempts[1].ErrorCategory != "success" {
		t.Fatalf("unexpected second attempt: %+v", attempts[1])
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["model"] != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %v", result["model"])
	}
}

func TestAllBackendsFail(t *testing.T) {
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer backend2.Close()

	cfg := config.Config{
		Backends: []config.Backend{
			{
				Name:     "b1",
				URL:      backend1.URL,
				Protocol: "openai",
				Priority: 1,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4", Aliases: []string{"my-model"}}},
			},
			{
				Name:     "b2",
				URL:      backend2.URL,
				Protocol: "openai",
				Priority: 2,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4", Aliases: []string{"my-model"}}},
			},
		},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Last backend's 502 should be returned since it's the final attempt
	if resp.StatusCode != 502 {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
}

func TestPrioritySelectionHealthy(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int32

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"content":"from primary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"content":"from secondary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer secondary.Close()

	cfg := config.Config{
		Backends: []config.Backend{
			{
				Name:     "primary",
				URL:      primary.URL,
				Protocol: "openai",
				Priority: 1,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4", Aliases: []string{"my-model"}}},
			},
			{
				Name:     "secondary",
				URL:      secondary.URL,
				Protocol: "openai",
				Priority: 2,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4", Aliases: []string{"my-model"}}},
			},
		},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	// Send multiple requests — all should go to primary
	for i := 0; i < 3; i++ {
		body := []byte(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)
		resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: expected 200, got %d", i, resp.StatusCode)
		}
	}

	if primaryHits.Load() != 3 {
		t.Errorf("expected primary hit 3 times, got %d", primaryHits.Load())
	}
	if secondaryHits.Load() != 0 {
		t.Errorf("expected secondary hit 0 times, got %d", secondaryHits.Load())
	}
}

func TestNoRetryOnClientError4xx(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int32

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad request"}}`)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4o","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer secondary.Close()

	cfg := config.Config{
		Backends: []config.Backend{
			{
				Name:     "primary",
				URL:      primary.URL,
				Protocol: "openai",
				Priority: 1,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
			},
			{
				Name:     "secondary",
				URL:      secondary.URL,
				Protocol: "openai",
				Priority: 2,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
			},
		},
	}
	observer := newCaptureObserver()
	srv := newTestServerWithObserver(cfg, observer)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if primaryHits.Load() != 1 {
		t.Fatalf("expected primary hit once, got %d", primaryHits.Load())
	}
	if secondaryHits.Load() != 0 {
		t.Fatalf("expected secondary not hit, got %d", secondaryHits.Load())
	}

	reqEvent, ok := observer.latestRequest()
	if !ok {
		t.Fatal("expected request metric event")
	}
	if reqEvent.RetryCount != 0 {
		t.Fatalf("expected retry_count=0, got %d", reqEvent.RetryCount)
	}
	if reqEvent.ErrorCategory != "http_4xx" {
		t.Fatalf("expected error_category=http_4xx, got %s", reqEvent.ErrorCategory)
	}
	if reqEvent.Result != "failure" {
		t.Fatalf("expected result=failure, got %s", reqEvent.Result)
	}
	if reqEvent.Backend != "primary" {
		t.Fatalf("expected backend=primary, got %s", reqEvent.Backend)
	}
}

func TestRetryOnRateLimit429ToNextBackend(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int32

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4o","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer secondary.Close()

	cfg := config.Config{
		Backends: []config.Backend{
			{
				Name:     "primary",
				URL:      primary.URL,
				Protocol: "openai",
				Priority: 1,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
			},
			{
				Name:     "secondary",
				URL:      secondary.URL,
				Protocol: "openai",
				Priority: 2,
				Weight:   1,
				Models:   []config.Model{{Name: "gpt-4o", Aliases: []string{"claude-sonnet"}}},
			},
		},
	}
	observer := newCaptureObserver()
	srv := newTestServerWithObserver(cfg, observer)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if primaryHits.Load() != 1 {
		t.Fatalf("expected primary hit once, got %d", primaryHits.Load())
	}
	if secondaryHits.Load() != 1 {
		t.Fatalf("expected secondary hit once, got %d", secondaryHits.Load())
	}

	reqEvent, ok := observer.latestRequest()
	if !ok {
		t.Fatal("expected request metric event")
	}
	if reqEvent.RetryCount != 1 {
		t.Fatalf("expected retry_count=1, got %d", reqEvent.RetryCount)
	}
	if reqEvent.ErrorCategory != "success" {
		t.Fatalf("expected error_category=success, got %s", reqEvent.ErrorCategory)
	}
	if reqEvent.Result != "success" {
		t.Fatalf("expected result=success, got %s", reqEvent.Result)
	}
	if reqEvent.Backend != "secondary" {
		t.Fatalf("expected backend=secondary, got %s", reqEvent.Backend)
	}

	attempts := observer.allAttempts()
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempt events, got %d", len(attempts))
	}
	if attempts[0].Backend != "primary" || attempts[0].Outcome != "failure" || attempts[0].ErrorCategory != "http_429" {
		t.Fatalf("unexpected first attempt: %+v", attempts[0])
	}
	if attempts[1].Backend != "secondary" || attempts[1].Outcome != "success" || attempts[1].ErrorCategory != "success" {
		t.Fatalf("unexpected second attempt: %+v", attempts[1])
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("3"); d != 3*time.Second {
		t.Fatalf("expected 3s, got %s", d)
	}
	future := time.Now().Add(2 * time.Second).Format(http.TimeFormat)
	if d := parseRetryAfter(future); d <= 0 {
		t.Fatalf("expected positive duration for HTTP date, got %s", d)
	}
	if d := parseRetryAfter("not-a-date"); d != 0 {
		t.Fatalf("expected 0 for invalid header, got %s", d)
	}
}

func TestFallbackWhenSingleBackendCircuitBroken(t *testing.T) {
	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= 3 {
			// First 3 requests fail to trigger circuit breaker
			w.WriteHeader(502)
			fmt.Fprint(w, `{"error":"bad gateway"}`)
			return
		}
		// After that, backend recovers
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "only-backend",
			URL:      backend.URL,
			Protocol: "openai",
			Models:   []config.Model{{Name: "gpt-4", Aliases: []string{"my-model"}}},
		}},
	}
	// Use FailThreshold=1 so circuit opens after first failure
	hm := NewHealthManager(HealthManagerConfig{
		FailThreshold:   1,
		RecoveryTimeout: 30 * time.Second,
	}, http.DefaultClient, logging.NewDebugLog(false, ""))
	srv := New(cfg, &logging.TokenLogger{}, logging.NewDebugLog(false, ""), http.DefaultClient, hm, NewNoopObserver())

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	sendReq := func() *http.Response {
		body := []byte(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)
		resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		return resp
	}

	// First request: reaches backend, gets 502, triggers circuit trip
	resp := sendReq()
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("req 1: expected 502, got %d", resp.StatusCode)
	}

	// Circuit is now tripped. Without fallback, this would return 503.
	// With fallback, it should still reach the backend.
	resp = sendReq()
	resp.Body.Close()
	// Backend still returns 502 (hits <= 3), but we get the real 502, not a 503
	if resp.StatusCode == 503 {
		t.Fatal("req 2: got 503 (circuit blocked), expected fallback to reach backend")
	}

	// Keep hitting until backend recovers (hit 4+)
	for hits.Load() < 4 {
		resp = sendReq()
		resp.Body.Close()
	}

	// Now backend should have recovered, next request should succeed
	resp = sendReq()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after recovery, got %d", resp.StatusCode)
	}

	// Verify circuit is healthy after success
	state := hm.CircuitState("only-backend")
	if state != "healthy" {
		t.Fatalf("expected circuit healthy after success, got %s", state)
	}
}

func TestDefaultMaxTokensInjection(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:             "gpt-4",
				Aliases:          []string{"my-model"},
				DefaultMaxTokens: 4096,
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	// Request WITHOUT max_tokens — should get default injected
	body := []byte(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	maxTokens, ok := upstream["max_tokens"].(float64)
	if !ok {
		t.Fatalf("expected max_tokens in upstream request, got body: %s", receivedBody)
	}
	if maxTokens != 4096 {
		t.Fatalf("max_tokens = %v, want 4096", maxTokens)
	}
}

func TestDefaultMaxTokensNotOverrideExisting(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:             "gpt-4",
				Aliases:          []string{"my-model"},
				DefaultMaxTokens: 4096,
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	// Request WITH max_tokens — should NOT be overridden
	body := []byte(`{"model":"my-model","max_tokens":512,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	maxTokens, ok := upstream["max_tokens"].(float64)
	if !ok {
		t.Fatalf("expected max_tokens in upstream request, got body: %s", receivedBody)
	}
	if maxTokens != 512 {
		t.Fatalf("max_tokens = %v, want 512 (client value should be preserved)", maxTokens)
	}
}

func TestOpenAIRequestNormalizesReasoningEffortAliasWhenEnabled(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-oss-120b","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "agentify-backend",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:                          "openai/gpt-oss-120b",
				Aliases:                       []string{"claude-haiku-4-5-20251001"},
				NormalizeXHighReasoningEffort: true,
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-haiku-4-5-20251001","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	if got, _ := upstream["reasoning_effort"].(string); got != "high" {
		t.Fatalf("reasoning_effort = %q, want high", got)
	}
}

func TestOpenAIRequestKeepsReasoningEffortAliasWhenDisabled(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:    "gpt-4",
				Aliases: []string{"my-model"},
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"my-model","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	if got, _ := upstream["reasoning_effort"].(string); got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh", got)
	}
}

func TestGlobalReasoningModelDefaultOverridesModelReasoningDefaultEffort(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-5","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		ModelDefaults: map[string]config.ModelDefaultConfig{
			"gpt-5": {ReasoningEffort: "low"},
		},
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:                   "gpt-5",
				Aliases:                []string{"claude-opus"},
				ReasoningDefaultEffort: "high",
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if !bytes.Contains(receivedBody, []byte(`"reasoning":{"effort":"low"}`)) {
		t.Fatalf("expected global model responses reasoning low in upstream request, got: %s", receivedBody)
	}
	if bytes.Contains(receivedBody, []byte(`"reasoning_effort":`)) {
		t.Fatalf("responses upstream request should not contain reasoning_effort, got: %s", receivedBody)
	}
}

func TestAnthropicRequestWithoutConfiguredReasoningOmitsReasoningField(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"resp_1","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "responses",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5",
				Aliases: []string{"claude-opus"},
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if bytes.Contains(receivedBody, []byte(`"reasoning":`)) {
		t.Fatalf("expected responses request to omit reasoning when no config exists, got: %s", receivedBody)
	}
	if bytes.Contains(receivedBody, []byte(`"reasoning_effort":`)) {
		t.Fatalf("expected responses request to omit reasoning_effort when no config exists, got: %s", receivedBody)
	}
}

func TestAnthropicRequestUsesConfiguredXHighReasoningEffortWithoutThinking(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"resp_1","model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		ModelDefaults: map[string]config.ModelDefaultConfig{
			"gpt-5.4": {ReasoningEffort: "xhigh"},
		},
		Backends: []config.Backend{{
			Name:     "responses",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5.4",
				Aliases: []string{"claude-opus-4-6"},
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus-4-6","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if !bytes.Contains(receivedBody, []byte(`"reasoning":{"effort":"xhigh"}`)) {
		t.Fatalf("expected responses request to preserve xhigh, got: %s", receivedBody)
	}
}

func TestAnthropicRequestNormalizesConfiguredXHighReasoningEffortWhenEnabled(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"resp_1","model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		ModelDefaults: map[string]config.ModelDefaultConfig{
			"gpt-5.4": {ReasoningEffort: "xhigh", NormalizeXHighReasoningEffort: true},
		},
		Backends: []config.Backend{{
			Name:     "responses",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5.4",
				Aliases: []string{"claude-opus-4-6"},
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus-4-6","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if !bytes.Contains(receivedBody, []byte(`"reasoning":{"effort":"high"}`)) {
		t.Fatalf("expected responses request to normalize xhigh to high, got: %s", receivedBody)
	}
	if bytes.Contains(receivedBody, []byte(`"reasoning":{"effort":"xhigh"}`)) {
		t.Fatalf("expected xhigh to be normalized away, got: %s", receivedBody)
	}
}
func TestAnthropicRequestUsesConfiguredHighReasoningEffortWithoutThinking(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"resp_1","model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		ModelDefaults: map[string]config.ModelDefaultConfig{
			"gpt-5.4": {ReasoningEffort: "high"},
		},
		Backends: []config.Backend{{
			Name:     "responses",
			URL:      backend.URL,
			Protocol: "openai-responses",
			Models: []config.Model{{
				Name:    "gpt-5.4",
				Aliases: []string{"claude-opus-4-6"},
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"claude-opus-4-6","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	reasoning, ok := upstream["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %T, want object", upstream["reasoning"])
	}
	if got, _ := reasoning["effort"].(string); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high", got)
	}
	if _, ok := upstream["reasoning_effort"]; ok {
		t.Fatalf("responses upstream request should not contain reasoning_effort")
	}
}

func TestDefaultTemperatureInjection(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	temp := 0.6
	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:               "gpt-4",
				Aliases:            []string{"my-model"},
				DefaultTemperature: &temp,
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	temperature, ok := upstream["temperature"].(float64)
	if !ok {
		t.Fatalf("expected temperature in upstream request, got body: %s", receivedBody)
	}
	if temperature != 0.6 {
		t.Fatalf("temperature = %v, want 0.6", temperature)
	}
}

func TestDefaultTemperatureNotOverrideExisting(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	temp := 0.6
	cfg := config.Config{
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:               "gpt-4",
				Aliases:            []string{"my-model"},
				DefaultTemperature: &temp,
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"my-model","temperature":1.0,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	temperature, ok := upstream["temperature"].(float64)
	if !ok {
		t.Fatalf("expected temperature in upstream request")
	}
	if temperature != 1.0 {
		t.Fatalf("temperature = %v, want 1.0 (client value should be preserved)", temperature)
	}
}

func TestDefaultTemperatureFromModelDefaults(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer backend.Close()

	globalTemp := 0.5
	cfg := config.Config{
		ModelDefaults: map[string]config.ModelDefaultConfig{
			"gpt-4": {DefaultTemperature: &globalTemp},
		},
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:    "gpt-4",
				Aliases: []string{"my-model"},
			}},
		}},
	}
	srv := newTestServer(cfg)
	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	body := []byte(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var upstream map[string]any
	if err := json.Unmarshal(receivedBody, &upstream); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	temperature, ok := upstream["temperature"].(float64)
	if !ok {
		t.Fatalf("expected temperature in upstream request, got body: %s", receivedBody)
	}
	if temperature != 0.5 {
		t.Fatalf("temperature = %v, want 0.5", temperature)
	}
}

func TestResolveModelDefaultsProtocolKey(t *testing.T) {
	cfg := config.Config{
		SystemPrompt: "global prompt",
		ModelDefaults: map[string]config.ModelDefaultConfig{
			"gpt-5": {
				SystemPrompt: "generic prompt",
			},
			"gpt-5@openai": {
				SystemPrompt: "none",
			},
			"gpt-5@anthropic": {
				SystemPrompt: "anthropic prompt",
			},
		},
	}

	backend := &config.Backend{Name: "b1", Protocol: "openai"}
	cand := candidateResult{
		backend:      backend,
		backendModel: "gpt-5",
	}

	// OpenAI client: gpt-5@openai sets "none" → should clear prompt
	mc := resolveModelDefaults(cfg, cand, "openai")
	if mc.systemPrompt != "" {
		t.Fatalf("openai client: systemPrompt = %q, want empty", mc.systemPrompt)
	}

	// Anthropic client: gpt-5@anthropic overrides
	mc = resolveModelDefaults(cfg, cand, "anthropic")
	if mc.systemPrompt != "anthropic prompt" {
		t.Fatalf("anthropic client: systemPrompt = %q, want %q", mc.systemPrompt, "anthropic prompt")
	}

	// Unknown client protocol: falls back to generic key
	mc = resolveModelDefaults(cfg, cand, "")
	if mc.systemPrompt != "generic prompt" {
		t.Fatalf("empty protocol: systemPrompt = %q, want %q", mc.systemPrompt, "generic prompt")
	}

	// Protocol key not configured: falls back to generic key
	mc = resolveModelDefaults(cfg, cand, "other")
	if mc.systemPrompt != "generic prompt" {
		t.Fatalf("other protocol: systemPrompt = %q, want %q", mc.systemPrompt, "generic prompt")
	}
}

func TestDirectOpenAIClientSystemPromptInjection(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"chatcmpl-1","model":"gpt-5","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		SystemPrompt: "Global style",
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:    "gpt-5",
				Aliases: []string{"gpt-5"},
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	// OpenAI client direct request (no protocol conversion)
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if !bytes.Contains(receivedBody, []byte(`"role":"system"`)) {
		t.Fatalf("expected system role in upstream request, got: %s", receivedBody)
	}
	if !bytes.Contains(receivedBody, []byte(`"content":"Global style"`)) {
		t.Fatalf("expected global system prompt injected, got: %s", receivedBody)
	}
}

func TestDirectOpenAIClientSystemPromptNone(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"chatcmpl-1","model":"gpt-5","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	cfg := config.Config{
		SystemPrompt: "Global style",
		ModelDefaults: map[string]config.ModelDefaultConfig{
			"gpt-5@openai": {
				SystemPrompt: "none", // explicitly disable for OpenAI clients
			},
		},
		Backends: []config.Backend{{
			Name:     "test",
			URL:      backend.URL,
			Protocol: "openai",
			Models: []config.Model{{
				Name:    "gpt-5",
				Aliases: []string{"gpt-5"},
			}},
		}},
	}
	srv := newTestServer(cfg)

	proxyServer := httptest.NewServer(http.HandlerFunc(srv.Handle))
	defer proxyServer.Close()

	// OpenAI client direct request — system_prompt should NOT be injected
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	// Should NOT have a system message injected
	if bytes.Contains(receivedBody, []byte(`"role":"system"`)) {
		t.Fatalf("expected NO system role in upstream request when 'none', got: %s", receivedBody)
	}
}
