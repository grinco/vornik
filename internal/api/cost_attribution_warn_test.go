// Tests for warnOnAnonymousAttribution — the diagnostic warn that
// fires when an external API call lands on _external because no
// project could be derived. Pins the rate-limit so a misconfigured
// HA-style client polling every 30s doesn't flood the log.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// newWarnTestServer builds a Server with a zerolog sink that
// captures emitted lines into a bytes.Buffer the tests inspect.
func newWarnTestServer() (*Server, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	srv := &Server{logger: zerolog.New(buf).Level(zerolog.WarnLevel)}
	return srv, buf
}

// TestWarnOnAnonymousAttribution_NonAnonymousNoop — only the
// AttributionAnonymous case fires; the other three sources skip
// silently.
func TestWarnOnAnonymousAttribution_NonAnonymousNoop(t *testing.T) {
	srv, buf := newWarnTestServer()
	r := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	for _, src := range []AttributionSource{
		AttributionFromDBKey, AttributionFromHeader, AttributionFromFallback,
	} {
		srv.warnOnAnonymousAttribution(r, src)
	}
	if buf.Len() != 0 {
		t.Errorf("non-anonymous attribution should not log; got %q", buf.String())
	}
}

// TestWarnOnAnonymousAttribution_LogsExpectedFields — the warn
// includes the diagnostic signals the operator needs: path, UA,
// auth-presence flags. Pin them via JSON parse so the contract
// is machine-readable.
func TestWarnOnAnonymousAttribution_LogsExpectedFields(t *testing.T) {
	srv, buf := newWarnTestServer()
	r := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	r.Header.Set("User-Agent", "ollama-python/0.5.1")
	r.Header.Set("Authorization", "Bearer some-key")

	srv.warnOnAnonymousAttribution(r, AttributionAnonymous)
	if buf.Len() == 0 {
		t.Fatal("anonymous attribution should produce a warn line")
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("warn line is not JSON: %v\n%s", err, buf.String())
	}
	wantStr := map[string]string{
		"path":       "/api/chat",
		"user_agent": "ollama-python/0.5.1",
	}
	for k, want := range wantStr {
		got, _ := entry[k].(string)
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if entry["has_authorization"] != true {
		t.Errorf("has_authorization = %v, want true (Bearer header set)", entry["has_authorization"])
	}
	if entry["has_x_api_key"] != false {
		t.Errorf("has_x_api_key = %v, want false (no X-API-Key header)", entry["has_x_api_key"])
	}
	msg, _ := entry["message"].(string)
	if !strings.Contains(msg, "_external") {
		t.Errorf("message should name the _external bucket; got %q", msg)
	}
}

// TestWarnOnAnonymousAttribution_MissingUserAgent — when the
// client doesn't send a User-Agent, the warn reports "(missing)"
// instead of an empty string so operators can spot it in log
// review.
func TestWarnOnAnonymousAttribution_MissingUserAgent(t *testing.T) {
	srv, buf := newWarnTestServer()
	r := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	srv.warnOnAnonymousAttribution(r, AttributionAnonymous)

	var entry map[string]any
	_ = json.Unmarshal(buf.Bytes(), &entry)
	if got, _ := entry["user_agent"].(string); got != "(missing)" {
		t.Errorf("user_agent = %q, want (missing)", got)
	}
}

// TestWarnOnAnonymousAttribution_RateLimitedByRoute — first call
// for a (path, UA) pair fires; subsequent calls within the
// cadence are silent. Each NEW pair fires its own first-time warn.
func TestWarnOnAnonymousAttribution_RateLimitedByRoute(t *testing.T) {
	srv, buf := newWarnTestServer()

	r1 := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	r1.Header.Set("User-Agent", "ollama-python/0.5.1")
	srv.warnOnAnonymousAttribution(r1, AttributionAnonymous)
	srv.warnOnAnonymousAttribution(r1, AttributionAnonymous)
	srv.warnOnAnonymousAttribution(r1, AttributionAnonymous)

	r2 := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
	r2.Header.Set("User-Agent", "ollama-python/0.5.1")
	srv.warnOnAnonymousAttribution(r2, AttributionAnonymous)

	r3 := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	r3.Header.Set("User-Agent", "OpenWebUI/0.5.7")
	srv.warnOnAnonymousAttribution(r3, AttributionAnonymous)

	// 1 line for r1's first call, 1 for r2's first call, 1 for r3's
	// first call = 3 total. r1's repeat calls are suppressed.
	lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
	if lines != 3 {
		t.Errorf("expected 3 warn lines (1 per unique path+UA); got %d:\n%s", lines, buf.String())
	}
}

// TestWarnOnAnonymousAttribution_CadenceResetsAfterWindow — once
// the cadence window passes, the same (path, UA) fires a fresh
// warn. We don't actually wait 5 minutes; we mutate the cache to
// pretend the window has elapsed.
func TestWarnOnAnonymousAttribution_CadenceResetsAfterWindow(t *testing.T) {
	srv, buf := newWarnTestServer()
	r := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	r.Header.Set("User-Agent", "ollama-python/0.5.1")

	srv.warnOnAnonymousAttribution(r, AttributionAnonymous)
	firstCount := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1

	// Roll back the cache entry to before the cadence window.
	srv.anonAttrWarner.mu.Lock()
	for k := range srv.anonAttrWarner.lastFor {
		srv.anonAttrWarner.lastFor[k] = time.Now().Add(-10 * time.Minute)
	}
	srv.anonAttrWarner.mu.Unlock()

	srv.warnOnAnonymousAttribution(r, AttributionAnonymous)
	secondCount := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1

	if secondCount <= firstCount {
		t.Errorf("after cadence window: line count %d → %d, expected another fire", firstCount, secondCount)
	}
}

// TestWarnOnAnonymousAttribution_NilRequestGuard — defensive: nil
// request must not panic.
func TestWarnOnAnonymousAttribution_NilRequestGuard(t *testing.T) {
	srv, _ := newWarnTestServer()
	srv.warnOnAnonymousAttribution(nil, AttributionAnonymous)
}
