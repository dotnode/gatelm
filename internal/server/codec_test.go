package server

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dotnode/gatelm/internal/logging"
)

// A single SSE line exceeding the scanner's 1MB buffer (e.g. a huge tool-call
// argument or base64 blob) must not be silently treated as a clean
// end-of-stream — the client must see an explicit error event instead of a
// stream that just looks like it finished successfully, and the caller must
// get a non-nil error back so it can log the request as failed rather than
// as a success.
func TestConvertStreamReportsOversizedLineInsteadOfSilentTruncation(t *testing.T) {
	huge := strings.Repeat("x", 2*1024*1024) // 2MB, exceeds the 1MB scanner buffer
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n" +
		"data: " + huge + "\n\n"

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	debug := logging.NewDebugLog(false, "")
	codec := newResponsesCodec(debug)
	decoder := codec.NewStreamDecoder()
	w := httptest.NewRecorder()
	encoder := codec.NewStreamEncoder(w)

	_, err := convertStream(w, resp, decoder, encoder, debug, "test-req")
	if err == nil {
		t.Fatal("expected convertStream to return an error for the oversized line")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("expected bufio.ErrTooLong, got %v", err)
	}

	out := w.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("expected an error event surfaced to the client, got:\n%s", out)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation to be mentioned in the error event, got:\n%s", out)
	}
}
