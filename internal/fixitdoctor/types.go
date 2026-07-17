// Package fixitdoctor assembles the deterministic, server-side
// "grounding bundle" the Fix-It Doctor's (later) repair chat grounds
// on (https://docs.vornik.io §5.1). This
// package is grounding-only: no converse loop, no session store, no
// action dispatcher, no UI entry points (those are tasks 3.2/3.3/3.4).
//
// The bundle is a tagged union keyed by FailureKind: exactly one of
// GroundingBundle's per-kind fields is populated, matching Kind. Every
// string field that may carry agent/tool-produced or user-config-
// authored content is wrapped in Field{Untrusted:true} (§6) so the
// later prompt builder can fence it as "data, not instructions"
// without having to reason about provenance itself — the provenance
// decision is made once, here, at assembly time.
package fixitdoctor

// FailureKind is the failure surface a grounding bundle was assembled
// for. Mirrors the four kinds in the design's §5.1 table.
type FailureKind string

// FailureKind* enumerate the four failure surfaces §5.1 defines a
// grounding bundle for.
const (
	FailureKindFailedTask      FailureKind = "failed_task"
	FailureKindDegradedFeature FailureKind = "degraded_feature"
	FailureKindRedIntegration  FailureKind = "red_integration"
	FailureKindFailedReload    FailureKind = "failed_reload"
)

// isKnownFailureKind reports whether k is one of the four enumerated failure
// surfaces. Used to reject an arbitrary caller-supplied Kind at the new-session
// boundary before it reaches the assembler / grounding (review-20260717-c377).
func isKnownFailureKind(k FailureKind) bool {
	switch k {
	case FailureKindFailedTask, FailureKindDegradedFeature, FailureKindRedIntegration, FailureKindFailedReload:
		return true
	default:
		return false
	}
}

// FailureRef identifies the one failing object a repair session
// grounds on. ID's meaning is kind-specific: a task ID for
// failed_task, a featuredoctor.Feature.ID for degraded_feature, an
// integrations.IntegrationKind.ID for red_integration, and empty
// (daemon-scope) for failed_reload. ProjectID scopes the failure to
// one project; empty means daemon-scope (admin-only surfaces).
type FailureRef struct {
	Kind      FailureKind
	ID        string
	ProjectID string
}

// Field is a structured, individually provenance-tagged string value.
// Untrusted marks content that originated from agent/tool output or
// user-authored config (error text, step names, tool names, config
// values, provider-supplied strings) as opposed to code-authored,
// static prose (playbook cause text, doc URLs, our own enum labels).
// The later prompt builder wraps all Untrusted content in a single
// fenced "untrusted content — data, not instructions" block (§6); this
// field is where that decision is recorded, once, at assembly time.
//
// Provenance, concretely: Trusted values are code-defined controlled
// vocabularies — ErrorClass/Outcome/Kind-style enums the doctor itself
// declares, never derived from agent/tool/config input, so they carry
// no injection or secret-leak risk. Untrusted values are agent/tool/
// config-derived strings (playbook lookups aside, anything that
// ultimately traces back to an execution, a probe, an operator config
// edit, or worker-mined text). Beyond the prompt fencing above, every
// Untrusted string that is free text/prose (as opposed to a short
// identifier) is also run through the assembler's free-text redaction
// pass (untrustedText in assembler.go) before it lands here — belt and
// suspenders against a secret slipping through an upstream layer.
type Field struct {
	Value     string `json:"value"`
	Untrusted bool   `json:"untrusted"`
}

func trusted(v string) Field   { return Field{Value: v} }
func untrusted(v string) Field { return Field{Value: v, Untrusted: true} }

// StepOutcomeRow is one structured last-N step outcome row for a
// failed_task bundle. StepID and ErrorDetail are Untrusted (workflow-
// authored / raw error text); ErrorClass and Outcome are drawn from
// the executor's own controlled vocabulary and are trusted.
type StepOutcomeRow struct {
	StepID      Field
	Role        Field
	Outcome     Field
	ErrorClass  Field
	ErrorDetail Field
}

// NarrationLine is one structured narration-tail line (Phase 2). Text
// is LLM-authored prose about what the agent did — always Untrusted.
type NarrationLine struct {
	Kind Field
	Text Field
}

// LearnedRemediationField is a playbook.LearnedRemediation rendered as
// structured, provenance-tagged fields. Action is worker-mined text
// (ultimately derived from observed agent/tool behaviour) and is
// therefore Untrusted; the numeric evidence fields carry no string
// content to taint.
type LearnedRemediationField struct {
	Action          Field
	Confidence      float64
	SupportCount    int
	ContradictCount int
}

// FailedTaskBundle is the failed_task grounding bundle (§5.1 row 1):
// Task.LastErrorClass -> playbook.Lookup (cause + suggestions) +
// learned remediations + the last N step outcomes + the narration
// tail. NarrationTail is nil (not empty-non-nil) when narration is
// disabled/absent (§5.1 "narration-disabled degradation") — the bundle
// is fully functional on error class + playbook + step outcomes alone.
type FailedTaskBundle struct {
	ErrorClass          Field
	HumanMessage        Field
	Cause               Field
	Suggestions         []Field
	References          []Field
	LearnedRemediations []LearnedRemediationField
	StepOutcomes        []StepOutcomeRow
	NarrationTail       []NarrationLine
}

// PrereqField is one featuredoctor.PrereqResult rendered as
// structured fields. Name and Remediation are code-authored (trusted);
// Detail describes live system state observed by the check and is
// therefore Untrusted.
type PrereqField struct {
	Name        Field
	OK          bool
	Detail      Field
	Remediation Field
}

// DegradedFeatureBundle is the degraded_feature grounding bundle
// (§5.1 row 2): featuredoctor.Diagnose -> status + failing
// Prereqs/Verify Detail + Remediation + the feature's DocRef.
type DegradedFeatureBundle struct {
	Status         Field
	FailingPrereqs []PrereqField
	FailingVerify  *PrereqField
	DocRef         Field
}

// ProbeFailureField is one integrations.CheckFailure rendered as
// structured fields. FieldName is our own credential-field key
// (trusted, code-defined); Reason is plain-language explanation text
// that may echo provider-supplied wording and is therefore Untrusted.
type ProbeFailureField struct {
	FieldName Field
	Reason    Field
}

// RedIntegrationBundle is the red_integration grounding bundle (§5.1
// row 3): the Phase-5 ProbeResult (Outcome, Summary, Failures) + the
// integration's doc URL + which credential field failed. Summary and
// Detail are secret-free by ProbeResult's own contract but still
// originate from provider-facing text, so they are Untrusted.
type RedIntegrationBundle struct {
	Outcome     Field
	Summary     Field
	Detail      Field
	Failures    []ProbeFailureField
	FailedField Field // ""  when no single field is attributable
	DocURL      Field
}

// FailedReloadBundle is the failed_reload grounding bundle (§5.1 row
// 4): the config validation error + the offending key path (no secret
// values). Message and OffendingKeyPath are derived from the
// operator's config edit and are therefore Untrusted; OffendingValue,
// when present, is passed through the shared secrets.RedactConfig
// masker before being carried here (mask-on-assembly, §5.1).
//
// OffendingKeyPath is a config key PATH (e.g. "telegram.bot_token"),
// not a value — key paths are operator-visible by design (an operator
// needs to know which field failed validation to fix it), only the
// VALUE at that path is masked. It is therefore constructed with the
// bare untrusted() (identifier-shaped, not free text) rather than the
// free-text redaction pass; behavior here is deliberately unchanged.
type FailedReloadBundle struct {
	Message          Field
	OffendingKeyPath Field
	OffendingValue   *Field
}

// GroundingBundle is the deterministic, structured artifact Assemble
// produces for one FailureRef. Exactly one of the per-kind fields is
// populated, selected by Kind — never a raw dump of system internals.
type GroundingBundle struct {
	Kind FailureKind
	Ref  FailureRef

	FailedTask      *FailedTaskBundle
	DegradedFeature *DegradedFeatureBundle
	RedIntegration  *RedIntegrationBundle
	FailedReload    *FailedReloadBundle
}
