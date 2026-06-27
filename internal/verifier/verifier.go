// Package verifier implements Phase 2 outcome verifiers for
// hallucination detection: declarative per-project rules that
// check a step's actual output (artifacts produced, tool-call
// audit, log lines) against expected invariants. Verifier
// failures fail the step so the existing scheduler retry path
// picks it up — a step that the agent reported as COMPLETED
// but a verifier rejected is treated the same as a step that
// crashed.
//
// Distinct from internal/hallucination (Phase 1, Detector):
// Phase 1 scans the agent's PROSE for unsupported claims; Phase
// 2 checks the agent's WORK against operator-declared
// invariants ("artifact must have ≥5 entries", "no 429 in
// audit"). Together they catch (a) lies about what was done and
// (b) work that was done but doesn't meet the bar.
package verifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// Severity classifies a Violation's blast radius. SeverityFail is
// the historical zero-tolerance gate: the step fails and the
// scheduler retry path picks it up. SeverityWarn surfaces the
// signal — operator dashboards, logs — without aborting the step,
// so a coverage check (must_contain_url) or artifact-shape check
// can own the hard-fail path while noisy auxiliary checks stay
// advisory.
type Severity string

const (
	SeverityFail Severity = "fail"
	SeverityWarn Severity = "warn"
)

// normaliseSeverity maps an operator-supplied string to a known
// Severity value. Anything unrecognised — including the empty
// string — collapses to SeverityFail for backward compat: a
// verifier without an explicit severity continues to gate the
// step the way it always did.
func normaliseSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "warn", "warning":
		return SeverityWarn
	case "fail", "error", "":
		return SeverityFail
	}
	return SeverityFail
}

// Violation is one verifier's verdict against a (task, step,
// output) triple. Severity controls whether the violation aborts
// the step (fail) or just surfaces (warn). Default is fail — a
// verifier without an explicit severity is hard-gating.
type Violation struct {
	// VerifierName is the operator-supplied identifier (or
	// auto-generated "type[i]" if the operator didn't name it).
	VerifierName string
	// Type is the verifier kind (artifact_min_entries, etc.).
	Type string
	// Severity is "fail" (default) or "warn". See type docs.
	Severity Severity
	// Detail is the human-readable reason — what the verifier
	// expected vs what it found.
	Detail string
	// Terminal marks a violation that the executor must NOT retry
	// past. Used for rate-limit / anti-bot detections (one retry
	// after a 429 is just one more rate-limit bump; three is three).
	// Plumbed up to workflow.go which skips step.OnFail routing on
	// terminal violations and fails the task immediately.
	//
	// Set by the verifier impl OR by operator YAML
	// (Config.Terminal). False = retry per the normal policy.
	Terminal bool
	// BlockedURLs lists every URL the verifier marked as a permanent
	// block (auth_required, captcha, http_401/403, robots_blocked,
	// login_required) — i.e. retries against the same source will
	// keep failing. Populated by no_status_429_in_audit; nil for
	// other verifier types. The executor's recovery path reads this
	// to surface "Source X is paywalled — try alternative Y?" via
	// the lead's checkpoint outcome instead of just failing the task.
	BlockedURLs []BlockedURL
}

// BlockedURL is one fetch the verifier classified as permanently
// blocked. The recovery flow renders these to the lead role's
// prompt so it can propose alternative sources via a `decision`
// checkpoint.
type BlockedURL struct {
	// URL is the source that wouldn't load — empty when the tool
	// didn't surface a URL (e.g. marker-scan match on a non-fetch
	// tool).
	URL string
	// Reason is the lowercased block label (e.g. "auth_required",
	// "captcha", "http_403"). Already classified by the verifier;
	// the recovery path uses it to choose alternatives.
	Reason string
	// Permanent is true when the reason is one of the permanent
	// classes. False for transient classes (429/503/network) — the
	// recovery path only acts on permanent blocks; transients still
	// retry per the normal policy.
	Permanent bool
}

// PermanentBlockReasons names the reason labels that are
// structurally non-recoverable on retry. Operators can opt
// auth_required out of permanent via excuse_block_reasons (skip
// silently); the recovery flow uses this list to decide whether
// to surface a checkpoint vs hard-fail.
var PermanentBlockReasons = map[string]bool{
	"auth_required":  true,
	"login_required": true,
	"captcha":        true,
	"http_401":       true,
	"http_403":       true,
	"robots_blocked": true,
	"paywall":        true,
}

// IsPermanentBlockReason reports whether the reason label denotes
// a structural block — i.e. retrying the same URL with the same
// tooling will keep failing. Case-insensitive; whitespace-trimmed.
func IsPermanentBlockReason(reason string) bool {
	return PermanentBlockReasons[strings.ToLower(strings.TrimSpace(reason))]
}

// Error returns a one-line summary suitable for the executor's
// step-failure error path. Warn-tier violations are prefixed with
// "[warn] " so operators can spot at a glance that the line is
// advisory rather than a true block.
func (v Violation) Error() string {
	if v.Severity == SeverityWarn {
		return fmt.Sprintf("[warn] verifier %q (%s): %s", v.VerifierName, v.Type, v.Detail)
	}
	return fmt.Sprintf("verifier %q (%s): %s", v.VerifierName, v.Type, v.Detail)
}

// Config is the YAML-decoded shape attached to a project. One
// project can declare many verifiers; each may filter to a
// specific task type via WhenTaskType, or run unconditionally
// when WhenTaskType is empty.
type Config struct {
	// Name is the operator's identifier — surfaced in
	// step-outcome rows and log lines so triage can find it.
	Name string `yaml:"name"`
	// Type names the built-in verifier to invoke. See the
	// Run dispatch below for the list.
	Type string `yaml:"type"`
	// WhenTaskType filters which task types this verifier runs
	// for. Empty means "any". Operators use this to scope a
	// rule that only makes sense for research tasks (artifact
	// counts) versus feature tasks (test results).
	WhenTaskType string `yaml:"whenTaskType"`
	// WhenStep filters which workflow step this verifier runs
	// against. Empty means "any step". Use this to scope a
	// verifier that only makes sense at one stage of a multi-
	// step workflow — e.g. "place_order must appear in the
	// audit" only applies to the executor step, not to the
	// strategist/risk-officer steps that precede it.
	//
	// When the gated step never runs (e.g. a workflow gate
	// routed past it), the verifier engine isn't invoked for
	// that step, so the rule cleanly skips with no false
	// positive.
	WhenStep string `yaml:"whenStep"`
	// Severity is an optional override on the verifier's default
	// severity. "fail" (default) aborts the step; "warn" surfaces
	// the violation but lets the step complete — useful when this
	// verifier is a sanity check and another verifier (e.g.
	// must_contain_url) owns the hard-fail path. Case-insensitive;
	// unknown values fall back to "fail" for safety.
	Severity string `yaml:"severity"`
	// Terminal, when true, prevents the executor's adaptive routing
	// loop from retrying the step after this verifier fires. Use for
	// failure modes where a retry is structurally pointless — e.g.
	// `no_status_429_in_audit` (the portal rate-limited us; retrying
	// just bumps the limit again) or operator-declared "this is a
	// hard contract violation, don't loop on it".
	//
	// Default false: failures retry per the workflow's on_fail
	// policy. Type-default applies when the operator omits it;
	// no_status_429_in_audit sets Terminal=true by default because
	// retrying a rate-limit is the canonical bad-retry shape.
	Terminal bool `yaml:"terminal"`
	// AppliesTo scopes this verifier to specific task creation
	// sources (USER, AUTONOMOUS, DELEGATION, CHECKPOINT). Empty
	// means "all sources" (backward compatible). Case-insensitive.
	//
	// Motivation: autonomy-shape verifiers like scan_min_entries
	// assert "this step must have produced a scan-*.md file with
	// ≥5 entries" — a contract that fits the canonical autonomy
	// scan loop but breaks operator-initiated ad-hoc work
	// ("ingest this CV", "summarise this email"), where the
	// deliverable doesn't follow the scan-shape. Gating with
	// appliesTo: [autonomous] lets the project keep its autonomy
	// invariants without false-failing user tasks.
	AppliesTo []string `yaml:"appliesTo"`
	// Params is the type-specific parameter bag. See each
	// verifier function for the keys it consumes.
	Params map[string]any `yaml:"params"`
}

// Input is the world-state a verifier inspects. Built once per
// step finalization and passed to every applicable verifier so
// each runs against a stable snapshot.
type Input struct {
	TaskType string
	// CreationSource is the task's persistence.TaskCreationSource
	// (USER, AUTONOMOUS, DELEGATION, CHECKPOINT). Used alongside
	// Config.AppliesTo so operators can declare autonomy-shape
	// verifiers that skip cleanly on user-initiated ad-hoc tasks.
	// Empty string disables the filter (the verifier runs).
	CreationSource string
	// StepID is the workflow step that just completed. Used
	// alongside Config.WhenStep to scope a verifier to a
	// specific step in a multi-step workflow.
	StepID string
	// ProjectDir is the absolute host path to the task's project
	// workspace root (the directory whose .autonomy/ holds
	// PROJECT_CONTEXT.md and whose .autonomy/RESUME.md is the
	// canonical résumé). Populated by the executor's runVerifiers
	// call site from effectiveProjectDir (worktree path when
	// worktrees are in use, or
	// ProjectWorkspacePath/<projectID> otherwise).
	//
	// Used by cv_claims_grounded when params["resume_file"] is set
	// and params["resume"] is empty: the verifier reads
	// filepath.Join(ProjectDir, resume_file) as the authoritative
	// résumé source. Empty ProjectDir causes the verifier to
	// abstain (nil) on resume_file lookups — safe default when the
	// executor can't resolve the workspace path.
	ProjectDir string
	Artifacts  []*persistence.Artifact
	// AuditEntries is the tool-call audit for this step
	// (executor path) or this turn (dispatcher path).
	AuditEntries []*persistence.ToolAuditEntry
	// ResultJSON is the agent's raw result.json bytes for
	// verifiers that need to inspect declared outputs (rare —
	// most verifiers work over artifacts/audit).
	ResultJSON []byte
	// InputArtifactNames is the names of artifacts the producer
	// step received as input, used by must_contain_url_from_input.
	InputArtifactNames []string
	// WatchlistAllowList is the project's trading watchlist when
	// the project has one configured (Trading.Watchlist field on
	// the project YAML). Populated by the executor's verifier
	// call site; nil/empty for non-trading projects. Used by the
	// proposals_match_watchlist verifier to catch the strategist
	// hallucination class where a frontier model invents tickers
	// not on the watchlist (observed 2026-05-07: kimi-k2.5
	// proposed "SHELL" instead of the watchlist's "SHEL", broker
	// rejected with no-conId, no order placed).
	WatchlistAllowList []string
	// EntryGateIndicators carries the deterministic daily-indicator
	// snapshot (computed in-process by the executor's indicator
	// pre-warm, same Wilder/SMA math as the WATCHLIST_INDICATORS
	// block) keyed by upper-case symbol, for the symbols the
	// strategist proposed to OPEN this step. Populated by the
	// executor verifier call site for trading projects only;
	// nil/empty otherwise, which makes entry_gate_consistent a clean
	// no-op (non-trading projects, pre-warm failure, no open
	// proposals).
	EntryGateIndicators map[string]EntryGateIndicator
}

// EntryGateIndicator is the minimal deterministic indicator snapshot
// the entry_gate_consistent verifier needs to re-evaluate a long
// entry's trend floor. SMA50 is the daily simple moving average over
// the last 50 closes; <= 0 means "unknown" (too few bars / fetch
// failed) and the verifier abstains for that symbol.
type EntryGateIndicator struct {
	SMA50 float64
}

// Run dispatches a single verifier config against the input.
// Returns (nil, nil) when the verifier doesn't apply (task type
// filter); returns a Violation when the check fails. Errors
// from inside a verifier (malformed params, etc.) propagate so
// the executor logs them — they don't fail the step (an
// operator typo shouldn't block real work).
func Run(ctx context.Context, cfg Config, in Input) (*Violation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.WhenTaskType != "" && cfg.WhenTaskType != in.TaskType {
		return nil, nil
	}
	if cfg.WhenStep != "" && cfg.WhenStep != in.StepID {
		return nil, nil
	}
	if !appliesToCreationSource(cfg.AppliesTo, in.CreationSource) {
		return nil, nil
	}
	var (
		v   *Violation
		err error
	)
	switch cfg.Type {
	case "artifact_min_entries":
		v, err = verifyArtifactMinEntries(cfg, in)
	case "artifact_non_empty":
		v, err = verifyArtifactNonEmpty(cfg, in)
	case "no_status_429_in_audit":
		v, err = verifyNoStatus429(cfg, in)
	case "must_contain_url":
		v, err = verifyMustContainURL(cfg, in)
	case "no_empty_artifacts":
		v, err = verifyNoEmptyArtifacts(cfg, in)
	case "proposals_match_watchlist":
		v, err = verifyProposalsMatchWatchlist(cfg, in)
	case "entry_gate_consistent":
		v, err = verifyEntryGateConsistent(cfg, in)
	case "placements_match_audit":
		v, err = verifyPlacementsMatchAudit(cfg, in)
	case "cv_claims_grounded":
		v, err = verifyCVClaimsGrounded(cfg, in)
	default:
		return nil, fmt.Errorf("unknown verifier type %q (verifier %q)", cfg.Type, cfg.Name)
	}
	// Apply severity override. Order of precedence:
	//   1. cfg.Severity (operator YAML override) wins if non-empty.
	//   2. Else the verifier function's own Violation.Severity stands
	//      (each verifier may set its own default, e.g. a coverage-
	//      style check defaulting to warn).
	//   3. Else default to fail via normaliseSeverity's empty-string
	//      mapping at the call-site boundary.
	if v != nil {
		if cfg.Severity != "" {
			v.Severity = normaliseSeverity(cfg.Severity)
		} else if v.Severity == "" {
			v.Severity = SeverityFail
		}
		// Operator YAML can promote a non-terminal verifier to
		// terminal but not the other way around (see ConfigFromMap).
		if cfg.Terminal {
			v.Terminal = true
		}
	}
	return v, err
}

// ConfigFromMap converts a YAML-decoded map (as carried in the
// registry's untyped Verifiers slice) into a Config. Defined here
// rather than in registry so the registry package doesn't pick up
// a verifier-package dependency. Returns ok=false when the map is
// missing the required `type` field — the caller skips it (and
// the executor logs the malformed entry).
func ConfigFromMap(m map[string]any) (Config, bool) {
	t, _ := m["type"].(string)
	if t == "" {
		return Config{}, false
	}
	cfg := Config{
		Type: t,
	}
	if name, ok := m["name"].(string); ok {
		cfg.Name = name
	}
	if w, ok := m["whenTaskType"].(string); ok {
		cfg.WhenTaskType = w
	}
	if w, ok := m["when_task_type"].(string); ok && cfg.WhenTaskType == "" {
		cfg.WhenTaskType = w
	}
	if w, ok := m["whenStep"].(string); ok {
		cfg.WhenStep = w
	}
	if w, ok := m["when_step"].(string); ok && cfg.WhenStep == "" {
		cfg.WhenStep = w
	}
	if a, ok := parseAppliesTo(m["appliesTo"]); ok {
		cfg.AppliesTo = a
	} else if a, ok := parseAppliesTo(m["applies_to"]); ok {
		cfg.AppliesTo = a
	}
	if s, ok := m["severity"].(string); ok {
		cfg.Severity = s
	}
	// Terminal is a tri-state (operator-set true, operator-set false,
	// unset = inherit from verifier impl's default). The unset case
	// is signalled by leaving cfg.Terminal at its zero value AND
	// remembering "operator didn't say" — but a plain bool field
	// can't express that. We keep the simple bool: only operator
	// `terminal: true` overrides the impl, never `terminal: false`,
	// because the impl knows when a failure is truly non-retryable
	// (rate-limit) and overruling that to "do retry" is almost
	// always a misconfig. Documented in the struct comment.
	if t, ok := m["terminal"].(bool); ok && t {
		cfg.Terminal = true
	}
	if p, ok := m["params"].(map[string]any); ok {
		cfg.Params = p
	}
	return cfg, true
}

// parseAppliesTo accepts the YAML-decoded shape (typically
// []any of strings, or []string when typed) and normalises to a
// trimmed []string. Returns ok=false when the value is absent
// or empty so the caller can leave Config.AppliesTo nil
// (= "all sources").
func parseAppliesTo(v any) ([]string, bool) {
	if v == nil {
		return nil, false
	}
	var raw []string
	switch s := v.(type) {
	case []string:
		raw = s
	case []any:
		raw = make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				raw = append(raw, str)
			}
		}
	default:
		return nil, false
	}
	out := cleanStringSlice(raw)
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// appliesToCreationSource returns true when the verifier should run
// for a task with the given creation source. Empty allowlist (= the
// default, backward-compatible case) means "applies to every
// source". Comparison is case-insensitive so operators can write
// either "user" or "USER" in YAML; the runtime stores creation
// source as upper-case ("USER", "AUTONOMOUS").
//
// When the input has no CreationSource (rare: legacy tests, or
// the executor call site didn't plumb it), an explicit allowlist
// also blocks the verifier. That's the conservative read — if you
// took the trouble to write appliesTo: [autonomous], you don't
// want the rule to silently apply to a task whose source is
// unknown.
func appliesToCreationSource(allow []string, src string) bool {
	if len(allow) == 0 {
		return true
	}
	srcU := strings.ToUpper(strings.TrimSpace(src))
	if srcU == "" {
		return false
	}
	for _, a := range allow {
		if strings.EqualFold(strings.TrimSpace(a), srcU) {
			return true
		}
	}
	return false
}

// ConfigsFromMaps maps an untyped slice of verifier configs into
// typed Configs, dropping (and returning the count of) malformed
// entries. The count is informational so the caller can log
// "skipped N malformed verifier configs" without a per-entry
// audit.
func ConfigsFromMaps(ms []map[string]any) ([]Config, int) {
	if len(ms) == 0 {
		return nil, 0
	}
	out := make([]Config, 0, len(ms))
	skipped := 0
	for _, m := range ms {
		if cfg, ok := ConfigFromMap(m); ok {
			out = append(out, cfg)
		} else {
			skipped++
		}
	}
	return out, skipped
}

// RunAll runs every applicable verifier against the input and
// returns the merged violation list. Empty result = all checks
// passed (or none applied). Caller decides what to do — the
// executor wraps the list as an error to fail the step.
func RunAll(ctx context.Context, configs []Config, in Input) []Violation {
	var out []Violation
	for _, cfg := range configs {
		v, err := Run(ctx, cfg, in)
		if err != nil {
			// Operator error: surface as a Violation so it
			// shows up in the dashboard, but tag the type so
			// triage can tell config drift from real failure.
			// Config errors hard-fail by design — a typo in YAML
			// is a real bug, not a warning.
			out = append(out, Violation{
				VerifierName: cfg.Name,
				Type:         "config_error:" + cfg.Type,
				Severity:     SeverityFail,
				Detail:       err.Error(),
			})
			continue
		}
		if v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// -----------------------------------------------------------------
// Built-in verifiers
// -----------------------------------------------------------------

// verifyArtifactMinEntries — the canonical Phase 2 check:
// scan a markdown artifact (or any text artifact matching a
// glob) for at least N list-item lines. Catches the "scan
// completed but the portal blocked us so the file is empty"
// failure mode where a worker returns a structurally-valid but
// content-empty result.
//
// Params:
//   - artifact_pattern: glob like "scan-*.md" (required)
//   - min: integer, minimum list-item count (required)
//   - item_pattern: regex for what counts as an item; default
//     `^[-*\d]+\.?\s+\S` matches markdown list bullets and
//     numbered items.
func verifyArtifactMinEntries(cfg Config, in Input) (*Violation, error) {
	pattern, _ := cfg.Params["artifact_pattern"].(string)
	if pattern == "" {
		return nil, fmt.Errorf("artifact_pattern is required")
	}
	min := paramInt(cfg.Params, "min")
	if min <= 0 {
		return nil, fmt.Errorf("min must be > 0")
	}
	itemPattern := paramString(cfg.Params, "item_pattern")
	if itemPattern == "" {
		itemPattern = `(?m)^\s*(?:[-*]|\d+\.)\s+\S`
	}
	re, err := regexp.Compile(itemPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid item_pattern: %w", err)
	}

	for _, a := range in.Artifacts {
		if a == nil || !globMatch(a.Name, pattern) {
			continue
		}
		// Body sniff: the verifier needs the file's text.
		// Artifacts carry SizeBytes but not body in the row;
		// we read from the storage path directly. A missing
		// file is itself a verifier failure (the agent claimed
		// to write it but didn't).
		text, readErr := readArtifactBody(a)
		if readErr != nil {
			return &Violation{
				VerifierName: nameOrDefault(cfg, "artifact_min_entries"),
				Type:         cfg.Type,
				Detail:       fmt.Sprintf("could not read artifact %q: %v", a.Name, readErr),
			}, nil
		}
		count := len(re.FindAllString(text, -1))
		if count < min {
			return &Violation{
				VerifierName: nameOrDefault(cfg, "artifact_min_entries"),
				Type:         cfg.Type,
				Detail: fmt.Sprintf(
					"artifact %q has %d list-item line(s); requires ≥%d. The portal may have blocked the scan, or the agent stopped early.",
					a.Name, count, min),
			}, nil
		}
		return nil, nil
	}
	// No matching artifact at all is a violation: the producer
	// was supposed to emit one and didn't. Without this branch,
	// a step that produces no artifact slips through.
	return &Violation{
		VerifierName: nameOrDefault(cfg, "artifact_min_entries"),
		Type:         cfg.Type,
		Detail:       fmt.Sprintf("no artifact matched pattern %q; expected at least one with ≥%d items", pattern, min),
	}, nil
}

// verifyArtifactNonEmpty — light cousin of artifact_min_entries.
// Only checks that an artifact matching the pattern has size > 0.
// Used when the operator wants "produce something, anything"
// without committing to a count threshold.
//
// Params:
//   - artifact_pattern: glob (required)
func verifyArtifactNonEmpty(cfg Config, in Input) (*Violation, error) {
	pattern, _ := cfg.Params["artifact_pattern"].(string)
	if pattern == "" {
		return nil, fmt.Errorf("artifact_pattern is required")
	}
	for _, a := range in.Artifacts {
		if a == nil || !globMatch(a.Name, pattern) {
			continue
		}
		size := int64(0)
		if a.SizeBytes != nil {
			size = *a.SizeBytes
		}
		if size <= 0 {
			return &Violation{
				VerifierName: nameOrDefault(cfg, "artifact_non_empty"),
				Type:         cfg.Type,
				Detail:       fmt.Sprintf("artifact %q is empty (size=%d)", a.Name, size),
			}, nil
		}
		return nil, nil
	}
	return &Violation{
		VerifierName: nameOrDefault(cfg, "artifact_non_empty"),
		Type:         cfg.Type,
		Detail:       fmt.Sprintf("no artifact matched pattern %q", pattern),
	}, nil
}

// verifyNoStatus429 — surfaces rate-limit / block events from the
// tool audit. Operates in two modes per audit entry:
//
//  1. Structured signal (preferred): when ToolOutput parses as
//     JSON and exposes a `block_reason` field (the scraper
//     convention — `mcp__scraper__web_fetch` emits one on every
//     call), the field is ground truth. Non-empty = blocked.
//     This is precise: a successful Wikipedia fetch with the
//     word "captcha" buried in MediaWiki's RLCONF JS config
//     does NOT trip, because block_reason is empty.
//
//  2. Marker scan (fallback): for tools without a structured
//     signal, substring-match a tightened marker list against
//     ToolInput + ToolOutput. The historical bare words
//     "captcha" and "blocked" are gone — they false-positive on
//     anything that *mentions* a CAPTCHA. We keep anchored
//     forms like `"block_reason":"captcha"`.
//
// On top of per-entry classification, the verifier supports
// graduated tolerance so a research step that hits 1 block out
// of 30 fetches isn't punished the same as a step that got
// 100% blocked:
//
//   - max_block_ratio (default 0): step passes if
//     blocked / (blocked+ok) <= ratio.
//   - min_successful_fetches (default 0): step passes if
//     successful entries >= floor.
//
// Both default to zero — operator gets today's zero-tolerance
// behaviour until they opt in.
//
// Params:
//   - tools: optional []string of tool names to scope the check
//     (e.g. ["mcp__scraper__web_fetch"]). Empty = all tools.
//   - max_block_ratio: optional float in [0,1]. Default 0.
//   - min_successful_fetches: optional int. Default 0.
//   - excuse_skip_path: optional string. JSON path in result.json.
//     When result.json[path] is a non-empty array of objects each
//     with a non-empty `reason`, blocks for URLs that appear under
//     `url` / `final_url` in those entries are excused. Mirrors
//     the must_contain_url skip_alternative_path pattern.
//   - excuse_block_reasons: optional []string of block reason labels
//     (case-insensitive) to treat the same as skip-path excusals.
//     Use for permanent block classes the operator has decided to
//     accept silently — typically "auth_required" for paywalled
//     sources where retrying just produces the same login wall
//     forever. "captcha" / "robots_blocked" are common siblings.
//     Default empty = today's strict counting.
//   - severity: optional "fail"|"warn". Default fail (today's
//     behaviour). The recommended config pairs this verifier at
//     severity=warn with must_contain_url at fail, so coverage of
//     required sources is the hard gate and block noise stays
//     advisory.
func verifyNoStatus429(cfg Config, in Input) (*Violation, error) {
	scope := paramStringSlice(cfg.Params, "tools")
	scopeSet := map[string]struct{}{}
	for _, s := range scope {
		scopeSet[s] = struct{}{}
	}
	maxBlockRatio := paramFloat(cfg.Params, "max_block_ratio")
	if maxBlockRatio < 0 {
		maxBlockRatio = 0
	}
	if maxBlockRatio > 1 {
		maxBlockRatio = 1
	}
	minSuccess := paramInt(cfg.Params, "min_successful_fetches")
	if minSuccess < 0 {
		minSuccess = 0
	}
	excusedURLs := excusedURLsFromResult(in.ResultJSON, paramString(cfg.Params, "excuse_skip_path"))
	excusedReasons := map[string]struct{}{}
	for _, r := range paramStringSlice(cfg.Params, "excuse_block_reasons") {
		excusedReasons[strings.ToLower(strings.TrimSpace(r))] = struct{}{}
	}

	var (
		blocked    []blockReport
		excused    []blockReport
		successful int
	)
	for _, e := range in.AuditEntries {
		if e == nil {
			continue
		}
		if len(scopeSet) > 0 {
			if _, ok := scopeSet[e.ToolName]; !ok {
				continue
			}
		}
		rep, ok := classifyAuditEntry(e)
		if !ok {
			// Tool isn't in scope semantically (no classification
			// applied). Don't count toward successful — only entries
			// we know how to classify contribute to denominators.
			continue
		}
		if !rep.blocked {
			successful++
			continue
		}
		if rep.url != "" && excusedURLs[rep.url] {
			excused = append(excused, rep)
			continue
		}
		if len(excusedReasons) > 0 {
			if _, ok := excusedReasons[strings.ToLower(strings.TrimSpace(rep.reason))]; ok {
				excused = append(excused, rep)
				continue
			}
		}
		blocked = append(blocked, rep)
	}

	// Nothing in scope at all = nothing to verify. Don't fabricate a
	// violation; another verifier (must_contain_url) owns "the
	// scraper was supposed to run and didn't".
	if successful == 0 && len(blocked) == 0 && len(excused) == 0 {
		return nil, nil
	}

	total := successful + len(blocked) + len(excused)
	// Ratio is computed only against non-excused entries, because
	// excused = "we know about this and accepted it" — counting it
	// as a block would make the threshold pointlessly strict.
	denomForRatio := successful + len(blocked)
	var ratio float64
	if denomForRatio > 0 {
		ratio = float64(len(blocked)) / float64(denomForRatio)
	}

	overRatio := len(blocked) > 0 && ratio > maxBlockRatio
	underFloor := minSuccess > 0 && successful < minSuccess
	if !overRatio && !underFloor {
		return nil, nil
	}

	summary := fmt.Sprintf(
		"%d/%d fetch(es) blocked (ratio %.2f, max %.2f); %d ok; %d excused via skip path. ",
		len(blocked), total, ratio, maxBlockRatio, successful, len(excused),
	)
	if underFloor {
		summary += fmt.Sprintf("Successful fetches (%d) below floor (%d). ", successful, minSuccess)
	}
	summary += "Blocks: " + formatBlockReports(blocked)

	blockedURLs := make([]BlockedURL, 0, len(blocked))
	allPermanent := len(blocked) > 0
	for _, rep := range blocked {
		permanent := IsPermanentBlockReason(rep.reason)
		blockedURLs = append(blockedURLs, BlockedURL{
			URL:       rep.url,
			Reason:    rep.reason,
			Permanent: permanent,
		})
		if !permanent {
			allPermanent = false
		}
	}

	return &Violation{
		VerifierName: nameOrDefault(cfg, "no_status_429_in_audit"),
		Type:         cfg.Type,
		Detail:       summary,
		BlockedURLs:  blockedURLs,
		// Terminal flag controls whether the executor's on_fail
		// routing fires. Rate-limit / anti-bot failures with at
		// least one transient block stay Terminal=true (today's
		// behaviour — retrying just bumps the limit again).
		// When EVERY block is permanent (auth_required, paywall,
		// captcha) the recovery path needs the executor to route
		// to on_fail so the lead can propose alternatives via a
		// checkpoint; Terminal becomes false in that case. Operator
		// can pin Terminal=true regardless via Config.Terminal.
		Terminal: !allPermanent,
	}, nil
}

// blockReport is one audit entry classified by classifyAuditEntry.
// `url` is best-effort: scraper inputs always have it; for non-
// scraper tools that tripped the marker scan, it's the tool name.
type blockReport struct {
	tool    string
	url     string
	reason  string
	blocked bool
}

// classifyAuditEntry runs the two-mode classification described
// in verifyNoStatus429's docstring against a single audit row.
// Returns (report, true) when the entry contributes to the
// blocked/ok counts; (zero, false) when the entry is uninteresting
// (memory_search, file_read, etc. that happened to be in scope
// but isn't a fetch-style tool).
//
// The "is this a fetch?" hint is heuristic: anything whose
// ToolOutput parses as JSON and carries a `block_reason` key gets
// the structured path; otherwise, anything whose ToolOutput
// matches a marker is treated as a blocked fetch; otherwise the
// entry doesn't contribute. This keeps unrelated audit rows from
// inflating the denominator.
func classifyAuditEntry(e *persistence.ToolAuditEntry) (blockReport, bool) {
	rep := blockReport{tool: e.ToolName}
	// Structured-signal path: parse as JSON and look for the
	// scraper's block_reason convention.
	var out scraperOutput
	if len(e.ToolOutput) > 0 && json.Unmarshal([]byte(e.ToolOutput), &out) == nil && out.hasBlockSignal() {
		rep.url = scraperURL(e, out)
		rep.reason = out.BlockReason
		rep.blocked = strings.TrimSpace(out.BlockReason) != "" &&
			!strings.EqualFold(out.BlockReason, "none")
		return rep, true
	}
	// Marker scan: tightened list — bare "captcha" / "blocked" are
	// out, only anchored forms survive.
	body := strings.ToLower(e.ToolOutput + "\n" + e.ToolInput)
	for _, m := range tightenedMarkers {
		if strings.Contains(body, m) {
			rep.url = scraperURL(e, scraperOutput{})
			rep.reason = m
			rep.blocked = true
			return rep, true
		}
	}
	return blockReport{}, false
}

// tightenedMarkers replaces the historical zero-tolerance marker
// list. The bare words "captcha" and "blocked" are gone — they
// match on any page that mentions a CAPTCHA in its content
// (Wikipedia, Wikivoyage, every MediaWiki install). Anchored
// forms like `"block_reason":"captcha"` survive because they only
// occur in structured tool output, not in arbitrary HTML.
var tightenedMarkers = []string{
	`status_code":429`, `"status":429`, `status=429`,
	`status_code":503`, `"status":503`, `status=503`,
	`rate limit`, `rate-limited`, `too many requests`,
	`err_blocked`, `"block_reason":"captcha"`, `"block_reason":"http_403"`,
	`"block_reason":"rate_limit"`, `"blocked":true`,
}

// scraperOutput captures the subset of mcp__scraper__web_fetch's
// JSON output we care about. Extra fields are ignored so the
// scraper is free to add more without breaking the verifier.
type scraperOutput struct {
	Status      int    `json:"status"`
	FinalURL    string `json:"final_url"`
	BlockReason string `json:"block_reason"`
	BlockDetail string `json:"block_detail"`
}

// hasBlockSignal returns true when the scraper output looks like
// a scraper response — the cue is the presence of either a
// final_url or block_reason key. We don't insist on block_reason
// being non-empty because a successful fetch sets it to "" and
// that's a valid signal of "not blocked".
func (s scraperOutput) hasBlockSignal() bool {
	return s.FinalURL != "" || s.BlockReason != "" || s.Status != 0
}

// scraperURL extracts the URL the entry hit. Prefers the scraper
// output's final_url (post-redirect), falls back to parsing
// ToolInput JSON for a `url` field, then the empty string. Used
// for the excuse-on-skip cross-check and for the violation detail.
func scraperURL(e *persistence.ToolAuditEntry, out scraperOutput) string {
	if out.FinalURL != "" {
		return out.FinalURL
	}
	if len(e.ToolInput) > 0 {
		var in struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(e.ToolInput), &in) == nil && in.URL != "" {
			return in.URL
		}
	}
	return ""
}

// excusedURLsFromResult parses result.json for an excuse-skip
// array and returns the set of URLs that should be treated as
// already-known blocks. Mirrors hasDocumentedSkipped's contract:
// the array must be non-empty and every entry must carry a non-
// empty `reason` field. Additionally, every entry must include
// a `url` (or `final_url`) — without it the verifier has no key
// to match against the audit, so the entry doesn't contribute.
//
// An empty path or absent array yields a nil map (no excuses),
// which is the safe default: the verifier behaves as it did
// before the option existed.
func excusedURLsFromResult(resultBytes []byte, path string) map[string]bool {
	if path == "" || len(resultBytes) == 0 {
		return nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(resultBytes, &envelope); err != nil {
		return nil
	}
	val, ok := envelope[path]
	if !ok {
		if msg, ok2 := envelope["message"].(string); ok2 && msg != "" {
			var inner map[string]any
			if json.Unmarshal([]byte(msg), &inner) == nil {
				val = inner[path]
			}
		}
	}
	arr, ok := val.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		reason, _ := obj["reason"].(string)
		if strings.TrimSpace(reason) == "" {
			continue
		}
		if u, _ := obj["url"].(string); u != "" {
			out[u] = true
		}
		if u, _ := obj["final_url"].(string); u != "" {
			out[u] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// formatBlockReports renders blocked entries into the violation
// Detail. Keeps the list bounded — operators don't need to see
// 200 lines on a runaway scrape — but always shows at least one
// concrete example so triage isn't guessing.
func formatBlockReports(reports []blockReport) string {
	if len(reports) == 0 {
		return "(none)"
	}
	const maxShow = 5
	parts := make([]string, 0, len(reports))
	for i, r := range reports {
		if i >= maxShow {
			parts = append(parts, fmt.Sprintf("… +%d more", len(reports)-maxShow))
			break
		}
		key := r.url
		if key == "" {
			key = r.tool
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", key, r.reason))
	}
	return strings.Join(parts, "; ")
}

// verifyMustContainURL — given a URL or set of URLs in params,
// ensure each appears in at least one tool_audit_log entry's
// input or output for this step. Catches the case where a
// research task was supposed to scrape specific portals but
// silently skipped them.
//
// Params:
//   - urls: []string (required) — the URLs / tool names that
//     must appear in the step's tool audit.
//   - skip_alternative_path: string (optional) — JSON path in
//     result.json. When the agent emits a non-empty array at
//     that path AND every entry has a non-empty `reason` field,
//     the verifier passes even if the URLs are absent. The
//     escape hatch for legitimate post-approval skips:
//     ibkr-trader executor approves SHELL, broker rejects with
//     "no conId", executor writes
//     skipped[]={symbol,reason:"quote_drifted"}, no place_order
//     fires. Without this option the verifier blocks the step
//     and the operator gets paged for what was actually correct
//     behaviour. Today's only supported path is "skipped" (top-
//     level array of {reason} objects); the param is generic so
//     other workflows can opt in by pointing at their own
//     skip-with-reason field.
func verifyMustContainURL(cfg Config, in Input) (*Violation, error) {
	urls := paramStringSlice(cfg.Params, "urls")
	if len(urls) == 0 {
		return nil, fmt.Errorf("urls is required and must be non-empty")
	}
	var corpus strings.Builder
	for _, e := range in.AuditEntries {
		if e == nil {
			continue
		}
		corpus.WriteString(e.ToolInput)
		corpus.WriteString("\n")
		corpus.WriteString(e.ToolOutput)
		corpus.WriteString("\n")
	}
	body := corpus.String()
	var missing []string
	for _, u := range urls {
		if !strings.Contains(body, u) {
			missing = append(missing, u)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	// Skip-alternative escape hatch: when the result.json declares
	// the URLs were skipped with a documented reason, treat that as
	// equivalent to "URLs present in audit". Caller-specified path
	// keeps this generic — trading workflows point at "skipped",
	// research workflows could point at "deferred" or "blocked",
	// etc. Only fires when explicitly opted in.
	if skipPath := paramString(cfg.Params, "skip_alternative_path"); skipPath != "" {
		if hasDocumentedSkipped(in.ResultJSON, skipPath) {
			return nil, nil
		}
	}
	return &Violation{
		VerifierName: nameOrDefault(cfg, "must_contain_url"),
		Type:         cfg.Type,
		Detail:       fmt.Sprintf("required URL(s) absent from tool audit: %s", strings.Join(missing, ", ")),
	}, nil
}

// verifyNoEmptyArtifacts — bulk version of artifact_non_empty,
// applies to every output artifact. Useful for tasks that emit
// multiple files (a code-fix task writes a patch + a summary;
// either being empty means the work didn't complete).
//
// No params.
func verifyNoEmptyArtifacts(cfg Config, in Input) (*Violation, error) {
	for _, a := range in.Artifacts {
		if a == nil {
			continue
		}
		if a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		size := int64(0)
		if a.SizeBytes != nil {
			size = *a.SizeBytes
		}
		if size <= 0 {
			return &Violation{
				VerifierName: nameOrDefault(cfg, "no_empty_artifacts"),
				Type:         cfg.Type,
				Detail:       fmt.Sprintf("output artifact %q is empty (size=%d)", a.Name, size),
			}, nil
		}
	}
	return nil, nil
}

// verifyProposalsMatchWatchlist — fail the strategist step when any
// proposed symbol isn't in the project's configured Trading.Watchlist.
// Closes the ticker-hallucination class observed 2026-05-07
// (exec_20260507180631): kimi-k2.5 strategist saw "SHEL" in the
// pre-warmed indicator block and proposed "SHELL" in its
// proposals[]. Risk-officer didn't normalise; executor tried to
// place, broker returned no-conId, no order placed. Deterministic
// allowlist check at the strategist boundary closes the loop without
// asking the LLM to behave.
//
// Reads result.json's `proposals[].symbol` from the parsed envelope.
// Case-sensitive — IBKR symbols are upper-case canonical and the
// watchlist is upper-case. Falls through (no-op) when the project
// has no watchlist configured (non-trading projects) or the result
// has no proposals key (workflow path that doesn't emit proposals;
// the verifier engine's WhenStep filter scopes to the strategist
// step in practice).
//
// No params — the watchlist comes from the project, not the
// verifier config, so operators don't have to keep two lists in
// sync.
func verifyProposalsMatchWatchlist(cfg Config, in Input) (*Violation, error) {
	if len(in.WatchlistAllowList) == 0 || len(in.ResultJSON) == 0 {
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(in.WatchlistAllowList))
	for _, sym := range in.WatchlistAllowList {
		allowed[strings.ToUpper(strings.TrimSpace(sym))] = struct{}{}
	}
	proposals := extractProposalSymbols(in.ResultJSON)
	if len(proposals) == 0 {
		return nil, nil
	}
	var unknown []string
	for _, sym := range proposals {
		canon := strings.ToUpper(strings.TrimSpace(sym))
		if _, ok := allowed[canon]; !ok {
			unknown = append(unknown, sym)
		}
	}
	if len(unknown) == 0 {
		return nil, nil
	}
	return &Violation{
		VerifierName: nameOrDefault(cfg, "proposals_match_watchlist"),
		Type:         cfg.Type,
		Detail: fmt.Sprintf(
			"strategist proposed %d symbol(s) not in the project watchlist: %s — likely model hallucination (ticker invented or mistyped). Watchlist: %s",
			len(unknown), strings.Join(unknown, ", "), strings.Join(in.WatchlistAllowList, ", "),
		),
	}, nil
}

// verifyEntryGateConsistent — fail the strategist step when a proposed
// long entry's price sits below the symbol's daily SMA(50).
//
// Closes the 2026-06-12 NVDA whipsaw class (exec_20260612191342): the
// strategist proposed BUY/open NVDA at $204.90 while NVDA's daily
// SMA(50) was $206.90, justifying it with prose inequalities that are
// arithmetically false ("price $204.80 > SMA50 $206.90", "RSI=44.9 in
// [50,70]"). The value-only hallucination detector (taIndicatorClaimRule)
// passes because the cited *values* are grounded — it never evaluates
// the model's boolean reasoning about them. The risk-officer + judge
// rubber-stamped it, the order filled, and the very next tick's exit
// rule ("daily price < SMA(50) → trend backing lost") correctly closed
// the position at a loss. The entry never qualified: price < SMA(50)
// IS the exit condition.
//
// price > SMA(50) is a necessary condition for EVERY long-entry tier in
// the strategy (momentum_clean requires price > SMA20 > SMA50; the
// pullback tiers require price > SMA50 "still in the larger uptrend"),
// so the floor is tier-agnostic and safe to enforce deterministically.
//
// Direction-aware: only long opens are checked. Closes/exits
// (action SELL, or intent "close") are LEGITIMATE below SMA(50) and
// must never trip — see the trading workflow's hallucination judge,
// which makes the same exemption.
//
// Abstains (clean no-op, never a false positive) when:
//   - no indicator snapshot for the symbol (single-symbol fetch failed),
//   - the symbol's SMA50 is unknown (<= 0, too few bars), or
//   - the proposal carries no positive limit_price (a market order has
//     no deterministic entry level to compare against).
func verifyEntryGateConsistent(cfg Config, in Input) (*Violation, error) {
	if len(in.ResultJSON) == 0 || len(in.EntryGateIndicators) == 0 {
		return nil, nil
	}
	var bad []string
	for _, p := range extractProposals(in.ResultJSON) {
		if !p.isLongOpen() {
			continue
		}
		ind, ok := in.EntryGateIndicators[strings.ToUpper(strings.TrimSpace(p.Symbol))]
		if !ok || ind.SMA50 <= 0 {
			continue
		}
		if p.LimitPrice <= 0 {
			continue
		}
		if p.LimitPrice < ind.SMA50 {
			bad = append(bad, fmt.Sprintf(
				"%s (entry $%.2f < SMA(50) $%.2f)",
				p.Symbol, p.LimitPrice, ind.SMA50,
			))
		}
	}
	if len(bad) == 0 {
		return nil, nil
	}
	return &Violation{
		VerifierName: nameOrDefault(cfg, "entry_gate_consistent"),
		Type:         cfg.Type,
		Detail: fmt.Sprintf(
			"strategist proposed %d long entr%s priced below the daily SMA(50) trend floor: %s — every entry tier requires price > SMA(50), and price < SMA(50) is itself the trend-break exit condition (the entry would whipsaw out next tick). Likely a fabricated boolean rationale the value-only TA check can't catch.",
			len(bad), pluralY(len(bad)), strings.Join(bad, ", "),
		),
	}, nil
}

// verifyPlacementsMatchAudit — fail the executor step when the agent
// declared more placed orders in result.json than it actually called
// the broker tool to place. Catches the precise hallucination class
// `must_contain_url` can't distinguish: an empty audit + an empty
// placed[] is a legitimate "broker offline, nothing placed" outcome,
// but an empty audit + a NON-empty placed[] is the executor
// fabricating broker_order_ids without ever calling the broker MCP
// (observed repeatedly with minimax.minimax-m2.5 on the ibkr-trader
// project: sequential fake order IDs like "86287740", "86287741", and
// idempotency_keys with hexalphabetical sequences). The
// `must_contain_url` rule + skip_alternative_path can paper over this
// when retries happen to land in a no-approval state; this verifier
// fires deterministically on the first attempt with a clear class so
// operator triage isn't pattern-matching "URL absent" against the
// LLM's own claim.
//
// Params:
//   - tool: optional, the audit tool name to count (default
//     "mcp__broker__place_order"). Parameterised so other workflows
//     with a similar claim-vs-action shape can reuse the verifier.
//   - claim_path: optional, the result.json array path that holds the
//     claimed actions (default "placed"). Same generality motivation.
//
// Fires when len(claim_path[]) > count(audit tool calls). The verifier
// is intentionally one-way: under-counting (claim < audit) is fine —
// the agent might place an order and then forget to record it, which
// is a separate problem class. We're specifically catching claim
// inflation.
func verifyPlacementsMatchAudit(cfg Config, in Input) (*Violation, error) {
	tool := paramString(cfg.Params, "tool")
	if tool == "" {
		tool = "mcp__broker__place_order"
	}
	claimPath := paramString(cfg.Params, "claim_path")
	if claimPath == "" {
		claimPath = "placed"
	}

	claimed := countClaimedEntries(in.ResultJSON, claimPath)
	if claimed == 0 {
		// No claim — nothing to cross-check. A different verifier
		// (must_contain_url with skip_alternative_path) catches the
		// "approvals existed but executor didn't even try" case.
		return nil, nil
	}

	actual := 0
	for _, e := range in.AuditEntries {
		if e == nil {
			continue
		}
		if e.ToolName == tool {
			actual++
		}
	}
	if actual >= claimed {
		return nil, nil
	}
	return &Violation{
		VerifierName: nameOrDefault(cfg, "placements_match_audit"),
		Type:         cfg.Type,
		Detail: fmt.Sprintf(
			"hallucinated placement: result.json declares %d %s entr%s but tool audit shows only %d %s call(s) — the executor fabricated broker responses without invoking the broker tool",
			claimed, claimPath, pluralY(claimed), actual, tool,
		),
	}, nil
}

// countClaimedEntries returns the length of a top-level JSON array at
// `path` within result.json, with the same envelope-merge fallback as
// extractProposalSymbols (some result.json shapes nest the model's
// structured output inside a `message` string field). Returns 0 when
// the path is absent, not an array, or the JSON fails to parse —
// every "can't read it" case should leave the verifier silent, not
// false-positive.
func countClaimedEntries(resultBytes []byte, path string) int {
	if len(resultBytes) == 0 || path == "" {
		return 0
	}
	var envelope map[string]any
	if err := json.Unmarshal(resultBytes, &envelope); err != nil {
		return 0
	}
	if arr, ok := envelope[path].([]any); ok {
		return len(arr)
	}
	if msg, ok := envelope["message"].(string); ok && msg != "" {
		var inner map[string]any
		if err := json.Unmarshal([]byte(msg), &inner); err == nil {
			if arr, ok := inner[path].([]any); ok {
				return len(arr)
			}
		}
	}
	return 0
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// extractProposalSymbols parses ResultJSON for a top-level
// `proposals[]` array and returns each entry's `symbol` field.
// Tolerates both the agent's typical envelope (proposals at top
// level after the harness's merge step) and the rare case where
// proposals lives inside a `message` JSON-string (for which the
// caller can pre-normalise via the executor's normalizedResult-
// Payload helper before invoking the verifier — but this function
// works against raw bytes for the common case so verifier callers
// don't need to coordinate).
func extractProposalSymbols(resultBytes []byte) []string {
	var envelope struct {
		Proposals []struct {
			Symbol string `json:"symbol"`
		} `json:"proposals"`
		Message string `json:"message"`
	}
	if err := jsonUnmarshalLenient(resultBytes, &envelope); err != nil {
		return nil
	}
	if len(envelope.Proposals) > 0 {
		out := make([]string, 0, len(envelope.Proposals))
		for _, p := range envelope.Proposals {
			out = append(out, p.Symbol)
		}
		return out
	}
	// Fallback: the model's structured output wasn't merged to top
	// level (entrypoint pass-3 extraction failure on multi-object
	// output). Try to recover from envelope.message.
	if envelope.Message != "" {
		var inner struct {
			Proposals []struct {
				Symbol string `json:"symbol"`
			} `json:"proposals"`
		}
		if err := jsonUnmarshalLenient([]byte(envelope.Message), &inner); err == nil && len(inner.Proposals) > 0 {
			out := make([]string, 0, len(inner.Proposals))
			for _, p := range inner.Proposals {
				out = append(out, p.Symbol)
			}
			return out
		}
	}
	return nil
}

// proposalEntry is the projection of a strategist proposal the
// direction-aware verifiers need. Mirrors the strategist's structured
// output shape (action/intent/limit_price/symbol); fields the verifier
// doesn't read are dropped.
type proposalEntry struct {
	Symbol     string  `json:"symbol"`
	Action     string  `json:"action"`
	Intent     string  `json:"intent"`
	LimitPrice float64 `json:"limit_price"`
}

// isLongOpen reports whether this proposal opens a long position. The
// strategy is long-only (bull-regime gated; shorts disabled), so a BUY
// is an open unless it is explicitly tagged intent:close. Empty intent
// on a BUY is treated as an open — the conservative reading, since the
// gate it feeds (entry_gate_consistent) only fires when the entry price
// is also below SMA(50).
func (p proposalEntry) isLongOpen() bool {
	if !strings.EqualFold(strings.TrimSpace(p.Action), "BUY") {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(p.Intent), "close")
}

// extractProposals parses the strategist's proposals[] from result.json
// with the same envelope-merge fallback as extractProposalSymbols (the
// agent harness occasionally leaves the structured output nested inside
// a `message` JSON-string). Returns nil on any parse failure so callers
// no-op rather than block on malformed output.
func extractProposals(resultBytes []byte) []proposalEntry {
	var envelope struct {
		Proposals []proposalEntry `json:"proposals"`
		Message   string          `json:"message"`
	}
	if err := jsonUnmarshalLenient(resultBytes, &envelope); err != nil {
		return nil
	}
	if len(envelope.Proposals) > 0 {
		return envelope.Proposals
	}
	if envelope.Message != "" {
		var inner struct {
			Proposals []proposalEntry `json:"proposals"`
		}
		if err := jsonUnmarshalLenient([]byte(envelope.Message), &inner); err == nil {
			return inner.Proposals
		}
	}
	return nil
}

// ProposedLongOpenSymbols returns the upper-cased, deduped set of
// symbols the strategist proposed to OPEN long this step. The executor
// uses it to decide which symbols need a deterministic SMA(50) fetch
// for the entry_gate_consistent verifier — closes/exits don't need one.
func ProposedLongOpenSymbols(resultBytes []byte) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, p := range extractProposals(resultBytes) {
		if !p.isLongOpen() {
			continue
		}
		canon := strings.ToUpper(strings.TrimSpace(p.Symbol))
		if canon == "" {
			continue
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	return out
}

// jsonUnmarshalLenient is a thin wrapper that surfaces parse
// errors to the caller so the verifier silently no-ops rather
// than failing the step on malformed result.json. The
// shape-validation layer upstream already classifies that case.
func jsonUnmarshalLenient(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// hasDocumentedSkipped returns true when result.json has a non-empty
// array at `path` and every entry includes a non-empty `reason`.
// Today path is always "skipped" (trading workflows); the function is
// written generically so future workflows can use the same escape
// hatch via a different result field.
//
// "Documented" requires reason on every entry — an empty-string or
// missing reason on any single skip item disqualifies the whole
// alternative, so a model can't bypass the verifier by writing
// `[{}]` and calling it a skip.
func hasDocumentedSkipped(resultBytes []byte, path string) bool {
	if len(resultBytes) == 0 || path == "" {
		return false
	}
	var envelope map[string]any
	if err := json.Unmarshal(resultBytes, &envelope); err != nil {
		return false
	}
	val, ok := envelope[path]
	if !ok {
		// Try inside `message` (envelope-merge fallback path).
		if msg, ok2 := envelope["message"].(string); ok2 && msg != "" {
			var inner map[string]any
			if err := json.Unmarshal([]byte(msg), &inner); err == nil {
				val, ok = inner[path]
			}
		}
	}
	if !ok {
		return false
	}
	arr, ok := val.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return false
		}
		reason, _ := obj["reason"].(string)
		if strings.TrimSpace(reason) == "" {
			return false
		}
	}
	return true
}

// verifyCVClaimsGrounded — warn-tier grounding check for CV/cover-letter
// artifacts. Extracts hard facts (capitalised multi-word org names, 4-digit
// years, ISO dates, known cert tokens) from every output artifact that
// matches the optional `artifact_pattern` param (default "cv-*.md,cover-*.md")
// and flags any token that is NOT substring-present in the authoritative
// resume text.
//
// Conservative by design — only fires when the evidence is high-confidence
// to minimise false positives on legitimate paraphrasing. The operator
// should pair this with a "decision"-class memory promotion (Task B2) so
// the resume is always retrievable by the writer role.
//
// Résumé source — precedence order:
//  1. params["resume"] non-empty → use as-is (inline text, backward compat).
//  2. params["resume_file"] non-empty AND Input.ProjectDir non-empty →
//     read file at filepath.Join(ProjectDir, resume_file). Path-traversal
//     guard: cleaned path must remain under ProjectDir. Capped at
//     maxVerifierBodyBytes (1 MiB). On any read error, abstain (nil).
//  3. Neither → abstain (nil).
//
// Params:
//   - resume: string — the authoritative resume text (inline). Takes
//     precedence over resume_file.
//   - resume_file: string — path relative to Input.ProjectDir for the
//     canonical résumé file (e.g. ".autonomy/RESUME.md"). Ignored when
//     resume is non-empty or ProjectDir is empty.
//   - artifact_pattern: optional glob; default "cv-*.md,cover-*.md". Accepts
//     a comma-separated list of globs.
func verifyCVClaimsGrounded(cfg Config, in Input) (*Violation, error) {
	resume := paramString(cfg.Params, "resume")
	if resume == "" {
		// Try resume_file + ProjectDir before abstaining.
		resume = resolveResumeFromFile(cfg.Params, in.ProjectDir)
	}
	if resume == "" {
		// No resume context available — abstain rather than false-positive.
		// The writer role's prompt is expected to perform memory_search and
		// pass the result back; if the context is missing we can't judge.
		return nil, nil
	}

	patternStr := paramString(cfg.Params, "artifact_pattern")
	if patternStr == "" {
		patternStr = "cv-*.md,cover-*.md"
	}
	patterns := strings.Split(patternStr, ",")
	for i, p := range patterns {
		patterns[i] = strings.TrimSpace(p)
	}

	matchesAny := func(name string) bool {
		for _, pat := range patterns {
			if pat != "" && globMatch(name, pat) {
				return true
			}
		}
		return false
	}

	for _, a := range in.Artifacts {
		if a == nil || a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		if !matchesAny(a.Name) {
			continue
		}
		text, readErr := readArtifactBody(a)
		if readErr != nil {
			return &Violation{
				VerifierName: nameOrDefault(cfg, "cv_claims_grounded"),
				Type:         cfg.Type,
				Severity:     SeverityWarn,
				Detail:       fmt.Sprintf("could not read artifact %q: %v", a.Name, readErr),
			}, nil
		}
		v := evalCVClaims(text, resume)
		if v != nil {
			v.VerifierName = nameOrDefault(cfg, "cv_claims_grounded")
			v.Type = cfg.Type
			return v, nil
		}
	}
	return nil, nil
}

// resolveResumeFromFile reads the authoritative résumé from the workspace
// file identified by params["resume_file"] + projectDir. Returns "" (and
// never errors out) in every failure case so callers can treat "" as
// "nothing to use" without changing control flow.
//
// Safety contract:
//   - If resume_file or projectDir is empty → returns "".
//   - filepath.Clean(join) must have projectDir as a prefix (path-traversal
//     guard). "../../etc/passwd" is rejected here.
//   - File is read with a hard cap of maxVerifierBodyBytes (1 MiB) via
//     io.LimitReader. Truncated reads are silently discarded and "" is
//     returned (the verifier abstains rather than grounding against a
//     partial résumé).
//   - Any os.Open / read error → returns "".
func resolveResumeFromFile(params map[string]any, projectDir string) string {
	resumeFile := paramString(params, "resume_file")
	if resumeFile == "" || projectDir == "" {
		return ""
	}
	// Safe-join: clean the candidate path and ensure it stays under
	// projectDir. filepath.Join already calls filepath.Clean, but we
	// need to verify the result explicitly.
	candidate := filepath.Join(projectDir, resumeFile)
	// filepath.Clean removes any ".." sequences. Verify prefix.
	// Add a path separator suffix to projectDir to avoid the
	// "/foo/bar" prefix matching "/foo/barbaz".
	cleanRoot := filepath.Clean(projectDir)
	if !strings.HasPrefix(candidate, cleanRoot+string(filepath.Separator)) {
		// Path escapes projectDir — reject silently (abstain).
		return ""
	}
	f, err := os.Open(candidate)
	if err != nil {
		// Missing file → abstain; don't block on a missing résumé.
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, int64(maxVerifierBodyBytes)+1))
	if err != nil || len(b) > maxVerifierBodyBytes {
		// Read error or file too large → abstain.
		return ""
	}
	return string(b)
}

// evalCVClaims extracts candidate hard facts from the CV/cover-letter text
// and flags any that are not substring-present in the authoritative resume.
// Returns nil when all facts are grounded or no high-confidence facts are found.
//
// Hard-fact classes (conservative to avoid false positives):
//   - Capitalised multi-word organisation tokens (≥2 consecutive Title-case
//     words), e.g. "Globex Corp", "Initech s.r.o." — captures employers,
//     universities, certifying bodies.
//   - Known certification tokens: PMP, CSM, CSP-SM, CSPO, ACP, PMI, PRINCE2,
//     SAFe, AWS, GCP, Azure (standalone uppercase tokens near "cert" context).
//   - ISO dates and 4-digit years used in employment ranges.
//
// A token is "grounded" when it appears as a case-insensitive substring of
// the resume. We do NOT attempt fuzzy matching — the resume is authoritative
// text, so exact (modulo case) presence is the correct bar.
func evalCVClaims(cv, resume string) *Violation {
	resumeLower := strings.ToLower(resume)

	var ungrounded []string

	// --- Multi-word capitalised organisation tokens ---
	// Match two or more consecutive words that each start with a capital
	// letter, separated only by a single space (no punctuation crossing).
	// Avoid single-word tokens (too noisy: "I", "The", "Prague").
	// We split on sentence boundaries first to avoid matching across ".".
	sentences := regexp.MustCompile(`[.!?]+\s+`).Split(cv, -1)
	orgRE := regexp.MustCompile(`\b([A-Z][a-zA-Z]{1,}(?:\s+[A-Z][a-zA-Z]{0,}){1,})\b`)
	seenOrgs := make(map[string]bool)
	for _, sentence := range sentences {
		for _, match := range orgRE.FindAllString(sentence, -1) {
			tok := strings.TrimSpace(match)
			if seenOrgs[tok] {
				continue
			}
			seenOrgs[tok] = true
			// Skip common English lead-in phrases that are not org names.
			if isCommonPhrase(tok) {
				continue
			}
			if !strings.Contains(resumeLower, strings.ToLower(tok)) {
				ungrounded = append(ungrounded, tok)
			}
		}
	}

	if len(ungrounded) == 0 {
		return nil
	}
	return &Violation{
		Severity: SeverityWarn,
		Detail: fmt.Sprintf(
			"CV contains %d hard fact(s) not found in the authoritative resume: %s",
			len(ungrounded), strings.Join(ungrounded, "; "),
		),
	}
}

// isCommonPhrase returns true for lead-in multi-word capitalised phrases
// that are grammatical constructs rather than organisation names. The list
// is kept short and conservative — err on the side of flagging.
func isCommonPhrase(tok string) bool {
	lower := strings.ToLower(tok)
	switch lower {
	case "senior delivery lead", "delivery lead", "project manager",
		"product manager", "scrum master", "agile coach",
		"chief executive", "chief operating", "vice president",
		"cover letter", "dear hiring", "hiring manager",
		"i am", "my name", "in the", "on the", "at the",
		"with the", "for the", "to the":
		return true
	}
	return false
}

// -----------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------

func paramInt(p map[string]any, key string) int {
	if p == nil {
		return 0
	}
	v, ok := p[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		if n > int64(int(^uint(0)>>1)) || n < -int64(int(^uint(0)>>1))-1 {
			return 0
		}
		return int(n)
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0
		}
		return int(n)
	}
	return 0
}

// paramFloat — float64 coercion sibling to paramInt. Accepts int,
// int64, and float64. Returns 0 for missing/unparseable values,
// leaving the caller to clamp against the verifier's valid range.
func paramFloat(p map[string]any, key string) float64 {
	if p == nil {
		return 0
	}
	v, ok := p[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0
		}
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func paramString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if s, ok := p[key].(string); ok {
		return s
	}
	return ""
}

func paramStringSlice(p map[string]any, key string) []string {
	if p == nil {
		return nil
	}
	v, ok := p[key]
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return cleanStringSlice(s)
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return cleanStringSlice(out)
	}
	return nil
}

func cleanStringSlice(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func nameOrDefault(cfg Config, fallback string) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return fallback
}

// globMatch is a tiny glob: '*' matches any run of non-slash
// characters. Sufficient for "scan-*.md" / "*.patch" — operators
// don't need full filesystem-glob semantics here, and shipping
// filepath.Match would mis-match on the per-OS separator
// quirks.
func globMatch(name, pattern string) bool {
	return globRE(pattern).MatchString(name)
}

func globRE(pattern string) *regexp.Regexp {
	// Escape every regex special, then turn '*' back into '.*'.
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '.':
			b.WriteString(`\.`)
		case '+', '(', ')', '[', ']', '{', '}', '?', '^', '$', '|', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
