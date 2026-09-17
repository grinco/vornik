package configassist

import (
	"path"
	"sort"
	"strings"
)

// Blast-radius classes (design §6). Ordered most permissive → most
// restrictive; Rank orders them so "most restrictive wins" is a max.
const (
	ClassA  = "A"  // tuning: cadence, timeouts, retry counts DOWN, maxTasksPerHour DOWN
	ClassB1 = "B1" // steering prose that reaches a system prompt UNWRAPPED (autonomy.goal, step prompt)
	ClassB2 = "B2" // descriptive prose the daemon wraps as data (PROJECT_CONTEXT.md, description)
	ClassC  = "C"  // topology: steps, transitions, feeds, new workflow/swarm files
	ClassD  = "D"  // spend: budget caps UP, cadence DOWN (more ticks), model changes, retries/timeouts UP
	ClassE  = "E"  // authority: allowedTools, permissions.*, MCP grants, admin.*, and anything unclassifiable
)

// Rank orders classes by restrictiveness (design §6.1: E > D > C > B > A).
func Rank(class string) int {
	switch class {
	case ClassA:
		return 0
	case ClassB2:
		return 1
	case ClassB1:
		return 2
	case ClassC:
		return 3
	case ClassD:
		return 4
	default:
		return 5 // E and anything unknown
	}
}

// MostRestrictive returns the highest-ranked class among the inputs; an
// empty input is class E (nothing classified = deny by default).
func MostRestrictive(classes ...string) string {
	best := ""
	for _, c := range classes {
		if best == "" || Rank(c) > Rank(best) {
			best = c
		}
	}
	if best == "" {
		return ClassE
	}
	return best
}

// Touch is one classified change: a file, the dotted key (or "" for a
// whole-file/topology change), the class and the reason.
type Touch struct {
	File   string
	Key    string
	Class  string
	Reason string
}

// Change is what the classifier sees for one key: the file it lives in,
// the dotted key, and the before/after scalar rendering ("" when absent).
// Direction matters for cadence, retries, timeouts and caps.
type Change struct {
	File   string
	Key    string
	Before string
	After  string
}

// BundleClass is the classification result for a bundle.
type BundleClass struct {
	Class   string
	Touches []Touch
}

// Reasons returns the per-touch explanations that produced the bundle class.
func (b BundleClass) Reasons() []string {
	var out []string
	for _, t := range b.Touches {
		out = append(out, t.File+" "+t.Key+" → "+t.Class+" ("+t.Reason+")")
	}
	return out
}

// ClassifyChanges applies the deterministic class rules to every touched
// key and returns the bundle class (most restrictive wins — design §6.1)
// with the per-touch record the operator sees.
func ClassifyChanges(changes []Change) BundleClass {
	var out BundleClass
	for _, ch := range changes {
		t := classifyOne(ch)
		out.Touches = append(out.Touches, t)
	}
	classes := make([]string, 0, len(out.Touches))
	for _, t := range out.Touches {
		classes = append(classes, t.Class)
	}
	out.Class = MostRestrictive(classes...)
	sort.SliceStable(out.Touches, func(i, j int) bool { return Rank(out.Touches[i].Class) > Rank(out.Touches[j].Class) })
	return out
}

// ClassifyFileEvent classifies a whole-file event: a NEW file under
// workflows/ or swarms/ is topology (C); a new project file is C; an
// unknown location is E.
func ClassifyFileEvent(file string, created bool) Touch {
	dir := strings.ToLower(path.Dir(filepathToSlash(file)))
	base := strings.ToLower(path.Base(file))
	switch {
	case !created:
		return Touch{File: file, Class: ClassA, Reason: "existing file replaced; keys classified individually"}
	case strings.HasPrefix(dir, "workflows"), strings.HasPrefix(dir, "swarms"):
		return Touch{File: file, Class: ClassC, Reason: "new workflow/swarm file is a topology change"}
	case strings.HasPrefix(dir, "projects") && (strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml")):
		return Touch{File: file, Class: ClassC, Reason: "new project file is a topology change"}
	case strings.HasPrefix(dir, "projects") && strings.HasSuffix(base, ".md"):
		return Touch{File: file, Class: ClassB2, Reason: "new project prose (wrapped as data by the daemon)"}
	default:
		return Touch{File: file, Class: ClassE, Reason: "new file outside the classified tree (unclassifiable = E)"}
	}
}

func filepathToSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// classifyOne is the rule table. Keys are the dotted path within the file
// (project YAML keys as registry.ProjectConfigKeys renders them; workflow
// frontmatter as steps.<id>.<field>; swarm frontmatter as
// roles.<name>.<field>). A key no rule recognises is E (design §6.1); prose
// no rule recognises is B1 (design §6.4).
func classifyOne(ch Change) Touch {
	k := ch.Key
	lk := strings.ToLower(k)
	t := Touch{File: ch.File, Key: k}
	switch {
	// ---- E: a document the flattener refused to expand (audit CA-02:
	// cyclic aliases, runaway expansion, unparseable YAML). Checked before
	// everything, because the alternative reading — an empty document, no
	// changes, class A — is exactly the one an attacker wants.
	case strings.HasPrefix(k, "<") && strings.HasSuffix(k, ">"):
		t.Class, t.Reason = ClassE, "the document could not be safely expanded ("+strings.Trim(k, "<>")+"): deny by default"
	// ---- E: authority. Checked FIRST so a widening cannot hide behind a
	// friendlier prefix (the allowed_tools/allowedTools casing collision is
	// exactly a key that fails the strict decode and lands here as E).
	case strings.Contains(lk, "allowedtools"), strings.Contains(lk, "allowed_tools"),
		strings.HasPrefix(lk, "permissions."), lk == "permissions",
		strings.HasPrefix(lk, "mcp."), lk == "mcp", strings.Contains(lk, ".mcp."),
		strings.HasPrefix(lk, "admin."), strings.Contains(lk, "secrets"),
		strings.Contains(lk, "sender_allowlist"), strings.Contains(lk, "channel_allowlist"),
		strings.Contains(lk, "allow_unlisted"), strings.Contains(lk, "acceptcallsfrom"), strings.Contains(lk, "cancallprojects"),
		strings.Contains(lk, "allowspawn"), strings.Contains(lk, "write_allowlist"), strings.Contains(lk, "webhooks"),
		strings.Contains(lk, "_env"), strings.Contains(lk, "private_key"), strings.Contains(lk, "webhook_secret"),
		strings.HasPrefix(lk, "firewall."), strings.HasPrefix(lk, "taint_lineage"), strings.Contains(lk, "runtimepolicy"),
		strings.HasPrefix(lk, "trading.killswitch"), strings.HasPrefix(lk, "trading.mode"), strings.HasPrefix(lk, "trading.entry_policy"),
		strings.HasPrefix(lk, "trading.caps"), strings.HasPrefix(lk, "trading_rate_limit"):
		t.Class, t.Reason = ClassE, "authority: permissions, tool grants, MCP, admin, secrets or trading guardrails"
	// ---- D: spend.
	case strings.HasPrefix(lk, "budget."):
		t.Class, t.Reason = ClassD, spendDirection(ch, "budget cap")
	case strings.HasSuffix(lk, ".model"), lk == "model", strings.HasSuffix(lk, "modelfallback"), strings.HasSuffix(lk, ".model_fallback"),
		strings.HasSuffix(lk, "maxtokens"), strings.HasSuffix(lk, "contextsize"), lk == "assistant.model", lk == "hallucinationjudge.model":
		t.Class, t.Reason = ClassD, "model or token-window change alters what a privileged role does with what it has"
	case lk == "autonomy.maxtasksperhour", strings.HasPrefix(lk, "rate_limit."), lk == "maxconcurrenttasks":
		switch {
		case removesNumericLimit(ch):
			// Manager.checkRateLimit: `MaxTasksPerHour <= 0` returns true,
			// "no limit". Zero is not a smaller number here, it is the
			// ABSENCE of the cap — and so is deleting the key, which decodes
			// to the same zero (audit 2026-09-15 CA-01).
			t.Class, t.Reason = ClassD, "throughput cap removed: zero or absent is the no-limit sentinel, so this RAISES throughput"
		case numericIncreased(ch):
			t.Class, t.Reason = ClassD, "throughput cap raised (more spend)"
		default:
			t.Class, t.Reason = ClassA, "throughput cap lowered or unchanged"
		}
	case strings.HasSuffix(lk, ".cadence"), lk == "autonomy.pollinterval", lk == "autonomy.feeds", strings.HasPrefix(lk, "autonomy.feeds"):
		switch {
		case durationShortened(ch):
			t.Class, t.Reason = ClassD, "cadence shortened: more ticks, more spend (design §6: a cadence edit is a spend edit wearing a tuning edit's clothes)"
		case ch.Before == "" && ch.After != "" && (lk == "autonomy.feeds" || strings.HasPrefix(lk, "autonomy.feeds.")):
			// Matching only the whole-key transition ("" -> a first feed)
			// missed the shape an insertion actually produces: per-item keys
			// like autonomy.feeds.<slug>.url. Adding a feed to a NON-EMPTY
			// list is the same topology change as declaring the first one
			// (audit 2026-09-15 CA-01).
			t.Class, t.Reason = ClassC, "feed declared: topology"
		default:
			t.Class, t.Reason = ClassA, "cadence lengthened or unchanged"
		}
	case strings.HasSuffix(lk, ".timeout"), strings.Contains(lk, "retrypolicy"), strings.Contains(lk, "retry"), strings.Contains(lk, "maxiterations"), strings.Contains(lk, "maxwallclock"), strings.Contains(lk, "maxstepvisits"):
		if valueIncreased(ch) {
			t.Class, t.Reason = ClassD, "retry/timeout/iteration increase can increase spend (review R7: increases are D, never safe tuning)"
		} else {
			t.Class, t.Reason = ClassA, "retry/timeout/iteration lowered or unchanged"
		}
	// ---- C: topology. Step sub-keys have their own table (a step's
	// prompt is B1, its transitions C, an unknown prose field B1, an
	// unknown non-prose field E).
	case strings.HasPrefix(lk, "steps."):
		t.Class, t.Reason = classifyStepKey(lk, ch)
	case lk == "steps", lk == "entrypoint", strings.HasPrefix(lk, "terminals"), lk == "roles", strings.HasPrefix(lk, "roles.") && (strings.HasSuffix(lk, ".name") || strings.HasSuffix(lk, ".count")),
		lk == "swarmid", lk == "defaultworkflowid", lk == "workflowid", lk == "projectid", lk == "autonomy.workflow_id", lk == "leadrole", lk == "adaptivecandidateworkflows", lk == "autonomy.mode", lk == "autonomy.enabled", lk == "autonomy.allowedtasktypes", lk == "verifiers", strings.HasPrefix(lk, "qualityscoring"):
		if isProseStepField(lk) {
			t.Class, t.Reason = ClassB1, "step prompt reaches the model's system prompt unwrapped"
		} else {
			t.Class, t.Reason = ClassC, "topology: steps, transitions, roles, workflow/swarm binding"
		}
	// ---- B1: steering prose that reaches a system prompt unwrapped.
	case lk == "autonomy.goal", lk == "chat.system_prefix", strings.HasSuffix(lk, "systemprompt"), lk == "roleprelude", lk == "hallucinationjudge.prompt", strings.HasSuffix(lk, ".prompt"):
		t.Class, t.Reason = ClassB1, "steering prose interpolated into a system prompt unwrapped (design §6.4)"
	// ---- B2: descriptive prose the daemon wraps as data.
	case lk == "description", lk == "displayname", strings.HasSuffix(lk, ".description"), lk == "autonomy.contextfilepath", lk == "autonomy.usercontextfilepath", lk == "autonomy.backlogfilepath",
		lk == "project_context.md", lk == "project.md", lk == "readme.md":
		t.Class, t.Reason = ClassB2, "descriptive prose the daemon wraps as data"
	// ---- A: tuning.
	case lk == "autonomy.duplicatewindow", lk == "autonomy.requireapproval", lk == "autonomy.evaluate_timeout", strings.HasPrefix(lk, "autonomy.precheck"),
		lk == "defaultpriority", strings.HasPrefix(lk, "retention."), strings.HasPrefix(lk, "narrator."), lk == "pedantic", lk == "repo_scope",
		strings.HasPrefix(lk, "recording."), strings.HasPrefix(lk, "backlogdeposits"), strings.HasPrefix(lk, "hallucinationjudge.enabled"),
		strings.HasPrefix(lk, "slack.progress"), strings.HasPrefix(lk, "slack.post_message"), strings.HasPrefix(lk, "email.poll_interval"), strings.HasPrefix(lk, "voice."),
		strings.HasPrefix(lk, "trading.scorecard"), strings.HasPrefix(lk, "trading.regime"), strings.HasPrefix(lk, "trading.analysis_evidence"), strings.HasPrefix(lk, "trading.watchlist"), strings.HasPrefix(lk, "trading.protected_symbols"), strings.HasPrefix(lk, "trading.notify"),
		lk == "version", strings.HasPrefix(lk, "forge.ci."), lk == "forge.review_draft_prs", lk == "forge.auto_review_on_push", lk == "git.enabled", strings.HasPrefix(lk, "memory."), strings.HasPrefix(lk, "lifecycle."):
		if lk == "autonomy.requireapproval" && removesApproval(ch) {
			t.Class, t.Reason = ClassE, "removing the approval requirement widens what runs unattended"
		} else {
			t.Class, t.Reason = ClassA, "tuning"
		}
	default:
		if looksLikeProse(ch) {
			t.Class, t.Reason = ClassB1, "unclassified prose is B1 until classified (design §6.4)"
		} else {
			t.Class, t.Reason = ClassE, "unclassifiable key is E (design §6.1, deny by default)"
		}
	}
	return t
}

func isProseStepField(lk string) bool {
	return strings.HasPrefix(lk, "steps.") && strings.HasSuffix(lk, ".prompt")
}

// classifyStepKey classifies steps.<id>.<field...>.
func classifyStepKey(lk string, ch Change) (string, string) {
	parts := strings.Split(lk, ".")
	field := ""
	if len(parts) >= 3 {
		field = parts[2]
	}
	switch field {
	case "prompt":
		return ClassB1, "step prompt reaches the model's system prompt unwrapped"
	case "type", "role", "on_success", "on_fail", "gates", "on_outcome", "":
		return ClassC, "topology: steps, transitions, roles"
	case "timeout", "retrypolicy":
		if valueIncreased(ch) {
			return ClassD, "retry/timeout increase can increase spend (review R7: increases are D, never safe tuning)"
		}
		return ClassA, "retry/timeout lowered or unchanged"
	default:
		if looksLikeProse(ch) {
			return ClassB1, "unclassified step prose is B1 until classified (design §6.4)"
		}
		return ClassE, "unclassified step field is E (design §6.1, deny by default)"
	}
}

// looksLikeProse: multi-word natural language (spaces, sentence length).
func looksLikeProse(ch Change) bool {
	v := strings.TrimSpace(ch.After)
	if v == "" {
		v = strings.TrimSpace(ch.Before)
	}
	return len(v) > 40 && strings.Count(v, " ") >= 5
}
