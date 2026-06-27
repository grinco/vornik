package memetic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Sentinels surface as errors.Is targets so callers (the admin
// endpoint) can return the right HTTP status without string-matching.

// ErrLowConfidence fires when the LLM returned a confidence below
// the configured floor. The proposal is dropped silently from the
// operator's queue; the API maps this to 204 No Content (architect
// ran, no proposal worth approving).
var ErrLowConfidence = errors.New("memetic: architect confidence below threshold")

// ErrInsufficientEvidence fires when the LLM's evidence_run_ids
// has fewer than MinEvidenceRunIDs entries. Maps to 422
// Unprocessable Entity.
var ErrInsufficientEvidence = errors.New("memetic: evidence_run_ids below required minimum")

// ErrEvidenceInvalid fires when one or more evidence_run_ids
// don't reference executions of the target workflow. Maps to 422.
var ErrEvidenceInvalid = errors.New("memetic: evidence_run_ids reference unknown or other-workflow executions")

// ErrWorkflowMismatch fires when the LLM emits a proposal whose
// workflow_id doesn't match the architect's input. Maps to 422.
// Guards against the LLM hallucinating a wholly different
// workflow.
var ErrWorkflowMismatch = errors.New("memetic: proposed workflow_id doesn't match input")

// ErrProposalYAMLInvalid fires when the proposed YAML fails
// registry.ValidateWorkflowMarkdown. Maps to 422.
var ErrProposalYAMLInvalid = errors.New("memetic: proposed YAML failed WORKFLOW.md validation")

// ErrMalformedOutput fires when the LLM emits text the JSON parser
// rejects, OR output past MaxOutputBytes. Maps to 502 Bad Gateway
// (the upstream model misbehaved).
var ErrMalformedOutput = errors.New("memetic: architect output is not valid JSON")

// architectResponseFormat asks the provider to constrain output to a
// JSON object. Set on the request context before the LLM call so the
// OpenAI-compatible / bedrock routes enforce it; routes that ignore it
// fall back to parseArchitectOutput's prose tolerance. json_object
// (not json_schema) keeps this provider-agnostic — tightening to a
// strict schema is a future change if a provider warrants it.
var architectResponseFormat = &chat.ResponseFormat{Type: "json_object"}

// ErrArchitectPaused fires when Config.Paused=true. Maps to 503
// ARCHITECT_PAUSED at the API layer. Operator-controllable kill
// switch (Slice 5) — LEVEL 1 (global).
var ErrArchitectPaused = errors.New("memetic: architect is paused by operator")

// ErrArchitectDisabledForWorkflow fires when the target workflow's
// frontmatter carries `architect_enabled: false` — LEVEL 2 of the
// three-level kill switch (per-workflow). Maps to 503 at the API
// layer, same as the global pause. Closes the §8.5 gap where only
// the global env-var level was wired.
var ErrArchitectDisabledForWorkflow = errors.New("memetic: architect is disabled for this workflow (architect_enabled: false)")

// ErrProposalKindDisabled fires when the architect emits a proposal
// whose kind is in Config.DisabledKinds — LEVEL 3 of the three-level
// kill switch (per-proposal-class). The proposal is dropped before
// insert. Maps to 403/409 at the API layer (operator declined this
// class of change). Only takes effect once the architect populates
// the proposal kind; until then every proposal is "unspecified" and
// passes unless "unspecified" itself is disabled.
var ErrProposalKindDisabled = errors.New("memetic: proposal kind is disabled by operator policy")

// Architect runs propose-only — Slice 2. It reads telemetry +
// current YAML, calls the configured LLM, validates the output,
// and persists a pending proposal. Apply path lives in Slice 4;
// rollback in Slice 5.
type Architect struct {
	provider   chat.Provider
	telemetry  TelemetrySource
	workflows  WorkflowSource
	execLookup ExecutionLookup
	proposals  ProposalSink
	cfg        Config

	// instincts is the optional evidence-prior source (Consumer B,
	// gated behind instinct.consumers.architect_priors). nil → the
	// architect behaves exactly as before. Wired via WithInstincts at
	// construction by the service layer only when the gate is on, so
	// gate-off behaviour is byte-for-byte identical to today.
	instincts InstinctSource

	// appWriter logs an instinct_applications row for every prior the
	// architect consults on a propose turn (review item W2). nil → no
	// application logging. Wired via WithApplicationWriter by the service
	// layer only when the gate is on. Best-effort: write errors never
	// fail the propose turn.
	appWriter ApplicationWriter

	// metrics, when set, lets the architect bump
	// ApplicationsTotal{architect_evidence,result} alongside each
	// recorded application. nil-safe at every call site.
	metrics *observability.InstinctMetrics

	// log records the architect's per-turn decisions: the parsed
	// confidence, whether a corrective retry fired, and which verdict
	// rejected (or accepted) the proposal. Defaults to a no-op logger
	// so a caller that doesn't wire one stays silent. The 2026-06-06
	// "confidence 0.00" incident was undiagnosable for days precisely
	// because this package logged nothing — every rejection looked
	// identical from the outside whether the model omitted the field,
	// emitted null, or honestly scored low.
	log zerolog.Logger
}

// ArchitectOption customises an Architect after the required deps are
// wired. Used for opt-in, gated extensions (Consumer B priors) so the
// base New signature — and gate-off behaviour — stays unchanged.
type ArchitectOption func(*Architect)

// WithInstincts wires the workflow-domain instinct prior source. Pass
// only when instinct.enabled && instinct.consumers.architect_priors;
// a nil source is a no-op (priors are simply never consulted).
func WithInstincts(src InstinctSource) ArchitectOption {
	return func(a *Architect) { a.instincts = src }
}

// WithApplicationWriter wires the instinct-application log sink (review
// item W2 — the architect-evidence surface of slice 7). Pass only when
// instinct.enabled && instinct.consumers.architect_priors; a nil writer
// is a no-op (applications are simply never recorded).
func WithApplicationWriter(w ApplicationWriter) ArchitectOption {
	return func(a *Architect) { a.appWriter = w }
}

// WithInstinctMetrics wires the instinct metrics so the architect can bump
// ApplicationsTotal when it records an application. nil → no metric
// emission (the recording still happens).
func WithInstinctMetrics(m *observability.InstinctMetrics) ArchitectOption {
	return func(a *Architect) { a.metrics = m }
}

// WithLogger wires the architect's decision logger. Pass the daemon's
// component logger (the service layer scopes it component=memetic) so
// every propose turn records whether the LLM was queried, the parsed
// confidence, retry decisions, and the rejecting verdict. A nil-value
// logger is tolerated — New defaults to zerolog.Nop() before options
// run, so omitting this option keeps the architect silent.
func WithLogger(l zerolog.Logger) ArchitectOption {
	return func(a *Architect) { a.log = l }
}

// New wires an Architect. Caller is responsible for passing a
// configured chat.Provider (model + endpoint already decided) —
// model selection lives in the daemon config, not here, so a
// future "different model for architect than for chat" swap is a
// service-layer change, not a memetic-package change.
func New(
	provider chat.Provider,
	telemetry TelemetrySource,
	workflows WorkflowSource,
	execLookup ExecutionLookup,
	proposals ProposalSink,
	cfg Config,
	opts ...ArchitectOption,
) *Architect {
	a := &Architect{
		provider:   provider,
		telemetry:  telemetry,
		workflows:  workflows,
		execLookup: execLookup,
		proposals:  proposals,
		cfg:        cfg,
		log:        zerolog.Nop(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

// Propose runs one architect turn for `workflowID`. Returns the
// inserted proposal on success; one of the sentinel errors above
// on a validation-class failure; persistence.ErrProposalRateLimited
// if a pending proposal already exists for this workflow
// (returned verbatim from the repository).
//
// The proposal's ID is generated here (prefix "wpr_"); the
// architect_model field comes from provider.Model() so the audit
// trail records which model emitted the proposal — useful when
// upgrading the architect model and comparing proposal quality.
func (a *Architect) Propose(ctx context.Context, workflowID string) (*persistence.WorkflowProposal, error) {
	return a.ProposeWithEvidence(ctx, workflowID, nil)
}

// ProposeWithEvidence runs one architect turn and includes trusted
// execution IDs from an upstream detector in the prompt. The IDs do
// not bypass validation; validateOutput still checks that every cited
// run belongs to this workflow before a proposal can be inserted.
func (a *Architect) ProposeWithEvidence(ctx context.Context, workflowID string, candidateEvidenceRunIDs []string) (proposal *persistence.WorkflowProposal, err error) {
	if workflowID == "" {
		return nil, fmt.Errorf("memetic: workflowID is required")
	}
	if a.cfg.Paused {
		return nil, ErrArchitectPaused
	}

	// Tag the call-site so the chat LoggingProvider attributes the
	// upcoming completion to the architect (rather than "unknown") in
	// the daemon log — this is how an operator confirms the architect
	// actually queried the LLM.
	ctx = chat.WithCallSite(ctx, "memetic.architect")
	a.log.Debug().
		Str("workflow_id", workflowID).
		Str("model", a.provider.Model()).
		Int("candidate_evidence", len(candidateEvidenceRunIDs)).
		Msg("memetic: architect turn start")

	currentYAML, err := a.workflows.Load(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("memetic: load workflow %q: %w", workflowID, err)
	}

	// LEVEL 2 — per-workflow kill switch. A workflow whose
	// frontmatter sets `architect_enabled: false` opts out of
	// architect proposals entirely. Defaults to enabled (absent /
	// malformed frontmatter never disables — fail-open so a parse
	// quirk can't silently mute every workflow).
	if !architectEnabledFor(currentYAML) {
		return nil, ErrArchitectDisabledForWorkflow
	}

	since := time.Now().Add(-a.cfg.Lookback)
	rollup, err := a.telemetry.ForWorkflow(ctx, workflowID, since)
	if err != nil {
		return nil, fmt.Errorf("memetic: telemetry rollup for %q: %w", workflowID, err)
	}

	// Consumer B — consult workflow-domain instincts for this workflow
	// as evidence priors. Advisory only: they're cited in the prompt
	// (positive support vs. negative 'architect-reject' contradictions),
	// and after validation the surviving proposal inherits the priors'
	// evidence run IDs and a confidence floor. nil source (gate off /
	// not wired) → priors is empty and behaviour is unchanged.
	priors := a.loadPriors(ctx, workflowID)

	// Recovery-domain priors: recovery actions the observer has seen
	// resolve the failure classes this rollup is showing (e.g.
	// "retrying the step resolved the container_non_zero_exit
	// failure"). Surfaced in the prompt only — they tell the architect
	// when a failure already self-resolves, so it can prefer encoding
	// the recovery structurally (or passing) over a blind rewrite.
	recovery := a.loadRecoveryPriors(ctx, rollup)

	// review item W2 — architect-evidence application logging. Capture the
	// full set of priors consulted this turn (workflow + recovery). On any
	// error return from here on (validation-class failure, malformed
	// output, kind disabled, insert failure) the whole consulted set is
	// recorded as rejected; on a successful insert the POSITIVE priors
	// folded into the proposal are recorded as accepted (done explicitly
	// below, which clears `err` so this defer is a no-op). Early exits
	// BEFORE the loads above (paused / disabled-for-workflow / telemetry
	// failure) never reach here, so nothing is recorded for them.
	allPriors := append(append([]prior(nil), priors...), recovery...)
	// evaluated flips once we have a parsed candidate proposal; insertAttempted
	// flips at the Insert call. The priors are recorded as REJECTED only for a
	// genuine quality decline — a parsed proposal turned down before insertion
	// (validateOutput / kind / low-confidence). Operational failures before a
	// parse (load / render / transport → evaluated=false) and insertion
	// outcomes (rate-limited or a transient DB error → insertAttempted=true; a
	// successful insert clears err) are NOT rejections of the priors and must
	// not feed negative lift.
	var evaluated, insertAttempted bool
	defer func() {
		if err != nil && evaluated && !insertAttempted && len(allPriors) > 0 {
			a.recordApplications(ctx, priorsToIDs(allPriors), persistence.InstinctResultRejected)
		}
	}()

	user, err := renderUserPrompt(workflowID, currentYAML, rollup, candidateEvidenceRunIDs, priors, recovery)
	if err != nil {
		return nil, err
	}

	system := a.cfg.SystemPrompt
	if system == "" {
		system = defaultSystemPrompt
	}

	// Constrain the provider to a JSON object. The system prompt asks
	// for JSON-only, but instruction alone isn't enough: open-weight
	// architect models intermittently prepend a prose preamble
	// ("Looking at the telemetry…") that fails the strict parse, and
	// whether they do depends on the per-workflow prompt content — so
	// one workflow succeeds while another fails the same architect.
	// json_object is honored by the OpenAI-compatible + bedrock routes
	// (chat.ResponseFormatStructFromContext → req.ResponseFormat);
	// providers that ignore it fall back to the prose-tolerant parser
	// below. Belt-and-suspenders, not either/or.
	ctx = chat.WithRequestResponseFormatStruct(ctx, architectResponseFormat)

	msgs := []chat.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	raw, output, err := a.completeAndParse(ctx, msgs)
	if errors.Is(err, ErrMalformedOutput) {
		a.log.Info().
			Str("workflow_id", workflowID).
			Int("raw_bytes", len(raw)).
			Err(err).
			Msg("memetic: architect output malformed — issuing one corrective retry")
		// ONE corrective retry — mirrors the executor's shape-retry-
		// with-one-hint pattern. Open-weight architect models
		// intermittently emit shape failures (prose wrap the carve-out
		// can't save, truncated JSON, omitted confidence field); a
		// single retry carrying the failed reply + a pointed hint
		// recovers most of them. Validation-class rejections (low
		// confidence, workflow mismatch, bad evidence) are honest
		// verdicts and deliberately do NOT retry.
		msgs = append(msgs,
			chat.Message{Role: "assistant", Content: raw},
			chat.Message{Role: "user", Content: fmt.Sprintf(
				"Your previous reply was rejected: %v. Reply again with ONLY a single JSON object — no prose, no code fences — containing exactly these fields: workflow_id (string), proposed_yaml (string), motivation (string), evidence_run_ids (array of strings), confidence (number between 0 and 1), and optionally kind (string).",
				err)},
		)
		_, output, err = a.completeAndParse(ctx, msgs)
	}
	if err != nil {
		return nil, err
	}
	// A parsed candidate proposal now exists: from here a non-nil err is a
	// quality decline (recorded rejected) rather than an operational failure.
	evaluated = true
	a.log.Debug().
		Str("workflow_id", workflowID).
		Float32("confidence", output.Confidence).
		Int("evidence_count", len(output.EvidenceRunIDs)).
		Int("proposed_yaml_bytes", len(output.ProposedYAML)).
		Msg("memetic: architect parsed a candidate proposal")

	if err := a.validateOutput(ctx, workflowID, output); err != nil {
		return nil, err
	}

	// Resolve the proposal kind. The architect output MAY carry a
	// `kind`; absent / empty defaults to the sentinel (the LLM-output
	// contract change to reliably populate this is a tracked
	// follow-on — see mitigation plan §8.5). Reject an out-of-set
	// value so a hallucinated kind can't widen the enum.
	kind := persistence.WorkflowProposalKind(strings.TrimSpace(output.Kind))
	if kind == "" {
		kind = persistence.WorkflowProposalKindUnspecified
	}
	if !persistence.ValidWorkflowProposalKind(kind) {
		return nil, fmt.Errorf("%w: architect emitted unknown kind %q", ErrMalformedOutput, output.Kind)
	}

	// LEVEL 3 — per-proposal-class kill switch. Drop the proposal
	// before insert when the operator has disabled this class.
	if a.cfg.DisabledKinds[string(kind)] {
		return nil, fmt.Errorf("%w: %s", ErrProposalKindDisabled, kind)
	}

	proposal = &persistence.WorkflowProposal{
		ID:             persistence.GenerateID("wpr"),
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		Kind:           kind,
		ProposalYAML:   output.ProposedYAML,
		Motivation:     output.Motivation,
		EvidenceRunIDs: output.EvidenceRunIDs,
		Confidence:     output.Confidence,
		ArchitectModel: a.provider.Model(),
		CreatedAt:      time.Now().UTC(),
	}
	// Consumer B — fold the priors into the proposal (advisory):
	//   - cite the supporting instincts in the motivation,
	//   - union their evidence run IDs into EvidenceRunIDs,
	//   - raise (never lower) confidence toward the strongest prior.
	// Skipped entirely when priors is empty (gate off / not wired), so
	// the proposal is byte-for-byte what it was before.
	a.applyPriors(proposal, priors)
	// From here the outcome is an insertion result, not a prior rejection: a
	// throttle (ErrProposalRateLimited) or transient DB error must not record
	// the priors as rejected (the proposal passed evaluation).
	insertAttempted = true
	if err = a.proposals.Insert(ctx, proposal); err != nil {
		return nil, err
	}
	// review item W2 — the proposal landed. Record the POSITIVE priors
	// folded into it as accepted applications. applyPriors stored exactly
	// those instinct IDs on proposal.InstinctIDs, so the recorded set
	// matches what shaped the proposal (negative priors are deliberately
	// excluded — they did not support this proposal). Clearing err here
	// makes the deferred rejected-recording a no-op.
	err = nil
	a.recordApplications(ctx, proposal.InstinctIDs, persistence.InstinctResultAccepted)
	a.log.Info().
		Str("workflow_id", workflowID).
		Str("proposal_id", proposal.ID).
		Float32("confidence", proposal.Confidence).
		Str("kind", string(proposal.Kind)).
		Int("evidence_count", len(proposal.EvidenceRunIDs)).
		Msg("memetic: architect proposal created")
	return proposal, nil
}

// recordApplications logs one instinct_applications row per instinct ID
// for the architect-evidence surface (review item W2) and bumps the
// applications metric. Best-effort: a nil writer, an empty ID set, and any
// per-row write error are all silently absorbed so application logging can
// never fail a propose turn. The metric is only bumped on a successful
// (error-free) write so the counter tracks rows that actually landed.
func (a *Architect) recordApplications(ctx context.Context, ids []string, result string) {
	if a.appWriter == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		writeErr := a.appWriter.RecordApplication(ctx, &persistence.InstinctApplication{
			ID:         persistence.GenerateID("iap"),
			InstinctID: id,
			Surface:    persistence.InstinctSurfaceArchitectEvidence,
			Result:     result,
			AppliedAt:  time.Now().UTC(),
		})
		if writeErr != nil {
			// Swallow — application logging is advisory.
			continue
		}
		if a.metrics != nil && a.metrics.ApplicationsTotal != nil {
			a.metrics.ApplicationsTotal.WithLabelValues(
				persistence.InstinctSurfaceArchitectEvidence, result).Inc()
		}
	}
}

// priorsToIDs extracts the underlying instinct IDs from a prior set,
// skipping nil instincts. Used by the rejected-application path (review
// item W2) so a failed propose turn implicates the whole consulted set —
// both positive and negative priors.
func priorsToIDs(priors []prior) []string {
	ids := make([]string, 0, len(priors))
	for _, pr := range priors {
		if pr.inst == nil {
			continue
		}
		ids = append(ids, pr.inst.ID)
	}
	return ids
}

// completeAndParse runs one LLM turn and parses the reply. Returns
// the raw reply text alongside the parsed output so the caller's
// corrective retry can echo the failed reply back to the model.
func (a *Architect) completeAndParse(ctx context.Context, msgs []chat.Message) (string, *ArchitectOutput, error) {
	resp, err := a.provider.Complete(ctx, msgs)
	if err != nil {
		return "", nil, fmt.Errorf("memetic: LLM call failed: %w", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		return "", nil, fmt.Errorf("memetic: LLM returned no choices")
	}
	raw := resp.Choices[0].Message.Content
	if a.cfg.MaxOutputBytes > 0 && len(raw) > a.cfg.MaxOutputBytes {
		// Cap the echoed payload: the retry would resend it verbatim.
		return raw[:a.cfg.MaxOutputBytes], nil, fmt.Errorf("%w: %d bytes exceeds %d cap",
			ErrMalformedOutput, len(raw), a.cfg.MaxOutputBytes)
	}
	output, err := parseArchitectOutput(raw)
	if err != nil {
		return raw, nil, err
	}
	return raw, output, nil
}

// architectEnabledFor reports whether the workflow's frontmatter
// permits architect proposals. Returns true (enabled) when the
// `architect_enabled` key is absent OR the frontmatter can't be
// parsed — fail-open so a YAML quirk never silently mutes a
// workflow. Only an explicit `architect_enabled: false` disables.
func architectEnabledFor(workflowYAML []byte) bool {
	fm := extractFrontmatter(workflowYAML)
	if len(fm) == 0 {
		return true
	}
	var shape struct {
		ArchitectEnabled *bool `yaml:"architect_enabled"`
	}
	if err := yaml.Unmarshal(fm, &shape); err != nil {
		return true
	}
	if shape.ArchitectEnabled == nil {
		return true
	}
	return *shape.ArchitectEnabled
}

// extractFrontmatter returns the YAML frontmatter block delimited by
// leading/closing `---` markers, or the whole input when there are
// no markers (some callers pass bare frontmatter). Self-contained to
// avoid an import on internal/registry (which would risk a cycle).
func extractFrontmatter(content []byte) []byte {
	s := strings.TrimSpace(string(content))
	if !strings.HasPrefix(s, "---") {
		// No frontmatter fence — treat the whole thing as YAML so a
		// bare-frontmatter workflow still parses.
		return []byte(s)
	}
	s = strings.TrimPrefix(s, "---")
	if idx := strings.Index(s, "\n---"); idx >= 0 {
		return []byte(s[:idx])
	}
	return []byte(s)
}

// parseArchitectOutput pulls the JSON object out of the LLM's
// response. Tolerates a leading / trailing code fence (some
// open-weight models emit ```json … ``` even when told not to);
// rejects everything else as malformed.
func parseArchitectOutput(raw string) (*ArchitectOutput, error) {
	trimmed := strings.TrimSpace(raw)
	// Strip a leading ```json or ``` fence + matching trailer.
	if strings.HasPrefix(trimmed, "```") {
		if idx := strings.Index(trimmed, "\n"); idx > 0 {
			trimmed = trimmed[idx+1:]
		}
		trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	// Some open-weight architect models prepend a prose preamble
	// ("Looking at the telemetry…") or append a trailing note despite
	// the JSON-only instruction and the json_object response_format.
	// When the payload isn't already a bare object, carve out the
	// outermost {…} span and parse that. The downstream workflow_id /
	// YAML validation rejects anything the carve-out gets wrong, so
	// this can't smuggle a malformed proposal past the safety rails.
	if !strings.HasPrefix(trimmed, "{") {
		if obj, ok := extractJSONObject(trimmed); ok {
			trimmed = obj
		}
	}
	var out ArchitectOutput
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedOutput, err)
	}
	// An OMITTED confidence field decodes to the float zero value and
	// would masquerade as an honest 0.00 verdict downstream — the
	// operator then sees "confidence below threshold: 0.00 < 0.60" with
	// no hint that the model simply dropped the field (2026-06-06
	// incident: minimax-m2 did this intermittently for days). Omission
	// is a shape failure, not a verdict; classify it as malformed so the
	// corrective-retry path fires.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
		if _, ok := probe["confidence"]; !ok {
			return nil, fmt.Errorf("%w: required field \"confidence\" is missing", ErrMalformedOutput)
		}
	}
	return &out, nil
}

// extractJSONObject returns the substring from the first '{' to the
// last '}' (inclusive) when both are present and well-ordered. It does
// not balance braces — the JSON decoder remains the real validator;
// this just strips surrounding prose so the decoder gets a fair shot
// at a model that wrapped its object in commentary. Returns ok=false
// when there's no plausible object span to carve.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return "", false
	}
	return s[start : end+1], true
}

// validateOutput runs the safety rails described in package doc
// and the design's Slice 2 § validation pipeline. Order matters:
// cheap checks first (workflow_id match, confidence floor,
// evidence count) so an obviously-bad proposal doesn't trigger
// the YAML validator or the execution-lookup query.
func (a *Architect) validateOutput(ctx context.Context, workflowID string, out *ArchitectOutput) error {
	if out.WorkflowID != workflowID {
		return fmt.Errorf("%w: input=%q output=%q",
			ErrWorkflowMismatch, workflowID, out.WorkflowID)
	}
	if out.Confidence < a.cfg.MinConfidence {
		// THE diagnostic line for the recurring "confidence 0.00"
		// report: the LLM was queried and parsed cleanly, the field was
		// present (omission is caught earlier as malformed + retried),
		// and the model's own score is below the floor. A 0.00 here that
		// recurs means the model is emitting an explicit 0 / null — pair
		// this line with the LoggingProvider's DEBUG "llm response" to
		// see the raw payload.
		a.log.Info().
			Str("workflow_id", workflowID).
			Float32("confidence", out.Confidence).
			Float32("threshold", a.cfg.MinConfidence).
			Str("model", a.provider.Model()).
			Msg("memetic: proposal rejected — architect confidence below threshold")
		return fmt.Errorf("%w: %.2f < %.2f",
			ErrLowConfidence, out.Confidence, a.cfg.MinConfidence)
	}
	if len(out.EvidenceRunIDs) < a.cfg.MinEvidenceRunIDs {
		return fmt.Errorf("%w: got %d, need %d",
			ErrInsufficientEvidence, len(out.EvidenceRunIDs), a.cfg.MinEvidenceRunIDs)
	}
	if strings.TrimSpace(out.ProposedYAML) == "" {
		return fmt.Errorf("%w: proposed_yaml is empty",
			ErrProposalYAMLInvalid)
	}

	// Evidence must actually reference executions of THIS
	// workflow. Caught here, not at the SQL layer, so the LLM
	// can't smuggle in unrelated execution IDs to clear the
	// "minimum 3" gate. nil ExecutionLookup short-circuits to
	// "trust the LLM" for tests; production paths always wire it.
	if a.execLookup != nil {
		_, allValid, err := a.execLookup.BelongsTo(ctx, workflowID, out.EvidenceRunIDs)
		if err != nil {
			return fmt.Errorf("memetic: evidence lookup: %w", err)
		}
		if !allValid {
			return ErrEvidenceInvalid
		}
	}

	// Final gate: proposed YAML must actually parse. Catches the
	// case where the LLM emits a syntactically broken WORKFLOW.md
	// or one that violates the schema (missing name, bad
	// transitions, etc.). The architect's confidence is irrelevant
	// if the YAML doesn't load.
	report := registry.ValidateWorkflowMarkdown([]byte(out.ProposedYAML), workflowID+".md")
	if report.HasErrors() {
		var sb strings.Builder
		for i, f := range report.Findings {
			if f.Severity != registry.SeverityError {
				continue
			}
			if i > 0 {
				sb.WriteString("; ")
			}
			sb.WriteString(f.String())
		}
		return fmt.Errorf("%w: %s", ErrProposalYAMLInvalid, sb.String())
	}
	return nil
}
