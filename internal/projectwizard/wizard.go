package projectwizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/rolelibrary"
	"vornik.io/vornik/internal/templates"
)

// systemPromptTemplate is the wizard's instructions to the LLM.
// %s placeholder is filled with the rendered template-gallery
// priors at runtime so a hot-reload of the gallery propagates
// without a daemon restart. The schema reminder + the
// envelope-only output constraint keep the LLM from emitting
// prose around the JSON.
const systemPromptTemplate = `You are vornik's project-setup wizard. The operator wants to
create a new project; your job is to extract their goal through
short clarifying questions and propose a concrete project YAML.

Output: ALWAYS a JSON object matching this schema exactly:
- message (string, required) — what the operator sees in chat
- proposal (object, optional) — when you have enough info, propose
  a full project YAML under "raw"
- ready_to_commit (boolean, required) — true ONLY when the proposal
  is complete AND the operator looks ready
- suggested_template (string, optional) — slug of the closest
  template if one matches
- open_questions (array of strings, optional) — short reply
  suggestions for the operator (e.g. "yes", "every 6 hours")

Rules:
1. Keep messages short — 2-3 sentences max.
2. Ask ONE clarifying question per turn, not a list.
3. Propose YAML as early as makes sense — operators iterate
   faster on a draft than on questions.
4. ready_to_commit=true is a strong signal — only flip when the
   proposal validates AND the operator's said something like
   "looks good", "commit", or "yes go ahead".
5. If the operator's description matches a template in the
   gallery, set suggested_template; otherwise leave empty.
6. Once the build is clear, ALSO emit a "composition" object:
   {"template": <slug>, "params": {...}, "addons": [...]}. Pick
   "template" from the grounded base-template list below when one
   fits the intent; omit "template" entirely to build from
   custom-base instead. Only reference MCP servers and tools that
   appear in the live grounding below — never invent one that
   isn't listed. Addons must match the documented vocabulary and
   field shapes exactly. A project has exactly ONE autonomy style: never combine a "schedule" addon with a
   "rag_source" addon in the same composition — they're mutually
   exclusive. For a multi-cadence need (frequent ingestion AND a daily
   digest), use ONE llm-mode loop whose goal manages both cadences.
7. In the composition "params", only use params the chosen base template
   declares (shown per-template in the grounding above) - never invent a
   param such as "topic". ALWAYS set projectId (a slug).

%s

When your description matches a template in the gallery above, ALWAYS
set suggested_template — the project is built from that template, so
the proposal only needs to carry the template's key fields and the
operator gets the same vetted swarm + workflow the gallery ships.

The proposal's "raw" object must include at minimum: projectId (slug)
and displayName (projectId is always required).
When no template fits, also include either an autonomy block (for
scheduled work) or a roles block (for human-driven work), and use
existing role names from the templates above when possible.`

// SessionStore is the narrow interface the wizard needs from
// the persistence layer. The full ProjectWizardSessionRepository
// signature lives on persistence.ProjectWizardSessionRepository;
// duck-typed structural conformance lets the wizard accept any
// implementation that exposes these five methods.
type SessionStore interface {
	Get(ctx context.Context, id string) (*persistence.ProjectWizardSession, error)
	Insert(ctx context.Context, s *persistence.ProjectWizardSession) error
	Update(ctx context.Context, s *persistence.ProjectWizardSession) error
	// CommitTo atomically stamps committed_project_id on the row,
	// flipping the session into a terminal read-only state.
	CommitTo(ctx context.Context, sessionID, projectID string) error
	// Cancel atomically stamps cancelled_at on an uncommitted session
	// owned by operatorID, freeing the operator's active-session slot.
	// Idempotent on an already-cancelled session.
	Cancel(ctx context.Context, sessionID, operatorID string) error
	// ListByOperator returns the operator's most recently-updated
	// sessions. Phase C drafts banner + per-operator session cap
	// both consume this; capacity check is done by counting
	// uncommitted rows in the returned slice.
	ListByOperator(ctx context.Context, operatorID string, pageSize int) ([]*persistence.ProjectWizardSession, error)
}

// ErrTooManySessions — the operator hit their concurrent
// uncommitted-session cap. Operator must finish or abandon an
// existing draft before starting a new one.
var ErrTooManySessions = errors.New("projectwizard: too many active sessions")

// UsageRecorder is the narrow audit/cost hook so the wizard's
// LLM spend lands on the spend dashboard. Optional — nil
// disables billing rows (tests, deployments without the repo).
type UsageRecorder interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// Reloader is the narrow hot-reload trigger the journaled bundle-
// commit path polls once the project file has landed in the live
// tree (design §5.6 step 5; task 1.2b slice ii). TryReload MUST be
// bounded by d and MUST NOT block past it — ok=true means the cycle
// completed and activated cleanly within d; ok=false (with err
// describing why — timeout, busy/deferred, or a hard
// validate/activate rejection) means the commit's rollback must run.
// The concrete implementation (internal/service) adapts
// *config.ConfigReloader.TryReload, the SAME instance
// api.NewConfigHandlers and the doctor reloader already wrap — this
// package never duplicates the reload cycle or reaches for the HTTP
// endpoint. nil is a documented graceful degradation (CE/minimal
// wiring without a reloader): the commit still lands its files and
// does NOT hard-fail; it just relies on the daemon's own watcher/next
// reload to pick the new project up, exactly like slice i did before
// this field existed.
type Reloader interface {
	TryReload(d time.Duration) (ok bool, err error)
}

// Validator is the narrow interface the wizard calls to check a
// proposal before exposing ready_to_commit=true. Implementations
// return nil when the proposal validates; the returned error's
// message is appended to the assistant's chat reply so the
// operator sees why their description didn't land yet.
//
// Phase A's default validator is permissive — any non-empty
// map.raw with an id key passes. Phase B replaces with the
// internal/registry parser proper.
type Validator interface {
	Validate(p *ProjectYAML) error
}

// Wizard is the per-daemon orchestrator. One instance shared
// across all operators; sessions are looked up by ID on each
// call.
type Wizard struct {
	// Sessions persists transcript + proposal state across turns.
	Sessions SessionStore
	// Chat is the LLM provider — typically the daemon's chat
	// router so the wizard runs on the same models the rest of
	// the system uses.
	Chat chat.Provider
	// Model is the LLM model identifier passed via
	// chat.ModelOverridable. Empty leaves the router's default.
	Model string
	// Validator gates ready_to_commit on the from-scratch path
	// (no suggested_template). nil → permissiveValidator.
	Validator Validator
	// Templates anchors a committed proposal on a vetted project
	// template — the same catalog the /ui/projects/new gallery
	// renders. When the LLM sets suggested_template and the slug
	// resolves here, the proposal is validated + committed by
	// materialising the template (project.yaml + swarm.md + …) with
	// parameters derived from the proposal, rather than writing the
	// LLM's raw YAML. nil → from-scratch path only (raw proposal
	// must itself carry swarmId + defaultWorkflowId).
	Templates TemplateSource
	// Priors is the rendered template-gallery summary spliced
	// into the system prompt. Loaded once at construction; if
	// the gallery changes the daemon hot-reload reconstructs
	// the Wizard.
	Priors []TemplatePrior
	// LLMUsage records one row per call for the spend dashboard.
	// Optional.
	Spend llmspend.Recorder
	// Writer commits the final proposal as a project on disk
	// (Phase B). Optional — without it (and without Proposer), Commit
	// returns ErrWriterUnwired. Converse works fine without a writer;
	// the wizard just can't finalise the session. When Proposer is
	// wired the writer is the CE fallback used only if no ledger exists.
	Writer ProjectWriter
	// Proposer routes commit through the control-plane proposal ledger:
	// the composed file set becomes a reviewable DRAFT scaffold proposal
	// instead of a direct disk write. When wired (production control
	// plane) it takes precedence over Writer. Optional — nil falls back
	// to the direct-write Writer.
	Proposer ScaffoldProposer
	// Metrics counts converse + commit outcomes for operator
	// dashboards (Phase C). Optional — nil is no-op.
	Metrics *Metrics
	// MaxActiveSessions caps the number of uncommitted sessions
	// one operator can hold open at once (Phase C). 0 → 5.
	MaxActiveSessions int
	// MaxTurns caps the conversation at this many user turns to
	// bound LLM spend. 0 → 20.
	MaxTurns int
	// Timeout caps each LLM call. 0 → 60s.
	Timeout time.Duration
	// MCP grounds the system prompt in the daemon's live MCP server
	// + tool inventory, so the LLM only proposes mcp_server addons
	// referencing servers/tools that actually exist. Optional — nil
	// still renders the addon vocabulary via BuildGrounding, just
	// without a live server list.
	MCP MCPGroundingSource
	// Models grounds the system prompt in the daemon's live model
	// catalog. Optional — nil renders a "default model only" note.
	Models ModelLister
	// KnownMCP returns the set of MCP server names configured on the
	// daemon, keyed for O(1) lookup. This is the same dependency the
	// compose engine's mcp_server applier uses (ComposeDeps.KnownMCP)
	// to reject a server the LLM hallucinated; the wizard holds its
	// own accessor here because Compose only runs at commit time,
	// after the session's conversation is done. Optional.
	KnownMCP func(ctx context.Context) map[string]bool
	// Resolver resolves optionsFrom sources (mcp_registry, models)
	// when materialising a multi-value template parameter at commit
	// time — the same seam ComposeDeps.Resolver fills. Optional; nil
	// leaves dynamic optionsFrom values unresolved.
	Resolver templates.OptionsResolver

	// TemplateMeta resolves a base template's declared params + autonomy
	// block for the convergence normalizer (normalizeComposition). Optional;
	// nil skips base-aware behavior and derives only projectId.
	TemplateMeta TemplateMetaLookup

	// --- NL Automation Composer tier-3 engine (task 1.1b) ---

	// Composer tunes tier arbitration + guardrail defaults
	// (composer.max_tier, composer.max_tier3_turns,
	// composer.default_budget). Zero value disables tier-3 entirely
	// (MaxTier defaults to 0, which is below the tier-3 gate) — the
	// daemon wiring passes the loaded config.ComposerConfig (already
	// defaulted) when the composer feature is enabled.
	Composer config.ComposerConfig
	// RoleLibrary is the curated archetype set (rolelibrary.Load),
	// keyed by ArchetypeID, that tier-3 bundle materialization expands
	// composed roles against. Nil/empty → every tier-3 turn fails
	// materialization (no archetypes to expand into) — effectively
	// disabling tier-3 output gracefully.
	RoleLibrary map[string]*rolelibrary.RoleArchetype
	// LiveConfigDir is the daemon's deployed configs root
	// (~/.config/vornik/configs) — staged validation layers a tier-3
	// bundle's rendered files over this directory so cross-references
	// to pre-existing swarms/workflows resolve. Empty runs staged
	// validation against the bundle alone (fine for tests; production
	// always wires this).
	LiveConfigDir string
	// Reloader triggers + bounds-polls the daemon's config hot-reload
	// once a tier-3 commit's project file lands (design §5.6 step 5).
	// nil disables the trigger — see the Reloader doc comment for the
	// exact (documented, non-fatal) degradation.
	Reloader Reloader
	// ReloadTimeout bounds the post-commit reload poll (design §5.6
	// step 5's "30 s deadline"). 0 -> 30s. Overridable so tests never
	// need a real 30-second wait to exercise the timeout path — the
	// injected Reloader in a test typically returns immediately
	// regardless of the duration it's handed, but a shorter default
	// here keeps any future real-reloader integration test fast too.
	ReloadTimeout time.Duration
	// SystemHandlerNames is the daemon executor's registered
	// system-step handler names (the same Container.systemHandlerNames
	// snapshot api.DoctorHandlers.SetSystemHandlerNames wires into the
	// role-library doctor check) — grounds the tier-3 system prompt's
	// step vocabulary (design §5.3: "agent, gate, approval, and the
	// registry-enumerated system handlers") so the model only proposes
	// a `system` step naming a handler that actually exists on this
	// daemon. Optional — nil/empty still documents the step KINDS, just
	// without a concrete handler list.
	SystemHandlerNames []string

	// commitMu serialises the check-and-land critical section of a tier-3
	// bundle commit (collision-check → journaled rename) so two concurrent
	// commits with colliding IDs can't both pass the check and then clobber
	// each other's landed files (review-20260716-8f22 TOCTOU). One coarse mutex
	// is adequate for this low-volume path (commits are rare, operator-driven);
	// a multi-daemon deployment would still need a DB/advisory lock on the
	// live-config tree — deferred with the other multi-instance items.
	commitMu sync.Mutex
}

// ErrSessionCommitted — the session was already committed; further
// converse calls are refused.
var ErrSessionCommitted = errors.New("projectwizard: session already committed")

// ErrSessionCancelled — the session was cancelled; converse calls
// are refused and the slot has been freed.
var ErrSessionCancelled = errors.New("projectwizard: session already cancelled")

// ErrTurnsExhausted — the session hit MaxTurns; new turns are
// refused. Operator must start a fresh session.
var ErrTurnsExhausted = errors.New("projectwizard: session turn limit reached")

// Converse appends the operator's message to the session
// transcript, calls the LLM, parses the envelope, validates the
// proposal, persists the updated session, and returns the
// envelope.
//
// Pass sessionID="" to start a fresh session — the wizard
// allocates an ID, inserts the row, and returns it on the
// Result.
func (w *Wizard) Converse(ctx context.Context, sessionID, operatorID, userMessage string) (res *Result, retErr error) {
	if w == nil || w.Sessions == nil || w.Chat == nil {
		return nil, errors.New("projectwizard: not fully wired")
	}
	userMessage = strings.TrimSpace(userMessage)
	if userMessage == "" {
		return nil, errors.New("projectwizard: empty user message")
	}
	if operatorID == "" {
		return nil, errors.New("projectwizard: operator id required")
	}

	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Load or create session.
	createdNew := sessionID == ""
	sessionInserted := false
	var session *persistence.ProjectWizardSession
	// A brand-new session is Inserted BEFORE the LLM call so the
	// transcript can persist per turn. If that first turn then fails
	// (e.g. the assistant model hits its token limit), the row would
	// otherwise linger as an uncommitted draft — and because the error
	// path returns no session_id, the client can't reuse it, so every
	// retry orphans another draft and the projects-page banner counter
	// climbs. Cancel the just-created session on a failed first turn so
	// it never reaches the banner. A RESUMED session (createdNew=false)
	// is left intact on failure — its prior turns are real work.
	defer func() {
		if retErr != nil && createdNew && sessionInserted && session != nil {
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer ccancel()
			_ = w.Sessions.Cancel(cctx, session.ID, operatorID)
		}
	}()
	if sessionID == "" {
		// Enforce per-operator concurrent-session cap before
		// allocating a new ID. Counts only uncommitted rows.
		maxActive := w.MaxActiveSessions
		if maxActive <= 0 {
			maxActive = 5
		}
		if existing, err := w.Sessions.ListByOperator(callCtx, operatorID, maxActive*2+1); err == nil {
			active := 0
			for _, s := range existing {
				if s != nil && s.CommittedProjectID == nil && s.CancelledAt == nil {
					active++
				}
			}
			if active >= maxActive {
				w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
				return nil, ErrTooManySessions
			}
		}
		session = &persistence.ProjectWizardSession{
			ID:         persistence.GenerateID("pw"),
			OperatorID: operatorID,
		}
		if err := w.Sessions.Insert(callCtx, session); err != nil {
			w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
			return nil, fmt.Errorf("projectwizard: insert session: %w", err)
		}
		sessionInserted = true
	} else {
		got, err := w.Sessions.Get(callCtx, sessionID)
		if err != nil {
			w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
			return nil, fmt.Errorf("projectwizard: load session: %w", err)
		}
		if got == nil {
			w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
			return nil, persistence.ErrNotFound
		}
		if got.CommittedProjectID != nil {
			w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
			return nil, ErrSessionCommitted
		}
		if got.CancelledAt != nil {
			w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
			return nil, ErrSessionCancelled
		}
		if got.OperatorID != operatorID {
			w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
			return nil, persistence.ErrNotFound
		}
		session = got
	}

	// Decode prior transcript, append user turn, check budget.
	transcript, err := decodeTranscript(session.Transcript)
	if err != nil {
		return nil, fmt.Errorf("projectwizard: decode transcript: %w", err)
	}
	maxTurns := w.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	userTurns := countUserTurns(transcript)
	if userTurns >= maxTurns {
		w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeRejected)
		return nil, ErrTurnsExhausted
	}
	transcript = append(transcript, Turn{
		Role:      "user",
		Content:   userMessage,
		CreatedAt: time.Now().UTC(),
	})

	// Build LLM messages.
	msgs := w.buildChatMessages(callCtx, transcript)

	// Apply response_format=json_schema via context so the chat
	// router enforces the envelope shape (Bedrock injects a
	// synthetic tool whose schema is the envelope, forces
	// tool_choice; Anthropic emits a tool call we unwrap).
	//
	// Whole-branch review C1 fix: when the composer is available for
	// this turn, hand the superset composerResponseFormat() instead of
	// the plain tier-1/2 envelopeResponseFormat() — otherwise the model
	// is structurally incapable of emitting `tier`/`bundle` (Bedrock's
	// forced synthetic tool enforces exactly the schema handed here),
	// so the tier-3 engine below (arbitrateTier3 / applyBundle) never
	// sees a tier-3 envelope in production. responseFormatForTurn keeps
	// the tier-1/2 contract byte-for-byte unchanged when the composer
	// isn't available.
	schemaCtx := chat.WithRequestResponseFormatStruct(callCtx, w.responseFormatForTurn())

	client := pickModel(w.Chat, w.Model)
	resp, err := client.Complete(schemaCtx, msgs)
	if err != nil {
		w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeLLMError)
		return nil, fmt.Errorf("projectwizard: chat: %w", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeLLMError)
		return nil, errors.New("projectwizard: empty chat response")
	}
	rawContent := resp.Choices[0].Message.Content
	envelope, err := parseEnvelope(rawContent)
	if err != nil {
		w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeLLMError)
		return nil, fmt.Errorf("projectwizard: parse envelope: %w", err)
	}

	// Tier arbitration (design §5.1): refuses/downgrades tier-3 output
	// per composer.max_tier / the per-session tier-3 cap / the anchor-
	// score confidence gate, via a corrective re-prompt (never a
	// downgrade-in-place of the offending envelope — it is replaced
	// wholesale, never persisted).
	//
	// Usage/cost rows are recorded HERE (one per actual LLM call this
	// turn — 1, or 2 when the corrective retry fired) rather than once
	// at the end, so a retry's extra spend is never silently dropped
	// from the composer's cost dashboards (design §5.8's stated
	// concern that tier-3 turns are the expensive ones). The original
	// call's role reflects what was actually asked of the model THIS
	// call, independent of whether arbitration went on to accept it.
	originalWasTier3 := envelope.Tier == 3 && envelope.Bundle != nil
	arbitratedEnvelope, retryResp, arbErr := w.arbitrateTier3(schemaCtx, client, msgs, session, transcript, userMessage, envelope)
	if arbErr != nil {
		w.Metrics.recordTurn(tierLabelUnknown, turnOutcomeLLMError)
		return nil, fmt.Errorf("projectwizard: tier arbitration: %w", arbErr)
	}
	originalRole := RoleProjectWizard
	if originalWasTier3 {
		originalRole = RoleAutomationComposer
	}
	w.recordUsage(ctx, resp, session.ID, originalRole, nil)
	if retryResp != nil {
		w.recordUsage(ctx, retryResp, session.ID, RoleAutomationComposer, nil)
		resp = retryResp
	}
	envelope = arbitratedEnvelope

	// Dispatch: tier-3 bundle pipeline (§5.2/§5.4), wizard v2
	// Compose against the structured composition, or the legacy raw-
	// proposal validator. Failure at any stage appends the reason to
	// the message and resets ready_to_commit so the UI doesn't show an
	// enabled commit button on an invalid draft. Each branch clears
	// the OTHER two session build fields — the session must track only
	// the LATEST turn's build (the whole-branch review's Important #1
	// finding: a stale session.Composition left over from an earlier
	// turn let Commit silently commit an old build instead of what the
	// operator is now looking at; the same rule now applies to
	// session.Bundle).
	var turnOutcome string
	switch {
	case envelope.Tier == 3 && envelope.Bundle != nil:
		session.Composition = nil
		turnOutcome = w.applyBundle(session, transcript, envelope)
	case envelope.Composition != nil:
		session.Bundle = nil
		turnOutcome = w.applyComposition(callCtx, session, envelope)
	default:
		session.Composition = nil
		session.Bundle = nil
		turnOutcome = w.applyProposal(session, envelope)
	}
	if envelope.Proposal == nil && envelope.Composition == nil && envelope.Bundle == nil {
		envelope.ReadyToCommit = false
	}
	w.Metrics.recordTurn(tierLabel(envelope), turnOutcome)

	// Append assistant turn + persist.
	transcript = append(transcript, Turn{
		Role:      "assistant",
		Content:   envelope.Message,
		Envelope:  envelope,
		CreatedAt: time.Now().UTC(),
	})
	transcriptBytes, _ := json.Marshal(transcript)
	session.Transcript = transcriptBytes
	session.ReadyToCommit = envelope.ReadyToCommit
	if envelope.SuggestedTemplate != "" {
		session.SuggestedTemplate = envelope.SuggestedTemplate
	}
	if envelope.Proposal != nil {
		proposalBytes, _ := json.Marshal(envelope.Proposal)
		session.CurrentProposal = proposalBytes
	}
	if err := w.Sessions.Update(callCtx, session); err != nil {
		// Persist failure is non-fatal to the operator's reply —
		// they get the envelope; the next turn will overwrite this
		// turn's row from the operator's resent transcript. But
		// log via the error so operators see the issue.
		return &Result{SessionID: session.ID, Envelope: envelope},
			fmt.Errorf("projectwizard: update session: %w", err)
	}

	return &Result{SessionID: session.ID, Envelope: envelope}, nil
}

// applyComposition runs the 3a Compose pipeline against the
// envelope's structured composition, persisting it onto the session
// on success or folding the failure reason into the operator-visible
// message (and resetting ready_to_commit) on failure. Returns the
// turn outcome for the metrics counter.
func (w *Wizard) applyComposition(ctx context.Context, session *persistence.ProjectWizardSession, envelope *Envelope) string {
	// Deterministic convergence pass (2026-07-16): merge ≥2 autonomy
	// addons into one llm-mode loop and repair template params BEFORE the
	// composition is composed or persisted, so the previewed and committed
	// composition are the normalized one. A merge that can't converge
	// (e.g. cron-enabled base) surfaces as a steer.
	notes, question, nerr := normalizeComposition(envelope.Composition, w.TemplateMeta, w.Model)
	if nerr != nil {
		envelope.Message += "\n\n(composition: " + strings.TrimPrefix(nerr.Error(), "composition: ") + ")"
		envelope.ReadyToCommit = false
		w.retainLastGoodComposition(session, envelope)
		return turnOutcomeValidationError
	}
	for _, n := range notes {
		envelope.Message += "\n\n(" + string(n) + ")"
	}
	if question != "" {
		envelope.Message += "\n\n" + question
		envelope.ReadyToCommit = false
	}

	if _, _, cerr := w.composeFromEnvelope(ctx, envelope); cerr != nil {
		// ComposeError.Error() already self-prefixes "composition: "
		// on structural (AddonIndex<0) failures; don't double it up —
		// this text is fed back to the LLM to self-correct, so a
		// "(composition: composition: ...)" stutter reads as noise.
		msg := cerr.Error()
		if !strings.HasPrefix(msg, "composition:") {
			msg = "composition: " + msg
		}
		envelope.Message += "\n\n(" + msg + ")"
		envelope.ReadyToCommit = false
		w.retainLastGoodComposition(session, envelope)
		return turnOutcomeValidationError
	}
	if question != "" {
		// Composition is structurally valid but a required param is still
		// unanswered — keep it as the preview, but don't mark committable.
		compositionBytes, _ := json.Marshal(envelope.Composition)
		session.Composition = compositionBytes
		return turnOutcomeValidationError
	}
	compositionBytes, _ := json.Marshal(envelope.Composition)
	session.Composition = compositionBytes
	return turnOutcomeAssistantReply
}

// retainLastGoodComposition prevents a failed turn from blanking the
// preview to "No proposal yet" (2026-07-16 §4.3): when this turn's
// composition couldn't be applied but the session still holds a prior
// valid one, surface that prior composition on the envelope so the UI
// keeps showing the last-good draft (still not committable — the caller
// has already set ReadyToCommit=false).
func (w *Wizard) retainLastGoodComposition(session *persistence.ProjectWizardSession, envelope *Envelope) {
	if len(session.Composition) == 0 {
		return // no prior valid draft to fall back to
	}
	var prior Composition
	if err := json.Unmarshal(session.Composition, &prior); err != nil {
		return
	}
	envelope.Composition = &prior
	envelope.Message += "\n\n(kept your previous valid draft; the latest change couldn't be applied)"
}

// applyProposal validates the legacy raw-proposal path, folding a
// validation failure into the operator-visible message (and
// resetting ready_to_commit) the same way applyComposition does for
// the wizard v2 path. Returns the turn outcome for the metrics
// counter; a bare question turn (no proposal at all) is a no-op.
func (w *Wizard) applyProposal(session *persistence.ProjectWizardSession, envelope *Envelope) string {
	if envelope.Proposal == nil {
		return turnOutcomeAssistantReply
	}
	// Prefer this turn's template hint; fall back to the value
	// stickied on the session from an earlier turn.
	slug := envelope.SuggestedTemplate
	if slug == "" {
		slug = session.SuggestedTemplate
	}
	if verr := w.validateProposal(envelope.Proposal, slug); verr != nil {
		envelope.Message += "\n\n(validation: " + verr.Error() + ")"
		envelope.ReadyToCommit = false
		return turnOutcomeValidationError
	}
	return turnOutcomeAssistantReply
}

// buildChatMessages composes the system+conversation pair the
// chat router consumes. System message reuses the template (with
// priors spliced) plus the live grounding block (MCP servers,
// models, base templates, addon vocabulary); conversation is the
// transcript rendered as role/content pairs. BuildGrounding is
// nil-safe on w.MCP/w.Models, so this always appends — even with
// no grounding deps wired the addon vocabulary still renders.
func (w *Wizard) buildChatMessages(ctx context.Context, transcript []Turn) []chat.Message {
	system := fmt.Sprintf(systemPromptTemplate, RenderPriors(w.Priors))
	system += "\n\n" + BuildGrounding(ctx, w.MCP, w.Models, w.Priors)
	// Whole-branch review C1 fix: additively ground the model in the
	// tier-3 "parts bin" (design §5.3 — role library, step vocabulary,
	// when to return a bundle) ONLY when the composer is actually
	// available this turn. When it isn't, this is a no-op and the
	// tier-1/2 prompt is unchanged, byte-for-byte, from before this fix.
	if w.composerTier3Available() {
		system += BuildComposerGrounding(w.RoleLibrary, w.SystemHandlerNames)
	}
	msgs := []chat.Message{
		{Role: "system", Content: system},
	}
	for _, t := range transcript {
		msgs = append(msgs, chat.Message{Role: t.Role, Content: t.Content})
	}
	return msgs
}

// composerTier3Available reports whether this turn should be offered
// the tier-3 composer contract: the operator pin allows tier 3
// fleet-wide (composer.max_tier) AND there is a non-empty curated role
// library to materialize a composed role against (design §5.1/§5.3).
// Without a role library, materializeBundle fails every tier-3 turn
// anyway (wizard.go's RoleLibrary doc comment), so there is nothing to
// gain — and something to lose (prompt bloat, an invitation the model
// can never legally fulfill) — from advertising tier-3 in that case.
//
// This gate decides whether to INVITE a tier-3 answer (response_format
// + prompt grounding); it is deliberately simpler than arbitrateTier3's
// runtime downgrade logic (which additionally treats an all-zero,
// unconfigured-test Composer as permissive) — a real daemon always
// loads config.ComposerConfig.applyDefaults, which never leaves
// MaxTier at 0, so that permissive special-case doesn't apply here.
func (w *Wizard) composerTier3Available() bool {
	return w.Composer.MaxTier >= 3 && len(w.RoleLibrary) > 0
}

// responseFormatForTurn selects the response_format handed to the chat
// client for this turn (whole-branch review C1 fix): the composer
// superset schema when tier-3 is available, otherwise the unchanged
// tier-1/2 envelope schema. Extracted as its own function so a test can
// assert on the selection directly rather than mocking the whole
// provider.
func (w *Wizard) responseFormatForTurn() *chat.ResponseFormat {
	if w.composerTier3Available() {
		return composerResponseFormat()
	}
	return envelopeResponseFormat()
}

// Usage-attribution roles for the wizard's LLM spend rows. The
// composer (task 1.1b) records tier-3 turns under
// RoleAutomationComposer so its spend is distinguishable in the
// task_llm_usage rollups (design §5.8); everything else stays
// RoleProjectWizard, preserving the historical attribution.
const (
	RoleProjectWizard      = "project_wizard"
	RoleAutomationComposer = "automation_composer"
)

func (w *Wizard) recordUsage(ctx context.Context, resp *chat.ChatResponse, sessionID, role string, _ []byte) {
	if w == nil || resp == nil {
		return
	}
	if role == "" {
		role = RoleProjectWizard
	}
	prompt := resp.Usage.PromptTokens
	completion := resp.Usage.CompletionTokens
	if prompt == 0 && completion == 0 {
		return
	}
	// ProjectID stays empty: the wizard runs BEFORE a project exists, so there is
	// nothing to attribute to. RoleOverride because the wizard bills under a
	// per-turn role.
	w.Spend.Record(ctx, llmspend.Input{
		Model:            firstNonEmpty(resp.Model, w.Model),
		PromptTokens:     prompt,
		CompletionTokens: completion,
		StepID:           sessionID,
		RoleOverride:     role,
	})
}

// envelopeResponseFormat returns the json_schema directive that
// constrains the LLM to the WizardEnvelope shape. Required so
// the parser never sees free-form prose.
func envelopeResponseFormat() *chat.ResponseFormat {
	schema := map[string]any{
		"type":     "object",
		"required": []string{"message", "ready_to_commit"},
		"properties": map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "Operator-facing assistant message.",
			},
			"ready_to_commit": map[string]any{
				"type":        "boolean",
				"description": "True when the proposal is complete and the operator is ready.",
			},
			"suggested_template": map[string]any{
				"type":        "string",
				"description": "Closest matching template slug, if any.",
			},
			"open_questions": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"proposal": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"raw": map[string]any{
						"type":        "object",
						"description": "Proposed project YAML as a generic map.",
					},
				},
			},
			"composition": map[string]any{
				"type":        "object",
				"description": "Structured build: template, params, and addons.",
				"properties": map[string]any{
					"template": map[string]any{
						"type":        "string",
						"description": "Base template slug.",
					},
					"params": map[string]any{
						"type":                 "object",
						"additionalProperties": true,
						"description":          "Template parameters (each value is a string or array of strings).",
					},
					"addons": map[string]any{
						"type":        "array",
						"description": "Ordered list of typed composition mutations.",
						"items": map[string]any{
							"type":                 "object",
							"required":             []string{"type"},
							"additionalProperties": true,
							"properties": map[string]any{
								"type": map[string]any{
									"type":        "string",
									"description": "Addon type selector (e.g., schedule, mcp_server).",
								},
							},
						},
					},
				},
			},
		},
	}
	schemaBytes, _ := json.Marshal(schema)
	return &chat.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &chat.ResponseJSONSchema{
			Name:        "ProjectWizardEnvelope",
			Description: "Structured output for one project-setup wizard turn.",
			Schema:      schemaBytes,
		},
	}
}

// parseEnvelope unmarshals the LLM's emitted JSON. Tolerant of
// surrounding whitespace, ```json fences, AND a prose preamble/epilogue
// — not every model honours response_format=json_schema (Gemini via
// Vertex, observed 2026-05-31, returns prose like "Absolutely! …{json}"
// and the strict parse failed with `invalid character 'A'`). When the
// whole body isn't valid JSON, fall back to the first balanced {...}
// object in it. Pinning chat.wizard_model to a schema-enforcing model
// is the clean fix; this keeps a chatty model from hard-502ing.
func parseEnvelope(raw string) (*Envelope, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty envelope")
	}
	env, err := unmarshalEnvelope(raw)
	if err != nil {
		// Prose-wrapped JSON: recover the first balanced object.
		if obj, ok := firstJSONObject(raw); ok {
			if env2, err2 := unmarshalEnvelope(obj); err2 == nil {
				env = env2
				err = nil
			}
		}
	}
	if err != nil {
		// Plain prose — no JSON envelope and no embedded object. The
		// model ignored response_format: not every backend honours the
		// json_schema contract (json_schema enforcement on Bedrock
		// relies on a forced synthetic tool; minimax / kimi / gemini on
		// this deployment answered in free prose), and the wizard's
		// chat path can't guarantee tool-forcing. Rather than 502 the
		// turn, treat the prose as the assistant's chat message — which
		// is exactly right for the clarifying-question turns the wizard
		// opens with. A proposal / ready_to_commit only ever arrives as
		// JSON, so a structured turn still parses through the path
		// above; this just keeps the conversation alive on a chatty
		// model instead of dead-ending the operator.
		return &Envelope{Message: raw, ReadyToCommit: false}, nil
	}
	if strings.TrimSpace(env.Message) == "" {
		return nil, errors.New("envelope missing required field: message")
	}
	return env, nil
}

func unmarshalEnvelope(s string) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// firstJSONObject returns the first balanced {...} object in s,
// respecting string literals and escapes so braces inside strings don't
// throw off the depth count. Returns ("", false) when there's no
// complete object.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// decodeTranscript handles the empty-blob case explicitly — a
// fresh row has transcript="[]" but defensive callers might pass
// nil bytes.
func decodeTranscript(b []byte) ([]Turn, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []Turn
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func countUserTurns(transcript []Turn) int {
	n := 0
	for _, t := range transcript {
		if t.Role == "user" {
			n++
		}
	}
	return n
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// pickModel applies a per-call model override when the provider
// supports it. Mirrors the helper in internal/memory/titler.go.
func pickModel(client chat.Provider, model string) chat.Provider {
	if model == "" {
		return client
	}
	if mo, ok := client.(chat.ModelOverridable); ok {
		return mo.WithModel(model)
	}
	return client
}

// composeFromEnvelope runs the 3a Compose pipeline against the
// envelope's structured composition. The anchor slug is
// composition.Template; an empty slug, or one the catalog doesn't
// recognise, anchors on "custom-base" instead — the LLM either
// deliberately built from scratch or hallucinated a slug, and either
// way custom-base is the safe materialisation target. Wizard.Templates
// is typed TemplateSource (the scalar single-value seam Commit's
// legacy path uses); the compose engine needs the multi-value
// MaterialiseMulti the production catalogTemplateSource adapter also
// implements, so this asserts for it and returns a clear ComposeError
// when the wired source doesn't support it (only relevant to
// misconfigured deployments/tests — production always wires the
// adapter).
//
// The []DeclaredSecret result is plumbed through from Compose for
// both call sites (Converse's compose-on-turn and Commit's
// re-compose-on-commit) to consume once the doctor-todo surfacing for
// declared-but-unset secrets lands; today both discard it.
func (w *Wizard) composeFromEnvelope(ctx context.Context, env *Envelope) (map[string]string, []DeclaredSecret, error) { //nolint:unparam // second result awaits the doctor-todo consumer; see comment above
	comp := env.Composition
	slug := comp.Template
	if slug == "" {
		slug = "custom-base"
	} else if w.Templates == nil {
		slug = "custom-base"
	} else if _, ok := w.Templates.Lookup(slug); !ok {
		slug = "custom-base"
	}

	mat, ok := w.Templates.(MultiMaterialiser)
	if !ok {
		return nil, nil, &ComposeError{AddonIndex: -1, Message: "template source does not support multi-value materialisation"}
	}

	var knownMCP map[string]bool
	if w.KnownMCP != nil {
		knownMCP = w.KnownMCP(ctx)
	}
	if knownMCP == nil {
		knownMCP = map[string]bool{}
	}

	in := ComposeInput{
		TemplateSlug: slug,
		Params:       comp.ParamsMulti(),
		Addons:       comp.Addons,
	}
	deps := ComposeDeps{
		Templates:    mat,
		Resolver:     w.Resolver,
		KnownMCP:     knownMCP,
		TemplateMeta: w.TemplateMeta,
	}
	return Compose(in, deps)
}

// validateProposal gates ready_to_commit. When a template is
// resolvable (the LLM set suggested_template and the catalog is
// wired), the proposal is validated the way it will actually be
// committed: derive the template parameters from the proposal,
// materialise the template, and run the rendered project YAML
// through the registry validator. This is what makes ready_to_commit
// reachable — the raw LLM proposal never carries swarmId, so
// validating it directly against the registry always failed.
//
// Without a resolvable template (the from-scratch path) it falls
// back to validating the raw proposal directly via w.Validator,
// which still requires the LLM to author a full project YAML.
func (w *Wizard) validateProposal(p *ProjectYAML, templateSlug string) error {
	if w.Templates != nil && templateSlug != "" {
		if spec, ok := w.Templates.Lookup(templateSlug); ok {
			if p == nil || len(p.Raw) == 0 {
				return errors.New("proposal is empty")
			}
			if strings.TrimSpace(ProposalProjectID(p)) == "" {
				return errors.New("projectId is required")
			}
			params := deriveTemplateParams(spec, p.Raw)
			files, err := w.Templates.Materialise(templateSlug, params)
			if err != nil {
				return err
			}
			return validateRenderedProject(files)
		}
	}
	validator := w.Validator
	if validator == nil {
		validator = permissiveValidator{}
	}
	return validator.Validate(p)
}

// permissiveValidator is Phase A's default validator — any
// non-empty raw map with a "projectId" key passes. Phase B replaces
// with the internal/registry validator proper.
type permissiveValidator struct{}

func (permissiveValidator) Validate(p *ProjectYAML) error {
	if p == nil || len(p.Raw) == 0 {
		return errors.New("empty proposal")
	}
	if id := ProposalProjectID(p); strings.TrimSpace(id) == "" {
		return errors.New("project id is required")
	}
	return nil
}
