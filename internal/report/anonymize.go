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
	"vornik.io/vornik/internal/version"
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
type Check struct {
	Name, Status, Message string
	// Items are the check's supporting lines — for the journal check, the actual
	// error/fatal log lines it found.
	//
	// OPERATOR INSTRUCTION 2026-08-03: "make sure the appropriate logs are
	// included". The offline doctor has always tailed the journal into Items, but
	// the report dropped them, so a body said "5 recent error line(s)" and carried
	// none of them — the "bug report without any logs" complaint. They render now,
	// through the same scrubber as every other field and bounded by maxCheckItems /
	// maxCheckItemBytes (a doctor Item is an unrestricted string over the wire, and
	// this body has a URL budget).
	Items []string
}

const (
	// maxCheckItems caps rendered supporting lines per check, so one noisy check
	// cannot crowd the rest of the report out of the URL budget.
	maxCheckItems = 8
	// maxCheckItemBytes caps a single rendered line.
	maxCheckItemBytes = 200
)

// BodyInput is everything the public issue body is built from.
type BodyInput struct {
	Version, Edition, OS, Arch string
	// BuildDate is the ldflag-stamped build timestamp. A version alone does not
	// identify a build: the same tag is rebuilt (and `git describe` on a dev
	// checkout says nothing), so a report has to name the build too.
	BuildDate string
	Hostname  string // os.Hostname() — redacted literally from the body
	DaemonUp  bool
	Checks    []Check
	Symptom   string // the user's --summary free text (scrubbed too, #2)
}

// EditionTag is the short CE/EE marker.
//
// OPERATOR REQUEST 2026-08-03: a customer filed a bug report that named neither
// the edition nor the build, so triage could not tell whether the behaviour was
// even reachable in the build they were running. Every report path now carries
// both, and the tag also goes in the TITLE so maintainers can separate CE from
// EE in the issue LIST without opening each one.
//
// The value comes from version.NormalizeEdition, so an unstamped or untrusted
// ldflag string collapses to CE rather than being copied into a public title —
// which keeps reportTitle's rule (titles are built from controlled strings only)
// intact.
func EditionTag(edition string) string {
	if version.NormalizeEdition(edition) == version.EditionEnterprise {
		return "EE"
	}
	return "CE"
}

// EditionLabel renders the body's edition line — "community (CE)" — spelling out
// the normalized edition next to the tag so a reader who does not know the
// abbreviations still knows which build filed the report.
func EditionLabel(edition string) string {
	return version.NormalizeEdition(edition) + " (" + EditionTag(edition) + ")"
}

// Title prefixes a controlled base title with the edition tag: "[CE] <base>".
// Callers pass their own fixed title; only the tag is added here, so there is
// one place that decides how edition appears in a public issue title.
func Title(edition, base string) string {
	return "[" + EditionTag(edition) + "] " + base
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
	// rePEM takes a PEM block — and everything up to its END marker, or to the end
	// of the line if there is no END marker — in one bite.
	//
	// FOUND BY PROBE 2026-08-03 (design review-20260803-1094, D9): a key whose
	// newlines a JSON logger has collapsed onto ONE line survived every other net
	// here. secrets.Redact recognises a multi-line PEM block; this is the collapsed
	// and the truncated form, which is exactly the shape a journal line carries.
	// The installer's bash scrubber has covered PEM since review-20260725-a9da #3 —
	// the Go scrubber must not be the weaker of the two (same reasoning as
	// reKeyShapes).
	// The label class allows HYPHENS as well as spaces (review-20260803-4d44 #1):
	// RFC 7468 labels use spaces, but a logger that invents `ECDSA-PRIVATE-KEY`
	// would otherwise walk the key straight through. The class is matched
	// case-insensitively by the leading `(?i)` — the review believed otherwise,
	// which the lowercase case in TestAnonymizeBody_PEMAdversarial disproves.
	//
	// Case-insensitive (review-20260803-0e1b #2): RFC 7468 spells the markers in
	// uppercase, but a non-conformant logger that lower-cases its output would
	// otherwise walk a private key straight into a public issue, and `(?i)` costs
	// nothing — the `-----BEGIN`/`-----END` anchors are specific enough that
	// over-matching prose is not a realistic risk.
	rePEM = regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 -]{0,40}-----[\s\S]*?(?:-----END [A-Z0-9 -]{0,40}-----|$)`)
	// rePEMResidue catches the SECOND form of the same leak: secrets.Redact
	// replaces a single-line PEM's BEGIN header with its own marker and leaves the
	// key material in place, which also destroys the anchor rePEM needs. So rePEM
	// runs FIRST (below) and this net cleans up any marker-plus-material residue —
	// e.g. from a detector version that redacts the header only.
	// The material class deliberately excludes WHITESPACE (review-20260803-0e1b #1,
	// CRITICAL): with `\s` in the class the net consumed newlines and went on eating
	// every base64-ish line that followed a marker, which silently deleted the log
	// context this feature exists to deliver. It now stops at the end of the base64
	// run on that line, requires at least one character of material (so a bare
	// marker is left alone), and mops up a trailing END marker if the detector left
	// one — matched case-insensitively, like rePEM.
	rePEMResidue = regexp.MustCompile(`(?i)\[REDACTED:[a-z_]*(?:key|pem)[a-z_]*\][A-Za-z0-9+/=]+(?:-----END [A-Z0-9 -]{0,40}-----)?`)
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
		// PEM FIRST, before the detector: secrets.Redact replaces a single-line
		// block's BEGIN header with its own marker and leaves the key material
		// behind, which destroys the anchor this net matches on (probe 2026-08-03).
		s = rePEM.ReplaceAllString(s, "<redacted-pem>")
		s = string(secrets.Redact([]byte(s), det.Scan([]byte(s))))
		s = rePEMResidue.ReplaceAllString(s, "<redacted-pem>")
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
	// Edition and build are NOT scrubbed: EditionLabel maps them onto a
	// two-value enum, and an empty build date says "unknown" rather than
	// nothing, so a report never silently omits which build produced it.
	fmt.Fprintf(&b, "- **edition:** %s\n", EditionLabel(in.Edition))
	fmt.Fprintf(&b, "- **build:** %s\n", inline(buildDateOrUnknown(in.BuildDate)))
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
			writeCheckItems(&b, c.Items, inline)
		}
		b.WriteString("\n")
	}
	b.WriteString("_Filed via `vornikctl report`. Diagnostics anonymized; a redacted bundle may be attached separately._\n")
	return b.String(), nil
}

// BundleGuidance is the ONE copy of "here is the fuller evidence, here is where
// it lands, inspect it, here is how to get it onto the issue" — used by
// `vornikctl report`, the chat report_problem tool, and anything else that points
// a reporter at a support bundle.
//
// OPERATOR INSTRUCTION 2026-08-03: "the user should be instructed where the
// logs/blackbox export archive is and how to upload it". Naming the command was
// never enough: a reporter who does not know the archive's name, that they are
// expected to inspect it, or that GitHub takes it by drag-and-drop, does not
// attach it — which is how a bug report arrives with no logs.
//
// selector is the already-formed scope flag ("--task <id>" or "--since 2h").
// Nothing here is scrubbed because nothing here is caller data except selector,
// which callers pass through the body scrubber before display.
//
// EDITION SPLIT 2026-08-05: `vornikctl support-report` is gated behind the
// Enterprise admin surface and answers a Community caller with a 501
// "Enterprise-only feature". `vornikctl report` is itself a Community command,
// so printing the Enterprise instructions unconditionally walked every CE
// reporter into a wall at the exact moment they were trying to file a good bug.
// edition is normalized through version.NormalizeEdition, so an unstamped or
// untrusted build fails safe to the Community text.
func BundleGuidance(selector, edition string) string {
	if version.NormalizeEdition(edition) != version.EditionEnterprise {
		return communityBundleGuidance()
	}
	var b strings.Builder
	b.WriteString("For the full evidence — task timeline, container + daemon logs, the doctor\n")
	b.WriteString("snapshot, and (Enterprise) the task's Black Box trace — run:\n\n")
	b.WriteString("  vornikctl support-report " + selector + "\n\n")
	b.WriteString("It writes ONE archive named vornik-support-<scope>-<timestamp>.tar.gz into the\n")
	b.WriteString("current directory and prints the full path (override with --output <path>).\n")
	b.WriteString("It is redacted for secrets, but it MAY still carry project, swarm and workflow\n")
	b.WriteString("names, task ids and prompt text — OPEN AND\n")
	b.WriteString("INSPECT it before it leaves your machine:\n\n")
	b.WriteString("  tar -tzf <archive>              # what is in it\n")
	b.WriteString("  tar -xOzf <archive> MANIFEST.json   # sections, truncations, redaction counts\n\n")
	b.WriteString("To attach it: open the prefilled issue, then drag the .tar.gz into the comment\n")
	b.WriteString("box (GitHub accepts up to 25 MB per attachment — if the archive is larger, narrow the\n")
	b.WriteString("scope or shrink it with --max-size). You attach it yourself, under your own\n")
	b.WriteString("account, after reading it.")
	return b.String()
}

// communityBundleGuidance is the Community counterpart.
//
// It used to say there was no collector to point at. That was true until
// 2026-09-04, when `vornikctl support-report --local` gave Community a path
// that builds the SAME bundle in-process from the database and config on the
// host — authorised by shell access, which an operator on that host already
// has (support-bundle-in-CE design §2). So this text now names a command that
// WORKS, which is what the 2026-08-05 dead end was really about: the reporter
// was told to run something that answered 501.
//
// It still does not tell the reporter to paste journal output into the issue.
// That output carries hostnames, paths and credentials, and the whole point of
// this package is that anything reaching a public repo goes through the
// scrubber first.
func communityBundleGuidance() string {
	var b strings.Builder
	b.WriteString("For the full evidence — task timeline, daemon logs, the doctor snapshot and the\n")
	b.WriteString("deployed workflow prompts — collect the bundle on this host:\n\n")
	b.WriteString("  vornikctl support-report --local --task <task id>\n\n")
	b.WriteString("Community collects it LOCALLY, reading the database and config directly, so it\n")
	b.WriteString("works with the daemon down. Two sections are the daemon's live state and cannot\n")
	b.WriteString("be collected this way — health and metrics; they are listed in the archive's\n")
	b.WriteString("section_errors rather than silently missing. The Black Box trace is an\n")
	b.WriteString("Enterprise feature and is absent here by construction.\n\n")
	b.WriteString("It writes ONE archive named vornik-support-<scope>-<timestamp>.tar.gz into the\n")
	b.WriteString("current directory and prints the full path (override with --output <path>).\n")
	b.WriteString("It is redacted for secrets, but it MAY still carry project, swarm and workflow\n")
	b.WriteString("names, task ids and prompt text — OPEN AND\n")
	b.WriteString("INSPECT it before it leaves your machine:\n\n")
	b.WriteString("  tar -tzf <archive>                  # what is in it\n")
	b.WriteString("  tar -xOzf <archive> MANIFEST.json   # sections, truncations, redaction counts\n")
	b.WriteString("  tar -xOzf <archive> collection.json # which path collected it, and whose version\n\n")
	b.WriteString("To attach it: open the prefilled issue, then drag the .tar.gz into the comment\n")
	b.WriteString("box (GitHub accepts up to 25 MB per attachment — if the archive is larger, narrow the\n")
	b.WriteString("scope or shrink it with --max-size). You attach it yourself, under your own\n")
	b.WriteString("account, after reading it.\n\n")
	b.WriteString("  vornikctl doctor --json    # the same findings, machine-readable, for your own review")
	return b.String()
}

// writeCheckItems renders a check's supporting lines (the journal tail's actual
// error lines) as an indented, scrubbed, bounded sub-list. Truncation is stated
// rather than silent — an omitted tail must not read as "that was all of it".
func writeCheckItems(b *strings.Builder, items []string, inline func(string) string) {
	shown := items
	if len(shown) > maxCheckItems {
		// Keep the MOST RECENT lines: the journal tail appends in time order and
		// the failure that prompted the report is at the end.
		shown = shown[len(shown)-maxCheckItems:]
	}
	rendered := 0
	for _, it := range shown {
		s := strings.TrimSpace(inline(it))
		if s == "" {
			// A line that scrubs down to nothing is DROPPED — and counted as
			// omitted below (review-20260803-6eef): counting only the cap slice
			// undercounted, so the remainder read as the whole tail.
			continue
		}
		if len(s) > maxCheckItemBytes {
			s = s[:maxCheckItemBytes] + "…"
		}
		fmt.Fprintf(b, "  - `%s`\n", s)
		rendered++
	}
	if omitted := len(items) - rendered; omitted > 0 {
		fmt.Fprintf(b, "  - _(%d more line(s) omitted — attach the redacted bundle for the full log)_\n", omitted)
	}
}

// buildDateOrUnknown keeps the build line honest for an archive build with no
// ldflags rather than dropping the field (an absent line reads as "not
// collected"; "unknown" reads as "this build carries no stamp").
func buildDateOrUnknown(d string) string {
	if strings.TrimSpace(d) == "" {
		return version.UnknownBuildDate
	}
	return d
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
