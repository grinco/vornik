package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// verifySignature validates the X-Slack-Signature header against the
// configured signing secret using Slack's documented wire shape:
//
//	v0:<timestamp>:<body>  →  HMAC-SHA256 → hex →  prefixed "v0="
//
// Replay defence: Slack stamps every request with
// X-Slack-Request-Timestamp; deliveries older than maxReplayWindow (5
// minutes) are rejected even when the HMAC verifies. This stops an
// attacker who captured an earlier signed delivery from replaying it
// later.
//
// Returns nil on success; a descriptive error on any failure mode
// (missing headers, malformed timestamp, replay window exceeded,
// signature mismatch). HandleWebhook maps any non-nil return to HTTP
// 401 so the caller doesn't have to branch on the failure shape.
//
// now is supplied as a parameter (rather than read off c.clock) so
// callers in HandleWebhook can pin a single timestamp across the
// "verify + parse + dispatch" pipeline — the replay-window check
// must use the same wall clock as the rest of the request handling.
func (c *Channel) verifySignature(r *http.Request, body []byte, now time.Time) error {
	tsHeader := strings.TrimSpace(r.Header.Get("X-Slack-Request-Timestamp"))
	if tsHeader == "" {
		return errors.New("missing X-Slack-Request-Timestamp")
	}
	tsUnix, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return fmt.Errorf("malformed X-Slack-Request-Timestamp: %w", err)
	}
	delivered := time.Unix(tsUnix, 0)
	if delivered.After(now) {
		// Allow forward skew up to the same window — Slack's own
		// example does this so an NTP-skewed daemon doesn't reject
		// every delivery.
		if delivered.Sub(now) > maxReplayWindow {
			return fmt.Errorf("timestamp %d is %s in the future (> %s replay window)",
				tsUnix, delivered.Sub(now), maxReplayWindow)
		}
	} else if now.Sub(delivered) > maxReplayWindow {
		return fmt.Errorf("timestamp %d is %s old (> %s replay window)",
			tsUnix, now.Sub(delivered), maxReplayWindow)
	}

	sigHeader := strings.TrimSpace(r.Header.Get("X-Slack-Signature"))
	if sigHeader == "" {
		return errors.New("missing X-Slack-Signature")
	}
	const prefix = "v0="
	if !strings.HasPrefix(sigHeader, prefix) {
		return fmt.Errorf("malformed X-Slack-Signature: missing %q prefix", prefix)
	}
	gotHex := sigHeader[len(prefix):]
	gotBytes, err := hex.DecodeString(gotHex)
	if err != nil {
		return fmt.Errorf("malformed X-Slack-Signature hex: %w", err)
	}
	want := computeSlackHMAC(c.signingSecretBytes, tsHeader, body)
	if !hmac.Equal(gotBytes, want) {
		return errors.New("signature mismatch")
	}
	return nil
}

// computeSlackHMAC produces the HMAC-SHA256 of `v0:<ts>:<body>` with
// the supplied secret. Exposed at package scope (lowercase) so tests
// can stamp valid signatures without re-implementing the wire shape.
func computeSlackHMAC(secret []byte, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("v0:"))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte(":"))
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

// errURLVerification is returned by the URL-verification path of
// HandleWebhook so the caller knows to short-circuit the dispatch
// pipeline and write the challenge body. It's an internal sentinel,
// not exported.
var errURLVerification = errors.New("slack: url_verification handshake — short-circuit dispatch")
