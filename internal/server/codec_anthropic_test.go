package server

import (
	"encoding/json"
	"testing"
)

// decodeAll feeds a sequence of raw Anthropic SSE "data: ..." lines through
// the decoder and collects every canonical event produced (including Flush).
func decodeAllAnthropicLines(t *testing.T, d *anthropicStreamDecoder, lines []string) []CanonicalEvent {
	t.Helper()
	var all []CanonicalEvent
	for _, line := range lines {
		events, err := d.Decode("data: " + line)
		if err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		all = append(all, events...)
	}
	final, _ := d.Flush()
	all = append(all, final...)
	return all
}

func findEvent(events []CanonicalEvent, eventType string) (CanonicalEvent, bool) {
	for _, e := range events {
		if e.EventType == eventType {
			return e, true
		}
	}
	return CanonicalEvent{}, false
}

// Anthropic's extended thinking feature requires the exact thinking text and
// signature to be replayed unmodified in a later turn. The streaming decoder
// must surface both via a response.output_item.done event, even though the
// existing response.reasoning_summary_text.delta events only carry plain
// text with no signature.
func TestAnthropicStreamDecoderPreservesThinkingSignature(t *testing.T) {
	d := &anthropicStreamDecoder{}
	lines := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-3-opus","usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-part1"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"-part2"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	events := decodeAllAnthropicLines(t, d, lines)

	done, ok := findEvent(events, "response.output_item.done")
	if !ok {
		t.Fatalf("expected a response.output_item.done event, got: %+v", events)
	}
	var data struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			Type    string `json:"type"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(done.Data, &data); err != nil {
		t.Fatalf("unmarshal output_item.done data: %v", err)
	}
	if data.Item.Type != "reasoning" {
		t.Fatalf("item type = %q, want reasoning", data.Item.Type)
	}
	if len(data.Item.Summary) != 1 || data.Item.Summary[0].Text != "Let me think." {
		t.Fatalf("unexpected summary: %+v", data.Item.Summary)
	}
	payload, ok := decodeAnthropicThinkingPayload(data.Item.EncryptedContent)
	if !ok {
		t.Fatalf("expected encrypted_content to decode, got %q", data.Item.EncryptedContent)
	}
	if payload.Type != "thinking" || payload.Thinking != "Let me think." || payload.Signature != "sig-part1-part2" {
		t.Fatalf("unexpected decoded payload: %+v", payload)
	}
}

// redacted_thinking blocks are atomic (no delta events) — the decoder must
// surface the opaque data immediately at content_block_start.
func TestAnthropicStreamDecoderPreservesRedactedThinking(t *testing.T) {
	d := &anthropicStreamDecoder{}
	lines := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-3-opus","usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque-bytes"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	events := decodeAllAnthropicLines(t, d, lines)

	done, ok := findEvent(events, "response.output_item.done")
	if !ok {
		t.Fatalf("expected a response.output_item.done event, got: %+v", events)
	}
	var data struct {
		Item struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"item"`
	}
	json.Unmarshal(done.Data, &data)
	payload, ok := decodeAnthropicThinkingPayload(data.Item.EncryptedContent)
	if !ok || payload.Type != "redacted_thinking" || payload.Data != "opaque-bytes" {
		t.Fatalf("unexpected decoded payload: %+v ok=%v", payload, ok)
	}
}

// If the stream ends without a proper content_block_stop for the thinking
// block, Flush() must still emit the accumulated signature rather than
// losing it.
func TestAnthropicStreamDecoderFlushClosesUnterminatedThinkingBlock(t *testing.T) {
	d := &anthropicStreamDecoder{}
	lines := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-3-opus","usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"partial"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
	}
	events := decodeAllAnthropicLines(t, d, lines)

	done, ok := findEvent(events, "response.output_item.done")
	if !ok {
		t.Fatalf("expected Flush to emit response.output_item.done for the unterminated block, got: %+v", events)
	}
	var data struct {
		Item struct {
			EncryptedContent string `json:"encrypted_content"`
		} `json:"item"`
	}
	json.Unmarshal(done.Data, &data)
	payload, ok := decodeAnthropicThinkingPayload(data.Item.EncryptedContent)
	if !ok || payload.Thinking != "partial" || payload.Signature != "sig" {
		t.Fatalf("unexpected decoded payload: %+v ok=%v", payload, ok)
	}
}

// A thinking block with no signature (shouldn't happen in practice, but the
// decoder must degrade gracefully) must not emit a spurious
// response.output_item.done — only the existing summary delta/done events.
func TestAnthropicStreamDecoderNoSignatureNoOutputItemDone(t *testing.T) {
	d := &anthropicStreamDecoder{}
	lines := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-3-opus","usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"no signature here"}}`,
		`{"type":"content_block_stop","index":0}`,
	}
	events := decodeAllAnthropicLines(t, d, lines)
	if _, ok := findEvent(events, "response.output_item.done"); ok {
		t.Fatalf("expected no response.output_item.done without a signature, got: %+v", events)
	}
	if _, ok := findEvent(events, "response.reasoning_summary_text.done"); !ok {
		t.Fatalf("expected the normal reasoning_summary_text.done event to still fire, got: %+v", events)
	}
}
