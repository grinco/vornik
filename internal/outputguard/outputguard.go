// Package outputguard scans tool-call RESULTS for adversarial
// content before they enter conversation context. Inspired by
// Turnstone's Output Guard (docs/judge.md): the LLM judge
// validates intent before tool execution; the output guard
// validates content after.
//
// Threat model:
//
//   - Prompt injection. Retrieved content (web_fetch, memory
//     hits, file reads) may carry instructions intended for
//     the assistant: "ignore previous instructions", chat-
//     template tokens, role-impersonation. Naive concatenation
//     into the LLM's context lets the attacker hijack the
//     session.
//
//   - Credential leakage. Tool output may quote env vars,
//     config files, or HTTP error bodies that include real
//     secrets. We already redact secrets at persistence
//     boundaries (internal/secrets); this layer adds a
//     pre-conversation gate.
//
//   - Encoded payloads. Base64 / hex blocks past a threshold
//     are flagged. Operators see "the tool returned an
//     encoded blob" and can choose whether to feed it into
//     the conversation.
//
// Design:
//
//   - Pure-Go pattern table; no LLM call. Sub-ms per kilobyte
//     so it sits on the hot path between tool execution and
//     conversation-history append.
//
//   - Returns a Report with structured findings (severity,
//     kind, byte offsets). The caller decides what to do —
//     surface a warning banner, redact the offending span,
//     refuse the tool result, etc.
//
//   - Redact() is a separate, opt-in pass. Severities INFO /
//     WARN never auto-redact (operators can still benefit
//     from seeing the suspicious context); HIGH redacts the
//     span by default but the caller can still pass the
//     report through unmodified.
//
// 2026.7.0 F10. Pairs with internal/secrets which already
// handles the credential half; this package adds the
// injection-pattern half.

package outputguard

import (
	"fmt"
	"regexp"
	"strings"
)

// Severity levels mirror the four tiers used by Turnstone's
// Output Guard. Mapped to our taxonomy:
//
//	INFO — surfaces in audit logs but no UI banner; e.g. a
//	       short base64 blob in a markdown image.
//	WARN — UI banner; encourages operator review; e.g. a
//	       "system:" prefix in a memory_search result.
//	HIGH — UI banner + auto-redact by default; e.g.
//	       "ignore all previous instructions" near the
//	       start of a tool output.
type Severity string

const (
	SeverityInfo Severity = "info"
	SeverityWarn Severity = "warn"
	SeverityHigh Severity = "high"
)

// Kind names the rule that fired. Operators read this on
// the warning banner to understand the nature of the finding
// at a glance.
type Kind string

const (
	// KindInjectionInstruction — explicit instruction to
	// disregard prior context. The single highest-signal
	// pattern; almost always adversarial in tool output.
	KindInjectionInstruction Kind = "injection_instruction"
	// KindInjectionRoleSwap — "you are now …", "act as …".
	// Common in jailbreak attempts.
	KindInjectionRoleSwap Kind = "injection_role_swap"
	// KindInjectionChatTemplate — chat-template / ChatML
	// markers (<|im_start|>, <|system|>) inside a tool
	// result that shouldn't carry framing tokens.
	KindInjectionChatTemplate Kind = "injection_chat_template"
	// KindInjectionSystemMarker — "system:" / "[SYSTEM]"
	// pseudo-headers attempting to spoof a system message.
	KindInjectionSystemMarker Kind = "injection_system_marker"
	// KindEncodedPayload — long base64 / hex block. Could
	// be a legit image; could be smuggled instructions
	// pre-encoded to evade the other rules.
	KindEncodedPayload Kind = "encoded_payload"
	// KindAdversarialURL — URL with credential-shaped query
	// params (token=, api_key=, password=) or data:text/…
	// inline payloads.
	KindAdversarialURL Kind = "adversarial_url"
)

// Finding is one match the guard produced.
type Finding struct {
	Kind     Kind
	Severity Severity
	// Span is the byte-offset half-open range [Start, End)
	// of the offending substring in the original content.
	// Used by Redact to slice the spans out.
	Start int
	End   int
	// Evidence is the matched substring, truncated to
	// 200 chars for the audit / banner display.
	Evidence string
}

// Report bundles every finding from one Scan call.
type Report struct {
	Findings []Finding
}

// HasFinding returns true when at least one finding was
// produced. Cheap check for hot-path callers that only care
// about the existence of any signal.
func (r Report) HasFinding() bool {
	return len(r.Findings) > 0
}

// MaxSeverity returns the highest severity present in the
// report, or empty string if no findings.
func (r Report) MaxSeverity() Severity {
	var max Severity
	rank := func(s Severity) int {
		switch s {
		case SeverityHigh:
			return 3
		case SeverityWarn:
			return 2
		case SeverityInfo:
			return 1
		}
		return 0
	}
	for _, f := range r.Findings {
		if rank(f.Severity) > rank(max) {
			max = f.Severity
		}
	}
	return max
}

// ruleClass classifies a rule so ScanWithProvenance can
// skip injection-class rules for first-party content.
//
//   - classSecret — rules that detect credential leakage or
//     encoded payloads. These always run regardless of provenance.
//     This is the ZERO VALUE (fail-safe): a future rule added without
//     an explicit class tag defaults to secret (never skipped).
//   - classInjection — rules that defend against a third party
//     smuggling instructions into the model context (role-swap,
//     ignore-previous, chat-template, system-marker). These are
//     meaningless on first-party content and skipped when
//     provenance == ProvenanceFirstParty.
type ruleClass int

const (
	classSecret    ruleClass = iota // zero value — fail-safe: never skipped
	classInjection                  // skipped on ProvenanceFirstParty
)

// Provenance signals the origin trust-level of tool-result
// content. The zero value (ProvenanceUnknown) is deliberately
// the fail-safe: it is treated as third-party (full scan).
type Provenance int

const (
	// ProvenanceUnknown is the zero value. Treated as
	// third-party — runs the full rule set.
	ProvenanceUnknown Provenance = iota
	// ProvenanceThirdParty — content sourced from outside the
	// agent (scraped pages, MCP results, memory hits). Runs
	// the full rule set including injection-class rules.
	ProvenanceThirdParty
	// ProvenanceFirstParty — content the agent itself composed
	// (dispatcher builtins, task_output artifacts). Injection-
	// class rules are skipped; secret-class rules still run.
	ProvenanceFirstParty
)

// rulePattern pairs a compiled regex with the (kind, severity)
// it fires. All regexes are case-insensitive; (?i) prefix
// drops the need for ToLower copies on the hot path.
//
// The optional verify hook runs after a regex match to filter
// false-positives that regex alone can't express in RE2
// (no lookaround). It receives the full content + the matched
// span; return false to discard the match. Used by the encoded-
// payload rules to skip URL-context hits (long query strings on
// scraper requests are not adversarial encoded blobs).
type rulePattern struct {
	kind     Kind
	severity Severity
	class    ruleClass
	re       *regexp.Regexp
	verify   func(content string, start, end int) bool
}

// encodedPayloadIsRealBlob filters the long-alphanumeric regex
// against URL-context false positives. We saw real-world misfires
// on `mcp__scraper__web_fetch` results where the tool description
// embedded a long URL with a path or query string composed entirely
// of [A-Za-z0-9] (200+ chars). Standalone long base64 can also be
// all alphanumeric, so the filter must not require `+` or `/`;
// it rejects only matches that are part of an http(s) URL token.
func encodedPayloadIsRealBlob(content string, start, end int) bool {
	windowStart := start - 128
	if windowStart < 0 {
		windowStart = 0
	}
	window := content[windowStart:start]
	lastSpace := strings.LastIndexAny(window, " \t\r\n\"'<>")
	if lastSpace >= 0 {
		window = window[lastSpace+1:]
	}
	if strings.Contains(window, "http://") || strings.Contains(window, "https://") {
		return false
	}
	return true
}

// rules is the compiled rule table. Sorted by approximate
// expected hit frequency so the most likely matches come
// first — `re.FindAllStringIndex` short-circuits early when
// the input is huge and the rule wouldn't have matched.
var rules = []rulePattern{
	{
		kind:     KindInjectionInstruction,
		severity: SeverityHigh,
		class:    classInjection,
		re:       regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\s+(?:the\s+|all\s+|any\s+)?(previous|prior|earlier|above)\s+(instructions?|prompts?|context|messages?)`),
	},
	{
		kind:     KindInjectionInstruction,
		severity: SeverityHigh,
		class:    classInjection,
		re:       regexp.MustCompile(`(?i)\bnew\s+instructions?\s*[:\-]`),
	},
	{
		// KindInjectionRoleSwap — "you are now …", "pretend to be …",
		// "roleplay as …", and "act as <AI-persona-token>".
		//
		// The `act\s+as` alternative is intentionally narrowed to
		// require an injection-context object (an AI persona or
		// unrestricted-role word) immediately after. Without this
		// constraint the bare "act as" phrase fires on benign
		// document text — e.g. CV bullets like "Act as liaison
		// between development teams and business stakeholders" were
		// redacted as injection_role_swap on Telegram delivery.
		// Real jailbreak payloads almost always name an AI persona
		// or an unrestricted/uncensored role: "act as DAN", "act as
		// a language model", "act as an unrestricted assistant", etc.
		//
		// The other alternatives (`you\s+are\s+now`, `pretend\s+to\s+be`,
		// `roleplay\s+as`) are left unchanged: they are syntactically
		// more specific and their benign-use rate in first-party
		// document content is much lower.
		kind:     KindInjectionRoleSwap,
		severity: SeverityHigh,
		class:    classInjection,
		re:       regexp.MustCompile(`(?i)\b(you\s+are\s+now|act\s+as\s+(?:an?\s+|the\s+)?(?:ai|a\.i\.|assistant|chat\s?bot|language\s+model|llm|model|dan|system|developer\s+mode|unrestricted|jailbro?ken|uncensored)|pretend\s+to\s+be|roleplay\s+as)\b`),
	},
	{
		kind:     KindInjectionChatTemplate,
		severity: SeverityHigh,
		class:    classInjection,
		re:       regexp.MustCompile(`<\|(im_start|im_end|system|user|assistant|tool|chat_start|chat_end)\|>`),
	},
	{
		kind:     KindInjectionSystemMarker,
		severity: SeverityWarn,
		class:    classInjection,
		re:       regexp.MustCompile(`(?i)(^|\n)\s*(system|\[system\]|<system>)\s*:`),
	},
	{
		kind:     KindAdversarialURL,
		severity: SeverityWarn,
		class:    classSecret,
		re:       regexp.MustCompile(`(?i)https?://[^\s"'<>]+[?&](token|api[_-]?key|password|secret|auth)=`),
	},
	{
		kind:     KindAdversarialURL,
		severity: SeverityHigh,
		class:    classSecret,
		re:       regexp.MustCompile(`(?i)data:text/(html|javascript|plain);[^,\s]+,`),
	},
	{
		// Base64 blob detector. The character class admits URL
		// path/query bleed-over (Telegram users reported the
		// 200+ "encoded_payload" footer on long scraper URLs);
		// the verify hook filters those out when the match is part
		// of an http(s) URL token.
		kind:     KindEncodedPayload,
		severity: SeverityInfo,
		class:    classSecret,
		re:       regexp.MustCompile(`[A-Za-z0-9+/]{200,}={0,2}`),
		verify:   encodedPayloadIsRealBlob,
	},
	{
		// Hex blob detector. Bumped 60 → 96 chars: legitimate
		// hashes are typically 40 (sha1) or 64 (sha256) chars
		// and don't trip 96. 96+ catches multi-hash
		// concatenations and the smuggling cases we care about.
		// Case-sensitive (lowercase only) because hex by
		// convention is lowercase — case-insensitive matching
		// caught long capital-letter runs (e.g. all-A base64
		// FPs) that the base64 rule's verify hook had just
		// dropped.
		kind:     KindEncodedPayload,
		severity: SeverityInfo,
		class:    classSecret,
		re:       regexp.MustCompile(`\b[a-f0-9]{96,}\b`),
	},
}

// ScanWithProvenance runs the rule table against content,
// optionally skipping injection-class rules for first-party
// content. Secret-class rules always run regardless of
// provenance. ProvenanceUnknown and ProvenanceThirdParty both
// run the full rule set (fail-safe: unknown treated as
// third-party).
//
// Empty content returns an empty Report. Idempotent and
// concurrency-safe — the rules table is read-only after init.
func ScanWithProvenance(content string, prov Provenance) Report {
	if content == "" {
		return Report{}
	}
	var findings []Finding
	for _, r := range rules {
		// Skip injection-class rules for first-party content.
		// Secret-class rules (credential leakage, encoded
		// payloads) run regardless of provenance.
		if prov == ProvenanceFirstParty && r.class == classInjection {
			continue
		}
		matches := r.re.FindAllStringIndex(content, -1)
		for _, m := range matches {
			start, end := m[0], m[1]
			if r.verify != nil && !r.verify(content, start, end) {
				continue
			}
			evidence := content[start:end]
			if len(evidence) > 200 {
				evidence = evidence[:200] + "…"
			}
			findings = append(findings, Finding{
				Kind:     r.kind,
				Severity: r.severity,
				Start:    start,
				End:      end,
				Evidence: evidence,
			})
		}
	}
	return Report{Findings: findings}
}

// Scan runs every rule against the content and returns a
// Report with the matches. Empty content returns an empty
// Report. Idempotent and concurrency-safe — the rules table
// is read-only after init.
//
// Scan is equivalent to ScanWithProvenance(content,
// ProvenanceThirdParty) — it runs the full rule set. All
// existing callers retain their current full-scan behaviour.
func Scan(content string) Report {
	return ScanWithProvenance(content, ProvenanceThirdParty)
}

// Redact returns a copy of content with every HIGH-severity
// finding's span replaced by a typed marker. INFO / WARN
// spans are LEFT AS-IS — operators want to see the context
// for those, just with a warning banner.
//
// The marker shape is "[REDACTED:<kind>]" so downstream
// consumers can still distinguish what was removed. Spans
// are processed back-to-front so earlier offsets stay valid
// while we rewrite later ones.
func Redact(content string, report Report) string {
	if !report.HasFinding() {
		return content
	}
	type span struct {
		start, end int
		marker     string
	}
	highSpans := make([]span, 0)
	for _, f := range report.Findings {
		if f.Severity != SeverityHigh {
			continue
		}
		highSpans = append(highSpans, span{
			start:  f.Start,
			end:    f.End,
			marker: fmt.Sprintf("[REDACTED:%s]", f.Kind),
		})
	}
	if len(highSpans) == 0 {
		return content
	}
	// Sort by start desc so back-to-front splicing keeps the
	// earlier indices valid.
	for i := 1; i < len(highSpans); i++ {
		for j := i; j > 0 && highSpans[j].start > highSpans[j-1].start; j-- {
			highSpans[j], highSpans[j-1] = highSpans[j-1], highSpans[j]
		}
	}
	var b strings.Builder
	b.Grow(len(content))
	b.WriteString(content)
	out := b.String()
	for _, s := range highSpans {
		// Don't redact spans that have already been
		// overlapped by a later splice (shouldn't happen
		// given non-overlapping regex matches, but cheap
		// defense).
		if s.start < 0 || s.end > len(out) || s.start >= s.end {
			continue
		}
		out = out[:s.start] + s.marker + out[s.end:]
	}
	return out
}

// FormatBanner returns a short operator-facing summary of the
// report suitable for inline display next to a tool result.
// Empty when no findings. Sorted by severity (high first) so
// the most important signal anchors the line.
func FormatBanner(report Report) string {
	if !report.HasFinding() {
		return ""
	}
	bySev := map[Severity]int{}
	for _, f := range report.Findings {
		bySev[f.Severity]++
	}
	parts := []string{}
	if n := bySev[SeverityHigh]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d high-risk", n))
	}
	if n := bySev[SeverityWarn]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d warning", n))
	}
	if n := bySev[SeverityInfo]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d info", n))
	}
	return "Output guard: " + strings.Join(parts, ", ") + " finding(s) in tool result"
}
