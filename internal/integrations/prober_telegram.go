package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/telegram"
)

// telegramProber is the Prober adapter around telegram.ProbeToken (design
// §5.3). It owns nothing but a DialGuard-wrapped HTTP client and a timeout
// — the credential-test logic itself lives in internal/telegram, beside
// the client that uses the credential in production.
type telegramProber struct {
	client  *http.Client
	timeout time.Duration
}

// newTelegramProber constructs the telegram Prober. client MUST already be
// guarded (see DialGuard.HTTPClient) — telegramProber does not construct
// its own client, so there is no un-guarded dial path through this type.
// In practice Telegram's endpoint is a fixed public host (api.telegram.org,
// not user-configurable via CandidateConfig), so the guard here is
// defense-in-depth consistency rather than a live SSRF vector for this
// specific kind — see the task report for detail.
func newTelegramProber(client *http.Client, timeout time.Duration) telegramProber {
	return telegramProber{client: client, timeout: probeTimeout(timeout)}
}

func (p telegramProber) Kind() string { return "telegram" }

func (p telegramProber) Probe(ctx context.Context, cand CandidateConfig) ProbeResult {
	start := time.Now()
	token := strings.TrimSpace(cand.Values["bot_token"])
	if token == "" {
		return ProbeResult{
			Kind:     "telegram",
			OK:       false,
			Outcome:  OutcomeFail,
			Summary:  "Bot token is required",
			Failures: []CheckFailure{{Field: "bot_token", Reason: "required"}},
			Latency:  time.Since(start),
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	username, err := telegram.ProbeToken(probeCtx, p.client, token)
	latency := time.Since(start)
	if err != nil {
		outcome, failures := classifyTelegramErr(err)
		return ProbeResult{
			Kind:     "telegram",
			OK:       false,
			Outcome:  outcome,
			Summary:  telegramFailureSummary(outcome),
			Detail:   redactSecrets(err.Error(), cand),
			Failures: failures,
			Latency:  latency,
		}
	}
	return ProbeResult{
		Kind:    "telegram",
		OK:      true,
		Outcome: OutcomeOK,
		Summary: fmt.Sprintf("Connected as @%s", username),
		Latency: latency,
	}
}

func telegramFailureSummary(outcome Outcome) string {
	if outcome == OutcomeFail {
		return "Telegram rejected this bot token"
	}
	return "Couldn't reach Telegram — try again"
}

// classifyTelegramErr maps a telegram.ProbeToken error to an Outcome
// (design §5.2 classification rule): 401/403 is the provider actively
// rejecting the credential (OutcomeFail); everything else — network
// failure, timeout, 429, 5xx, malformed body — is reachability signal,
// not proof of invalidity (OutcomeError).
func classifyTelegramErr(err error) (Outcome, []CheckFailure) {
	var probeErr *telegram.ProbeError
	if errors.As(err, &probeErr) {
		if probeErr.StatusCode == http.StatusUnauthorized || probeErr.StatusCode == http.StatusForbidden {
			return OutcomeFail, []CheckFailure{{Field: "bot_token", Reason: probeErr.Description}}
		}
	}
	return OutcomeError, nil
}
