package chat

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// retryAfterCeiling caps how long the helper will wait on a 429
// Retry-After hint before giving up and returning the response to
// the caller. A server can legitimately return "Retry-After: 3600"
// during a quota reset, but blocking the daemon's hot path for an
// hour would freeze every downstream consumer. Above the ceiling
// we surface the 429 immediately so the agent can re-queue or fail
// the task; a short hint (≤ ceiling) is the only case worth waiting
// out in-process.
const retryAfterCeiling = 30 * time.Second

// retryableHTTPDo performs an HTTP request with retry on transient
// server errors (5xx) and rate-limit responses (429). The buildReq
// closure is called on every attempt so the caller can rebuild the
// body reader — Go's http.Request consumes the body stream on Do(),
// so a naive retry loop that reuses the same *http.Request wouldn't
// work.
//
// Retry strategy (rate-limit hardening sub-item 8):
//   - 5xx — exponential backoff capped at ~8s.
//   - 429 — honour the response Retry-After HEADER (HTTP/1.1
//     §7.1.3) when present. Falls back to the server's
//     body-embedded retry hint via the optional bodyRetryAfter
//     parser. Falls back to generic exponential backoff when
//     neither hint is parseable. Hints above retryAfterCeiling
//     short-circuit (the caller would rather see the 429 than
//     freeze for an hour).
//   - 2xx / 3xx / other 4xx — return immediately.
//
// Caller is responsible for closing the returned response body on
// the success path. Failed attempts' bodies are drained and closed
// internally so the underlying connection can be reused.
//
// Optional knobs (set via the variadic options) keep the common-
// case call site identical to before — without options the helper
// behaves exactly as it did pre-sub-item-8 (5xx-only retry).
func retryableHTTPDo(
	ctx context.Context,
	client *http.Client,
	buildReq func() (*http.Request, error),
	maxAttempts int,
	baseDelay time.Duration,
	logger zerolog.Logger,
	opts ...retryOption,
) (*http.Response, error) {
	cfg := retryConfig{
		nowFn: time.Now,
		sleepFn: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	for _, o := range opts {
		o(&cfg)
	}

	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	delay := baseDelay
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := buildReq()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode == http.StatusTooManyRequests && cfg.retryOn429:
			// 429 with a parseable Retry-After hint: drain the body
			// so the connection can be reused, then sleep the hinted
			// duration before retrying. If the hint is missing /
			// unparseable / above the ceiling, surface as an error
			// — generic backoff against rate limits amplifies the
			// burst. When retryOn429 is OFF (default), 429 falls
			// through to the "default" branch below and is handed
			// back to the caller unchanged (legacy behaviour).
			body, _ := readAllCapped(resp.Body, 4096)
			headerHint := parseRetryAfterHeader(resp.Header.Get("Retry-After"), cfg.nowFn())
			_ = resp.Body.Close()

			wait, ok := pickRetryAfter(headerHint, body, cfg.bodyRetryAfter)
			if ok && wait > 0 && wait <= retryAfterCeiling && attempt != maxAttempts {
				logger.Warn().
					Int("attempt", attempt).
					Int("max_attempts", maxAttempts).
					Dur("retry_after", wait).
					Msg("http: 429 received, honouring Retry-After")
				if err := cfg.sleepFn(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
			// No usable Retry-After hint (missing / unparseable / above
			// the ceiling) or last attempt. When the caller opted into
			// generic 429 backoff (withGenericBackoffOn429 — used by the
			// OpenAI-compat client for upstreams like Vertex that 429
			// without a Retry-After header), treat it like a transient
			// 5xx and fall through to the exponential-backoff block
			// below. Otherwise surface immediately (the conservative
			// default: generic backoff against an explicit rate limit
			// can amplify a burst).
			if !cfg.backoff429 || attempt == maxAttempts || wait > retryAfterCeiling {
				if wait > retryAfterCeiling {
					logger.Warn().
						Dur("hint", wait).
						Dur("ceiling", retryAfterCeiling).
						Msg("http: 429 retry-after exceeds in-process ceiling; surfacing to caller")
				}
				return nil, &retryableHTTPError{
					StatusCode: http.StatusTooManyRequests,
					Body:       string(body),
					RetryAfter: wait,
				}
			}
			lastErr = &retryableHTTPError{StatusCode: http.StatusTooManyRequests, Body: string(body)}
		case resp.StatusCode >= 500 && !cfg.skip5xx:
			// Transient server error — drain body into lastErr for
			// diagnostic context, close, and retry.
			body, _ := readAllCapped(resp.Body, 4096)
			_ = resp.Body.Close()
			lastErr = &retryableHTTPError{
				StatusCode: resp.StatusCode,
				Body:       string(body),
			}
		default:
			// 2xx / 3xx / 4xx (non-429) — hand off to caller.
			return resp, nil
		}

		if attempt == maxAttempts {
			break
		}
		logger.Warn().
			Int("attempt", attempt).
			Int("max_attempts", maxAttempts).
			Dur("backoff", delay).
			Err(lastErr).
			Msg("http: transient error, retrying")

		if err := cfg.sleepFn(ctx, delay); err != nil {
			return nil, err
		}
		// Exponential backoff capped at ~8s so the tail of a max-
		// attempts loop doesn't drag on indefinitely on a long
		// outage — operators would rather see the failure quickly.
		delay *= 2
		if delay > 8*time.Second {
			delay = 8 * time.Second
		}
	}
	return nil, lastErr
}

// retryConfig is the optional-knob bag for retryableHTTPDo. Defaults
// preserve the pre-sub-item-8 behaviour (5xx-only retry, generic
// backoff, no clock injection) so existing call sites don't need to
// touch the signature.
type retryConfig struct {
	retryOn429     bool
	backoff429     bool
	skip5xx        bool
	bodyRetryAfter func([]byte) (time.Duration, bool)
	nowFn          func() time.Time
	sleepFn        func(context.Context, time.Duration) error
}

type retryOption func(*retryConfig)

// withRetryOn429 opts the call site into the 429 → Retry-After
// retry path. Off by default so existing 5xx-only loops stay
// behavior-stable — the chat-router subscription clients turn this
// on; legacy bedrock retries leave it off.
func withRetryOn429(parser func([]byte) (time.Duration, bool)) retryOption {
	return func(c *retryConfig) {
		c.retryOn429 = true
		c.bodyRetryAfter = parser
	}
}

// withGenericBackoffOn429 makes a 429 WITHOUT a usable Retry-After hint
// retry on the same bounded exponential backoff as a 5xx, rather than
// surfacing immediately. Requires withRetryOn429 to also be set. Used by
// the OpenAI-compat client because some upstreams (notably Vertex,
// RESOURCE_EXHAUSTED) 429 without a Retry-After header, so the
// hint-only path would never retry them. Off by default so the
// subscription clients keep their burst-safe hint-only behaviour.
func withGenericBackoffOn429() retryOption {
	return func(c *retryConfig) {
		c.backoff429 = true
	}
}

// withNo5xxRetry disables the default 5xx retry, handing 5xx responses
// straight back to the caller. The OpenAI-compat client sets this so a
// 5xx still surfaces as a GatewayError for the dispatcher's
// context-pruning prune-and-retry (which a blind client-level retry of
// the same bloated history would defeat) — the client only adds the
// 429 retry that the dispatcher's prune can't help with.
func withNo5xxRetry() retryOption {
	return func(c *retryConfig) {
		c.skip5xx = true
	}
}

// withRetryClock injects a clock (now + sleep) so tests can advance
// time without actually sleeping. Production callers don't set this.
func withRetryClock(nowFn func() time.Time, sleepFn func(context.Context, time.Duration) error) retryOption {
	return func(c *retryConfig) {
		if nowFn != nil {
			c.nowFn = nowFn
		}
		if sleepFn != nil {
			c.sleepFn = sleepFn
		}
	}
}

// parseRetryAfterHeader parses the HTTP/1.1 Retry-After header per
// RFC 7231 §7.1.3: either a positive integer seconds count, or an
// HTTP-date in the form "Wed, 21 Oct 2015 07:28:00 GMT". Returns
// 0 + ok=false when the header is empty / unparseable / non-positive.
//
// The `now` parameter is the reference time for the HTTP-date form;
// production passes time.Now(), tests pass a pinned instant. We
// allow only a small set of canonical date formats to keep the
// surface tight — pathological servers that return non-RFC dates
// fall back to body / backoff.
func parseRetryAfterHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, value); err == nil {
			d := t.Sub(now)
			if d <= 0 {
				return 0
			}
			return d
		}
	}
	return 0
}

// pickRetryAfter chooses the retry-after duration following the
// sub-item 8 precedence: header → body → none. Returns (0, false)
// when no hint is available so the caller can short-circuit instead
// of waiting indefinitely.
func pickRetryAfter(headerHint time.Duration, body []byte, bodyParser func([]byte) (time.Duration, bool)) (time.Duration, bool) {
	if headerHint > 0 {
		return headerHint, true
	}
	if bodyParser != nil {
		if d, ok := bodyParser(body); ok && d > 0 {
			return d, true
		}
	}
	return 0, false
}

// retryableHTTPError is the error type surfaced when all retries
// exhaust a 5xx response, OR when a 429 response carries a
// Retry-After hint we honoured but the retry budget was already
// spent. Its shape mirrors the inline formatting the clients used
// before the helper so log messages and user-facing errors stay
// grep-able. RetryAfter is non-zero only on the 429 surface — it's
// the operator-readable hint clients can re-surface to their caller
// (the agent's MCP bridge already uses this for queue routing).
type retryableHTTPError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *retryableHTTPError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("HTTP %d (retry-after %s): %s", e.StatusCode, e.RetryAfter, e.Body)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}
