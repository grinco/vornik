package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/httpx/realip"
)

// AccessLogMiddleware emits one structured log line per HTTP
// request reaching the api mux. Captures the request method +
// path + query + remote address + User-Agent, plus the response
// status code + bytes written + total duration.
//
// Operator-facing — wired so a third-party integration
// failing to talk to vornik (Home Assistant Ollama config flow,
// OpenAI SDK setup, curl probes) leaves a clear breadcrumb
// trail in the journal regardless of which handler the request
// did or didn't reach. The status-code + remote-addr fields
// answer "did the request even arrive, what did we say back" in
// one query.
//
// Logging level rules:
//   - 5xx → Warn (operator should see them by default)
//   - 4xx → Info (probable client misconfig; helpful but not
//     alarming)
//   - everything else → Debug (high-volume happy path; visible
//     only with --log-level=debug)
//
// Health probes (/healthz, /readyz, /metrics) and SSE / long-
// poll streaming endpoints are intentionally skipped — they'd
// drown the log without adding signal. Worth revisiting if we
// add a "noisy-routes-allowed" debug flag later.
func AccessLogMiddleware(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skipAccessLog(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rw := &accessRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			dur := time.Since(start)

			ev := logger.Debug()
			switch {
			case rw.status >= 500:
				ev = logger.Warn()
			case rw.status >= 400:
				ev = logger.Info()
			}

			ev.
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("query", r.URL.RawQuery).
				Str("remote", remoteAddr(r)).
				Str("ua", truncateUA(r.UserAgent())).
				Int("status", rw.status).
				Int("bytes", rw.bytes).
				Dur("duration", dur).
				Msg("http request")
		})
	}
}

// skipAccessLog returns true for high-volume / low-signal
// endpoints. Keeps the journal scannable.
func skipAccessLog(path string) bool {
	switch path {
	case "/livez", "/healthz", "/readyz", "/metrics",
		"/health/live", "/health/ready":
		return true
	}
	// SSE / long-poll endpoints: skip to avoid duplicate lines
	// on every keepalive flush. The chat-completion proxy logs
	// its own provider-call entry, which is enough.
	if strings.HasSuffix(path, "/logs/stream") ||
		strings.HasSuffix(path, "/events") {
		return true
	}
	return false
}

// remoteAddr returns the client IP for the access-log line. It reads the
// centrally-resolved client IP that realip.Middleware stored in the request
// context (trusted-proxy aware, spoof-safe), falling back to RemoteAddr's
// host when the context is unset. It does NOT read X-Forwarded-For directly
// — that resolution is centralised so the log agrees with the rate-limit /
// lockout / audit views of "who the caller is".
// see LLD § https://docs.vornik.io
func remoteAddr(r *http.Request) string {
	if ip := realip.ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	return realip.RemoteHost(r)
}

// truncateUA clips overly verbose User-Agent strings. The
// dominant case in production logs is the agent harness's own
// curl-built UA, which is short; client UAs like
// "openai-python/1.x.x (...long-platform-info...)" can balloon.
// 200 chars is a generous ceiling.
func truncateUA(ua string) string {
	const max = 200
	if len(ua) <= max {
		return ua
	}
	return ua[:max] + "…"
}

// accessRecorder wraps http.ResponseWriter to capture the
// status code + byte count without burying the body. Standard
// library doesn't expose a hook for this, so we intercept
// WriteHeader and Write. Hijack is forwarded for handlers that
// upgrade (websocket / SSE) — without this the bufio.ReadWriter
// path breaks.
type accessRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (a *accessRecorder) WriteHeader(code int) {
	if a.wroteHeader {
		return
	}
	a.status = code
	a.wroteHeader = true
	a.ResponseWriter.WriteHeader(code)
}

func (a *accessRecorder) Write(b []byte) (int, error) {
	if !a.wroteHeader {
		// Default to 200 when the handler writes body without
		// explicit WriteHeader. Mirrors net/http's own semantics.
		a.WriteHeader(http.StatusOK)
	}
	n, err := a.ResponseWriter.Write(b)
	a.bytes += n
	return n, err
}

// Flush forwards to the wrapped writer so SSE / NDJSON
// streaming endpoints continue to flush per-chunk under the
// middleware.
func (a *accessRecorder) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the wrapped writer if it supports the
// interface so connection-upgrade paths (websocket) still work.
// Handlers that don't upgrade never hit this.
func (a *accessRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := a.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errAccessRecorderNoHijack
}

var errAccessRecorderNoHijack = errors.New("access-log recorder: wrapped ResponseWriter does not support Hijack")
