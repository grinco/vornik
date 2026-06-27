package a2a

// A2A push notifications (pushNotificationConfig). When a caller submits a
// task with a webhook url, the daemon POSTs task-state updates to that url so
// a caller NOT holding an open SSE stream still learns when the task needs
// steering (input-required) or finishes (completed/failed/canceled).
//
// Integration: PushNotifier implements the executor's CompletionNotifier
// (terminal states) and SteeringNotifier (AWAITING_INPUT/APPROVAL) by
// structural typing — the service container wires it into the same
// multiplexers as the chat/DM notifiers. The task is in hand at each hook, so
// no execution→task mapping or live-stream subscription is needed. A task
// with no stored config (the common, non-A2A case) is a fast skip.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// isBlockedPushIP reports whether an IP is a non-routable / internal target a
// webhook must never reach — the SSRF block set (loopback, link-local,
// unspecified, RFC-1918 private, and multicast). Used both at URL-validation
// time (literal-IP hosts) AND at connect time (the dialer Control hook), so a
// hostname that resolves to an internal address — DNS rebinding — is still
// refused at the moment of connect.
// blockPushIP is the connect-time predicate the dialer uses. It points at
// isBlockedPushIP in production; tests override it to reach loopback httptest
// servers (and one test leaves it at the default to prove the guard fires).
var blockPushIP = isBlockedPushIP

func isBlockedPushIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		ip.IsPrivate()
}

// PushConfigGetter is the narrow read the pusher needs.
type PushConfigGetter interface {
	Get(ctx context.Context, taskID string) (*persistence.A2APushConfig, error)
}

// PushNotifier POSTs A2A task-state updates to per-task webhook urls.
type PushNotifier struct {
	repo   PushConfigGetter
	client *http.Client
	logger zerolog.Logger
}

// NewPushNotifier builds a pusher. A nil repo yields a no-op notifier.
//
// The HTTP client is SSRF-hardened beyond the URL validation done at config
// time: a dialer Control hook re-checks the ACTUAL resolved IP on every
// connect (so a hostname that resolves to an internal address — DNS
// rebinding, or a TTL flip between validate and send — is refused), and
// redirects are rejected outright (so a 302 to an internal URL can't smuggle
// the request past the guard).
func NewPushNotifier(repo PushConfigGetter, logger zerolog.Logger) *PushNotifier {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if blockPushIP(net.ParseIP(host)) {
			return fmt.Errorf("a2a push: refusing to connect to non-public address %q", address)
		}
		return nil
	}
	return &PushNotifier{
		repo: repo,
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{DialContext: dialer.DialContext},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("a2a push: redirects are not allowed")
			},
		},
		logger: logger,
	}
}

// pushEnvelope is the JSON body POSTed to the caller's webhook. Shape mirrors
// the SSE status frame so a client can handle both surfaces uniformly.
type pushEnvelope struct {
	TaskID string `json:"taskId"`
	Status struct {
		State string `json:"state"`
		Final bool   `json:"final"`
	} `json:"status"`
}

// NotifyTaskCompleted (executor.CompletionNotifier) → POST a terminal state.
func (p *PushNotifier) NotifyTaskCompleted(ctx context.Context, task *persistence.Task, success bool, message string) {
	if task == nil {
		return
	}
	state := "completed"
	if !success {
		state = "failed"
		if strings.Contains(strings.ToLower(message), "cancel") {
			state = "canceled"
		}
	}
	p.push(ctx, task, state, true)
}

// NotifySteeringRequired (executor.SteeringNotifier) → POST input-required.
func (p *PushNotifier) NotifySteeringRequired(ctx context.Context, task *persistence.Task, _ string) {
	if task == nil {
		return
	}
	// Both AWAITING_INPUT and AWAITING_APPROVAL map to the A2A
	// "input-required" state — the task is blocked on the caller either way.
	p.push(ctx, task, "input-required", false)
}

func (p *PushNotifier) push(ctx context.Context, task *persistence.Task, state string, final bool) {
	if p == nil || p.repo == nil {
		return
	}
	cfg, err := p.repo.Get(ctx, task.ID)
	if err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			p.logger.Debug().Err(err).Str("task_id", task.ID).Msg("a2a push: config lookup failed; skipping")
		}
		return // no webhook configured (the common case) → nothing to push
	}
	if cfg == nil || cfg.URL == "" {
		return
	}

	var env pushEnvelope
	env.TaskID = task.ID
	env.Status.State = state
	env.Status.Final = final
	body, _ := json.Marshal(env)

	// One retry on transient failure; best-effort + non-fatal throughout.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
		if rerr != nil {
			lastErr = rerr
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "vornik-a2a-push/1")
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		resp, derr := p.client.Do(req)
		if derr != nil {
			lastErr = derr
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.logger.Info().Str("task_id", task.ID).Str("state", state).Int("status", resp.StatusCode).
				Msg("a2a push: delivered task-state webhook")
			return
		}
		lastErr = fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	p.logger.Warn().Err(lastErr).Str("task_id", task.ID).Str("state", state).
		Msg("a2a push: webhook delivery failed")
}

// ValidateWebhookURL guards the caller-supplied push url at SET time: a
// parseable http/https absolute URL, no localhost, and — for a literal-IP
// host — not an internal/non-routable address. This is the fast, early reject
// of obvious SSRF targets. It is NOT the only defense: a hostname that
// resolves to an internal address (DNS rebinding) is caught at connect time
// by the client's dialer Control hook (see NewPushNotifier), which re-checks
// the actual IP on every dial, and redirects are refused. Validation alone
// can't be rebinding-proof, so the connect-time check is the authority.
func ValidateWebhookURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("push url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("push url is not parseable: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("push url scheme must be http or https, got %q", u.Scheme)
	}
	// Normalize before the localhost compare so "Localhost", "LOCALHOST.",
	// and a trailing-dot FQDN can't slip past the literal check.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("push url has no host")
	}
	if host == "localhost" {
		return fmt.Errorf("push url host %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedPushIP(ip) {
		return fmt.Errorf("push url host %q is a non-routable/internal address", host)
	}
	// Alternate IPv4 encodings (e.g. 0x7f000001, 2130706433, 0177.0.0.1) and
	// hostnames that resolve to internal addresses (DNS rebinding) are NOT
	// rejected here — net.ParseIP returns nil for them and a set-time DNS
	// lookup is racy against rebinding. They are caught at CONNECT time by the
	// dialer Control hook (NewPushNotifier), which inspects the actual
	// resolved IP on every dial. That connect-time check is the authority.
	return nil
}
