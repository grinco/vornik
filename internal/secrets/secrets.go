// Package secrets detects and redacts sensitive values (API keys,
// access tokens, private keys, connection strings) flowing through
// vornik's persistence and display surfaces.
//
// Why it exists: agents in vornik run with broad credential exposure
// (project API keys, LLM gateway tokens, MCP secrets in env). A
// careless `printenv` in the bash tool, an accidentally-committed
// .env in a markdown blob, or a curl with an Authorization header
// in a tool argument can leak credentials into multiple durable
// stores at once — result.json, tool_audit_log, artifact storage,
// container logs, project memory. By the time an operator notices
// the leak in the dashboard or a Telegram message, the secret has
// already been written to several places.
//
// The detector layer is intentionally regex-heavy with an entropy
// fallback: regexes catch known-shape tokens (AWS, GitHub PATs,
// JWTs, etc.) with high precision; entropy catches custom or
// vendor-private formats the regex list doesn't know about.
// Operators tune via configs/secrets.yaml: add custom patterns,
// extend the allowlist for known-safe high-entropy strings (git
// SHAs, base64-encoded markdown images), and pick per-checkpoint
// action policy.
//
// Performance: every scan is on the persistence hot path so
// regexes compile once at construction, the entropy pass uses a
// fixed-size byte counter, and short bodies (under 16 chars) skip
// scanning entirely. A typical result.json (a few KB) scans in
// under a millisecond on a modern CPU.
package secrets

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Action declares what a checkpoint does on detection. Each
// persistence/display boundary picks a default; operators override
// via config.
type Action string

const (
	// ActionDetect logs + emits a metric but doesn't modify the
	// payload. Used for audit-class data (tool_audit_log) where
	// silent rewrites would defeat the purpose of the audit.
	ActionDetect Action = "detect"
	// ActionRedact substitutes each finding with a typed marker
	// before persistence/display. Default for most channels —
	// preserves the structural shape (so JSON parsers, gates,
	// downstream agents still work) while removing the secret.
	ActionRedact Action = "redact"
	// ActionBlock refuses the write. Used for inbound channels
	// (webhook payloads) where a payload containing a secret is
	// likely a misconfiguration the operator wants to know about
	// rather than silently pass through.
	ActionBlock Action = "block"
)

// IsValid reports whether the string is a known action. Unknown
// values coerce to ActionDetect at config load (a typo must not
// silently disable enforcement).
func (a Action) IsValid() bool {
	return a == ActionDetect || a == ActionRedact || a == ActionBlock
}

// Checkpoint names — string constants so call sites and config
// stay in sync. Adding a new checkpoint means: define a constant
// here, add a default action to DefaultCheckpoints, wire the call
// site, and document in configs/secrets.yaml.
const (
	CheckpointResultJSON    = "result_json"
	CheckpointToolAudit     = "tool_audit"
	CheckpointContainerLogs = "container_logs"
	CheckpointArtifacts     = "artifacts"
	CheckpointTelegram      = "telegram"
	CheckpointWebhook       = "webhook"
	CheckpointMemory        = "memory"
)

// DefaultCheckpoints returns the per-channel default action map.
// Picked deliberately:
//   - result_json: Redact — agent's primary output channel; agents
//     have been observed echoing env vars in result.message.
//   - tool_audit: Redact — tool_input/tool_output are persisted durably in
//     tool_audit_log, so Detect (the prior default) left live credentials at
//     rest there. Redaction substitutes a TYPED marker ([REDACTED:<type>]),
//     which preserves the surrounding I/O context AND the finding's
//     type/location for forensics — strictly more useful for audit than a
//     raw secret, and without the credential-leak liability. Operators who
//     genuinely need raw retention can set `secrets.checkpoints.tool_audit:
//     detect` explicitly.
//   - container_logs: Redact — last-50-lines tail surfaces on the
//     failed-task UI; redaction at read time leaves the raw
//     container log untouched.
//   - artifacts: Redact — durable storage indexed by memory.
//   - telegram: Redact — the highest-visibility exfiltration vector
//     once a bad string has slipped past upstream layers.
//   - webhook: Block — inbound channel; a payload containing a
//     secret usually indicates a misconfiguration the operator
//     should fix at the source rather than have us silently rewrite.
//   - memory: Redact — chunks live forever and surface via search.
func DefaultCheckpoints() map[string]Action {
	return map[string]Action{
		CheckpointResultJSON:    ActionRedact,
		CheckpointToolAudit:     ActionRedact,
		CheckpointContainerLogs: ActionRedact,
		CheckpointArtifacts:     ActionRedact,
		CheckpointTelegram:      ActionRedact,
		CheckpointWebhook:       ActionBlock,
		CheckpointMemory:        ActionRedact,
	}
}

// ResolveAction returns the action for `checkpoint`, falling back
// to the compiled default. Unknown action strings (config typos)
// coerce to ActionDetect — the operator sees the finding in logs
// without a silent enforcement disable.
//
// The MEMORY checkpoint is special: memory chunks live forever and surface
// via search, so admitting plaintext credentials there is the worst leak.
// Secret scanning on that checkpoint is therefore NON-DISABLEABLE — a
// `detect` override (which would admit plaintext) is clamped up to `redact`
// unless an operator sets the explicit escape hatch VORNIK_ALLOW_UNSCANNED_MEMORY.
// (secrets.enabled=false still removes the detector entirely; that bypass is
// surfaced by a startup warning in the config loader, not here.)
func ResolveAction(checkpoint string, override map[string]Action) Action {
	a := resolveActionRaw(checkpoint, override)
	if checkpoint == CheckpointMemory && a == ActionDetect && !allowUnscannedMemory() {
		return ActionRedact
	}
	return a
}

func resolveActionRaw(checkpoint string, override map[string]Action) Action {
	if a, ok := override[checkpoint]; ok && a.IsValid() {
		return a
	}
	if a, ok := DefaultCheckpoints()[checkpoint]; ok {
		return a
	}
	return ActionDetect
}

// allowUnscannedMemory is the explicit, deliberate escape hatch for the
// memory-checkpoint secret-scan floor. Off by default.
func allowUnscannedMemory() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VORNIK_ALLOW_UNSCANNED_MEMORY"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Finding describes one match the detector produced. Type is the
// pattern's symbolic name ("aws_access_key", "github_pat",
// "entropy") used for logging and the redaction marker. Match is
// the matched substring; Start/End are byte offsets into the
// scanned text. Findings are emitted in offset order so a single
// pass of Redact can substitute non-overlapping ranges.
type Finding struct {
	Type  string
	Match string
	Start int
	End   int
}

// FindingTypeEntropy is the Type value for high-entropy (Shannon)
// matches — the only finding class that legitimately collides with
// hash/timestamp-bearing path segments. Deterministic prefix-anchored
// patterns (aws_*, github_pat, jwt, generic_kv, …) never occur in a
// real filesystem path, so only entropy findings are path-exempt; see
// internal/executor/secrets_scan.go filterFindingsOutsidePathSpans.
const FindingTypeEntropy = "entropy"

// Detector is the abstraction every checkpoint consumes. The
// production implementation is *MultiDetector; tests can stub.
type Detector interface {
	Scan(text []byte) []Finding
}

// Pattern is one regex-driven detection rule. Name flows into the
// Finding.Type and the redaction marker; Regex must be RE2-
// compatible (no backreferences); Description is operator-facing
// docstring shown in `vornikctl secrets list-patterns` (deferred to
// Phase 3 but kept on the struct so the corpus doesn't change
// shape later).
type Pattern struct {
	Name        string
	Regex       string
	Description string
}

// DefaultPatterns returns the curated pattern list shipped with
// vornik. Operators extend by adding entries to
// configs/secrets.yaml's patterns.custom section — the loader
// concatenates custom + default. Disabling a default is done by
// listing the unwanted pattern name in patterns.disable.
//
// Pattern selection criteria: high precision (each regex catches
// only the shape it claims to catch), pinned to documented
// vendor formats where possible, no backreferences (RE2
// constraint). Entropy detection covers the long tail.
func DefaultPatterns() []Pattern {
	return []Pattern{
		// Cloud provider keys — vendor-published prefixes.
		{
			Name:        "aws_access_key",
			Regex:       `\bAKIA[0-9A-Z]{16}\b`,
			Description: "AWS Access Key ID",
		},
		{
			Name:        "aws_secret_key",
			Regex:       `(?i)aws(.{0,20})?(secret|sk)(.{0,20})?['"]?([A-Za-z0-9/+=]{40})['"]?`,
			Description: "AWS Secret Access Key (40-char base64-shaped value following an aws_secret label)",
		},
		{
			Name:        "aws_session_token",
			Regex:       `\bASIA[0-9A-Z]{16}\b`,
			Description: "AWS temporary session token prefix",
		},
		{
			Name:        "google_api_key",
			Regex:       `\bAIza[0-9A-Za-z\-_]{35}\b`,
			Description: "Google API key",
		},
		// Source-control PATs — vendor-documented prefixes.
		{
			Name:        "github_pat",
			Regex:       `\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`,
			Description: "GitHub personal/OAuth/server/refresh token",
		},
		{
			Name:        "gitlab_pat",
			Regex:       `\bglpat-[A-Za-z0-9_-]{20}\b`,
			Description: "GitLab personal access token",
		},
		// Chat/collaboration tokens.
		{
			Name:        "slack_token",
			Regex:       `\bxox[baprs]-[A-Za-z0-9-]{10,}\b`,
			Description: "Slack API token (bot/app/user/legacy/refresh)",
		},
		// LLM provider keys — common in our deployment.
		{
			Name:        "openai_key",
			Regex:       `\bsk-[A-Za-z0-9]{32,}\b`,
			Description: "OpenAI API key",
		},
		{
			Name:        "anthropic_key",
			Regex:       `\bsk-ant-[A-Za-z0-9_-]{32,}\b`,
			Description: "Anthropic API key",
		},
		// Cryptographic material.
		{
			Name:        "private_key_block",
			Regex:       `-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`,
			Description: "PEM private key header (RSA/EC/DSA/OpenSSH/PGP)",
		},
		// JWTs (header.payload.signature, all base64url).
		{
			Name:        "jwt",
			Regex:       `\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`,
			Description: "JSON Web Token",
		},
		// Connection strings with embedded credentials.
		{
			Name:        "connection_string",
			Regex:       `\b(?:postgres|postgresql|mysql|mongodb|redis|amqp)://[^\s/:]+:[^\s/@]+@`,
			Description: "Database/queue URI with embedded password",
		},
		// Generic key=value style — high-recall, lower-precision.
		// Length floor of 12 keeps "key=test" / "password=x" style
		// placeholder strings out; allowlist suppresses any false
		// positives operators run into.
		{
			Name:        "generic_kv",
			Regex:       `(?i)\b(?:api[_-]?key|password|passwd|secret|token|auth(?:[_-]?token)?)\s*[:=]\s*['"]?([A-Za-z0-9!@#$%^&*()_+\-=/.]{12,})['"]?`,
			Description: "Generic <key>=<long-value> pair",
		},
	}
}

// DefaultAllowlist returns the regex patterns that suppress known-
// safe high-entropy / generic_kv hits. Each entry is RE2 syntax;
// matches anywhere in the candidate's match window suppress that
// finding. Operators extend via the secrets.yaml allowlist.
//
// Selection: we only include suppressors whose absence would cause
// real false positives at the default entropy threshold (40 chars,
// 4.5 bits). Notably absent: short git SHAs ([a-f0-9]{7,12}) — the
// entry is too aggressive (digit-only runs inside legitimate
// secrets like Slack workspace IDs match it) and isn't needed
// because short hex strings don't satisfy the entropy length floor
// or any prefix-restricted regex. Full 40-char SHAs are also
// omitted: their per-char entropy is ~4.0 (log2 of 16), below the
// 4.5 threshold, so the entropy detector won't fire on them.
//
// What's left:
//   - base64-embedded Markdown images (data: URIs)
//   - UUIDs (8-4-4-4-12 hex) — short of the entropy floor anyway,
//     but defensive in case operators tighten the entropy length
//   - common placeholder strings that LLMs emit in example prose
func DefaultAllowlist() []string {
	return []string{
		`data:image/[a-z]+;base64,`,                                        // markdown image embed prefix
		`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`, // UUID
		`(?i)example|placeholder|your[_-]?api[_-]?key|<[^>]+>`,             // documentation placeholders
	}
}

// MultiDetector composes the curated regex pass + an optional
// entropy pass + an allowlist suppression layer. Constructed once
// at startup and reused across every scan call — the regex
// compilation is the expensive part and we don't want it on the
// hot path.
type MultiDetector struct {
	patterns       []compiledPattern
	allowlist      []*regexp.Regexp
	entropyEnabled bool
	entropyMinLen  int
	entropyMinBits float64
	entropyTokenRe *regexp.Regexp
	mu             sync.RWMutex
}

type compiledPattern struct {
	name string
	re   *regexp.Regexp
}

// Config bundles the construction-time options. Zero values pick
// safe defaults (curated patterns + default allowlist + entropy
// enabled with 40-char/4.5-bit thresholds).
type Config struct {
	// Patterns to enable. Empty list uses DefaultPatterns(). Custom
	// entries from operator config get appended.
	Patterns []Pattern
	// Allowlist regexes that suppress matches. Empty uses
	// DefaultAllowlist(). Operator config appends.
	Allowlist []string
	// EntropyDisabled turns off the Shannon-entropy fallback.
	// Operators with high false-positive rates from the entropy
	// pass set this and rely on regexes alone.
	EntropyDisabled bool
	// EntropyMinLen is the minimum candidate-token length the
	// entropy detector considers. Default 40.
	EntropyMinLen int
	// EntropyMinBits is the per-character Shannon entropy floor.
	// Default 4.5 (catches typical base64-shaped tokens; lower
	// values fire on prose).
	EntropyMinBits float64
}

// NewMultiDetector compiles all regexes and returns the detector
// ready to scan. Returns an error if any custom pattern fails to
// compile — operator config typos surface at startup, not on the
// first scan.
func NewMultiDetector(cfg Config) (*MultiDetector, error) {
	patterns := cfg.Patterns
	if len(patterns) == 0 {
		patterns = DefaultPatterns()
	}
	compiled := make([]compiledPattern, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("secrets: pattern %q failed to compile: %w", p.Name, err)
		}
		compiled = append(compiled, compiledPattern{name: p.Name, re: re})
	}

	allowlist := cfg.Allowlist
	if len(allowlist) == 0 {
		allowlist = DefaultAllowlist()
	}
	allowRes := make([]*regexp.Regexp, 0, len(allowlist))
	for _, expr := range allowlist {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("secrets: allowlist entry %q failed to compile: %w", expr, err)
		}
		allowRes = append(allowRes, re)
	}

	d := &MultiDetector{
		patterns:       compiled,
		allowlist:      allowRes,
		entropyEnabled: !cfg.EntropyDisabled,
		entropyMinLen:  cfg.EntropyMinLen,
		entropyMinBits: cfg.EntropyMinBits,
	}
	if d.entropyMinLen <= 0 {
		d.entropyMinLen = 40
	}
	if d.entropyMinBits <= 0 {
		d.entropyMinBits = 4.5
	}
	// Tokens for the entropy pass: contiguous base64-ish runs.
	// Constrains the search to plausibly-secret-shaped strings so
	// we don't waste cycles on prose.
	d.entropyTokenRe = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{` + fmt.Sprintf("%d", d.entropyMinLen) + `,}`)
	return d, nil
}

// Scan returns every finding in offset order. Empty/short bodies
// short-circuit. Findings overlapping an allowlist match are
// suppressed (post-detection so a known-safe high-entropy git SHA
// doesn't fire even though the regex might match).
func (d *MultiDetector) Scan(text []byte) []Finding {
	if d == nil || len(text) < 16 {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	var findings []Finding

	// Regex pass. Each pattern yields all non-overlapping matches.
	for _, p := range d.patterns {
		for _, m := range p.re.FindAllIndex(text, -1) {
			// generic_kv has a captured group for the value;
			// callers care about the value bytes, not the
			// label+separator preamble, so prefer the inner
			// group when it exists. Other patterns have no
			// group and m[0]/m[1] cover the whole match.
			start, end := m[0], m[1]
			if p.name == "generic_kv" {
				if sub := p.re.FindSubmatchIndex(text[start:end]); len(sub) >= 4 && sub[2] >= 0 {
					start, end = start+sub[2], start+sub[3]
				}
			}
			findings = append(findings, Finding{
				Type:  p.name,
				Match: string(text[start:end]),
				Start: start,
				End:   end,
			})
		}
	}

	// Entropy pass. Only fire when no regex pattern already
	// covered this byte range — saves duplicate findings on the
	// same secret (e.g. a long base64-ish string that already
	// matched openai_key shouldn't also fire entropy).
	if d.entropyEnabled {
		regexSpans := spanIndex(findings)
		for _, m := range d.entropyTokenRe.FindAllIndex(text, -1) {
			start, end := m[0], m[1]
			if regexSpans.overlaps(start, end) {
				continue
			}
			tok := text[start:end]
			if shannonEntropy(tok) < d.entropyMinBits {
				continue
			}
			findings = append(findings, Finding{
				Type:  "entropy",
				Match: string(tok),
				Start: start,
				End:   end,
			})
		}
	}

	// Allowlist suppression. Apply after the regex+entropy passes
	// so operators can write narrow allow rules ("this specific
	// token is fine") without having to disable whole patterns.
	if len(d.allowlist) > 0 && len(findings) > 0 {
		filtered := findings[:0]
		for _, f := range findings {
			if d.allowed(text[f.Start:f.End]) {
				continue
			}
			filtered = append(filtered, f)
		}
		findings = filtered
	}

	// Stable order by Start so Redact's left-to-right pass works.
	// Equal Start (regex + entropy at the same offset is rare
	// because of the overlap guard above, but defensive) breaks
	// on End so longer matches win — Redact replaces in span
	// order and a wider match should consume the narrower.
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Start != findings[j].Start {
			return findings[i].Start < findings[j].Start
		}
		return findings[i].End > findings[j].End
	})

	// De-duplicate exact-overlap findings (same Start AND End)
	// so two patterns matching the same bytes only emit one
	// finding. The earlier-listed pattern wins by virtue of
	// being inserted first.
	if len(findings) > 1 {
		dedup := findings[:1]
		for i := 1; i < len(findings); i++ {
			prev := dedup[len(dedup)-1]
			if findings[i].Start == prev.Start && findings[i].End == prev.End {
				continue
			}
			dedup = append(dedup, findings[i])
		}
		findings = dedup
	}

	return findings
}

// allowed reports whether `match` is suppressed by any allowlist
// regex. A match anywhere in the bytes (not just full-match)
// suppresses — short SHAs inside longer strings are still safe.
func (d *MultiDetector) allowed(match []byte) bool {
	for _, re := range d.allowlist {
		if re.Match(match) {
			return true
		}
	}
	return false
}

// Span is a half-open byte range [Start, End) used by callers that
// need to filter findings against context-specific exclusions. See
// internal/executor.extractPathFieldSpans for the canonical
// consumer — file paths inside result.json must not be redacted
// because downstream verification stats the literal path on disk.
type Span struct {
	Start int
	End   int
}

// Redact applies findings to `text`, replacing each finding with
// the typed marker `[REDACTED:<type>]`. Returns the new bytes; the
// input is not modified. Findings must be in offset order (Scan's
// guarantee) and non-overlapping after Scan's dedup pass —
// callers passing hand-constructed Findings out of order may get
// truncated output.
func Redact(text []byte, findings []Finding) []byte {
	if len(findings) == 0 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	cur := 0
	for _, f := range findings {
		if f.Start < cur || f.End < f.Start || f.End > len(text) {
			// Defensive: skip malformed range rather than
			// panic. The caller's downstream sees the
			// remaining un-redacted prefix; the Scan-emitted
			// findings won't take this branch.
			continue
		}
		out.Write(text[cur:f.Start])
		out.WriteString("[REDACTED:")
		out.WriteString(f.Type)
		out.WriteString("]")
		cur = f.End
	}
	if cur < len(text) {
		out.Write(text[cur:])
	}
	return []byte(out.String())
}

// CountByType groups findings by Type and returns the per-type
// count. Used by the dashboard badge ("3 secrets redacted: 2
// openai_key + 1 entropy") so the operator sees the breakdown
// without re-scanning.
func CountByType(findings []Finding) map[string]int {
	out := make(map[string]int, len(findings))
	for _, f := range findings {
		out[f.Type]++
	}
	return out
}

// shannonEntropy computes per-character entropy in bits. Used by
// the entropy detector. Empty input returns 0.
func shannonEntropy(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range b {
		counts[c]++
	}
	n := float64(len(b))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// spanList is a small auxiliary used during scan to reject
// entropy candidates that overlap a regex-pass finding. Sorted on
// insert so overlap checks are linear in the number of findings —
// fine for the few-dozen findings we expect per scan.
type spanList struct {
	spans [][2]int
}

func spanIndex(findings []Finding) *spanList {
	out := &spanList{spans: make([][2]int, 0, len(findings))}
	for _, f := range findings {
		out.spans = append(out.spans, [2]int{f.Start, f.End})
	}
	sort.Slice(out.spans, func(i, j int) bool { return out.spans[i][0] < out.spans[j][0] })
	return out
}

func (s *spanList) overlaps(start, end int) bool {
	for _, sp := range s.spans {
		if sp[0] < end && start < sp[1] {
			return true
		}
		if sp[0] >= end {
			break
		}
	}
	return false
}
