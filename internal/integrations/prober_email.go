package integrations

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/email"
)

// emailProber is the Prober adapter combining the IMAP and SMTP legs
// (design §5.3): IMAP login is reused verbatim via email.ProbeIMAP (which
// wraps email.NewIMAPClient / emersionIMAPClient.Connect — the same
// adapter the production email channel uses); SMTP EHLO+STARTTLS+AUTH is
// the new email.ProbeSMTP.
//
// Email is the one kind whose candidate host is BOTH user-supplied AND
// reachable by a project-scoped (non-admin) user (design §6) — every dial
// here goes through the injected DialGuard, keyed per-leg to the
// respective host so an operator's AllowedHosts entry for the IMAP host
// doesn't accidentally also whitelist an unrelated SMTP host.
type emailProber struct {
	guard   DialGuard
	timeout time.Duration
}

func newEmailProber(guard DialGuard, timeout time.Duration) emailProber {
	return emailProber{guard: guard, timeout: probeTimeout(timeout)}
}

func (p emailProber) Kind() string { return "email" }

func (p emailProber) Probe(ctx context.Context, cand CandidateConfig) ProbeResult {
	start := time.Now()
	imapHost := strings.TrimSpace(cand.Values["imap_host"])
	smtpHost := strings.TrimSpace(cand.Values["smtp_host"])
	// Keys mirror the catalog's reconciled per-leg fields (task 5.2b):
	// imap_username/imap_password_env and smtp_username/smtp_password_env
	// name the PERSISTED config field (an env-var name at rest for the two
	// *_env ones), but at probe/candidate time — CandidateConfig's doc —
	// the value under each key is the literal the user typed, not an env
	// var name.
	imapUsername := cand.Values["imap_username"]
	imapPassword := cand.Values["imap_password_env"]
	smtpUsername := cand.Values["smtp_username"]
	smtpPassword := cand.Values["smtp_password_env"]

	var missing []CheckFailure
	if imapHost == "" {
		missing = append(missing, CheckFailure{Field: "imap_host", Reason: "required"})
	}
	if smtpHost == "" {
		missing = append(missing, CheckFailure{Field: "smtp_host", Reason: "required"})
	}
	if len(missing) > 0 {
		return ProbeResult{
			Kind:     "email",
			OK:       false,
			Outcome:  OutcomeFail,
			Summary:  "IMAP host and SMTP host are required",
			Failures: missing,
			Latency:  time.Since(start),
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var failures []CheckFailure
	sawAuthFailure := false
	sawConnectFailure := false

	imapCfg := email.IMAPDialConfig{
		Host:     imapHost,
		Port:     parsePortOrZero(cand.Values["imap_port"]),
		Username: imapUsername,
		Password: imapPassword,
		Dialer:   p.guard.Dialer(imapHost),
	}
	if err := email.ProbeIMAP(probeCtx, email.NewIMAPClient(), imapCfg); err != nil {
		if email.IsIMAPAuthFailure(err) {
			sawAuthFailure = true
			failures = append(failures, CheckFailure{Field: "imap_password_env", Reason: redactSecrets(err.Error(), cand)})
		} else {
			sawConnectFailure = true
			failures = append(failures, CheckFailure{Field: "imap_host", Reason: redactSecrets(err.Error(), cand)})
		}
	}

	smtpCfg := email.SMTPDialConfig{
		Host:     smtpHost,
		Port:     parsePortOrZero(cand.Values["smtp_port"]),
		Username: smtpUsername,
		Password: smtpPassword,
	}
	// DialGuard.DialContext already has the exact SMTPDialFunc shape and
	// re-derives the host from addr itself, so the guard's per-host check
	// applies to whatever host ProbeSMTP actually dials.
	if err := email.ProbeSMTP(probeCtx, email.SMTPDialFunc(p.guard.DialContext), smtpCfg); err != nil {
		if errors.Is(err, email.ErrSMTPAuthFailed) {
			sawAuthFailure = true
			failures = append(failures, CheckFailure{Field: "smtp_password_env", Reason: redactSecrets(err.Error(), cand)})
		} else {
			sawConnectFailure = true
			failures = append(failures, CheckFailure{Field: "smtp_host", Reason: redactSecrets(err.Error(), cand)})
		}
	}

	latency := time.Since(start)
	switch {
	case sawAuthFailure:
		return ProbeResult{Kind: "email", OK: false, Outcome: OutcomeFail, Summary: "Email provider rejected these credentials", Failures: failures, Latency: latency}
	case sawConnectFailure:
		return ProbeResult{Kind: "email", OK: false, Outcome: OutcomeError, Summary: "Couldn't reach the mail server — try again", Failures: failures, Latency: latency}
	default:
		return ProbeResult{Kind: "email", OK: true, Outcome: OutcomeOK, Summary: "IMAP and SMTP both connected", Latency: latency}
	}
}

// parsePortOrZero parses s as a port number; empty or unparseable input
// returns 0 (the callee's "use the protocol default" sentinel).
func parsePortOrZero(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
