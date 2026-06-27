// Package intentjudge evaluates tool-call intent before
// execution. The 2026.7.0 F11 ships the heuristic (rule-table)
// tier; the LLM tier — Turnstone calls it "semantic evaluation
// on a daemon thread with read-only tool access" — lands in a
// follow-up.
//
// Risk taxonomy (Turnstone-aligned):
//
//	Critical → recommend Deny. Destructive patterns the
//	           operator almost certainly didn't intend.
//	           Examples: rm -rf /, chmod 777 /, curl | sh
//	           pipe-to-shell.
//	High     → recommend Review. Sudo-elevation, secret
//	           reads, raw SQL DDL, credential files.
//	Medium   → recommend Review. Interpreter execution,
//	           cloud-CLI mutations, package installs.
//	Low      → recommend Approve. Read-only bash, search,
//	           file inspection, list operations.
//
// Each rule carries a base risk + confidence (0.0–1.0)
// that the rule's pattern alone is enough to justify the
// recommendation. The aggregator picks the highest-risk
// match; ties promote the highest-confidence rule.
//
// Performance: 30-ish compiled regexes; sub-millisecond per
// evaluation. Sits on the tool-call approval path.

package intentjudge

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// Risk is the four-tier ordinal scale.
type Risk string

const (
	RiskCritical Risk = "critical"
	RiskHigh     Risk = "high"
	RiskMedium   Risk = "medium"
	RiskLow      Risk = "low"
)

// rank lets the aggregator pick the "highest-risk" rule.
// Bigger int = more severe. Unknown values rank lower than
// Low so a typo never wins.
func rank(r Risk) int {
	switch r {
	case RiskCritical:
		return 4
	case RiskHigh:
		return 3
	case RiskMedium:
		return 2
	case RiskLow:
		return 1
	}
	return 0
}

// Recommendation is the operator-facing action verb derived
// from the verdict's risk level + confidence. The judge
// surface (UI / Telegram banner) renders this as a coloured
// pill next to the tool-call preview.
type Recommendation string

const (
	RecommendDeny    Recommendation = "deny"
	RecommendReview  Recommendation = "review"
	RecommendApprove Recommendation = "approve"
)

// Tier marks where a verdict came from. "heuristic" is what
// this package produces; "llm" is reserved for the follow-up
// async-LLM tier. Persisting both lets calibration analyses
// compare heuristic-vs-LLM agreement over time.
type Tier string

const (
	TierHeuristic Tier = "heuristic"
	TierLLM       Tier = "llm"
)

// Verdict is one judge result.
type Verdict struct {
	// Tool is the function name being evaluated.
	Tool string
	// IntentSummary is a one-sentence operator-facing
	// description of what the tool is about to do. The
	// heuristic tier produces a brief composed string; the
	// LLM tier overrides with semantic phrasing.
	IntentSummary string
	// Risk + Confidence form the ranked output.
	Risk       Risk
	Confidence float64
	// Recommendation is the verb (deny / review / approve).
	Recommendation Recommendation
	// Reasoning is the human-readable "why". Heuristic
	// verdicts list every matched rule name.
	Reasoning string
	// Evidence is the matched substring(s) — useful when
	// the operator wants to see precisely what tripped the
	// rule.
	Evidence []string
	// Tier records the producer (heuristic / llm). Set by
	// EvaluateHeuristic; the LLM tier overwrites.
	Tier Tier
	// LatencyMs is the wall-clock cost of the evaluation,
	// kept so calibration can compare tiers.
	LatencyMs int64
}

// rule is one entry in the heuristic table.
type rule struct {
	// name is the operator-facing rule identifier surfaced
	// in Verdict.Reasoning. Stable across releases so audit
	// trails stay correlatable.
	name string
	// tool matches the tool function name (exact, lowercase).
	// "*" matches any tool — used for cross-tool argument
	// patterns like "pipe to shell" that show up under
	// multiple tools (bash, web_fetch, etc.).
	tool string
	// argPattern (optional) is a regex that must match the
	// concatenated argument JSON for the rule to fire. Nil
	// means tool-name match alone is enough.
	argPattern *regexp.Regexp
	// risk + confidence are the verdict the rule produces.
	risk       Risk
	confidence float64
}

// rules is the compiled heuristic table. Ordered by approximate
// expected match frequency for hot-path latency.
var rules = []rule{
	// --- CRITICAL: destructive patterns ---
	//
	// Rule regexes match against the raw JSON-encoded arguments,
	// so the leading anchor accepts the JSON value-quote `"` as
	// a command separator alongside the shell-native ones.
	{
		name: "bash_rm_rf_root", tool: "bash",
		// `rm -rf` (any combination of r/f flags) is intrinsically
		// dangerous regardless of the target path — even
		// `rm -rf temp/` recurses force-delete. Cover the long
		// form too.
		argPattern: regexp.MustCompile(`(?i)\brm\s+(-[a-zA-Z]*(rf|fr)\b|-r(ecursive)?\s+-f(orce)?\b|-f(orce)?\s+-r(ecursive)?\b|--recursive\s+--force|--force\s+--recursive)`),
		risk:       RiskCritical, confidence: 0.95,
	},
	{
		name: "bash_pipe_to_shell", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)(curl|wget)\s+[^|]+\|\s*(sh|bash|zsh|python3?)\b`),
		risk:       RiskCritical, confidence: 0.92,
	},
	{
		name: "bash_chmod_world_root", tool: "bash",
		argPattern: regexp.MustCompile(`\bchmod\s+(777|666|a\+w)\s+/`),
		risk:       RiskCritical, confidence: 0.90,
	},
	{
		name: "bash_dd_to_device", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)\bdd\b[^|]*\bof=/dev/(sd[a-z]|nvme|disk)`),
		risk:       RiskCritical, confidence: 0.95,
	},

	// --- HIGH: elevation, secret reads, DDL, credential files ---
	{
		name: "bash_sudo", tool: "bash",
		// The JSON wrapping puts a `"` before the command. Plus
		// shell-native separators so multi-step commands still
		// fire when the sudo lives downstream of `cd / && …`.
		argPattern: regexp.MustCompile(`(?i)(^|["\s;|&])sudo(\s|$)`),
		risk:       RiskHigh, confidence: 0.80,
	},
	{
		name: "bash_sql_ddl", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)\b(DROP|TRUNCATE|ALTER)\s+(TABLE|DATABASE|SCHEMA)\b`),
		risk:       RiskHigh, confidence: 0.85,
	},
	{
		name: "file_read_credentials", tool: "read_file",
		argPattern: regexp.MustCompile(`(?i)(\.env|/etc/passwd|/etc/shadow|\.aws/credentials|\.ssh/id_|\.netrc|\.kube/config|credentials\.json)`),
		risk:       RiskHigh, confidence: 0.85,
	},
	{
		name: "bash_read_credentials", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)\b(cat|less|head|tail)\s+[^\n]*?(\.env|/etc/passwd|/etc/shadow|\.aws/credentials|\.ssh/id_|\.netrc|\.kube/config)`),
		risk:       RiskHigh, confidence: 0.82,
	},

	// --- MEDIUM: interpreter exec, cloud mutations, installs ---
	{
		name: "bash_interpreter", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)\b(python3?|node|ruby|perl)\s+(-c|-e)\s+`),
		risk:       RiskMedium, confidence: 0.70,
	},
	{
		name: "bash_package_install", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)\b(apt|apt-get|yum|dnf|brew|pip|npm|gem|cargo|go)\s+(install|add|get)\b`),
		risk:       RiskMedium, confidence: 0.75,
	},
	{
		name: "bash_cloud_mutation", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)\b(aws|gcloud|az|kubectl|terraform)\s+(rm|delete|destroy|apply)\b`),
		risk:       RiskMedium, confidence: 0.80,
	},

	// --- LOW: safe defaults ---
	{
		name: "tool_search", tool: "tool_search",
		risk: RiskLow, confidence: 0.95,
	},
	{
		name: "memory_search", tool: "memory_search",
		risk: RiskLow, confidence: 0.95,
	},
	{
		name: "read_file_default", tool: "read_file",
		risk: RiskLow, confidence: 0.85, // overridden by credential-pattern rule above
	},
	{
		name: "list_artifacts", tool: "list_artifacts",
		risk: RiskLow, confidence: 0.95,
	},
	{
		name: "get_task_status", tool: "get_task_status",
		risk: RiskLow, confidence: 0.95,
	},
	{
		name: "bash_read_only", tool: "bash",
		argPattern: regexp.MustCompile(`(?i)^\s*(ls|pwd|whoami|hostname|date|uname|echo|cat\s+/var/log|grep|find\s+[^-]*-name)\b`),
		risk:       RiskLow, confidence: 0.70,
	},
}

// EvaluateHeuristic runs the rule table against (tool, argsJSON)
// and returns a Verdict. argsJSON is the raw JSON-encoded
// arguments — passed verbatim so rules can match on full text
// without per-rule deserialisation. tool is matched
// case-insensitively against rule.tool (or the wildcard "*").
//
// When multiple rules fire, the verdict carries the highest-
// rank risk; ties prefer the highest confidence. Reasoning
// concatenates the matched rule names so the audit trail
// preserves the full set.
//
// No-rule-match returns a default Low / Approve verdict
// because the heuristic tier has no opinion. The LLM tier
// (when wired) is expected to upgrade these when needed.
func EvaluateHeuristic(tool, argsJSON string) Verdict {
	start := time.Now()
	toolLower := strings.ToLower(strings.TrimSpace(tool))
	matched := make([]rule, 0, 2)
	for _, r := range rules {
		if r.tool != "*" && r.tool != toolLower {
			continue
		}
		if r.argPattern != nil && !r.argPattern.MatchString(argsJSON) {
			continue
		}
		matched = append(matched, r)
	}
	// Sort matched rules by (risk desc, confidence desc).
	sort.SliceStable(matched, func(i, j int) bool {
		if rank(matched[i].risk) != rank(matched[j].risk) {
			return rank(matched[i].risk) > rank(matched[j].risk)
		}
		return matched[i].confidence > matched[j].confidence
	})

	var v Verdict
	v.Tool = tool
	v.Tier = TierHeuristic
	if len(matched) == 0 {
		v.Risk = RiskLow
		v.Confidence = 0.50
		v.Recommendation = RecommendApprove
		v.Reasoning = "no rule fired"
		v.IntentSummary = "Call " + tool
	} else {
		top := matched[0]
		v.Risk = top.risk
		v.Confidence = top.confidence
		v.Recommendation = recommendation(top.risk)
		v.IntentSummary = describeIntent(tool, top.name)
		names := make([]string, 0, len(matched))
		evidence := make([]string, 0, len(matched))
		for _, m := range matched {
			names = append(names, m.name)
			if m.argPattern != nil {
				if hit := m.argPattern.FindString(argsJSON); hit != "" {
					if len(hit) > 200 {
						hit = hit[:200] + "…"
					}
					evidence = append(evidence, hit)
				}
			}
		}
		v.Reasoning = "matched rules: " + strings.Join(names, ", ")
		v.Evidence = evidence
	}
	v.LatencyMs = time.Since(start).Milliseconds()
	return v
}

// recommendation maps a risk tier to the operator-facing
// action verb. Critical denies; High + Medium ask for
// review; Low approves.
func recommendation(r Risk) Recommendation {
	switch r {
	case RiskCritical:
		return RecommendDeny
	case RiskHigh, RiskMedium:
		return RecommendReview
	default:
		return RecommendApprove
	}
}

// describeIntent produces a short operator-facing summary of
// what the tool is about to do, using the matched rule's
// name as a hint. Coarse — the LLM tier will replace these
// with semantic phrasing — but enough for the heuristic-only
// path to render a sensible banner.
func describeIntent(tool, ruleName string) string {
	switch {
	case strings.Contains(ruleName, "rm_rf"):
		return "Delete files recursively from " + tool
	case strings.Contains(ruleName, "pipe_to_shell"):
		return "Download remote content and execute as shell"
	case strings.Contains(ruleName, "chmod_world"):
		return "Grant world-writable permissions on a path"
	case strings.Contains(ruleName, "dd_to_device"):
		return "Write raw bytes to a block device"
	case strings.Contains(ruleName, "sudo"):
		return "Run a command with elevated privileges"
	case strings.Contains(ruleName, "sql_ddl"):
		return "Mutate database schema (DROP/TRUNCATE/ALTER)"
	case strings.Contains(ruleName, "credentials"):
		return "Read a credential-bearing file"
	case strings.Contains(ruleName, "interpreter"):
		return "Execute interpreter code from the command line"
	case strings.Contains(ruleName, "package_install"):
		return "Install a software package"
	case strings.Contains(ruleName, "cloud_mutation"):
		return "Mutate cloud infrastructure"
	case strings.Contains(ruleName, "read_only"):
		return "Run a read-only " + tool + " command"
	default:
		return "Call " + tool
	}
}
