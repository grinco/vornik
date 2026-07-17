package fixitdoctor

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
	"vornik.io/vornik/internal/secrets"
)

// defaultStepOutcomeLimit / defaultNarrationLimit bound how much of a
// failed task's recent history rides in the bundle — enough context
// for a repair conversation, not a full replay dump.
const (
	defaultStepOutcomeLimit = 5
	defaultNarrationLimit   = 10
	defaultLearnedLimit     = 3
)

// freeTextDetector is the package-wide default secrets scanner backing
// untrustedText below. Built once (regex compilation is the expensive
// part of secrets.NewMultiDetector) and reused across every Assemble
// call.
var (
	freeTextDetectorOnce sync.Once
	freeTextDetector     *secrets.MultiDetector
)

// freeTextRedactor returns the lazily-built default detector.
// secrets.NewMultiDetector only errors on a malformed custom pattern;
// the zero-value Config uses secrets.DefaultPatterns()/DefaultAllowlist(),
// which are known-compilable, so the error branch is defensive only —
// a failure there degrades to a nil detector (untrustedText then
// passes the value through unscanned, same as before this
// defense-in-depth pass existed) rather than panicking.
func freeTextRedactor() *secrets.MultiDetector {
	freeTextDetectorOnce.Do(func() {
		d, err := secrets.NewMultiDetector(secrets.Config{})
		if err == nil {
			freeTextDetector = d
		}
	})
	return freeTextDetector
}

// untrustedText builds an Untrusted Field for free-text content — the
// assembler's defense-in-depth line against secret leakage (companion
// review 2026-07-10, review-20260710-e0b1.md CRITICAL finding). Every
// Untrusted string this package emits that is prose/free text (as
// opposed to a short controlled-shape identifier like a step ID or a
// config key path) MUST be constructed through this helper rather than
// the bare untrusted() constructor, so that:
//
//   - the assembler never relies on an upstream contract for secret
//     safety (Phase-5 probes are "secret-free by contract", Phase-2
//     narration is "redacted at source" — both are upstream promises,
//     not guarantees this package can verify), and
//   - every current AND future Untrusted free-text field goes through
//     exactly one scan+redact call site, so extending a bundle with a
//     new free-text field can't silently skip masking.
//
// Structured config keeps using the separate secrets.RedactConfig path
// (map-shaped, keyed by field name); this helper is for scalar prose.
func untrustedText(v string) Field {
	if d := freeTextRedactor(); d != nil {
		if findings := d.Scan([]byte(v)); len(findings) > 0 {
			v = string(secrets.Redact([]byte(v), findings))
		}
	}
	return untrusted(v)
}

// ReloadValidationError is the failed-reload input the (later, 3.4)
// UI wiring supplies: the config validation error message plus the
// offending key path, mirroring internal/ui/config_reload.go's
// unexported reloadResult shape without this package importing
// internal/ui (which would risk an import cycle and reaches into a
// type that isn't exported). OffendingValue is the raw (possibly
// secret-shaped) value that failed validation, if known; Assemble
// masks it through the shared secrets.RedactConfig before it reaches
// the bundle — never pass an already-redacted value here.
type ReloadValidationError struct {
	Message          string
	OffendingKeyPath string
	OffendingValue   string
}

// ReloadStatusProvider is the seam the failed_reload builder reads
// through. Implementations adapt whatever in-memory or persisted
// reload state a deployment tracks (task 3.4 wires the real one from
// ui.Server); nil is treated as "no reload error known" (§5.1
// graceful-degradation posture — the doctor never panics on a missing
// optional dependency).
type ReloadStatusProvider interface {
	LatestReloadError(ctx context.Context, ref FailureRef) (ReloadValidationError, bool, error)
}

// IntegrationProbeProvider is the seam the red_integration builder
// reads through. Implementations adapt however a deployment caches
// Phase-5 probe results (task 3.4 wires the real one from
// ui.Server's per-kind+project probe cache); nil is treated as "no
// probe result known".
type IntegrationProbeProvider interface {
	LatestProbe(ctx context.Context, ref FailureRef) (result integrations.ProbeResult, docURL string, ok bool, err error)
}

// Assembler builds grounding bundles from a FailureRef, deterministically
// and server-side (§5.1). Every field is optional-nil-safe per its own
// documented degradation: an Assembler zero value with only Tasks set
// still assembles a functional failed_task bundle without learned
// remediations or narration.
type Assembler struct {
	// Tasks / Executions / StepOutcomes back the failed_task bundle.
	Tasks        persistence.TaskRepository
	Executions   persistence.ExecutionRepository
	StepOutcomes persistence.ExecutionStepOutcomeRepository

	// Narration is optional; nil means narration disabled/absent and
	// the failed_task bundle simply omits NarrationTail (§5.1).
	Narration persistence.ExecutionNarrationRepository

	// Learned is optional; nil degrades to the static playbook corpus
	// only (playbook.LearnedRemediations is itself nil-safe, mirrored
	// here for callers that construct an Assembler without it).
	Learned playbook.LearnedRemediationLister

	// Features backs the degraded_feature bundle. Defaults to
	// featuredoctor.Registry() when nil.
	Features    []featuredoctor.Feature
	FeatureDeps featuredoctor.Deps

	// IntegrationProbes backs the red_integration bundle. Optional;
	// nil yields ErrNoProbeResult for that kind.
	IntegrationProbes IntegrationProbeProvider

	// ReloadStatus backs the failed_reload bundle. Optional; nil
	// yields ErrNoReloadError for that kind.
	ReloadStatus ReloadStatusProvider

	// StepOutcomeLimit / NarrationLimit / LearnedLimit cap the
	// respective slices; <=0 uses the package defaults.
	StepOutcomeLimit int
	NarrationLimit   int
	LearnedLimit     int
}

func (a *Assembler) stepOutcomeLimit() int {
	if a.StepOutcomeLimit > 0 {
		return a.StepOutcomeLimit
	}
	return defaultStepOutcomeLimit
}

func (a *Assembler) narrationLimit() int {
	if a.NarrationLimit > 0 {
		return a.NarrationLimit
	}
	return defaultNarrationLimit
}

func (a *Assembler) learnedLimit() int {
	if a.LearnedLimit > 0 {
		return a.LearnedLimit
	}
	return defaultLearnedLimit
}

func (a *Assembler) features() []featuredoctor.Feature {
	if a.Features != nil {
		return a.Features
	}
	return featuredoctor.Registry()
}

// Assemble turns a (failure_kind, ref) into a masked, structured
// grounding bundle. Assembly is deterministic: given the same
// underlying state, the same bundle comes back every time.
func (a *Assembler) Assemble(ctx context.Context, ref FailureRef) (GroundingBundle, error) {
	switch ref.Kind {
	case FailureKindFailedTask:
		return a.assembleFailedTask(ctx, ref)
	case FailureKindDegradedFeature:
		return a.assembleDegradedFeature(ctx, ref)
	case FailureKindRedIntegration:
		return a.assembleRedIntegration(ctx, ref)
	case FailureKindFailedReload:
		return a.assembleFailedReload(ctx, ref)
	default:
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: unknown failure kind %q", ref.Kind)
	}
}

// --- failed_task -----------------------------------------------------

func (a *Assembler) assembleFailedTask(ctx context.Context, ref FailureRef) (GroundingBundle, error) {
	if a.Tasks == nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: failed_task requires a TaskRepository")
	}
	task, err := a.Tasks.Get(ctx, ref.ID)
	if err != nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: load task %s: %w", ref.ID, err)
	}

	class := ""
	if task.LastErrorClass != nil {
		class = *task.LastErrorClass
	}
	entry := playbook.Lookup(class)

	bundle := &FailedTaskBundle{
		ErrorClass:   trusted(class),
		HumanMessage: trusted(entry.HumanFriendly()),
		Cause:        trusted(entry.Cause),
	}
	for _, s := range entry.Suggestions {
		bundle.Suggestions = append(bundle.Suggestions, trusted(s))
	}
	for _, r := range entry.References {
		bundle.References = append(bundle.References, trusted(r))
	}

	learned, lerr := playbook.LearnedRemediations(ctx, a.Learned, class, task.ProjectID, "", a.learnedLimit())
	if lerr != nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: learned remediations for task %s: %w", ref.ID, lerr)
	}
	for _, l := range learned {
		bundle.LearnedRemediations = append(bundle.LearnedRemediations, LearnedRemediationField{
			Action:          untrustedText(l.Action),
			Confidence:      l.Confidence,
			SupportCount:    l.SupportCount,
			ContradictCount: l.ContradictCount,
		})
	}

	var executionID string
	if a.Executions != nil {
		if exec, eerr := a.Executions.GetByTaskID(ctx, ref.ID); eerr == nil && exec != nil {
			executionID = exec.ID
		}
		// A missing/errored execution lookup degrades gracefully — the
		// bundle is still functional on error class + playbook alone.
	}

	if executionID != "" && a.StepOutcomes != nil {
		rows, serr := a.StepOutcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{
			ExecutionID: &executionID,
			PageSize:    a.stepOutcomeLimit(),
		})
		if serr != nil {
			return GroundingBundle{}, fmt.Errorf("fixitdoctor: step outcomes for execution %s: %w", executionID, serr)
		}
		for _, r := range rows {
			if r == nil {
				continue
			}
			bundle.StepOutcomes = append(bundle.StepOutcomes, StepOutcomeRow{
				StepID:      untrusted(r.StepID),
				Role:        untrusted(r.Role),
				Outcome:     trusted(r.Outcome),
				ErrorClass:  trusted(r.ErrorClass),
				ErrorDetail: untrustedText(r.ErrorDetail),
			})
		}
	}

	if executionID != "" && a.Narration != nil {
		lines, nerr := a.Narration.ListByExecution(ctx, executionID)
		if nerr != nil {
			return GroundingBundle{}, fmt.Errorf("fixitdoctor: narration for execution %s: %w", executionID, nerr)
		}
		bundle.NarrationTail = narrationTail(lines, a.narrationLimit())
	}
	// Narration disabled/absent (a.Narration == nil, or no rows found)
	// leaves NarrationTail nil — the design's mandated degradation.

	return GroundingBundle{Kind: FailureKindFailedTask, Ref: ref, FailedTask: bundle}, nil
}

// narrationTail sorts defensively by seq ascending (mirrors
// ui.storyLines' documented invariant) and returns the last `limit`
// lines — the most recent chronological slice of the story.
//
// Nil entries are dropped BEFORE sorting (regression test
// TestAssemble_FailedTask_NarrationSkipsNilRows, task 3.1 TDD pass:
// the original version filtered nils only after sort.SliceStable, so
// the less-func itself dereferenced a nil row mid-sort and panicked).
func narrationTail(rows []*persistence.ExecutionNarration, limit int) []NarrationLine {
	filtered := make([]*persistence.ExecutionNarration, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Seq < filtered[j].Seq })
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	out := make([]NarrationLine, 0, len(filtered))
	for _, r := range filtered {
		out = append(out, NarrationLine{
			Kind: trusted(r.Kind),
			Text: untrustedText(r.Text),
		})
	}
	return out
}

// --- degraded_feature --------------------------------------------------

func (a *Assembler) assembleDegradedFeature(ctx context.Context, ref FailureRef) (GroundingBundle, error) {
	var feature *featuredoctor.Feature
	for _, f := range a.features() {
		if f.ID == ref.ID {
			ff := f
			feature = &ff
			break
		}
	}
	if feature == nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: unknown feature %q", ref.ID)
	}

	diag := featuredoctor.Diagnose(ctx, *feature, a.FeatureDeps)

	bundle := &DegradedFeatureBundle{
		Status: trusted(string(diag.Status)),
		DocRef: trusted(feature.DocRef),
	}
	for _, named := range diag.Prereqs {
		if named.OK {
			continue
		}
		bundle.FailingPrereqs = append(bundle.FailingPrereqs, PrereqField{
			Name:        trusted(named.Name),
			OK:          named.OK,
			Detail:      untrustedText(named.Detail),
			Remediation: trusted(named.Remediation),
		})
	}
	if diag.Verify != nil && !diag.Verify.OK {
		bundle.FailingVerify = &PrereqField{
			Name:        trusted("verify"),
			OK:          diag.Verify.OK,
			Detail:      untrustedText(diag.Verify.Detail),
			Remediation: trusted(diag.Verify.Remediation),
		}
	}

	return GroundingBundle{Kind: FailureKindDegradedFeature, Ref: ref, DegradedFeature: bundle}, nil
}

// --- red_integration ----------------------------------------------------

func (a *Assembler) assembleRedIntegration(ctx context.Context, ref FailureRef) (GroundingBundle, error) {
	if a.IntegrationProbes == nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: red_integration requires an IntegrationProbeProvider")
	}
	result, docURL, ok, err := a.IntegrationProbes.LatestProbe(ctx, ref)
	if err != nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: latest probe for %q: %w", ref.ID, err)
	}
	if !ok {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: no probe result known for %q", ref.ID)
	}

	bundle := &RedIntegrationBundle{
		Outcome: trusted(string(result.Outcome)),
		Summary: untrustedText(result.Summary),
		Detail:  untrustedText(result.Detail),
		DocURL:  trusted(docURL),
	}
	for _, f := range result.Failures {
		bundle.Failures = append(bundle.Failures, ProbeFailureField{
			FieldName: trusted(f.Field),
			Reason:    untrustedText(f.Reason),
		})
	}
	if len(result.Failures) > 0 {
		bundle.FailedField = trusted(result.Failures[0].Field)
	}

	return GroundingBundle{Kind: FailureKindRedIntegration, Ref: ref, RedIntegration: bundle}, nil
}

// --- failed_reload -------------------------------------------------------

func (a *Assembler) assembleFailedReload(ctx context.Context, ref FailureRef) (GroundingBundle, error) {
	if a.ReloadStatus == nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: failed_reload requires a ReloadStatusProvider")
	}
	rv, ok, err := a.ReloadStatus.LatestReloadError(ctx, ref)
	if err != nil {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: latest reload error: %w", err)
	}
	if !ok {
		return GroundingBundle{}, fmt.Errorf("fixitdoctor: no reload error known for daemon scope")
	}

	bundle := &FailedReloadBundle{
		// Message is free text: a validator can (and in practice does)
		// echo the offending value inline in its error string, so it
		// goes through the free-text redaction pass rather than the
		// bare untrusted() constructor. OffendingKeyPath is a config
		// key path, not free text — see the OffendingKeyPath doc
		// comment on FailedReloadBundle (types.go) for why it stays
		// unredacted.
		Message:          untrustedText(rv.Message),
		OffendingKeyPath: untrusted(rv.OffendingKeyPath),
	}
	if rv.OffendingValue != "" {
		// Mask-on-assembly (§5.1): pass the single offending value
		// through the shared redactor keyed by its own path so a
		// secret-shaped key path (e.g. "telegram.bot_token") never
		// carries its raw value into the bundle. RedactConfig walks
		// map[string]any, so wrap the single key/value in one.
		// ,ok guard (review-20260716-d95b #1): RedactConfig returns `any`;
		// a future change to its return shape must not panic here. On a
		// non-map result, fall back to a fully-redacted placeholder rather
		// than crash — never carry the raw value through.
		maskedValue := "[redacted]"
		if masked, ok := secrets.RedactConfig(map[string]any{
			rv.OffendingKeyPath: rv.OffendingValue,
		}).(map[string]any); ok {
			if mv, ok := masked[rv.OffendingKeyPath].(string); ok {
				maskedValue = mv
			}
		}
		f := untrusted(maskedValue)
		bundle.OffendingValue = &f
	}

	return GroundingBundle{Kind: FailureKindFailedReload, Ref: ref, FailedReload: bundle}, nil
}
