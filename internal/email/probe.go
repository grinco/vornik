package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// ProbeIMAP validates candidate IMAP credentials by dialling and logging in
// — WITHOUT persisting anything — via the given IMAPClient (production
// callers pass NewIMAPClient(), same adapter the email channel uses, so the
// probe exercises the exact code path a real connection would). Always
// closes the client before returning, on both success and failure.
//
// SSRF hardening (design §6): the injected client is expected to carry a
// guarded Dialer on cfg (IMAPDialConfig.Dialer) — ProbeIMAP itself does not
// construct one, so the caller (internal/integrations) is responsible for
// setting cfg.Dialer to a DialGuard-backed *net.Dialer before calling.
func ProbeIMAP(ctx context.Context, client IMAPClient, cfg IMAPDialConfig) error {
	err := client.Connect(ctx, cfg)
	_ = client.Close()
	return err
}

// IsIMAPAuthFailure reports whether err came from the LOGIN step (bad
// credentials) as opposed to the dial or SELECT step. emersionIMAPClient.
// Connect (imap_emersion.go) wraps a login rejection distinctly
// ("IMAP login: %w") from a dial failure ("dial TLS %s: %w") or a mailbox
// SELECT failure ("IMAP select %q: %w") — there is no typed sentinel for
// this in the email package today, so classification is by that stable
// wrap-prefix. Used by the integrations emailProber to classify
// OutcomeFail (bad credentials) versus OutcomeError (unreachable host, bad
// mailbox name, timeout).
func IsIMAPAuthFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "IMAP login:")
}

// SMTPDialConfig is ProbeSMTP's candidate parameter bundle — separate from
// IMAPDialConfig since the two protocols' credentials, while often the same
// mailbox in practice, are configured as independent CredentialFields in
// the integrations catalog (a self-hosted relay may use a different
// account than the IMAP mailbox).
type SMTPDialConfig struct {
	Host     string
	Port     int
	Username string
	Password string
}

// SMTPDialFunc is the seam ProbeSMTP dials through. Production callers pass
// a DialGuard-wrapped dial function (design §6) — net/smtp has no
// DialFunc/net.Dialer injection point of its own (smtp.Dial always uses
// net.Dial internally), so ProbeSMTP dials itself and hands the resulting
// net.Conn to smtp.NewClient, which IS a public seam.
type SMTPDialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ErrSMTPAuthFailed is the sentinel ProbeSMTP wraps into its returned error
// when the server rejects AUTH — as opposed to a dial/EHLO/STARTTLS
// failure, which is a reachability problem, not a credential problem.
var ErrSMTPAuthFailed = errors.New("email: SMTP authentication failed")

// smtpDefaultPort is used when cfg.Port is unset (0). 587 (STARTTLS
// submission) is the modern default; 25/465 are configured explicitly via
// cfg.Port when a provider needs them.
const smtpDefaultPort = 587

// ProbeSMTP validates candidate SMTP credentials with EHLO, then STARTTLS
// if the server advertises it, then AUTH if a username is supplied —
// WITHOUT sending any mail. Returns an error wrapping ErrSMTPAuthFailed on
// an AUTH rejection; any other error (dial, EHLO, STARTTLS) is a
// reachability failure.
func ProbeSMTP(ctx context.Context, dial SMTPDialFunc, cfg SMTPDialConfig) error {
	port := cfg.Port
	if port == 0 {
		port = smtpDefaultPort
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)

	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Hello("vornik-probe"); err != nil {
		return fmt.Errorf("smtp: EHLO: %w", err)
	}

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("smtp: STARTTLS: %w", err)
		}
	}

	if strings.TrimSpace(cfg.Username) == "" {
		return nil
	}
	if ok, _ := client.Extension("AUTH"); !ok {
		// No AUTH advertised — nothing more to validate; a username was
		// supplied but the server doesn't support authenticating it here.
		return nil
	}
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("%w: %v", ErrSMTPAuthFailed, err)
	}
	return nil
}
