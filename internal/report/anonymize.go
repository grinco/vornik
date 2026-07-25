// Package report builds a PUBLIC-safe, anonymized problem-report body and a
// prefilled grinco/vornik GitHub issue URL for `vornikctl report` (design
// https://docs.vornik.io). Because the body is
// posted to a PUBLIC repo, it is anonymized in TWO tiers: secret redaction
// (internal/secrets) on every free-text field PLUS a stricter public scrubber
// that strips emails, home paths, LAN IPs, and the machine hostname. The body is
// built from a FIXED template that never interpolates project/swarm identifiers;
// every interpolated value passes through the scrubber (single choke point).
// Fail-closed: a scrubber-construction error yields a static error and no body.
package report

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/secrets"
)

const (
	// IssueRepo is the public CE repo problem reports are filed against.
	IssueRepo = "grinco/vornik"
	// defaultLabel is applied to `vornikctl report` issues.
	defaultLabel = "bug"
	// urlMaxBytes is the practical GitHub prefilled-issue URL cap; GitHub
	// silently drops overlong URLs, so we truncate the (already-anonymized) body.
	urlMaxBytes = 8000
)

// errAnonymize is the STATIC fail-closed error — it never wraps the offending
// value (review-20260725-a9da #8).
var errAnonymize = errors.New("anonymization failed — cannot proceed")

// Check is a doctor check summary (subset of the CLI's doctorCheck).
type Check struct{ Name, Status, Message string }

// BodyInput is everything the public issue body is built from.
type BodyInput struct {
	Version, Edition, OS, Arch string
	Hostname                   string // os.Hostname() — redacted literally from the body
	DaemonUp                   bool
	Checks                     []Check
	Symptom                    string // the user's --summary free text (scrubbed too, #2)
}

var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// reHome collapses a home/root/tilde-anchored path AND ITS TAIL to <path>.
	// Keeping the tail leaks project names (~/projects/<proj>/…, review-20260725-7530
	// #1/#2), so the whole path — including a leading `~` form — is redacted.
	reHome = regexp.MustCompile(`(?:(?:/var)?/home/[^/\s]+|/Users/[^/\s]+|/root|~)(?:/[^\s,;)]*)?`)
	// reIPv4 redacts ANY IPv4 (not just RFC1918): a public orchestration endpoint
	// in a doctor message is a linkable identifier on a PUBLIC issue (#4).
	reIPv4 = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// reIPv6Token splits text into whitespace/punctuation-delimited TOKENS,
	// each of which is then validated as a whole by redactIPv6Token.
	//
	// A substring regex cannot do this job: hex digits overlap with ordinary
	// letters, so a pattern like `(?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4}`
	// matches INSIDE ordinary text that merely contains "::" — and net.ParseIP
	// accepts the fragment it grabs, because "::", "d::" and "e::f" are all
	// valid IPv6 literals. That turned `core::fmt::Debug` into
	// `cor<ip>mt<ip>ug` and `"created_at"::text` into `"created_at"<ip>text`
	// in public issue bodies (audit 2026-07-25 follow-up A19).
	reIPv6Token = regexp.MustCompile("[^\\s,;\"'`(){}<>=|]+")
	// reJWT / reKeyShapes are high-precision key-SHAPE nets applied on top of
	// secrets.Redact. The default detector's sk-/entropy rules are calibrated for
	// REAL (long) keys — a bare key shape shorter than the 40-char entropy floor
	// and without `key=` assignment context slips through (found live 2026-07-25).
	// The installer scrubber already catches these (sk- 12+, AKIA, JWT); mirror it
	// so daemon-up reporting is no weaker than the install-time path.
	reJWT       = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`)
	reKeyShapes = regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]{12,}|sk-[A-Za-z0-9_-]{12,}|AKIA[0-9A-Z]{12,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,})`)
)

// newDetector is a package var so tests can force the fail-closed path.
var newDetector = func() (*secrets.MultiDetector, error) {
	return secrets.NewMultiDetector(secrets.Config{})
}

// scrubber returns a public-safe scrubber closure (secret redaction + email /
// home-path / LAN-IP / hostname stripping), or an error if the secret detector
// can't be built.
// isIPv6Literal reports whether s is a parseable IPv6 address. The colon check
// excludes IPv4 (reIPv4 already handled those) and the hex-digit check excludes
// the degenerate "::" — a valid unspecified address, but in prose it is far more
// likely to be a scope operator (Haskell/Rust/C++/SQL cast) than a host.
func isIPv6Literal(s string) bool {
	if !strings.Contains(s, ":") || !strings.ContainsAny(s, "0123456789abcdefABCDEF") {
		return false
	}
	return net.ParseIP(s) != nil
}

// redactIPv6Token replaces the IPv6 literal in a single delimited token with
// <ip>, preserving the surrounding syntax (brackets, port, zone) so the message
// stays readable. Tokens that are not IPv6 addresses are returned untouched.
func redactIPv6Token(tok string) string {
	// Bracketed authority form, optionally with a port: [2001:db8::1]:8080
	if strings.HasPrefix(tok, "[") {
		if end := strings.Index(tok, "]"); end > 1 && isIPv6Literal(tok[1:end]) {
			return "[<ip>]" + tok[end+1:]
		}
	}
	// Trailing sentence punctuation is not part of the address.
	core := strings.TrimRight(tok, ".,;:!?)]")
	trailer := tok[len(core):]
	// Zone identifier: fe80::1%eth0 — keep the interface, drop the address.
	zone := ""
	if i := strings.Index(core, "%"); i >= 0 {
		zone, core = core[i:], core[:i]
	}
	if isIPv6Literal(core) {
		return "<ip>" + zone + trailer
	}
	return tok
}

func scrubber(hostname string) (func(string) string, error) {
	det, err := newDetector()
	if err != nil || det == nil {
		return nil, errAnonymize
	}
	host := strings.TrimSpace(hostname)
	var hostPattern *regexp.Regexp
	if host != "" {
		hostPattern = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(host))
	}
	return func(s string) string {
		s = string(secrets.Redact([]byte(s), det.Scan([]byte(s))))
		// Belt-and-suspenders key-shape nets (see reKeyShapes) — before the
		// path/email passes so a token that happens to embed `/` or `@` is gone.
		s = reJWT.ReplaceAllString(s, "<redacted-jwt>")
		s = reKeyShapes.ReplaceAllString(s, "<redacted-key>")
		s = reEmail.ReplaceAllString(s, "<email>")
		s = reHome.ReplaceAllString(s, "<path>")
		s = reIPv4.ReplaceAllString(s, "<ip>")
		s = reIPv6Token.ReplaceAllStringFunc(s, redactIPv6Token)
		if hostPattern != nil {
			s = hostPattern.ReplaceAllString(s, "<host>")
		}
		// Never let a stray newline/backtick break the markdown table/list.
		s = strings.ReplaceAll(s, "\r", "")
		return s
	}, nil
}

// AnonymizeBody builds the PUBLIC-safe markdown issue body. Every interpolated
// value goes through the scrubber; the template interpolates no project/swarm
// identifiers of its own. Returns errAnonymize (static) if the scrubber can't be
// built (fail-closed).
func AnonymizeBody(in BodyInput) (string, error) {
	scrub, err := scrubber(in.Hostname)
	if err != nil {
		return "", errAnonymize
	}
	inline := func(s string) string { return strings.ReplaceAll(scrub(s), "\n", " ") }

	var b strings.Builder
	b.WriteString("### vornik problem report\n\n")
	fmt.Fprintf(&b, "- **version:** %s\n", inline(in.Version))
	fmt.Fprintf(&b, "- **edition:** %s\n", inline(in.Edition))
	fmt.Fprintf(&b, "- **platform:** %s/%s\n", inline(in.OS), inline(in.Arch))
	fmt.Fprintf(&b, "- **daemon:** %s\n\n", upDown(in.DaemonUp))

	if s := strings.TrimSpace(in.Symptom); s != "" {
		fmt.Fprintf(&b, "**Symptom**\n\n%s\n\n", scrub(s))
	}
	if len(in.Checks) > 0 {
		b.WriteString("**Doctor findings**\n\n")
		for _, c := range in.Checks {
			line := fmt.Sprintf("- `%s` = %s", inline(c.Name), inline(c.Status))
			if m := strings.TrimSpace(c.Message); m != "" {
				line += " — " + inline(m)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("_Filed via `vornikctl report`. Diagnostics anonymized; a redacted bundle may be attached separately._\n")
	return b.String(), nil
}

func upDown(up bool) string {
	if up {
		return "running"
	}
	return "down (offline diagnostics)"
}

// IssueURL builds the prefilled grinco/vornik issue URL from an ALREADY-anonymized
// body (design D4/#5: anonymize → truncate → note → encode). If the encoded URL
// exceeds urlMaxBytes it truncates the body (safe — the body is already scrubbed)
// and appends an attach-note.
func IssueURL(title, body string) string {
	base := "https://github.com/" + IssueRepo + "/issues/new"
	enc := func(t, bd string) string {
		q := url.Values{}
		q.Set("title", t)
		q.Set("body", bd)
		q.Set("labels", defaultLabel)
		return base + "?" + q.Encode()
	}
	if u := enc(title, body); len(u) <= urlMaxBytes {
		return u
	}
	const note = "\n\n_(diagnostics truncated — attach the redacted bundle for the full context)_"
	trimmed := body
	for len(trimmed) > 0 {
		trimmed = trimmed[:len(trimmed)*9/10]
		if u := enc(title, trimmed+note); len(u) <= urlMaxBytes {
			return u
		}
	}
	return enc(title, strings.TrimSpace(note))
}
