package memetic

// Consumer B of the continuous-learning instinct layer: the architect
// consults workflow-domain instincts for the target workflow as
// evidence priors, and rejected proposals are written back as
// 'architect-reject' contradictions. Both halves are advisory and
// gated behind instinct.consumers.architect_priors (default false)
// AND instinct.enabled; the service layer only wires the InstinctSource
// / InstinctSink when the gate is on, so with the gate off the
// architect's behaviour is byte-for-byte unchanged.
//
// Scope discipline (mirrors the architect's v1 invariant): priors only
// influence the prompt framing + the proposal's motivation / evidence /
// confidence. They NEVER widen what the architect may edit (structural
// steps / terminals / transitions only) and never auto-apply — every
// proposal still passes the full validation pipeline and lands as a
// pending row the operator must approve.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/instinctmodel"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// architectInstinctsPageSize caps the priors pulled per propose turn.
// Workflow-domain instincts for a single workflow are few; this is a
// guard rail, not a paging knob.
const architectInstinctsPageSize = 50

// prior is one workflow-domain instinct relevant to the workflow being
// proposed on, split by polarity at load time so the prompt and the
// post-validation merge don't re-walk the polarity field.
type prior struct {
	inst     *persistence.Instinct
	negative bool // source 'architect-reject' → a previously declined change
}

// workflowInstinctTrigger is the canonical trigger shape for a
// workflow-domain instinct keyed to a specific workflow. The architect
// only knows the workflow by ID (it has no project handle), so the
// workflow ID rides in the TaskType field — the one Trigger axis that
// carries a free-form identifier. Both the priors query and the
// rejection write-back use this shape so they dedup onto the same row.
func workflowInstinctTrigger(workflowID string) instinctmodel.Trigger {
	return instinctmodel.Trigger{TaskType: workflowID}
}

// loadPriors fetches workflow-domain instincts for workflowID and
// returns them split into positive (support) and negative
// ('architect-reject') priors. Returns nil when no source is wired
// (gate off) or on any read error — priors are strictly best-effort
// and must never fail a propose turn.
func (a *Architect) loadPriors(ctx context.Context, workflowID string) []prior {
	if a.instincts == nil {
		return nil
	}
	domain := persistence.InstinctDomainWorkflow
	got, err := a.instincts.List(ctx, persistence.InstinctFilter{
		Domain:   &domain,
		PageSize: architectInstinctsPageSize,
	})
	if err != nil || len(got) == 0 {
		return nil
	}
	wantKey := instinctmodel.TriggerKey(domain, workflowInstinctTrigger(workflowID))
	out := make([]prior, 0, len(got))
	for _, inst := range got {
		if inst == nil {
			continue
		}
		// Match by the canonical trigger key so we only surface
		// instincts about THIS workflow, not every workflow-domain row.
		if inst.TriggerKey != wantKey {
			continue
		}
		// Retired instincts have decayed below the floor — they no
		// longer represent a live signal, so don't prime the architect
		// with them.
		if inst.Status == persistence.InstinctStatusRetired {
			continue
		}
		out = append(out, prior{
			inst: inst,
			negative: inst.Source == persistence.InstinctSourceArchitectReject ||
				strings.TrimSpace(inst.Action) != "" && hasContradictMajority(inst),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// loadRecoveryPriors fetches recovery-domain instincts whose trigger
// matches a failure class the rollup actually observed in the window —
// the observer-mined "retrying the step resolved the <error_class>
// failure" / "switching off model X resolved …" patterns. Without
// these, the architect proposes structural changes blind to recovery
// actions that already resolve the very failures it is reasoning about
// (2026-06-06 report). Same discipline as loadPriors: strictly
// advisory, best-effort (nil source / read error / no failures → nil),
// and never widens the architect's edit scope.
//
// Matching: a recovery instinct's trigger is {role, error_class[,
// model]}. The instinct is relevant when its error_class appears in
// the rollup's TopFailureClasses or as some step's TopErrorClass; a
// role-qualified trigger additionally requires a step with that role
// failing on that class. Retired and contradict-majority rows are
// skipped — their signal has decayed.
func (a *Architect) loadRecoveryPriors(ctx context.Context, rollup *workflowtelemetry.Rollup) []prior {
	if a.instincts == nil || rollup == nil {
		return nil
	}
	failClasses := make(map[string]bool)
	failPairs := make(map[string]bool) // role + "\x00" + class
	for _, fc := range rollup.TopFailureClasses {
		if fc.ErrorClass != "" {
			failClasses[fc.ErrorClass] = true
		}
	}
	for _, st := range rollup.Steps {
		if st.TopErrorClass == "" {
			continue
		}
		failClasses[st.TopErrorClass] = true
		failPairs[st.Role+"\x00"+st.TopErrorClass] = true
	}
	// Healthy window → nothing to match; don't even pay the query, so
	// a no-failure propose turn stays byte-for-byte unchanged.
	if len(failClasses) == 0 {
		return nil
	}
	domain := persistence.InstinctDomainRecovery
	got, err := a.instincts.List(ctx, persistence.InstinctFilter{
		Domain:   &domain,
		PageSize: architectInstinctsPageSize,
	})
	if err != nil || len(got) == 0 {
		return nil
	}
	out := make([]prior, 0, len(got))
	for _, inst := range got {
		if inst == nil || inst.Status == persistence.InstinctStatusRetired {
			continue
		}
		if hasContradictMajority(inst) {
			continue
		}
		var tr instinctmodel.Trigger
		if len(inst.Trigger) > 0 {
			if uerr := json.Unmarshal(inst.Trigger, &tr); uerr != nil {
				continue
			}
		}
		if tr.ErrorClass == "" || !failClasses[tr.ErrorClass] {
			continue
		}
		if tr.Role != "" && !failPairs[tr.Role+"\x00"+tr.ErrorClass] {
			continue
		}
		out = append(out, prior{inst: inst})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hasContradictMajority reports whether an instinct's evidence leans
// contradicting. Used as a secondary negative-prior signal alongside
// the source tag, so a support-sourced instinct that has accumulated
// mostly contradictions is still framed as a caution.
func hasContradictMajority(inst *persistence.Instinct) bool {
	return inst.ContradictCount > inst.SupportCount
}

// applyPriors folds positive priors into the proposal: cites them in
// the motivation, unions their evidence-equivalent signal, and raises
// (never lowers) the proposal's confidence toward the strongest
// supporting prior. Negative priors are not folded in — a proposal
// that survived validation despite a prior rejection should still be
// shown to the operator on its own merits; the contradiction already
// lowered the instinct's confidence and shaped the prompt. No-op when
// priors is empty.
func (a *Architect) applyPriors(p *persistence.WorkflowProposal, priors []prior) {
	if p == nil || len(priors) == 0 {
		return
	}
	var cites []string
	var ids []string
	var maxSupport float32
	for _, pr := range priors {
		if pr.negative || pr.inst == nil {
			continue
		}
		action := strings.TrimSpace(pr.inst.Action)
		if action != "" {
			cites = append(cites, action)
			// Record the contributing instinct's ID alongside its
			// action text (2026-06-07 review: motivation-only folding
			// made proposals untraceable back to their priors).
			ids = append(ids, pr.inst.ID)
		}
		if c := float32(pr.inst.Confidence); c > maxSupport {
			maxSupport = c
		}
	}
	if len(cites) == 0 {
		return
	}
	sort.Strings(cites)
	sort.Strings(ids)
	p.InstinctIDs = ids
	var b strings.Builder
	b.WriteString(strings.TrimSpace(p.Motivation))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString("Learned priors (instinct layer): ")
	b.WriteString(strings.Join(cites, "; "))
	p.Motivation = b.String()

	// Raise confidence toward the strongest prior, never above 1.0 and
	// never below what the LLM self-reported — priors corroborate, they
	// don't override an already-higher self-assessment.
	if maxSupport > p.Confidence {
		p.Confidence = maxSupport
		if p.Confidence > 1.0 {
			p.Confidence = 1.0
		}
	}
}

// RecordRejection writes a rejected proposal back as a workflow-domain
// 'architect-reject' contradiction instinct so the confidence model
// learns to stop re-proposing what operators decline. It is the second
// half of Consumer B and shares the trigger key with the priors query,
// so a re-rejection of the same workflow's change accumulates on one
// row rather than spawning duplicates.
//
// Best-effort and idempotent: a nil sink (gate off / not wired) is a
// silent no-op, and the underlying Upsert + AddEvidence are keyed so a
// replayed rejection doesn't double-count. The caller (the proposal
// reject handler) must NOT fail the rejection if this returns an error
// — the operator's decision already landed; the write-back is a
// learning side effect.
func (a *Architect) RecordRejection(ctx context.Context, sink InstinctSink, p *persistence.WorkflowProposal) error {
	if sink == nil || p == nil {
		return nil
	}
	if strings.TrimSpace(p.WorkflowID) == "" {
		return fmt.Errorf("memetic: RecordRejection: proposal has no workflow_id")
	}
	trig := workflowInstinctTrigger(p.WorkflowID)
	triggerJSON, err := instinctmodel.MarshalTrigger(trig)
	if err != nil {
		return fmt.Errorf("memetic: RecordRejection: marshal trigger: %w", err)
	}
	action := rejectionAction(p)
	now := time.Now().UTC()
	inst := &persistence.Instinct{
		Scope:      persistence.InstinctScopeProject,
		Domain:     persistence.InstinctDomainWorkflow,
		TriggerKey: instinctmodel.TriggerKey(persistence.InstinctDomainWorkflow, trig),
		Trigger:    triggerJSON,
		Action:     action,
		Source:     persistence.InstinctSourceArchitectReject,
		Status:     persistence.InstinctStatusCandidate,
		LastSeenAt: now,
	}
	id, err := sink.Upsert(ctx, inst)
	if err != nil {
		return fmt.Errorf("memetic: RecordRejection: upsert: %w", err)
	}
	// The contradiction evidence is the proposal itself; key the
	// evidence row on the proposal ID so a replayed rejection is a
	// no-op (AddEvidence is idempotent on (instinct_id, outcome_id)).
	if _, err := sink.AddEvidence(ctx, &persistence.InstinctEvidence{
		InstinctID: id,
		OutcomeID:  p.ID,
		Polarity:   persistence.InstinctPolarityContradict,
		CreatedAt:  now,
	}); err != nil {
		return fmt.Errorf("memetic: RecordRejection: add evidence: %w", err)
	}
	return nil
}

// rejectionAction renders the human-readable action text stored on the
// 'architect-reject' instinct. Kept short and operator-facing — it
// surfaces verbatim as a negative prior on the next propose turn.
func rejectionAction(p *persistence.WorkflowProposal) string {
	kind := strings.TrimSpace(string(p.Kind))
	if kind == "" || kind == string(persistence.WorkflowProposalKindUnspecified) {
		return fmt.Sprintf("operator rejected a structural proposal for workflow %s", p.WorkflowID)
	}
	return fmt.Sprintf("operator rejected a %s proposal for workflow %s", kind, p.WorkflowID)
}
