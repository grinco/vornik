package mcp

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"vornik.io/vornik/internal/ratelimit"
)

// ToolRateLimitSpec is the per-tool token-bucket configuration as
// the daemon wires it into the MCP client. Mirrors the operator-
// authored YAML shape (registry.ToolRateLimitSpec) so the registry
// and the MCP package agree on a single primitive without the
// registry having to import the keybucket implementation. Both
// values must be > 0 for the bucket to be active.
type ToolRateLimitSpec struct {
	RPS   int
	Burst int
}

// Enabled mirrors the registry helper — RPS≤0 OR Burst≤0 disables
// enforcement (matches the keybucket's bypass contract).
func (s ToolRateLimitSpec) Enabled() bool {
	return s.RPS > 0 && s.Burst > 0
}

// ToolRateLimiter wraps a per-tool APIKeyLimiter so the MCP client
// can ask "may I call this tool now?" without the keybucket
// primitive needing to know about MCP-specific labels. Configured
// once per Client (via ServerConfig.ToolRateLimits) and consulted
// by Client.CallTool before the JSON-RPC write.
//
// Match precedence inside Spec():
//  1. exact "server.tool" key                — most specific
//  2. bare "tool" key                        — server-agnostic
//  3. no entry                               — unlimited
//
// Concurrency: APIKeyLimiter is safe for concurrent use; this
// wrapper adds nothing of its own. The specs map is immutable
// after Client construction.
type ToolRateLimiter struct {
	limiter *ratelimit.APIKeyLimiter
	specs   map[string]ToolRateLimitSpec
	// nowFn is the clock the bucket consults — overridable so tests
	// can deterministically advance time without sleeping. Defaults
	// to time.Now in NewToolRateLimiter.
	nowFn func() time.Time
}

// NewToolRateLimiter constructs a tool-rate limiter from the raw
// per-tool spec map. Returns nil when specs is empty / contains no
// enabled entries — callers can short-circuit on nil to skip the
// allow check entirely (zero overhead for projects that haven't
// opted in). Filters out entries with RPS≤0 OR Burst≤0 so a
// half-configured YAML block doesn't leak buckets.
func NewToolRateLimiter(specs map[string]ToolRateLimitSpec) *ToolRateLimiter {
	if len(specs) == 0 {
		return nil
	}
	active := make(map[string]ToolRateLimitSpec, len(specs))
	for k, v := range specs {
		if !v.Enabled() {
			continue
		}
		active[k] = v
	}
	if len(active) == 0 {
		return nil
	}
	return &ToolRateLimiter{
		limiter: ratelimit.NewAPIKeyLimiter(),
		specs:   active,
		nowFn:   time.Now,
	}
}

// Spec resolves the tool name to a configured spec, honouring the
// server.tool precedence rule. Returns ok=false when no entry
// matches — caller treats that as "unlimited". Exported for the
// test harness; the client uses Allow.
func (r *ToolRateLimiter) Spec(serverName, toolName string) (ToolRateLimitSpec, bool) {
	spec, _, ok := r.specWithBucketKey(serverName, toolName)
	return spec, ok
}

func (r *ToolRateLimiter) specWithBucketKey(serverName, toolName string) (ToolRateLimitSpec, string, bool) {
	if r == nil {
		return ToolRateLimitSpec{}, "", false
	}
	if serverName != "" {
		key := serverName + "." + toolName
		if s, ok := r.specs[key]; ok {
			return s, key, true
		}
	}
	if s, ok := r.specs[toolName]; ok {
		return s, toolName, true
	}
	return ToolRateLimitSpec{}, "", false
}

// Allow consumes one token from the bucket for (serverName, toolName).
// Returns blocked=true with retryAfter set when no token is available.
// retryAfter is rounded up to the next whole second so the HTTP
// Retry-After header (which the caller surfaces in its error string)
// is at least as long as the bucket actually needs.
//
// Safe on nil receiver — when the limiter is disabled, every call
// passes immediately (zero overhead).
func (r *ToolRateLimiter) Allow(serverName, toolName string) (blocked bool, retryAfter time.Duration) {
	if r == nil {
		return false, 0
	}
	spec, bucketKey, ok := r.specWithBucketKey(serverName, toolName)
	if !ok {
		return false, 0
	}
	// keybucket Allow keys on a single string; namespace the bucket
	// with the matched spec key. Exact server.tool specs are isolated;
	// bare tool specs intentionally share one server-agnostic bucket.
	d := r.limiter.Allow(bucketKey, spec.RPS, spec.Burst, r.nowFn())
	if !d.Blocked {
		return false, 0
	}
	retryAfter = d.RetryAfter
	// Round UP to the next whole second so HTTP Retry-After (whole
	// seconds per RFC 7231) doesn't undershoot the bucket's deficit.
	if retryAfter%time.Second != 0 {
		retryAfter = ((retryAfter / time.Second) + 1) * time.Second
	}
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return true, retryAfter
}

// ToolRateLimitError is the failure path Client.CallTool emits when
// a tool's bucket is empty. Carries the Retry-After hint so the
// caller (agent / dispatcher) can back off precisely instead of
// burning a generic backoff window. The "rate_limit_error" prefix
// matches the error-shape convention agents already pattern-match
// on for HTTP 429s — keeps the catch-and-retry logic uniform.
type ToolRateLimitError struct {
	Server     string
	Tool       string
	RetryAfter time.Duration
}

func (e *ToolRateLimitError) Error() string {
	return fmt.Sprintf("rate_limit_error: mcp tool %q on server %q throttled; retry after %ds",
		e.Tool, e.Server, int(e.RetryAfter.Seconds()))
}

// --- Prometheus surface ---

var (
	toolRateLimitedOnce  sync.Once
	toolRateLimitedTotal *prometheus.CounterVec
)

// getToolRateLimitedCounter lazily registers the per-tool
// rate-limit counter against the default Prometheus registerer.
// Lazy init lets the package stay test-friendly: a test that
// touches the counter doesn't drag a full Prometheus registry
// requirement into every importer.
func getToolRateLimitedCounter() *prometheus.CounterVec {
	toolRateLimitedOnce.Do(func() {
		toolRateLimitedTotal = promauto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "mcp",
				Name:      "tool_rate_limited_total",
				Help:      "Count of outbound MCP tool calls rejected by the in-daemon per-tool token bucket (rate-limit hardening sub-item 3). Labelled by (project,server,tool) so dashboards can spot a misbehaving project hammering a specific upstream.",
			},
			[]string{"project", "server", "tool"},
		)
	})
	return toolRateLimitedTotal
}

// parseRetryAfterHeaderForMCP is the small subset of the chat-package
// Retry-After parser we need on the MCP SSE error path (sub-item 8
// step 3). Accepts the numeric-seconds form per RFC 7231; HTTP-date
// is intentionally omitted because no MCP server in production
// surfaces dates on 429 — keeping the parser tight reduces
// attack surface on a code path that touches untrusted upstream
// headers.
//
// Returns 0 when the header is empty, non-numeric, or non-positive.
// Callers floor to 1s before surfacing so the Retry-After window
// is always actionable.
func parseRetryAfterHeaderForMCP(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	secs, err := parseInt(value)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// parseInt is a tiny helper to keep ratelimit.go free of yet another
// strconv import — we only need positive integers.
func parseInt(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit")
		}
		n = n*10 + int(r-'0')
		// Guard against pathological 6-digit-plus inputs that would
		// arithmetic-overflow into negative values; well above the
		// retryAfterCeiling so a clamp is safe.
		if n > 1_000_000 {
			return 0, fmt.Errorf("too large")
		}
	}
	if len(s) == 0 {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}

// ObserveToolRateLimited bumps the per-tool counter. Safe on empty
// labels (skipped) — defends against an empty project ID slipping
// in via a test path.
func ObserveToolRateLimited(project, server, tool string) {
	if strings.TrimSpace(project) == "" || strings.TrimSpace(server) == "" || strings.TrimSpace(tool) == "" {
		return
	}
	getToolRateLimitedCounter().WithLabelValues(project, server, tool).Inc()
}
