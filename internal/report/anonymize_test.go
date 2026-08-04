package report

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

// The public body must NOT leak identifiers — email, home paths (with the
// username + project dir), LAN IPs, the machine hostname, or a secret token —
// whether they appear in a doctor Message or the user's --summary.
func TestAnonymizeBody_NoLeaks(t *testing.T) {
	in := BodyInput{
		Version: "2026.7.4-60", Edition: "ce", OS: "linux", Arch: "amd64",
		Hostname: "AI-PC", DaemonUp: false,
		Symptom: "failed connecting to swarm at 192.0.2.10 or 2001:db8::1234, see /var/home/vadim/projects/vornik-marketing/logs; ping vadim@vornik.io",
		Checks: []Check{
			{Name: "database", Status: "fail", Message: "dial 10.88.0.4:5432 refused for /var/home/vadim/x on ai-pc"},
			{Name: "config", Status: "fail", Message: "token=sk-abcdef0123456789abcdef leaked in config"},
			// BARE key shapes (no `key=` context, below the 40-char entropy
			// floor) — these slipped past secrets.Redact until reKeyShapes was
			// added (found live 2026-07-25). Public post must not carry them.
			{Name: "journal", Status: "warn", Message: "saw sk-bareABCDEF0123456789 and AKIABAREKEY01234567 and ghp_bareGithubToken0123456789ABCDEFxx in logs"},
		},
	}
	body, err := AnonymizeBody(in)
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	for _, leak := range []string{
		"vadim@vornik.io", "/var/home/vadim", "192.0.2.10", "10.88.0.4", "AI-PC",
		"2001:db8::1234", "ai-pc",
		"sk-abcdef0123456789abcdef", "vadim",
		// review-20260725-7530 #1/#2: the home-path TAIL (project dir) must not
		// survive; the whole path collapses to <path>.
		"vornik-marketing", "projects", "/x",
		// bare key shapes (no assignment context) — reKeyShapes net.
		"sk-bareABCDEF0123456789", "AKIABAREKEY01234567", "ghp_bareGithubToken0123456789ABCDEFxx",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("public body LEAKS %q:\n%s", leak, body)
		}
	}
	// Safe/controlled fields survive. The edition is rendered from the normalized
	// enum ("ce" is not a stamped edition, so it reads community) — see
	// TestAnonymizeBody_MarksEditionAndBuild.
	for _, keep := range []string{"2026.7.4-60", "community (CE)", "linux/amd64", "database", "config", "down"} {
		if !strings.Contains(body, keep) {
			t.Errorf("body missing expected %q:\n%s", keep, body)
		}
	}
}

// Fail-closed: if the secret detector can't be built, no body is emitted and the
// error is the STATIC message (no wrapped offending value).
func TestAnonymizeBody_FailClosed(t *testing.T) {
	orig := newDetector
	newDetector = func() (*secrets.MultiDetector, error) { return nil, errors.New("boom-secret-VALUE") }
	defer func() { newDetector = orig }()

	body, err := AnonymizeBody(BodyInput{Version: "1", Symptom: "boom-secret-VALUE"})
	if err == nil {
		t.Fatal("expected fail-closed error")
	}
	if body != "" {
		t.Errorf("fail-closed must emit no body, got %q", body)
	}
	if strings.Contains(err.Error(), "boom-secret-VALUE") {
		t.Errorf("error must be static, must not leak the value: %v", err)
	}
}

// REGRESSION 2026-08-03 (operator report): a CE customer filed a bug report
// carrying neither the edition nor the build, so triage could not tell whether
// the behaviour was even reachable in the build they ran. Every report body must
// name both, an unstamped/untrusted edition string must normalize rather than be
// echoed, and a build with no ldflags must say "unknown" instead of dropping the
// line (an absent line reads as "not collected").
func TestAnonymizeBody_MarksEditionAndBuild(t *testing.T) {
	for _, tc := range []struct {
		name           string
		edition, build string
		wantPresent    []string
		wantAbsent     []string
	}{
		{
			name: "enterprise build", edition: "enterprise", build: "2026-08-03T09:14:00Z",
			wantPresent: []string{"**edition:** enterprise (EE)", "**build:** 2026-08-03T09:14:00Z"},
		},
		{
			name: "community build", edition: "community", build: "2026-08-03T09:14:00Z",
			wantPresent: []string{"**edition:** community (CE)"},
			wantAbsent:  []string{"(EE)"},
		},
		{
			name: "unstamped edition normalizes to CE", edition: "", build: "x",
			wantPresent: []string{"**edition:** community (CE)"},
		},
		{
			name: "untrusted edition string is not echoed", edition: "ce-totally-bogus", build: "x",
			wantPresent: []string{"**edition:** community (CE)"},
			wantAbsent:  []string{"bogus"},
		},
		{
			name: "unstamped build says unknown", edition: "community", build: "  ",
			wantPresent: []string{"**build:** unknown"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := AnonymizeBody(BodyInput{
				Version: "2026.7.7", Edition: tc.edition, BuildDate: tc.build,
				OS: "linux", Arch: "amd64", Hostname: "h", DaemonUp: true,
			})
			if err != nil {
				t.Fatalf("AnonymizeBody: %v", err)
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q:\n%s", want, body)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body must not contain %q:\n%s", absent, body)
				}
			}
		})
	}
}

// OPERATOR INSTRUCTION 2026-08-03: "make sure the appropriate logs are included".
// The offline doctor already tails the daemon journal into Check.Items, but the
// public body dropped them — a report said "5 recent error line(s)" and never
// carried a single one, which is precisely the "bug report without any logs"
// complaint. Items must render, be scrubbed like everything else, and stay
// bounded (a doctor Message/Item is an unrestricted string over the wire).
func TestAnonymizeBody_RendersScrubbedLogLines(t *testing.T) {
	// Journal lines arrive in time order and the renderer keeps the MOST RECENT
	// ones, so the interesting lines go LAST — that is what a real tail looks like.
	var items []string
	for i := 0; i < 20; i++ { // well over the cap
		items = append(items, "filler error line")
	}
	items = append(items,
		`{"level":"error","host":"AI-PC","msg":"dial 10.88.0.4:5432: refused for /var/home/vadim/p"}`,
		`{"level":"fatal","msg":"token=sk-abcdef0123456789abcdef rejected"}`,
	)
	body, err := AnonymizeBody(BodyInput{
		Version: "2026.7.7", Edition: "community", BuildDate: "b",
		OS: "linux", Arch: "amd64", Hostname: "AI-PC", DaemonUp: false,
		Checks: []Check{{
			Name: "journal", Status: "warn",
			Message: "22 recent error/fatal line(s) in the daemon journal",
			Items:   items,
		}},
	})
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	// The lines are there — actionable content, not just a count.
	for _, want := range []string{"dial", "refused", "rejected"} {
		if !strings.Contains(body, want) {
			t.Errorf("body dropped the log line content %q:\n%s", want, body)
		}
	}
	// …and scrubbed exactly like the rest of the body.
	for _, leak := range []string{"AI-PC", "10.88.0.4", "/var/home/vadim", "sk-abcdef0123456789abcdef"} {
		if strings.Contains(body, leak) {
			t.Errorf("log line LEAKS %q into a public body:\n%s", leak, body)
		}
	}
	// …and bounded, so one noisy check cannot crowd out the rest of the report.
	if got := strings.Count(body, "filler error line"); got > maxCheckItems {
		t.Errorf("rendered %d filler lines, cap is %d", got, maxCheckItems)
	}
	if !strings.Contains(body, "more line(s) omitted") {
		t.Errorf("a truncated item list must say so:\n%s", body)
	}
}

// A single pathological line cannot blow the URL budget on its own.
func TestAnonymizeBody_TruncatesAnOverlongLogLine(t *testing.T) {
	body, err := AnonymizeBody(BodyInput{
		Version: "v", Edition: "community", Hostname: "h0st", DaemonUp: true,
		Checks: []Check{{Name: "journal", Status: "warn", Items: []string{strings.Repeat("z", 900)}}},
	})
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	if strings.Contains(body, strings.Repeat("z", maxCheckItemBytes+1)) {
		t.Errorf("overlong log line not truncated:\n%s", body)
	}
}

// The edition tag also goes in the TITLE, so maintainers can separate CE from EE
// in the issue LIST without opening each one. Titles must stay built from
// controlled strings only (see reportTitle), which is why the tag is derived
// through version.NormalizeEdition rather than interpolated raw.
func TestEditionTagAndTitle(t *testing.T) {
	for _, tc := range []struct {
		edition  string
		wantTag  string
		wantText string
	}{
		{"enterprise", "EE", "[EE] vornik problem report"},
		{"community", "CE", "[CE] vornik problem report"},
		{"", "CE", "[CE] vornik problem report"},
		{"Enterprise", "CE", "[CE] vornik problem report"}, // NormalizeEdition is exact-match, fail-safe
	} {
		if got := EditionTag(tc.edition); got != tc.wantTag {
			t.Errorf("EditionTag(%q) = %q, want %q", tc.edition, got, tc.wantTag)
		}
		if got := Title(tc.edition, "vornik problem report"); got != tc.wantText {
			t.Errorf("Title(%q, …) = %q, want %q", tc.edition, got, tc.wantText)
		}
	}
	if got := EditionLabel("enterprise"); got != "enterprise (EE)" {
		t.Errorf("EditionLabel(enterprise) = %q", got)
	}
}

func TestIssueURL_Basic(t *testing.T) {
	u := IssueURL("Install failure: podman", "body **markdown** here")
	if !strings.HasPrefix(u, "https://github.com/grinco/vornik/issues/new?") {
		t.Fatalf("wrong repo/base: %s", u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("unparseable URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("labels") != "bug" {
		t.Errorf("labels = %q, want bug", q.Get("labels"))
	}
	if q.Get("title") != "Install failure: podman" {
		t.Errorf("title round-trip failed: %q", q.Get("title"))
	}
	if q.Get("body") != "body **markdown** here" {
		t.Errorf("body round-trip failed: %q", q.Get("body"))
	}
}

func TestIssueURL_Truncates(t *testing.T) {
	huge := strings.Repeat("x", 20000)
	u := IssueURL("t", huge)
	if len(u) > urlMaxBytes {
		t.Errorf("URL not truncated under cap: len=%d", len(u))
	}
	parsed, _ := url.Parse(u)
	if !strings.Contains(parsed.Query().Get("body"), "truncated") {
		t.Errorf("truncated URL must carry the attach-note: %q", parsed.Query().Get("body"))
	}
}

// TestRedactIPv6Token pins both halves of the IPv6 redaction contract:
// real addresses are redacted (A19), and ordinary text containing "::" is left
// intact. The original substring regex failed the second half because "::",
// "d::" and "e::f" all parse as valid IPv6, so a fragment grabbed from the
// middle of a word was redacted — `core::fmt::Debug` became `cor<ip>mt<ip>ug`
// in PUBLIC issue bodies (audit 2026-07-25 follow-up).
func TestRedactIPv6Token(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// --- must redact -------------------------------------------------
		{"loopback", "::1", "<ip>"},
		{"link-local with zone", "fe80::1%eth0", "<ip>%eth0"},
		{"compressed global", "2001:db8::1", "<ip>"},
		{"full form", "2001:0db8:0000:0000:0000:0000:0000:0001", "<ip>"},
		{"multicast", "ff02::1", "<ip>"},
		{"bracketed with port", "[2001:db8::1]:8080", "[<ip>]:8080"},
		{"trailing sentence period", "2001:db8::1.", "<ip>."},
		// --- must NOT be touched -----------------------------------------
		{"rust path", "core::fmt::Debug", "core::fmt::Debug"},
		{"cpp symbol", "std::vector", "std::vector"},
		{"postgres cast", "::text", "::text"},
		{"bare scope operator", "::", "::"},
		{"timestamp", "16:52:10Z", "16:52:10Z"},
		{"ipv4 left to reIPv4", "192.168.1.1", "192.168.1.1"},
		{"plain word", "database", "database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactIPv6Token(tc.in); got != tc.want {
				t.Errorf("redactIPv6Token(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestScrubberIPv6InContext runs the whole scrubber over realistic doctor
// message text: the address must go, the surrounding diagnostics must survive
// verbatim so the public issue is still actionable.
func TestScrubberIPv6InContext(t *testing.T) {
	scrub, err := scrubber("build-host-01")
	if err != nil {
		t.Fatalf("scrubber: %v", err)
	}
	for _, tc := range []struct {
		name        string
		in          string
		wantAbsent  string
		wantPresent []string
	}{
		{
			name:        "address redacted, prose intact",
			in:          "dial tcp [2001:db8:dead:beef::5]:5432: connection refused",
			wantAbsent:  "2001:db8",
			wantPresent: []string{"<ip>", "5432", "connection refused"},
		},
		{
			name:        "postgres cast survives",
			in:          `pq: operator does not exist: text = integer in "created_at"::text`,
			wantAbsent:  "<ip>",
			wantPresent: []string{"::text", "operator does not exist"},
		},
		{
			name:        "go/rust style path survives",
			in:          "panic in core::fmt::Debug while formatting the report",
			wantAbsent:  "<ip>",
			wantPresent: []string{"core::fmt::Debug"},
		},
		{
			name:        "case-insensitive hostname still redacted (A21)",
			in:          "connecting to BUILD-Host-01 failed",
			wantAbsent:  "BUILD-Host-01",
			wantPresent: []string{"<host>"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scrub(tc.in)
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("scrub(%q) = %q, must not contain %q", tc.in, got, tc.wantAbsent)
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(got, want) {
					t.Errorf("scrub(%q) = %q, want it to retain %q", tc.in, got, want)
				}
			}
		})
	}
}

// OPERATOR INSTRUCTION 2026-08-03: "the user should be instructed where the
// logs/blackbox export archive is and how to upload it". A pointer at
// `support-report` is not enough on its own — a reporter who does not know where
// the archive lands, that they must inspect it, or how to get it onto the issue
// simply never attaches it. One copy of that guidance, used by every path.
func TestBundleGuidance(t *testing.T) {
	g := BundleGuidance("--task task_20260803110416_1b040469e7c058da")

	for _, want := range []string{
		"vornikctl support-report --task task_20260803110416_1b040469e7c058da", // the exact command
		"vornik-support-", // where it lands: the archive name
		".tar.gz",         //  …and its shape
		"--output",        // how to put it somewhere else
		"MANIFEST.json",   // inspect-before-attach, concretely
		"drag",            // how to upload
		"25 MB",           // the limit they will hit
		"--max-size",      // …and the way out of it
		"Black Box",       // the EE trace is in there
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guidance missing %q:\n%s", want, g)
		}
	}
	// It must never suggest we upload for them: the bundle can carry project
	// names, so the human inspects and attaches it under their own account.
	for _, absent := range []string{"automatically", "we will upload"} {
		if strings.Contains(strings.ToLower(g), absent) {
			t.Errorf("guidance must not promise an automatic upload (%q):\n%s", absent, g)
		}
	}
}

// LEAK-CLASS COVERAGE for the log lines D9 added to the PUBLIC body (design
// review-20260803-1094, finding D9 — "underspecified leak classes"). Probed
// empirically rather than assumed: JWTs, AWS keys, long base64 runs and cloud
// IPv4s were already caught, but a **single-line PEM block survived** — the
// installer's bash scrubber has covered PEM since review-20260725-a9da #3 while
// the Go scrubber did not, so a private key pasted into a log line could reach a
// public issue. Every class below is now pinned.
func TestAnonymizeBody_ItemLeakClasses(t *testing.T) {
	items := []string{
		`{"level":"error","token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"}`,
		`{"level":"error","key":"AKIAIOSFODNN7EXAMPLE"}`,
		`{"level":"error","msg":"payload PAYLOADBASE64Zm9vYmFyYmF6cXV1eGNvcmdlZ3JhdWx0Z2FycGx5MTIzNDU2Nzg5MA=="}`,
		`{"level":"error","msg":"cloud host 52.94.236.248 unreachable"}`,
		// PEM on ONE line — how a key looks when a JSON logger stringifies it.
		`{"level":"fatal","msg":"-----BEGIN RSA PRIVATE KEY-----MIIEowIBAAKCAQEAsecretkeymaterial-----END RSA PRIVATE KEY-----"}`,
		// PEM truncated mid-key (a tail cut by the journal or by our own cap):
		// the BEGIN marker with no END must still take the material with it.
		`{"level":"fatal","msg":"-----BEGIN OPENSSH PRIVATE KEY-----b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAA"}`,
	}
	body, err := AnonymizeBody(BodyInput{
		Version: "v", Edition: "community", Hostname: "AI-PC", DaemonUp: true,
		Checks: []Check{{Name: "journal", Status: "warn", Items: items}},
	})
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	for _, leak := range []string{
		"eyJhbGciOiJIUzI1NiJ9",                  // JWT
		"AKIAIOSFODNN7EXAMPLE",                  // AWS access key id
		"PAYLOADBASE64Zm9vYmFyYmF6cXV1eGNvcmdl", // long base64 run
		"52.94.236.248",                         // ANY IPv4, not just RFC1918
		"MIIEowIBAAKCAQEAsecretkeymaterial",     // PEM body, single-line form
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmU",       // PEM body, truncated form
	} {
		if strings.Contains(body, leak) {
			t.Errorf("public body LEAKS %q from a log line:\n%s", leak, body)
		}
	}
}

// review-20260803-6eef HIGH: the omitted-count was computed from the CAP slice
// alone, so a line that scrubs down to nothing (or arrives blank) was dropped
// silently and not counted — "N more omitted" undercounted, and the reader has no
// way to know a line vanished. The count must reflect what was actually RENDERED.
func TestAnonymizeBody_OmittedCountIncludesLinesLostToScrubbing(t *testing.T) {
	body, err := AnonymizeBody(BodyInput{
		Version: "v", Edition: "community", Hostname: "AI-PC", DaemonUp: true,
		Checks: []Check{{Name: "journal", Status: "warn", Items: []string{
			"real error line one",
			"   ",    // blank
			"AI-PC",  // scrubs to a bare <host> marker, still renderable
			"\t\n  ", // whitespace only
			"real error line two",
		}}},
	})
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	if !strings.Contains(body, "real error line one") || !strings.Contains(body, "real error line two") {
		t.Errorf("body lost a real line:\n%s", body)
	}
	// Two blank items were dropped, so the body must SAY two lines are missing
	// rather than presenting the remainder as the whole tail.
	if !strings.Contains(body, "2 more line(s) omitted") {
		t.Errorf("omitted count must account for lines dropped by scrubbing:\n%s", body)
	}
}

// ADVERSARIAL PEM cases (review-20260803-0e1b, findings 1-3 against the fix that
// closed the original leak). Three properties, one test: lowercase markers must
// not evade the net; two blocks on one line must both go; and the residue net must
// NOT eat the lines that follow it — over-redaction destroys the log fidelity the
// whole feature exists to deliver, so it is a defect, not a safe default.
func TestAnonymizeBody_PEMAdversarial(t *testing.T) {
	body, err := AnonymizeBody(BodyInput{
		Version: "v", Edition: "community", Hostname: "AI-PC", DaemonUp: true,
		// The symptom is scrubbed as ONE multi-line string, so it is where a
		// newline-hungry residue regex would do its damage.
		Symptom: "key dump follows\n" +
			"-----BEGIN RSA PRIVATE KEY-----MIIEowIBAAKCAQEAupperkeymaterial-----END RSA PRIVATE KEY-----\n" +
			"then the daemon rejected the connection\n" +
			"and the retry also failed with status 500\n",
		Checks: []Check{{Name: "journal", Status: "warn", Items: []string{
			// lowercase markers — a non-conformant logger's output
			`{"msg":"-----begin rsa private key-----MIIEowIBAAKCAQEAlowerkeymaterial-----end rsa private key-----"}`,
			// two blocks on one line
			`{"msg":"-----BEGIN KEY1-----AAAAfirstkeymaterialAAAA-----END KEY1----------BEGIN KEY2-----BBBBsecondkeymaterialBBBB-----END KEY2-----"}`,
		}}},
	})
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	for _, leak := range []string{
		"MIIEowIBAAKCAQEAupperkeymaterial",
		"MIIEowIBAAKCAQEAlowerkeymaterial", // lowercase markers must not evade
		"AAAAfirstkeymaterialAAAA",
		"BBBBsecondkeymaterialBBBB", // the SECOND block on the line too
	} {
		if strings.Contains(body, leak) {
			t.Errorf("public body LEAKS %q:\n%s", leak, body)
		}
	}
	// …and the prose after the key survives: the redaction must be minimal.
	for _, keep := range []string{
		"then the daemon rejected the connection",
		"and the retry also failed with status 500",
	} {
		if !strings.Contains(body, keep) {
			t.Errorf("PEM redaction ATE following content %q (over-redaction):\n%s", keep, body)
		}
	}
}

// review-20260803-0e1b finding 1 (CRITICAL): the residue net's character class
// included \s, so after a detector marker it consumed newlines and kept eating
// every base64-ish line that followed. Exercised directly, because rePEM (which
// now runs first) normally catches the block before the residue net is reached —
// the hazard is real but only reachable through this net, so this is where it is
// pinned.
func TestPEMResidue_DoesNotEatFollowingLines(t *testing.T) {
	in := "[REDACTED:private_key_block]MIIEowIBAAKCAQEAmaterial\n" +
		"connection refused\n" +
		"RetryScheduled\n"
	got := rePEMResidue.ReplaceAllString(in, "<redacted-pem>")
	if strings.Contains(got, "MIIEowIBAAKCAQEAmaterial") {
		t.Errorf("residue net left key material behind: %q", got)
	}
	for _, keep := range []string{"connection refused", "RetryScheduled"} {
		if !strings.Contains(got, keep) {
			t.Errorf("residue net ATE %q — it must stop at the end of the base64 run: %q", keep, got)
		}
	}
}

// review-20260803-4d44 suggestions 1-3: hyphenated PEM labels (non-standard but a
// logger can invent them), and the empty-material forms. Note the review also
// claimed the label class stays case-sensitive despite the leading `(?i)` — it
// does not, as TestAnonymizeBody_PEMAdversarial's lowercase case already proves;
// the hyphen gap was the real one.
func TestAnonymizeBody_PEMLabelAndEmptyMaterialForms(t *testing.T) {
	body, err := AnonymizeBody(BodyInput{
		Version: "v", Edition: "community", Hostname: "AI-PC", DaemonUp: true,
		Checks: []Check{{Name: "journal", Status: "warn", Items: []string{
			// hyphenated label
			`{"msg":"-----BEGIN ECDSA-PRIVATE-KEY-----HYPHENLABELkeymaterial0000-----END ECDSA-PRIVATE-KEY-----"}`,
			// detector marker with an END but no material between them
			`{"msg":"[REDACTED:private_key_block]-----END PRIVATE KEY-----"}`,
			// bare marker, nothing after it: must be left exactly as-is
			`{"msg":"[REDACTED:private_key_block] the connection then dropped"}`,
		}}},
	})
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	if strings.Contains(body, "HYPHENLABELkeymaterial0000") {
		t.Errorf("hyphenated PEM label evaded the net:\n%s", body)
	}
	// A bare marker is diagnostic-safe and its following prose must survive.
	if !strings.Contains(body, "the connection then dropped") {
		t.Errorf("a bare marker must not consume the prose after it:\n%s", body)
	}
}
