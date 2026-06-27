package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/httpx/realip"
)

// TestAccessLog_EmitsLinePerRequest pins the headline contract:
// every request reaching the mux produces one structured log
// line with the canonical fields.
func TestAccessLog_EmitsLinePerRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})
	wrapped := AccessLogMiddleware(logger)(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	req.Header.Set("User-Agent", "ollama-python/0.5")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode log line: %v — raw=%s", err, buf.String())
	}
	if got["method"] != "GET" {
		t.Errorf("method = %v, want GET", got["method"])
	}
	if got["path"] != "/api/tags" {
		t.Errorf("path = %v, want /api/tags", got["path"])
	}
	if got["status"].(float64) != 200 {
		t.Errorf("status = %v, want 200", got["status"])
	}
	if got["bytes"].(float64) != 5 {
		t.Errorf("bytes = %v, want 5 (hello)", got["bytes"])
	}
	if got["ua"] != "ollama-python/0.5" {
		t.Errorf("ua = %v, want ollama-python/0.5", got["ua"])
	}
	if got["level"] != "debug" {
		t.Errorf("level = %v, want debug for 2xx", got["level"])
	}
}

// TestAccessLog_4xxLogsAtInfo — a 404 (the HA failure mode)
// should surface at INFO so an operator scanning the journal
// at the default log level sees client misconfig without
// having to enable debug.
func TestAccessLog_4xxLogsAtInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	wrapped := AccessLogMiddleware(logger)(inner)

	req := httptest.NewRequest(http.MethodPost, "/api/show", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	var got map[string]any
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got["level"] != "info" {
		t.Errorf("level = %v, want info for 404", got["level"])
	}
	if got["status"].(float64) != 404 {
		t.Errorf("status = %v, want 404", got["status"])
	}
}

// TestAccessLog_5xxLogsAtWarn — 5xx promoted to warn so
// production noise doesn't bury server-side failures.
func TestAccessLog_5xxLogsAtWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	wrapped := AccessLogMiddleware(logger)(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	var got map[string]any
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got["level"] != "warn" {
		t.Errorf("level = %v, want warn for 500", got["level"])
	}
}

// TestAccessLog_SkipsHealthProbes — /healthz, /readyz,
// /metrics are intentionally silent. Verifying by checking the
// log buffer stays empty.
func TestAccessLog_SkipsHealthProbes(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/health/live", "/health/ready"} {
		t.Run(path, func(t *testing.T) {
			var buf bytes.Buffer
			logger := zerolog.New(&buf)
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			wrapped := AccessLogMiddleware(logger)(inner)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			wrapped.ServeHTTP(rr, req)
			if buf.Len() != 0 {
				t.Errorf("%s: should be silent, got %q", path, buf.String())
			}
		})
	}
}

// TestAccessLog_SkipsStreamingEndpoints — SSE / event-stream
// suffixes are skipped to avoid duplicate lines on every
// keepalive flush.
func TestAccessLog_SkipsStreamingEndpoints(t *testing.T) {
	for _, path := range []string{"/ui/tasks/x/logs/stream", "/ui/tasks/x/events"} {
		t.Run(path, func(t *testing.T) {
			var buf bytes.Buffer
			logger := zerolog.New(&buf)
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
			wrapped := AccessLogMiddleware(logger)(inner)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			wrapped.ServeHTTP(rr, req)
			if buf.Len() != 0 {
				t.Errorf("%s: should be silent, got %q", path, buf.String())
			}
		})
	}
}

// TestAccessLog_UsesResolvedContextIP — the access log records the
// centrally-resolved client IP (what realip.Middleware stored in context),
// so the log agrees with the rate-limit / lockout / audit views.
func TestAccessLog_UsesResolvedContextIP(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	wrapped := AccessLogMiddleware(logger)(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	req = req.WithContext(realip.WithClientIP(req.Context(), "203.0.113.7"))
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	var got map[string]any
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got["remote"] != "203.0.113.7" {
		t.Errorf("remote = %v, want resolved context IP 203.0.113.7", got["remote"])
	}
}

// TestAccessLog_IgnoresForgedXFF is the access-log regression for the
// Cloudflare tunnel real-IP spoof: leftmost-XFF was attacker-controllable.
// With no realip context value (untrusted path) a forged X-Forwarded-For
// MUST NOT appear in the log — we record RemoteAddr's host.
func TestAccessLog_IgnoresForgedXFF(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	wrapped := AccessLogMiddleware(logger)(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	req.RemoteAddr = "198.51.100.9:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	var got map[string]any
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got["remote"] != "198.51.100.9" {
		t.Errorf("remote = %v, want RemoteAddr host 198.51.100.9 (forged XFF ignored)", got["remote"])
	}
}

// TestAccessLog_StripsPortFromRemoteAddr — raw RemoteAddr
// includes host:port; logs should just show the IP for
// readability.
func TestAccessLog_StripsPortFromRemoteAddr(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	wrapped := AccessLogMiddleware(logger)(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	req.RemoteAddr = "192.168.10.42:55214"
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	var got map[string]any
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got["remote"] != "192.168.10.42" {
		t.Errorf("remote = %v, want 192.168.10.42 (port stripped)", got["remote"])
	}
}

// TestAccessLog_DefaultsStatus200WhenHandlerOmitsWriteHeader —
// handlers that write the body without calling WriteHeader
// produce a 200 from net/http; the recorder must reflect that.
func TestAccessLog_DefaultsStatus200WhenHandlerOmitsWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})
	wrapped := AccessLogMiddleware(logger)(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	var got map[string]any
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got["status"].(float64) != 200 {
		t.Errorf("status = %v, want 200 (default)", got["status"])
	}
}

// TestTruncateUA — long User-Agent strings clip to keep log
// lines scannable.
func TestTruncateUA(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateUA(string(long))
	if len(got) > 205 {
		t.Errorf("len(got) = %d, want clipped to ~201 with ellipsis", len(got))
	}
}
