package email

import (
	"context"
	"fmt"
	"strings"
)

// SignatureVerifier is the inbound-authentication seam. Slice 1
// shipped NoopSignatureVerifier (every message passes). Slice 3
// adds HeaderAuthVerifier — a no-DNS, no-crypto verifier that
// consults the Authentication-Results / Received-SPF headers
// stamped by upstream MTAs (Gmail, Postfix with rspamd, AWS SES,
// Mailgun, …). Operators who terminate SMTP behind a mature relay
// get real SPF + DKIM enforcement without vornik having to do the
// cryptographic work itself.
//
// Implementations needing extra context (DNS resolver, trust
// anchors, allow-soft-fail toggle) keep that state on the struct;
// the interface stays at one method so future implementors (e.g.
// in-process DKIM crypto, ARC chain inspection) don't have to
// re-shape the channel.
type SignatureVerifier interface {
	// Verify inspects the parsed message and returns nil when the
	// message passes the verifier's policy, or an error describing
	// the rejection. The channel logs the error and drops the
	// message — there's no soft-pass path.
	Verify(ctx context.Context, msg ParsedMessage) error
}

// NoopSignatureVerifier is the default SignatureVerifier — every
// message passes. Constructed automatically by New() when
// Config.SignatureVerifier is nil so the channel works out of the
// box; operators who want real SPF/DKIM verification supply their
// own implementation (or wire HeaderAuthVerifier below).
//
// Holding the no-op as a named type (rather than a closure or a
// bare func) lets future telemetry pull "verifier kind" off the
// channel's Config for the operator-facing audit panel.
type NoopSignatureVerifier struct{}

// Verify always succeeds. The empty implementation is intentional —
// the slice-1 contract is "channel works without DKIM/SPF set up."
func (NoopSignatureVerifier) Verify(_ context.Context, _ ParsedMessage) error {
	return nil
}

// AuthPolicy controls how strict HeaderAuthVerifier is about
// upstream-stamped verdicts. Two settings:
//
//   - AuthPolicyRelaxed: reject only when the upstream relay
//     explicitly stamped a failure (spf=fail or dkim=fail) for
//     this sender. Messages without any auth header at all are
//     admitted — common for internal SMTP forwarding that strips
//     headers.
//
//   - AuthPolicyStrict: require at least one explicit pass and
//     reject anything else. Use only when every legitimate sender
//     traverses a header-stamping relay.
//
// Implementation-wise the two share the same parser; only the
// final verdict differs.
type AuthPolicy int

const (
	// AuthPolicyRelaxed lets unauthenticated mail through; only an
	// explicit upstream-stamped failure rejects.
	AuthPolicyRelaxed AuthPolicy = iota
	// AuthPolicyStrict requires explicit upstream-stamped pass on
	// SPF or DKIM; everything else (no headers, neutral, none,
	// fail) rejects.
	AuthPolicyStrict
)

// HeaderAuthVerifier implements SignatureVerifier by parsing
// Authentication-Results and Received-SPF headers stamped by
// upstream MTAs. Zero allocations on the happy path, no DNS
// lookups, no crypto. Pairs with the operator-facing
// `verify_inbound_auth` + `auth_policy` ProjectEmail config knobs
// (see resolveEmailConfig).
//
// What it does NOT do:
//   - Verify DKIM signatures cryptographically — the upstream
//     relay does that and stamps the verdict; we trust it. If
//     vornik ever runs on a bare mail VM with no SMTP termination
//     in front, a separate verifier (e.g. github.com/emersion/
//     go-msgauth/dkim) can implement SignatureVerifier directly.
//   - Verify ARC chains. Slice-3 scope is the bottom-line "did
//     SPF+DKIM say pass" question.
type HeaderAuthVerifier struct {
	// Policy selects relaxed vs strict matching; see AuthPolicy.
	Policy AuthPolicy

	// TrustedServerIDs is the set of authserv-ids whose
	// Authentication-Results headers are honoured (RFC 8601 §5). When
	// non-empty, an A-R header is only consulted if its leading
	// authserv-id (case-insensitive) is in this set, and Received-SPF
	// "pass"/soft verdicts are ignored entirely — only an explicit
	// Received-SPF "fail" is still honoured (fail-closed). This closes
	// the spoof where an attacker embeds a forged
	// `Authentication-Results: <anything>; dkim=pass` (or
	// `Received-SPF: pass`) inside the message they send to satisfy
	// strict policy (bug sweep 2026-06-04).
	//
	// When empty, every A-R / Received-SPF header is consulted
	// (legacy behaviour) — operators terminating SMTP behind a single
	// trusted relay should set this to that relay's authserv-id.
	TrustedServerIDs []string
}

// authServIDTrusted reports whether authservID matches one of the
// configured trusted servers (case-insensitive, whitespace-trimmed).
// An empty trusted set means "no filtering configured" and is handled
// by the caller, not here.
func (v HeaderAuthVerifier) authServIDTrusted(authservID string) bool {
	id := strings.ToLower(strings.TrimSpace(authservID))
	for _, t := range v.TrustedServerIDs {
		if strings.ToLower(strings.TrimSpace(t)) == id {
			return true
		}
	}
	return false
}

// Verify implements SignatureVerifier. Walks every
// Authentication-Results and Received-SPF header on the message,
// extracts the per-method verdict, then applies Policy:
//
//   - Any explicit "fail" verdict on SPF or DKIM rejects under
//     both policies (legitimate mail never produces an explicit
//     fail at the relay).
//   - At least one explicit "pass" is required under strict.
//   - Relaxed admits messages with no SPF/DKIM verdicts at all
//     (e.g. relay didn't stamp; internal forwarder; sender that
//     genuinely has no auth set up).
func (v HeaderAuthVerifier) Verify(_ context.Context, msg ParsedMessage) error {
	spf, dkim := v.summariseAuth(msg)
	// Hard reject on explicit fail regardless of policy — a relay
	// that stamped "fail" is telling us the sender's claimed
	// identity didn't authenticate, and admitting that mail
	// neutralises the verifier's whole purpose.
	if spf == authFail {
		return fmt.Errorf("inbound email: SPF fail (%s)", reasonOrEmpty(msg))
	}
	if dkim == authFail {
		return fmt.Errorf("inbound email: DKIM fail (%s)", reasonOrEmpty(msg))
	}
	if v.Policy == AuthPolicyStrict {
		if spf != authPass && dkim != authPass {
			return fmt.Errorf("inbound email: strict policy requires SPF or DKIM pass; got spf=%s dkim=%s", spf, dkim)
		}
	}
	return nil
}

// authVerdict is the parsed per-method outcome the relay stamped.
// The string form is what the spec uses (RFC 8601 §2.7.1 for SPF
// and §2.7.2 for DKIM) so we can pass them straight into log /
// error messages.
type authVerdict string

const (
	authNone     authVerdict = "none"      // no header stamped or empty result
	authPass     authVerdict = "pass"      // method explicitly passed
	authFail     authVerdict = "fail"      // method explicitly failed
	authNeutral  authVerdict = "neutral"   // SPF "neutral" / DKIM "neutral"
	authSoftFail authVerdict = "softfail"  // SPF "softfail"
	authTempFail authVerdict = "temperror" // SPF/DKIM "temperror"
	authPermFail authVerdict = "permerror" // SPF/DKIM "permerror"
	authPolicy   authVerdict = "policy"    // SPF "policy" (rfc7372)
)

// summariseAuth returns the highest-confidence SPF and DKIM verdicts
// found across every Authentication-Results header AND every
// Received-SPF header on the message. Explicit fail always wins:
// Verify's contract is fail-closed on relay-stamped authentication
// failures, so a later pass must not mask a fail in the same chain.
// After fail, pass outranks soft/neutral/temporary outcomes so strict
// mode still admits mail when at least one trusted relay stamped pass.
func (v HeaderAuthVerifier) summariseAuth(msg ParsedMessage) (spf, dkim authVerdict) {
	spf, dkim = authNone, authNone
	filtering := len(v.TrustedServerIDs) > 0
	for _, hdr := range msg.AuthResults {
		authservID, methods := parseAuthResultsHeader(hdr)
		// RFC 8601 §5: only honour A-R headers stamped by a trusted
		// authserv-id. An attacker-injected header carries some other
		// (or empty) authserv-id and is ignored when filtering is on.
		if filtering && !v.authServIDTrusted(authservID) {
			continue
		}
		for _, m := range methods {
			switch strings.ToLower(m.Method) {
			case "spf":
				spf = mergeVerdict(spf, m.Result)
			case "dkim":
				dkim = mergeVerdict(dkim, m.Result)
			}
		}
	}
	// Received-SPF: the result is the first token (RFC 7208 §9.1)
	// e.g. "Received-SPF: pass (mailfrom: example.com) client-ip=…".
	// Received-SPF carries no authserv-id, so when trusted filtering is
	// on we can't attribute it — honour only an explicit "fail"
	// (fail-closed) and ignore a (possibly forged) "pass"/soft verdict
	// so the only path to a pass is a trusted A-R header.
	for _, hdr := range msg.ReceivedSPF {
		if val := firstToken(hdr); val != authNone {
			if filtering && val != authFail {
				continue
			}
			spf = mergeVerdict(spf, val)
		}
	}
	return spf, dkim
}

// mergeVerdict picks the higher-confidence outcome between two
// verdicts for the same method. The ordering is documented in
// summariseAuth's docstring.
func mergeVerdict(prev, next authVerdict) authVerdict {
	if rank(next) > rank(prev) {
		return next
	}
	return prev
}

func rank(v authVerdict) int {
	switch v {
	case authFail:
		return 5
	case authPass:
		return 4
	case authSoftFail:
		return 3
	case authNeutral, authPolicy:
		return 2
	case authTempFail, authPermFail:
		return 1
	default:
		return 0
	}
}

// authResultsMethod is one method=verdict entry from a single
// Authentication-Results header. A header may carry several
// (spf=pass, dkim=pass, dmarc=pass …); the slice-3 verifier cares
// only about spf + dkim.
type authResultsMethod struct {
	Method string
	Result authVerdict
}

// parseAuthResultsHeader breaks one Authentication-Results header
// value into its per-method verdicts. Format per RFC 8601 §2.2:
//
//	authres-header = "Authentication-Results:" [CFWS] authserv-id
//	                 *(  ";" [CFWS] method "=" result *( "(" comment ")" )
//	                                   [ ptype "." property "=" value ] )
//
// We only need (method, result). Implementation favours
// permissiveness over strict-spec adherence: real-world stamps
// vary in whitespace and CFWS placement, so we tokenise on `;`
// and pull the `key=value` slice between the equals sign and the
// next semicolon / open-paren / property.
func parseAuthResultsHeader(hdr string) (authservID string, methods []authResultsMethod) {
	hdr = stripParenComments(hdr)
	parts := strings.Split(hdr, ";")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i == 0 {
			// First segment is the authserv-id (optionally followed by
			// a version token, RFC 8601 §2.2). Take the first
			// whitespace-delimited word.
			authservID = firstWord(p)
			continue
		}
		eq := strings.Index(p, "=")
		if eq <= 0 {
			continue
		}
		method := strings.ToLower(strings.TrimSpace(p[:eq]))
		rest := strings.TrimSpace(p[eq+1:])
		// rest may be "pass" or "pass ptype.property=value …" — we
		// only want the first whitespace-delimited token.
		v := firstToken(rest)
		if v == authNone {
			continue
		}
		methods = append(methods, authResultsMethod{Method: method, Result: v})
	}
	return authservID, methods
}

// firstWord returns the first whitespace-delimited word in s, or "".
func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// stripParenComments removes (…) groups from a header value, per
// RFC 5322's CFWS rules. Nested parens are tolerated. We do this
// before tokenising so a stamp like
// "spf=pass (sender SPF authorized) smtp.mailfrom=alice@x.com"
// doesn't trip the method=result parser on the parenthetical.
func stripParenComments(in string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// firstToken returns the first whitespace-delimited word in s
// converted to an authVerdict (lower-cased). Unrecognised tokens
// return authNone so the caller can ignore them.
func firstToken(s string) authVerdict {
	s = strings.TrimSpace(s)
	if s == "" {
		return authNone
	}
	if i := strings.IndexAny(s, " \t,"); i > 0 {
		s = s[:i]
	}
	switch strings.ToLower(s) {
	case "pass":
		return authPass
	case "fail":
		return authFail
	case "softfail":
		return authSoftFail
	case "neutral":
		return authNeutral
	case "temperror":
		return authTempFail
	case "permerror":
		return authPermFail
	case "policy":
		return authPolicy
	case "none":
		return authNone
	default:
		return authNone
	}
}

// reasonOrEmpty extracts a short operator-facing identifier (the
// envelope From) so the rejection log line is actionable. Returns
// empty when From is unparseable — we still log a generic message
// in that case.
func reasonOrEmpty(msg ParsedMessage) string {
	if msg.From != "" {
		return "from=" + msg.From
	}
	return ""
}
