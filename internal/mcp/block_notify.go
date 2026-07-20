package mcp

// Scraper block → Telegram notify loop (daemon side). Design:
// https://docs.vornik.io
//
// A post-hook at Manager.Execute (§3.2a: the single shared chokepoint for both
// agent-container and dispatcher web_fetch calls) turns a solvable scraper
// block on a curated portal into a proactive operator Telegram notification —
// screenshot + the exact `vornikctl scraper login` command. Everything past a
// cheap gate + bounded enqueue happens in a single background worker; the hot
// path never blocks and the worker swallows all delivery errors.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"
)

// OperatorNotifier delivers a scraper-block alert to the operator(s). The
// telegram.Bot satisfies this structurally via primitive params, so this
// package never imports telegram. A nil notifier makes the hook inert.
type OperatorNotifier interface {
	NotifyScraperBlock(ctx context.Context, project, host, reason, detail, loginCmd string, screenshotJPEG []byte) error
}

// solvableBlockReasons mirrors the scraper's SOLVABLE_BLOCK_REASONS: the gates a
// human can resolve via `vornikctl scraper login`. paywall / rate_limited /
// http_403 / server_error never notify (nothing a login solves).
var solvableBlockReasons = map[string]bool{
	"captcha":       true,
	"login_wall":    true,
	"auth_required": true,
}

const blockNotifyQueueSize = 16

type blockNotifyJob struct {
	project, host, reqURL, reason, detail string
	screenshot                            []byte
}

// BlockNotifier gates scraper blocks on a curated portal set, dedups per
// (project,host) with a cooldown, and hands survivors to a single delivery
// worker. Hot-path methods never block Execute; the worker swallows all errors.
type BlockNotifier struct {
	cooldown time.Duration
	portals  map[string]bool // curated hosts, lowercased
	notifier OperatorNotifier
	logger   zerolog.Logger

	now  func() time.Time // injectable clock (tests)
	mu   sync.Mutex
	last map[string]time.Time // project\x00host → last notify time

	queue chan blockNotifyJob
}

// NewBlockNotifier builds a notifier. cooldown ≤ 0 defaults to 6h; an empty
// portals set means nothing ever fires (safe default).
func NewBlockNotifier(portals []string, cooldown time.Duration, notifier OperatorNotifier, logger zerolog.Logger) *BlockNotifier {
	if cooldown <= 0 {
		cooldown = 6 * time.Hour
	}
	set := make(map[string]bool, len(portals))
	for _, p := range portals {
		if h := strings.ToLower(strings.TrimSpace(p)); h != "" {
			set[h] = true
		}
	}
	return &BlockNotifier{
		cooldown: cooldown,
		portals:  set,
		notifier: notifier,
		logger:   logger,
		now:      time.Now,
		last:     make(map[string]time.Time),
		queue:    make(chan blockNotifyJob, blockNotifyQueueSize),
	}
}

// Start launches the single delivery worker; cancel ctx to stop it. Nil-safe.
func (bn *BlockNotifier) Start(ctx context.Context) {
	if bn == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-bn.queue:
				bn.deliver(ctx, job)
			}
		}
	}()
}

// MaybeNotify is the hot-path hook, called from Manager.Execute after a
// successful (non-error) tool result. Cheap no-op unless the call was a scraper
// web_fetch that landed on a solvable block for a curated portal. Never blocks:
// the only work here is a JSON peek, a mutex-guarded dedup check-and-stamp, and
// a non-blocking enqueue. Nil-safe.
func (bn *BlockNotifier) MaybeNotify(project, toolName, argsJSON, resultJSON string) {
	if bn == nil || bn.notifier == nil {
		return
	}
	if toolName != "web_fetch" {
		return
	}
	var res struct {
		BlockReason     string `json:"block_reason"`
		BlockDetail     string `json:"block_detail"`
		BlockScreenshot string `json:"block_screenshot"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &res); err != nil {
		return // not a web_fetch result shape — nothing to do
	}
	if !solvableBlockReasons[res.BlockReason] {
		return
	}
	reqURL := urlFromArgs(argsJSON)
	host := hostOf(reqURL)
	if host == "" {
		return
	}
	portal, ok := bn.matchPortal(host)
	if !ok {
		return // not a curated portal — no notification
	}
	if !bn.claim(project, portal) {
		return // within cooldown for this (project, portal)
	}
	job := blockNotifyJob{
		project:    project,
		host:       portal,
		reqURL:     reqURL,
		reason:     res.BlockReason,
		detail:     res.BlockDetail,
		screenshot: decodeDataURI(res.BlockScreenshot),
	}
	select {
	case bn.queue <- job:
	default:
		blockNotifyDropped().Inc()
		bn.logger.Warn().Str("project", project).Str("host", portal).
			Msg("scraper block-notify: queue full, dropped notification")
	}
}

func (bn *BlockNotifier) deliver(ctx context.Context, job blockNotifyJob) {
	loginCmd := fmt.Sprintf("vornikctl scraper login start -p %s --url %s", job.project, job.reqURL)
	if err := bn.notifier.NotifyScraperBlock(ctx, job.project, job.host, job.reason, job.detail, loginCmd, job.screenshot); err != nil {
		blockNotifyFailed().Inc()
		bn.logger.Warn().Err(err).Str("project", job.project).Str("host", job.host).
			Msg("scraper block-notify: delivery failed")
		return
	}
	blockNotifySent().Inc()
	bn.logger.Info().Str("project", job.project).Str("host", job.host).Str("reason", job.reason).
		Bool("screenshot", len(job.screenshot) > 0).Msg("scraper block-notify: operator notified")
}

// claim returns true iff a notification for (project, portal) is allowed now,
// stamping the time. Mutex-guarded because MaybeNotify runs under concurrent
// Execute calls (Manager holds only a shared RLock across the call).
func (bn *BlockNotifier) claim(project, portal string) bool {
	key := project + "\x00" + portal
	bn.mu.Lock()
	defer bn.mu.Unlock()
	now := bn.now()
	// Opportunistic prune so the map can't grow unboundedly across many
	// (project,portal) pairs over the daemon's lifetime (companion review,
	// low-pri idle-memory finding). An entry older than the cooldown no
	// longer suppresses anything; 2× is a safety margin. Claims are rare
	// (only solvable blocks on curated portals), so the O(n) sweep is cheap.
	cutoff := now.Add(-2 * bn.cooldown)
	for k, t := range bn.last {
		if t.Before(cutoff) {
			delete(bn.last, k)
		}
	}
	if t, ok := bn.last[key]; ok && now.Sub(t) < bn.cooldown {
		return false
	}
	bn.last[key] = now
	return true
}

// matchPortal reports whether host is (or is a subdomain of) a curated portal,
// returning the matched portal so dedup keys on the portal, not the subdomain
// the login wall redirected to.
func (bn *BlockNotifier) matchPortal(host string) (string, bool) {
	host = strings.ToLower(host)
	if bn.portals[host] {
		return host, true
	}
	for p := range bn.portals {
		if strings.HasSuffix(host, "."+p) {
			return p, true
		}
	}
	return "", false
}

func urlFromArgs(argsJSON string) string {
	var a struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)
	return a.URL
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// decodeDataURI extracts the raw bytes from a "data:<type>;base64,XXXX" URI;
// nil for anything else (absent screenshot, malformed, non-base64).
func decodeDataURI(s string) []byte {
	if s == "" || !strings.HasPrefix(s, "data:") {
		return nil
	}
	i := strings.Index(s, ",")
	if i < 0 || !strings.Contains(s[:i], ";base64") {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s[i+1:])
	if err != nil {
		return nil
	}
	return b
}

// --- metrics (lazy, default registerer — mirrors ratelimit.go) ---
var (
	blockNotifyMetricsOnce sync.Once
	blockNotifySentC       prometheus.Counter
	blockNotifyDroppedVec  *prometheus.CounterVec
	blockNotifyFailedC     prometheus.Counter
)

func initBlockNotifyMetrics() {
	blockNotifyMetricsOnce.Do(func() {
		blockNotifySentC = promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "scraper", Name: "block_notify_total",
			Help: "Operator Telegram notifications sent for a scraper solvable-block on a curated portal.",
		})
		blockNotifyDroppedVec = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "scraper", Name: "block_notify_dropped_total",
			Help: "Scraper block notifications dropped before delivery (labelled by reason, e.g. queue_full).",
		}, []string{"reason"})
		blockNotifyFailedC = promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "scraper", Name: "block_notify_failed_total",
			Help: "Scraper block notifications that reached the worker but failed to deliver to Telegram.",
		})
	})
}

func blockNotifySent() prometheus.Counter   { initBlockNotifyMetrics(); return blockNotifySentC }
func blockNotifyFailed() prometheus.Counter { initBlockNotifyMetrics(); return blockNotifyFailedC }
func blockNotifyDropped() prometheus.Counter {
	initBlockNotifyMetrics()
	return blockNotifyDroppedVec.WithLabelValues("queue_full")
}
