package logging

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dotnode/gatelm/internal/config"

	_ "modernc.org/sqlite"
)

func TestExtractUsage(t *testing.T) {
	openai := []byte(`{"model":"gpt-4.1","usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`)
	u1 := ExtractUsage("openai", openai)
	if u1.TotalTokens != 14 || u1.PromptTokens != 10 || u1.CompletionTokens != 4 {
		t.Fatalf("openai usage parse failed: %+v", u1)
	}

	anthropic := []byte(`{"model":"claude-x","usage":{"input_tokens":11,"output_tokens":9}}`)
	u2 := ExtractUsage("anthropic", anthropic)
	if u2.TotalTokens != 20 || u2.InputTokens != 11 || u2.OutputTokens != 9 {
		t.Fatalf("anthropic usage parse failed: %+v", u2)
	}
}

func TestExtractOpenAIStreamingUsage(t *testing.T) {
	t.Run("chunk with usage", func(t *testing.T) {
		data := []byte(`{"model":"gpt-4","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		u, found := extractOpenAIStreamingUsage(data)
		if !found {
			t.Fatal("expected usage found")
		}
		if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 {
			t.Fatalf("unexpected usage: %+v", u)
		}
		if u.ResponseModel != "gpt-4" {
			t.Fatalf("unexpected model: %s", u.ResponseModel)
		}
	})

	t.Run("chunk without usage", func(t *testing.T) {
		data := []byte(`{"model":"gpt-4","choices":[{"delta":{"content":"hello"}}]}`)
		u, found := extractOpenAIStreamingUsage(data)
		if found {
			t.Fatal("expected usage not found")
		}
		if u.ResponseModel != "gpt-4" {
			t.Fatalf("model should still be extracted: %s", u.ResponseModel)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		_, found := extractOpenAIStreamingUsage([]byte(`not json`))
		if found {
			t.Fatal("expected not found for invalid json")
		}
	})
}

func TestExtractAnthropicStreamingUsage(t *testing.T) {
	t.Run("message_start with model", func(t *testing.T) {
		data := []byte(`{"type":"message_start","message":{"model":"claude-3","usage":{"input_tokens":25,"output_tokens":0}}}`)
		u, found := extractAnthropicStreamingUsage(data)
		if !found {
			t.Fatal("expected usage found for message_start with usage")
		}
		if u.ResponseModel != "claude-3" {
			t.Fatalf("unexpected model: %s", u.ResponseModel)
		}
		if u.InputTokens != 25 {
			t.Fatalf("unexpected input_tokens: %d", u.InputTokens)
		}
	})

	t.Run("message_delta with usage", func(t *testing.T) {
		data := []byte(`{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":42}}`)
		u, found := extractAnthropicStreamingUsage(data)
		if !found {
			t.Fatal("expected usage found")
		}
		if u.OutputTokens != 42 {
			t.Fatalf("unexpected output_tokens: %d", u.OutputTokens)
		}
	})

	t.Run("content_block_delta no usage", func(t *testing.T) {
		data := []byte(`{"type":"content_block_delta","delta":{"text":"hello"}}`)
		_, found := extractAnthropicStreamingUsage(data)
		if found {
			t.Fatal("expected usage not found")
		}
	})
}

func TestIsSQLiteFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"token_usage.db", true},
		{"token_usage.sqlite", true},
		{"token_usage.sqlite3", true},
		{"token_usage.DB", true},
		{"token_usage.log", false},
		{"token_usage.jsonl", false},
		{"data.txt", false},
		{"/path/to/file.db", true},
	}
	for _, tt := range tests {
		if got := isSQLiteFile(tt.path); got != tt.want {
			t.Errorf("isSQLiteFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestNewTokenLoggerSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := config.TokenLogConfig{Enabled: true, File: dbPath}
	logger, err := NewTokenLogger(cfg)
	if err != nil {
		t.Fatalf("NewTokenLogger failed: %v", err)
	}

	logger.Log(UsageLog{
		Time:            time.Now().UTC().Format(time.RFC3339),
		RequestID:       "req-1",
		Backend:         "test-backend",
		ClientProtocol:  "anthropic",
		BackendProtocol: "openai-responses",
		ClientKey:       "key1",
		StatusCode:      200,
		DurationMs:      123,
		RetryCount:      1,
		ErrorCategory:   "success",
		TotalTokens:     100,
		InputTokens:     60,
		OutputTokens:    40,
	})

	logger.Close()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db for verify: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM token_usage").Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}

	var backend, clientProtocol, backendProtocol, clientKey, requestID, errorCategory string
	var totalTokens, retryCount int
	var durationMs int64
	err = db.QueryRow("SELECT backend, client_protocol, backend_protocol, client_key, request_id, total_tokens, duration_ms, retry_count, error_category FROM token_usage LIMIT 1").Scan(&backend, &clientProtocol, &backendProtocol, &clientKey, &requestID, &totalTokens, &durationMs, &retryCount, &errorCategory)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}
	if backend != "test-backend" {
		t.Errorf("backend = %q, want %q", backend, "test-backend")
	}
	if clientKey != "key1" {
		t.Errorf("client_key = %q, want %q", clientKey, "key1")
	}
	if clientProtocol != "anthropic" {
		t.Errorf("client_protocol = %q, want %q", clientProtocol, "anthropic")
	}
	if backendProtocol != "openai-responses" {
		t.Errorf("backend_protocol = %q, want %q", backendProtocol, "openai-responses")
	}
	if requestID != "req-1" {
		t.Errorf("request_id = %q, want %q", requestID, "req-1")
	}
	if totalTokens != 100 {
		t.Errorf("total_tokens = %d, want 100", totalTokens)
	}
	if durationMs != 123 {
		t.Errorf("duration_ms = %d, want 123", durationMs)
	}
	if retryCount != 1 {
		t.Errorf("retry_count = %d, want 1", retryCount)
	}
	if errorCategory != "success" {
		t.Errorf("error_category = %q, want %q", errorCategory, "success")
	}
}

func TestTokenLoggerBatchWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "batch.db")
	cfg := config.TokenLogConfig{Enabled: true, File: dbPath}
	logger, err := NewTokenLogger(cfg)
	if err != nil {
		t.Fatalf("NewTokenLogger failed: %v", err)
	}

	for i := 0; i < 100; i++ {
		logger.Log(UsageLog{
			Time:        time.Now().UTC().Format(time.RFC3339),
			Backend:     "backend",
			StatusCode:  200,
			TotalTokens: i,
		})
	}

	logger.Close()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM token_usage").Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 100 {
		t.Fatalf("expected 100 rows, got %d", count)
	}

	var sum int
	if err := db.QueryRow("SELECT SUM(total_tokens) FROM token_usage").Scan(&sum); err != nil {
		t.Fatalf("query sum: %v", err)
	}
	expectedSum := 99 * 100 / 2 // 0+1+2+...+99
	if sum != expectedSum {
		t.Fatalf("sum(total_tokens) = %d, want %d", sum, expectedSum)
	}
}

func TestTokenLoggerJSONLBackcompat(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")
	cfg := config.TokenLogConfig{Enabled: true, File: logPath}
	logger, err := NewTokenLogger(cfg)
	if err != nil {
		t.Fatalf("NewTokenLogger failed: %v", err)
	}

	logger.Log(UsageLog{
		Time:            "2025-01-01T00:00:00Z",
		RequestID:       "req-jsonl",
		Backend:         "jsonl-backend",
		ClientProtocol:  "openai",
		BackendProtocol: "anthropic",
		StatusCode:      200,
		DurationMs:      88,
		RetryCount:      2,
		ErrorCategory:   "http_5xx",
		TotalTokens:     50,
	})

	logger.Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	content := string(data)
	if len(content) == 0 {
		t.Fatal("log file is empty")
	}
	if content[len(content)-1] != '\n' {
		t.Error("log file should end with newline")
	}
	if !strings.Contains(content, `"backend":"jsonl-backend"`) {
		t.Error("log file should contain backend field")
	}
	if !strings.Contains(content, `"client_protocol":"openai"`) {
		t.Error("log file should contain client_protocol field")
	}
	if !strings.Contains(content, `"backend_protocol":"anthropic"`) {
		t.Error("log file should contain backend_protocol field")
	}
	if !strings.Contains(content, `"request_id":"req-jsonl"`) {
		t.Error("log file should contain request_id field")
	}
	if !strings.Contains(content, `"duration_ms":88`) {
		t.Error("log file should contain duration_ms field")
	}
	if !strings.Contains(content, `"retry_count":2`) {
		t.Error("log file should contain retry_count field")
	}
	if !strings.Contains(content, `"error_category":"http_5xx"`) {
		t.Error("log file should contain error_category field")
	}
	if !strings.Contains(content, `"total_tokens":50`) {
		t.Error("log file should contain total_tokens field")
	}
}

func TestTokenLoggerDisabled(t *testing.T) {
	cfg := config.TokenLogConfig{Enabled: false}
	logger, err := NewTokenLogger(cfg)
	if err != nil {
		t.Fatalf("NewTokenLogger failed: %v", err)
	}
	// Should not panic
	logger.Log(UsageLog{Backend: "test", StatusCode: 200})
	logger.Close()
}

func TestTokenLoggerDefaultPath(t *testing.T) {
	if !isSQLiteFile("token_usage.db") {
		t.Error("default path should be detected as SQLite")
	}
}

func TestDroppedEntriesCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "drop.db")
	cfg := config.TokenLogConfig{Enabled: true, File: dbPath}
	logger, err := NewTokenLogger(cfg)
	if err != nil {
		t.Fatalf("NewTokenLogger failed: %v", err)
	}

	if logger.DroppedEntries() != 0 {
		t.Fatalf("expected 0 dropped entries initially, got %d", logger.DroppedEntries())
	}

	// Fill the channel (capacity 256) then overflow
	// First, block the batchWriter by not letting it process
	// We can't easily block batchWriter in a unit test, so we just verify the counter API works
	logger.Close()

	if logger.DroppedEntries() != 0 {
		t.Fatalf("expected 0 dropped entries after normal usage, got %d", logger.DroppedEntries())
	}
}

func TestDebugBodyTruncation(t *testing.T) {
	dir := t.TempDir()
	dl := NewDebugLog(true, dir)
	defer dl.Close()

	// Small body should be logged fully
	smallBody := []byte("hello world")
	dl.Body("test", smallBody) // should not panic

	// Large body should be truncated
	largeBody := make([]byte, 8192)
	for i := range largeBody {
		largeBody[i] = 'x'
	}
	dl.Body("test", largeBody) // should not panic, should truncate

	// Empty body should be no-op
	dl.Body("test", nil)
	dl.Body("test", []byte{})
}

func TestDebugLogCloseProtection(t *testing.T) {
	dir := t.TempDir()
	dl := NewDebugLog(true, dir)

	// Write something
	dl.Printf("test message")

	// Close
	dl.Close()

	// Printf after close should not panic
	dl.Printf("after close")

	// Double close should not panic
	dl.Close()
}

func TestCleanupOldEntries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cleanup.db")
	cfg := config.TokenLogConfig{Enabled: true, File: dbPath, RetentionDays: 7}
	logger, err := NewTokenLogger(cfg)
	if err != nil {
		t.Fatalf("NewTokenLogger failed: %v", err)
	}

	// Insert an old entry directly
	oldTime := time.Now().UTC().AddDate(0, 0, -10).Format(time.RFC3339)
	logger.Log(UsageLog{
		Time:       oldTime,
		Backend:    "old-backend",
		StatusCode: 200,
	})
	// Insert a recent entry
	logger.Log(UsageLog{
		Time:       time.Now().UTC().Format(time.RFC3339),
		Backend:    "new-backend",
		StatusCode: 200,
	})

	// Wait for batch to flush
	logger.Close()

	// Verify entries before cleanup
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM token_usage").Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows before cleanup, got %d", count)
	}

	// Create a new logger with retention and let the startup cleanup run
	logger2, err := newSQLiteLogger(dbPath, 7)
	if err != nil {
		t.Fatalf("newSQLiteLogger failed: %v", err)
	}

	// Wait a moment for startup cleanup to execute
	time.Sleep(200 * time.Millisecond)
	logger2.Close()

	// Verify old entry was cleaned up
	if err := db.QueryRow("SELECT COUNT(*) FROM token_usage").Scan(&count); err != nil {
		t.Fatalf("query count after cleanup: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after cleanup (old entry removed), got %d", count)
	}

	var backend string
	if err := db.QueryRow("SELECT backend FROM token_usage").Scan(&backend); err != nil {
		t.Fatalf("query backend: %v", err)
	}
	if backend != "new-backend" {
		t.Errorf("remaining entry should be new-backend, got %q", backend)
	}
}
