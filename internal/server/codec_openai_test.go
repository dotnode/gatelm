package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reproduces a backend whose response.completed event's "output" field is
// empty — as emitted by both anthropicStreamDecoder.Flush and
// openaiChatStreamDecoder.Flush, neither of which populate it — even though a
// tool call happened during the stream. The encoder must still report
// finish_reason: "tool_calls" (derived from output_item.added events seen
// live), since mistakenly reporting "stop" breaks client agent loops that
// gate on this field to decide whether to continue calling tools.
func TestOpenAIChatStreamEncoderReportsToolCallsFinishReasonEvenWithEmptyCompletedOutput(t *testing.T) {
	w := httptest.NewRecorder()
	enc := &openaiChatStreamEncoder{w: w}

	events := []CanonicalEvent{
		{EventType: "response.created", Data: json.RawMessage(`{"response":{"id":"resp_1","model":"gpt-5"}}`)},
		{EventType: "response.output_item.added", Data: json.RawMessage(`{"output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`)},
		{EventType: "response.function_call_arguments.delta", Data: json.RawMessage(`{"output_index":0,"delta":"{\"city\":\"NYC\"}"}`)},
		{EventType: "response.function_call_arguments.done", Data: json.RawMessage(`{"output_index":0}`)},
		{EventType: "response.completed", Data: json.RawMessage(`{"response":{"id":"resp_1","model":"gpt-5","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5}}}`)},
	}
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode %s: %v", ev.EventType, err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected finish_reason tool_calls, got body:\n%s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason must not be stop when a tool call occurred, got body:\n%s", body)
	}
}

// A response truncated by max_tokens must still report "length" even if a
// (necessarily incomplete) tool call was in progress — that's more
// informative to the caller than "tool_calls".
func TestOpenAIChatStreamEncoderLengthTakesPriorityOverToolCalls(t *testing.T) {
	w := httptest.NewRecorder()
	enc := &openaiChatStreamEncoder{w: w}

	events := []CanonicalEvent{
		{EventType: "response.created", Data: json.RawMessage(`{"response":{"id":"resp_1","model":"gpt-5"}}`)},
		{EventType: "response.output_item.added", Data: json.RawMessage(`{"output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`)},
		{EventType: "response.completed", Data: json.RawMessage(`{"response":{"id":"resp_1","model":"gpt-5","status":"incomplete","output":[],"usage":{"input_tokens":10,"output_tokens":5}}}`)},
	}
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode %s: %v", ev.EventType, err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("expected finish_reason length, got body:\n%s", body)
	}
}
