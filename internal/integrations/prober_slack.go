package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/slack"
)

// slackProber is the Prober adapter around slack.ProbeToken (design §5.3).
type slackProber struct {
	client  *http.Client
	timeout time.Duration
}

// newSlackProber constructs the slack Prober. client MUST already be
// guarded (see DialGuard.HTTPClient). As with telegramProber, Slack's
// auth.test endpoint is a fixed public host, not user-configurable via
// CandidateConfig — the guard is defense-in-depth consistency here, not a
// live SSRF vector for this kind.
func newSlackProber(client *http.Client, timeout time.Duration) slackProber {
	return slackProber{client: client, timeout: probeTimeout(timeout)}
}

func (p slackProber) Kind() string { return "slack" }

func (p slackProber) Probe(ctx context.Context, cand CandidateConfig) ProbeResult {
	start := time.Now()
	// bot_token_env is the catalog Key (matching ProjectSlack.BotTokenEnv's
	// real yaml tag, task 5.2b); the candidate value under it is the
	// literal bot token the user typed, not an env-var name.
	token := strings.TrimSpace(cand.Values["bot_token_env"])
	if token == "" {
		return ProbeResult{
			Kind:     "slack",
			OK:       false,
			Outcome:  OutcomeFail,
			Summary:  "Bot token is required",
			Failures: []CheckFailure{{Field: "bot_token_env", Reason: "required"}},
			Latency:  time.Since(start),
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	team, user, err := slack.ProbeToken(probeCtx, p.client, token)
	latency := time.Since(start)
	if err != nil {
		outcome, failures := classifySlackErr(err)
		return ProbeResult{
			Kind:     "slack",
			OK:       false,
			Outcome:  outcome,
			Summary:  slackFailureSummary(outcome),
			Detail:   redactSecrets(err.Error(), cand),
			Failures: failures,
			Latency:  latency,
		}
	}
	return ProbeResult{
		Kind:    "slack",
		OK:      true,
		Outcome: OutcomeOK,
		Summary: fmt.Sprintf("Connected to %s as %s", team, user),
		Latency: latency,
	}
}

func slackFailureSummary(outcome Outcome) string {
	if outcome == OutcomeFail {
		return "Slack rejected this bot token"
	}
	return "Couldn't reach Slack — try again"
}

// classifySlackErr maps a slack.ProbeToken error to an Outcome: a known
// invalid-credential auth.test error code (slack.IsKnownInvalidAuth) is
// OutcomeFail; everything else (rate limit, 5xx, transport failure,
// malformed body, or an error code Slack hasn't documented) is
// OutcomeError — reachability signal, not proof of invalidity.
func classifySlackErr(err error) (Outcome, []CheckFailure) {
	var authErr *slack.AuthTestError
	if errors.As(err, &authErr) && slack.IsKnownInvalidAuth(authErr.Code) {
		return OutcomeFail, []CheckFailure{{Field: "bot_token", Reason: authErr.Code}}
	}
	return OutcomeError, nil
}
