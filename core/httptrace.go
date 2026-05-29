package core

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// APITraceLog is one HTTP round-trip written to api-trace.jsonl.
type APITraceLog struct {
	At           string `json:"at"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	Status       int    `json:"status"`
	LatencyMS    int64  `json:"latency_ms"`
	RequestBytes int    `json:"request_bytes,omitempty"`
	ResponseBytes int   `json:"response_bytes,omitempty"`
	Error        string `json:"error,omitempty"`
	BodyPreview  string `json:"body_preview,omitempty"`
}

// TracingTransport logs every HTTP exchange to a JSONL file.
type TracingTransport struct {
	Base     http.RoundTripper
	Writer   *json.Encoder
	mu       sync.Mutex
	MaxBody  int
}

func NewTracingTransport(base http.RoundTripper, file *os.File, maxBodyPreview int) *TracingTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if maxBodyPreview <= 0 {
		maxBodyPreview = 512
	}
	return &TracingTransport{
		Base:    base,
		Writer:  json.NewEncoder(file),
		MaxBody: maxBodyPreview,
	}
}

func (t *TracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	var reqBytes int
	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			reqBytes = len(body)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
	}

	resp, err := t.Base.RoundTrip(req)
	latency := time.Since(start).Milliseconds()

	entry := APITraceLog{
		At:        time.Now().UTC().Format(time.RFC3339Nano),
		Method:    req.Method,
		URL:       req.URL.String(),
		LatencyMS: latency,
		RequestBytes: reqBytes,
	}
	if err != nil {
		entry.Error = err.Error()
		t.write(entry)
		return resp, err
	}

	entry.Status = resp.StatusCode
	if resp.Body != nil {
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil {
			entry.ResponseBytes = len(body)
			entry.BodyPreview = truncateUTF8(string(body), t.MaxBody)
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	t.write(entry)
	return resp, err
}

func (t *TracingTransport) write(entry APITraceLog) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.Writer.Encode(entry)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
