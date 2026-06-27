package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// URLLivenessChecker walks project_memory_chunks, extracts URLs from
// each chunk's content, and HEAD-pings them to flag dead URLs. The
// outcome is written back to the chunk (last_checked_at, is_alive)
// so the memory search response can surface freshness to consuming
// agents.
//
// The checker is a pure read/write driver — it does not own the
// scheduler. The CLI command `vornikctl memory recheck-urls --project
// <id>` invokes RecheckProject directly; a periodic auto-worker is a
// follow-up that can wrap RecheckProject in a tick loop without
// changing the checker's surface.
//
// Design choices:
//   - HEAD with a 5s timeout per URL bounds the wall-clock cost of a
//     full project sweep; chunks with many URLs degrade gracefully
//     (we stop at the first 2xx/3xx and call the chunk alive).
//   - 4xx (404/410/403/451) → dead. 5xx → unknown (treated as alive
//     to avoid flagging chunks during upstream outages).
//   - Connection / DNS / TLS errors → dead. The chunk's reference is
//     unfetchable from the daemon's egress, which is what matters
//     for downstream agents using the same path.
type URLLivenessChecker struct {
	repo    *Repository
	client  HTTPDoer
	metrics *Metrics
	logger  zerolog.Logger
	// timeout per HEAD request. Defaults to 5s. Configurable for tests
	// that need a tighter ceiling.
	timeout time.Duration
	// allowPrivateNetworks bypasses SSRF private-address blocking.
	// Production leaves this false; tests that use httptest.Server
	// on 127.0.0.1 opt in explicitly.
	allowPrivateNetworks bool
}

// HTTPDoer is the minimal HTTP client interface URLLivenessChecker
// needs. Tests substitute a *httptest.Server's client; production
// uses an http.Client tuned for short-lived HEAD calls.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewURLLivenessChecker builds a checker against the provided
// Repository. The HTTP client defaults to one with a 5s overall
// timeout; pass WithHTTPClient to override (used by tests).
func NewURLLivenessChecker(repo *Repository) *URLLivenessChecker {
	c := &URLLivenessChecker{
		repo:    repo,
		logger:  zerolog.Nop(),
		timeout: 5 * time.Second,
	}
	c.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: c.dialContext,
		},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if err := c.validateLivenessURL(req.Context(), req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	return c
}

// SetLogger wires a logger for per-URL debug + per-chunk warn.
func (c *URLLivenessChecker) SetLogger(l zerolog.Logger) { c.logger = l }

// SetMetrics wires a Metrics instance so each recheck increments
// vornik_memory_url_liveness_total{project,alive}.
func (c *URLLivenessChecker) SetMetrics(m *Metrics) { c.metrics = m }

// SetHTTPClient swaps the HTTP doer (used by tests to point at an
// httptest.Server). The injected client is responsible for its own
// timeout — the checker no longer wraps the request context.
func (c *URLLivenessChecker) SetHTTPClient(d HTTPDoer) {
	if d != nil {
		c.client = d
	}
}

// SetAllowPrivateNetworksForTest permits localhost/private-network
// targets. It exists for httptest-backed unit tests only; production
// must keep the default false so liveness checks cannot SSRF the
// daemon's loopback, RFC1918 networks, or cloud metadata endpoints.
func (c *URLLivenessChecker) SetAllowPrivateNetworksForTest(v bool) {
	c.allowPrivateNetworks = v
}

// SetTimeout overrides the per-URL deadline. Zero / negative falls
// back to the constructor default.
func (c *URLLivenessChecker) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout = d
	}
}

// urlRegex extracts http(s) URLs from chunk content. Conservative —
// stops at whitespace, common Markdown punctuation, and angle
// brackets. The trailing punctuation trim handles the "see
// https://example.com." case where a sentence-ending period bled
// into the match.
var urlRegex = regexp.MustCompile(`https?://[^\s<>"'\)]+`)

// trimURLPunct strips trailing characters that almost certainly
// belong to the surrounding prose rather than the URL itself.
func trimURLPunct(s string) string {
	for len(s) > 0 {
		last := s[len(s)-1]
		if last == '.' || last == ',' || last == ';' || last == ':' ||
			last == ')' || last == ']' || last == '!' || last == '?' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// extractURLs returns the unique URL list found in content, in the
// order they first appear. Duplicates are dropped so a chunk that
// repeats the same link doesn't multiply the HEAD cost.
func extractURLs(content string) []string {
	matches := urlRegex.FindAllString(content, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		u := trimURLPunct(m)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// checkURL HEAD-pings one URL. Returns:
//   - alive=true  on any 2xx / 3xx response, OR a 5xx (treated as
//     "upstream hiccup, don't flag")
//   - alive=false on 4xx OR on transport-level errors (DNS / TLS /
//     connect / read)
//
// The fallback to GET is deliberate-omitted: many sites reject HEAD
// on principle, which would flood the daemon with false-positive
// "dead" calls. If a HEAD fails at the transport layer we trust the
// signal; if it returns 405 / 501 we conservatively treat as alive.
func (c *URLLivenessChecker) checkURL(ctx context.Context, u string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.validateLivenessURL(reqCtx, u); err != nil {
		c.logger.Debug().Err(err).Str("url", u).Msg("memory: URL liveness target rejected")
		return false
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, u, nil)
	if err != nil {
		// Malformed URL — we can't fetch it, so it's dead from the
		// agent's perspective.
		return false
	}
	// Some CDNs (Cloudflare, Akamai) reject the default Go UA with
	// 403. A vanilla browser-style UA gets through. Not a stealth
	// move — we just want the same "is this URL alive" signal a
	// human's browser would see.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; vornik-liveness/1.0)")
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		return true
	case resp.StatusCode == http.StatusMethodNotAllowed,
		resp.StatusCode == http.StatusNotImplemented:
		// Server rejected HEAD on principle. Treat as alive — a 405 to
		// HEAD doesn't mean the resource is gone, just that the server
		// doesn't speak HEAD.
		return true
	case resp.StatusCode >= 500:
		// Upstream outage — don't flag the chunk dead on a transient
		// failure. The next recheck will re-evaluate.
		return true
	default:
		// 4xx — 404, 410, 403, 451, etc. Dead from the agent's POV.
		return false
	}
}

func (c *URLLivenessChecker) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := c.publicIPsForHost(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	dialer := &net.Dialer{}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("host %q resolved no addresses", host)
}

// validateLivenessURL rejects URL targets that would let memory
// content trigger SSRF from the daemon. Only http(s) URLs with
// public-routable hosts are allowed by default. Hostnames are
// resolved before the request; any private/loopback/link-local/etc.
// answer rejects the target. The same function is used by the
// default client's redirect hook, so a public URL cannot redirect
// the checker to 127.0.0.1 or 169.254.169.254.
func (c *URLLivenessChecker) validateLivenessURL(ctx context.Context, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return fmt.Errorf("URL userinfo is not allowed")
	}
	_, err = c.publicIPsForHost(ctx, parsed.Hostname())
	return err
}

func (c *URLLivenessChecker) publicIPsForHost(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, fmt.Errorf("URL host is empty")
	}
	if c != nil && c.allowPrivateNetworks {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}, nil
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve host %q: %w", host, err)
		}
		return ipAddrsToIPs(addrs), nil
	}
	if isLocalHostname(host) {
		return nil, fmt.Errorf("local hostname %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateLivenessIP(ip) {
			return nil, fmt.Errorf("private IP %q is not allowed", host)
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("host %q resolved no addresses", host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if isPrivateLivenessIP(addr.IP) {
			return nil, fmt.Errorf("host %q resolves to private IP %q", host, addr.IP.String())
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

func ipAddrsToIPs(addrs []net.IPAddr) []net.IP {
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.IP)
	}
	return out
}

func isLocalHostname(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

func isPrivateLivenessIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsUnspecified() ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast()
}

// LivenessOutcome aggregates one chunk's recheck result.
type LivenessOutcome struct {
	ChunkID    string
	URLCount   int
	AliveCount int
	IsAlive    bool // overall verdict: any alive URL => true
	HasURLs    bool
}

// RecheckOutcome is the aggregate result of a project-wide sweep.
type RecheckOutcome struct {
	ChunksScanned   int
	ChunksWithURLs  int
	URLsChecked     int
	URLsAlive       int
	URLsDead        int
	ChunksFlagged   int // chunks moved into is_alive=false
	ChunksConfirmed int // chunks moved into is_alive=true
}

// RecheckProject walks every chunk in the project, extracts URLs,
// HEAD-pings each, and stamps last_checked_at + is_alive on the
// chunk. Chunks with no URLs are left untouched (is_alive remains
// NULL) so the search-side caller can distinguish "no URLs to check"
// from "URLs were checked and dead".
//
// The walk is bounded by limit (0 = unlimited). Callers passing 0
// should hold a tight grip on the project's chunk count — a project
// with 50k URL-carrying chunks at 5s/URL is a multi-hour run.
func (c *URLLivenessChecker) RecheckProject(ctx context.Context, projectID string, limit int) (RecheckOutcome, error) {
	if c == nil || c.repo == nil {
		return RecheckOutcome{}, errors.New("memory: liveness checker not initialised")
	}
	if projectID == "" {
		return RecheckOutcome{}, errors.New("memory: project_id is required")
	}

	chunks, err := c.repo.ListChunksForLivenessCheck(ctx, projectID, limit)
	if err != nil {
		return RecheckOutcome{}, fmt.Errorf("list chunks: %w", err)
	}

	out := RecheckOutcome{}
	for _, ch := range chunks {
		if err := ctx.Err(); err != nil {
			// Context cancelled mid-walk; return what we've done.
			return out, err
		}
		out.ChunksScanned++
		urls := extractURLs(ch.Content)
		if len(urls) == 0 {
			continue
		}
		out.ChunksWithURLs++
		anyAlive := false
		for _, u := range urls {
			alive := c.checkURL(ctx, u)
			out.URLsChecked++
			if alive {
				out.URLsAlive++
				anyAlive = true
			} else {
				out.URLsDead++
			}
			c.recordLivenessMetric(projectID, alive)
		}
		if anyAlive {
			out.ChunksConfirmed++
		} else {
			out.ChunksFlagged++
		}
		if err := c.repo.UpdateChunkLiveness(ctx, ch.ID, anyAlive, time.Now()); err != nil {
			c.logger.Warn().Err(err).
				Str("project_id", projectID).
				Str("chunk_id", ch.ID).
				Msg("memory: failed to persist URL liveness flag")
		}
	}
	return out, nil
}

func (c *URLLivenessChecker) recordLivenessMetric(projectID string, alive bool) {
	if c.metrics == nil || c.metrics.URLLivenessTotal == nil {
		return
	}
	v := "false"
	if alive {
		v = "true"
	}
	c.metrics.URLLivenessTotal.WithLabelValues(projectID, v).Inc()
}

// LivenessChunk is the slim chunk row used by the recheck walker.
// content carries the full chunk body so the URL regex can run; id
// is the PK used by UpdateChunkLiveness.
type LivenessChunk struct {
	ID      string
	Content string
}

// ListChunksForLivenessCheck reads every chunk for a project (or up
// to limit when > 0), newest first. The query is deliberately
// strung off the existing project_memory_chunks shape; future work
// can layer "only re-check chunks where last_checked_at < cutoff"
// but the first cut walks everything so initial labelling lands
// densely.
func (r *Repository) ListChunksForLivenessCheck(ctx context.Context, projectID string, limit int) ([]LivenessChunk, error) {
	if projectID == "" {
		return nil, errors.New("project_id required")
	}
	q := `
SELECT id, content
FROM project_memory_chunks
WHERE project_id = $1
ORDER BY last_checked_at NULLS FIRST, created_at DESC`
	args := []interface{}{projectID}
	if limit > 0 {
		q += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LivenessChunk
	for rows.Next() {
		var c LivenessChunk
		if err := rows.Scan(&c.ID, &c.Content); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateChunkLiveness writes the recheck verdict to one chunk.
func (r *Repository) UpdateChunkLiveness(ctx context.Context, chunkID string, isAlive bool, checkedAt time.Time) error {
	if chunkID == "" {
		return errors.New("chunk_id required")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE project_memory_chunks SET is_alive = $1, last_checked_at = $2 WHERE id = $3`,
		isAlive, checkedAt, chunkID,
	)
	return err
}

// LivenessMethodHEADName is exposed so callers and tests can match on the
// HTTP method we use; the actual constant string is centralised here so
// a future migration to GET-with-Range can flip one place.
const LivenessMethodHEADName = http.MethodHead

// ensure sql.ErrNoRows handling stays explicit in vendor-friendly form.
var _ = sql.ErrNoRows
var _ = strings.TrimSpace
