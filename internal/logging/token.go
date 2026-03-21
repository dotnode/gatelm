package logging

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dotnode/gatelm/internal/config"

	_ "modernc.org/sqlite"
)

type UsageLog struct {
	Time             string `json:"time"`
	RequestID        string `json:"request_id,omitempty"`
	Backend          string `json:"backend"`
	ClientProtocol   string `json:"client_protocol"`
	BackendProtocol  string `json:"backend_protocol"`
	ClientKey        string `json:"client_key"`
	RequestModel     string `json:"request_model,omitempty"`
	ForwardedModel   string `json:"forwarded_model,omitempty"`
	ResponseModel    string `json:"response_model,omitempty"`
	StatusCode       int    `json:"status_code"`
	DurationMs       int64  `json:"duration_ms,omitempty"`
	RetryCount       int    `json:"retry_count,omitempty"`
	ErrorCategory    string `json:"error_category,omitempty"`
	Transport        string `json:"transport,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	TotalTokens      int    `json:"total_tokens,omitempty"`
	InputTokens      int    `json:"input_tokens,omitempty"`
	OutputTokens     int    `json:"output_tokens,omitempty"`
	Error            string `json:"error,omitempty"`
}

type TokenLogger struct {
	enabled bool
	mode    string // "sqlite" or "jsonl"
	path    string

	// JSONL mode
	mu   sync.Mutex
	file *os.File

	// SQLite mode
	db             *sql.DB
	stmt           *sql.Stmt
	logCh          chan UsageLog
	done           chan struct{}
	retentionDays  int
	droppedEntries atomic.Int64
}

func NewTokenLogger(cfg config.TokenLogConfig) (*TokenLogger, error) {
	if !cfg.Enabled {
		return &TokenLogger{enabled: false}, nil
	}
	p := cfg.File
	if p == "" {
		p = "token_usage.db"
	}
	if isSQLiteFile(p) {
		return newSQLiteLogger(p, cfg.RetentionDays)
	}
	return newJSONLLogger(p)
}

func isSQLiteFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".db" || ext == ".sqlite" || ext == ".sqlite3"
}

func newJSONLLogger(path string) (*TokenLogger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &TokenLogger{enabled: true, mode: "jsonl", path: path, file: f}, nil
}

func newSQLiteLogger(path string, retentionDays int) (*TokenLogger, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	stmt, err := db.Prepare(`INSERT INTO token_usage (
		time, request_id, backend, client_protocol, backend_protocol, client_key,
		request_model, forwarded_model, response_model,
		status_code, duration_ms, retry_count, error_category, transport,
		prompt_tokens, completion_tokens, total_tokens,
		input_tokens, output_tokens, error
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		db.Close()
		return nil, err
	}

	l := &TokenLogger{
		enabled:       true,
		mode:          "sqlite",
		path:          path,
		db:            db,
		stmt:          stmt,
		logCh:         make(chan UsageLog, 256),
		done:          make(chan struct{}),
		retentionDays: retentionDays,
	}
	go l.batchWriter()
	return l, nil
}

func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS token_usage (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		time              TEXT NOT NULL DEFAULT '',
		request_id        TEXT NOT NULL DEFAULT '',
		backend           TEXT NOT NULL DEFAULT '',
		client_protocol   TEXT NOT NULL DEFAULT '',
		backend_protocol  TEXT NOT NULL DEFAULT '',
		client_key        TEXT NOT NULL DEFAULT '',
		request_model     TEXT NOT NULL DEFAULT '',
		forwarded_model   TEXT NOT NULL DEFAULT '',
		response_model    TEXT NOT NULL DEFAULT '',
		status_code       INTEGER NOT NULL DEFAULT 0,
		duration_ms       INTEGER NOT NULL DEFAULT 0,
		retry_count       INTEGER NOT NULL DEFAULT 0,
		error_category    TEXT NOT NULL DEFAULT '',
		transport         TEXT NOT NULL DEFAULT '',
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens      INTEGER NOT NULL DEFAULT 0,
		input_tokens      INTEGER NOT NULL DEFAULT 0,
		output_tokens     INTEGER NOT NULL DEFAULT 0,
		error             TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_token_usage_time ON token_usage(time);
	CREATE INDEX IF NOT EXISTS idx_token_usage_client_key ON token_usage(client_key);
	CREATE INDEX IF NOT EXISTS idx_token_usage_key_time ON token_usage(client_key, time);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	for _, stmt := range []string{
		`ALTER TABLE token_usage ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE token_usage ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE token_usage ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE token_usage ADD COLUMN error_category TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE token_usage ADD COLUMN transport TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE token_usage ADD COLUMN backend_protocol TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	return nil
}

func (l *TokenLogger) batchWriter() {
	defer close(l.done)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token log: batchWriter panic: %v", r)
		}
	}()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var cleanupTicker *time.Ticker
	var cleanupCh <-chan time.Time
	if l.retentionDays > 0 {
		cleanupTicker = time.NewTicker(1 * time.Hour)
		cleanupCh = cleanupTicker.C
		defer cleanupTicker.Stop()
		// Run cleanup once at startup
		l.cleanupOldEntries()
	}

	batch := make([]UsageLog, 0, 64)

	for {
		select {
		case entry, ok := <-l.logCh:
			if !ok {
				if len(batch) > 0 {
					l.writeBatch(batch)
				}
				return
			}
			batch = append(batch, entry)
			if len(batch) >= 32 {
				l.writeBatch(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				l.writeBatch(batch)
				batch = batch[:0]
			}
		case <-cleanupCh:
			l.cleanupOldEntries()
		}
	}
}

func (l *TokenLogger) writeBatch(entries []UsageLog) {
	if err := l.tryWriteBatch(entries); err != nil {
		log.Printf("token log: batch write failed, retrying: %v", err)
		time.Sleep(50 * time.Millisecond)
		if err := l.tryWriteBatch(entries); err != nil {
			log.Printf("token log: batch write retry failed, dropping %d entries: %v", len(entries), err)
			l.droppedEntries.Add(int64(len(entries)))
		}
	}
}

func (l *TokenLogger) tryWriteBatch(entries []UsageLog) error {
	tx, err := l.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	stmt := tx.Stmt(l.stmt)
	for _, e := range entries {
		_, err := stmt.Exec(
			e.Time, e.RequestID, e.Backend, e.ClientProtocol, e.BackendProtocol, e.ClientKey,
			e.RequestModel, e.ForwardedModel, e.ResponseModel,
			e.StatusCode, e.DurationMs, e.RetryCount, e.ErrorCategory, e.Transport,
			e.PromptTokens, e.CompletionTokens, e.TotalTokens,
			e.InputTokens, e.OutputTokens, e.Error,
		)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (l *TokenLogger) cleanupOldEntries() {
	if l.retentionDays <= 0 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -l.retentionDays).Format(time.RFC3339)
	result, err := l.db.Exec("DELETE FROM token_usage WHERE time < ?", cutoff)
	if err != nil {
		log.Printf("token log: cleanup failed: %v", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		log.Printf("token log: cleaned up %d entries older than %d days", n, l.retentionDays)
	}
}

func (l *TokenLogger) Log(entry UsageLog) {
	if !l.enabled {
		return
	}
	if l.mode == "sqlite" {
		select {
		case l.logCh <- entry:
		default:
			n := l.droppedEntries.Add(1)
			if n == 1 || n%100 == 0 {
				log.Printf("token log: channel full, dropped %d entries total", n)
			}
		}
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		log.Printf("token log: marshal failed: %v", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Write(append(b, '\n')); err != nil {
		log.Printf("token log: write failed: %v", err)
	}
}

// DroppedEntries returns the total number of log entries dropped due to channel overflow.
func (l *TokenLogger) DroppedEntries() int64 {
	return l.droppedEntries.Load()
}

func (l *TokenLogger) Mode() string {
	if l == nil {
		return "disabled"
	}
	if !l.enabled {
		return "disabled"
	}
	return l.mode
}

func (l *TokenLogger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

type UsageLogFilter struct {
	Limit         int
	Offset        int
	ClientKey     string
	Backend       string
	StatusCode    int
	ErrorCategory string
	StartTime     string
	EndTime       string
}

type UsageLogSummary struct {
	TotalRequests int `json:"total_requests"`
	TotalTokens   int `json:"total_tokens"`
	ErrorCount    int `json:"error_count"`
}

func (l *TokenLogger) QueryUsageLogs(filter UsageLogFilter) ([]UsageLog, UsageLogSummary, error) {
	if l == nil || !l.enabled {
		return nil, UsageLogSummary{}, nil
	}
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	switch l.mode {
	case "sqlite":
		return l.querySQLiteUsageLogs(filter)
	case "jsonl":
		return l.queryJSONLUsageLogs(filter)
	default:
		return nil, UsageLogSummary{}, nil
	}
}

func (l *TokenLogger) querySQLiteUsageLogs(filter UsageLogFilter) ([]UsageLog, UsageLogSummary, error) {
	where, args := buildUsageLogWhereClause(filter)
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, filter.Limit, filter.Offset)

	rows, err := l.db.Query(`SELECT time, request_id, backend, client_protocol, backend_protocol, client_key,
		request_model, forwarded_model, response_model,
		status_code, duration_ms, retry_count, error_category, transport,
		prompt_tokens, completion_tokens, total_tokens,
		input_tokens, output_tokens, error
		FROM token_usage`+where+` ORDER BY time DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, UsageLogSummary{}, err
	}
	defer rows.Close()

	logs := make([]UsageLog, 0, filter.Limit)
	for rows.Next() {
		var entry UsageLog
		if err := rows.Scan(
			&entry.Time, &entry.RequestID, &entry.Backend, &entry.ClientProtocol, &entry.BackendProtocol, &entry.ClientKey,
			&entry.RequestModel, &entry.ForwardedModel, &entry.ResponseModel,
			&entry.StatusCode, &entry.DurationMs, &entry.RetryCount, &entry.ErrorCategory, &entry.Transport,
			&entry.PromptTokens, &entry.CompletionTokens, &entry.TotalTokens,
			&entry.InputTokens, &entry.OutputTokens, &entry.Error,
		); err != nil {
			return nil, UsageLogSummary{}, err
		}
		logs = append(logs, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, UsageLogSummary{}, err
	}

	var summary UsageLogSummary
	if err := l.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0)
		FROM token_usage`+where, args...).Scan(&summary.TotalRequests, &summary.TotalTokens, &summary.ErrorCount); err != nil {
		return nil, UsageLogSummary{}, err
	}
	return logs, summary, nil
}

func (l *TokenLogger) queryJSONLUsageLogs(filter UsageLogFilter) ([]UsageLog, UsageLogSummary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		if err := l.file.Sync(); err != nil {
			log.Printf("token log: sync failed before query: %v", err)
		}
	}

	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, UsageLogSummary{}, nil
		}
		return nil, UsageLogSummary{}, err
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	matched := make([]UsageLog, 0)
	var summary UsageLogSummary
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry UsageLog
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if !matchUsageLogFilter(entry, filter) {
			continue
		}
		summary.TotalRequests++
		summary.TotalTokens += entry.TotalTokens
		if entry.StatusCode >= 400 {
			summary.ErrorCount++
		}
		matched = append(matched, entry)
	}

	if filter.Offset >= len(matched) {
		return []UsageLog{}, summary, nil
	}
	end := filter.Offset + filter.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[filter.Offset:end], summary, nil
}

func buildUsageLogWhereClause(filter UsageLogFilter) (string, []any) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if filter.ClientKey != "" {
		conditions = append(conditions, "client_key = ?")
		args = append(args, filter.ClientKey)
	}
	if filter.Backend != "" {
		conditions = append(conditions, "backend = ?")
		args = append(args, filter.Backend)
	}
	if filter.StatusCode > 0 {
		conditions = append(conditions, "status_code = ?")
		args = append(args, filter.StatusCode)
	}
	if filter.ErrorCategory != "" {
		conditions = append(conditions, "error_category = ?")
		args = append(args, filter.ErrorCategory)
	}
	if filter.StartTime != "" {
		conditions = append(conditions, "time >= ?")
		args = append(args, filter.StartTime)
	}
	if filter.EndTime != "" {
		conditions = append(conditions, "time <= ?")
		args = append(args, filter.EndTime)
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func matchUsageLogFilter(entry UsageLog, filter UsageLogFilter) bool {
	if filter.ClientKey != "" && entry.ClientKey != filter.ClientKey {
		return false
	}
	if filter.Backend != "" && entry.Backend != filter.Backend {
		return false
	}
	if filter.StatusCode > 0 && entry.StatusCode != filter.StatusCode {
		return false
	}
	if filter.ErrorCategory != "" && entry.ErrorCategory != filter.ErrorCategory {
		return false
	}
	if filter.StartTime != "" && entry.Time < filter.StartTime {
		return false
	}
	if filter.EndTime != "" && entry.Time > filter.EndTime {
		return false
	}
	return true
}

func (l *TokenLogger) Close() {
	if l.mode == "sqlite" {
		close(l.logCh)
		select {
		case <-l.done:
			// Normal shutdown
		case <-time.After(5 * time.Second):
			log.Printf("token log: Close() timeout after 5s, forcing shutdown")
		}
		if l.stmt != nil {
			l.stmt.Close()
		}
		if l.db != nil {
			l.db.Close()
		}
		return
	}
	if l.file != nil {
		_ = l.file.Close()
	}
}

type UsageInfo struct {
	ResponseModel    string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	InputTokens      int
	OutputTokens     int
}

func ExtractUsage(protocol string, body []byte) UsageInfo {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "anthropic":
		return extractAnthropicUsage(body)
	case "openai-responses":
		return extractOpenAIResponsesUsage(body)
	default:
		return extractOpenAIUsage(body)
	}
}

func extractOpenAIUsage(body []byte) UsageInfo {
	var payload struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return UsageInfo{}
	}
	return UsageInfo{
		ResponseModel:    payload.Model,
		PromptTokens:     payload.Usage.PromptTokens,
		CompletionTokens: payload.Usage.CompletionTokens,
		TotalTokens:      payload.Usage.TotalTokens,
	}
}

func extractAnthropicUsage(body []byte) UsageInfo {
	var payload struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return UsageInfo{}
	}
	return UsageInfo{
		ResponseModel: payload.Model,
		InputTokens:   payload.Usage.InputTokens,
		OutputTokens:  payload.Usage.OutputTokens,
		TotalTokens:   payload.Usage.InputTokens + payload.Usage.OutputTokens,
	}
}

func ExtractStreamingUsage(protocol string, data []byte) (UsageInfo, bool) {
	switch protocol {
	case "anthropic":
		return extractAnthropicStreamingUsage(data)
	case "openai-responses":
		return extractOpenAIResponsesStreamingUsage(data)
	default:
		return extractOpenAIStreamingUsage(data)
	}
}

func extractOpenAIStreamingUsage(data []byte) (UsageInfo, bool) {
	var chunk struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return UsageInfo{}, false
	}
	if chunk.Usage == nil {
		return UsageInfo{ResponseModel: chunk.Model}, false
	}
	return UsageInfo{
		ResponseModel:    chunk.Model,
		PromptTokens:     chunk.Usage.PromptTokens,
		CompletionTokens: chunk.Usage.CompletionTokens,
		TotalTokens:      chunk.Usage.TotalTokens,
	}, true
}

func extractAnthropicStreamingUsage(data []byte) (UsageInfo, bool) {
	var chunk struct {
		Type  string `json:"type"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Message *struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return UsageInfo{}, false
	}
	if chunk.Message != nil && chunk.Message.Model != "" {
		info := UsageInfo{ResponseModel: chunk.Message.Model}
		if chunk.Message.Usage != nil {
			info.InputTokens = chunk.Message.Usage.InputTokens
			info.OutputTokens = chunk.Message.Usage.OutputTokens
			info.TotalTokens = info.InputTokens + info.OutputTokens
		}
		return info, chunk.Message.Usage != nil
	}
	if chunk.Usage != nil {
		return UsageInfo{
			InputTokens:  chunk.Usage.InputTokens,
			OutputTokens: chunk.Usage.OutputTokens,
			TotalTokens:  chunk.Usage.InputTokens + chunk.Usage.OutputTokens,
		}, true
	}
	return UsageInfo{}, false
}

func extractOpenAIResponsesUsage(body []byte) UsageInfo {
	var payload struct {
		Model  string `json:"model"`
		Status string `json:"status"`
		Usage  *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return UsageInfo{}
	}
	info := UsageInfo{ResponseModel: payload.Model}
	if payload.Usage != nil {
		info.InputTokens = payload.Usage.InputTokens
		info.OutputTokens = payload.Usage.OutputTokens
		info.TotalTokens = payload.Usage.TotalTokens
		info.PromptTokens = payload.Usage.InputTokens
		info.CompletionTokens = payload.Usage.OutputTokens
	}
	return info
}

func extractOpenAIResponsesStreamingUsage(data []byte) (UsageInfo, bool) {
	var event struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return UsageInfo{}, false
	}
	info := UsageInfo{ResponseModel: event.Model}
	if event.Usage != nil {
		info.InputTokens = event.Usage.InputTokens
		info.OutputTokens = event.Usage.OutputTokens
		info.TotalTokens = event.Usage.TotalTokens
		info.PromptTokens = event.Usage.InputTokens
		info.CompletionTokens = event.Usage.OutputTokens
		return info, true
	}
	return info, false
}
