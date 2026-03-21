package logging

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const maxDebugBodySize = 4096

type DebugLog struct {
	enabled bool
	closed  bool
	mu      sync.Mutex
	file    *os.File
	writer  *log.Logger
	date    string
	dir     string
}

func NewDebugLog(enabled bool, dir string) *DebugLog {
	d := &DebugLog{enabled: enabled, dir: dir}
	if enabled {
		d.mu.Lock()
		d.openFile()
		d.mu.Unlock()
	}
	return d
}

// openFile must be called with d.mu held.
func (d *DebugLog) openFile() {
	today := time.Now().Format("2006-01-02")
	if d.file != nil && today == d.date {
		return
	}
	name := fmt.Sprintf("%s/debug-%s.log", d.dir, today)
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("open %s failed: %v", name, err)
		return // keep old file handle if any
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	d.file = f
	d.date = today
	d.writer = log.New(f, "[DEBUG] ", log.LstdFlags)
}

func (d *DebugLog) IsEnabled() bool {
	return d != nil && d.enabled
}

func (d *DebugLog) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
		d.writer = nil
	}
}

func (d *DebugLog) Printf(format string, args ...any) {
	if d == nil || !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.openFile()
	if d.writer != nil {
		d.writer.Printf(format, args...)
	}
}

func (d *DebugLog) Headers(prefix string, h http.Header) {
	if d == nil || !d.enabled {
		return
	}
	var sb strings.Builder
	for k, vv := range h {
		kl := strings.ToLower(k)
		for _, v := range vv {
			if kl == "authorization" || kl == "x-api-key" {
				if len(v) > 8 {
					v = v[:4] + "****" + v[len(v)-4:]
				} else {
					v = "****"
				}
			}
			sb.WriteString(fmt.Sprintf("  %s: %s\n", k, v))
		}
	}
	d.Printf("%s headers:\n%s", prefix, sb.String())
}

func (d *DebugLog) Body(prefix string, body []byte) {
	if d == nil || !d.enabled || len(body) == 0 {
		return
	}
	if len(body) > maxDebugBodySize {
		d.Printf("%s body (%d bytes, showing first %d):\n%s\n...(truncated %d bytes)",
			prefix, len(body), maxDebugBodySize, string(body[:maxDebugBodySize]), len(body)-maxDebugBodySize)
		return
	}
	d.Printf("%s body (%d bytes):\n%s", prefix, len(body), string(body))
}
