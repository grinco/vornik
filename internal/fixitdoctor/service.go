// Package fixitdoctor — task 3.2 adds the converse loop on top of
// task 3.1's grounding assembler: a durable, per-FailureRef repair
// chat session, a schema-constrained FixItEnvelope, untrusted-content
// fencing in the prompt, re-grounding on every turn, and the
// Resolved-status-poll handshake. Mirrors internal/projectwizard's
// Wizard.Converse shape throughout — see
// https://docs.vornik.io §5.2.
//
// Deliberately NOT in scope here: actually dispatching/executing a
// proposed action (task 3.3's deny-by-default action dispatcher) and
// UI entry-point buttons (task 3.4). Mutating action kinds render as
// preview-only cards; only ActionKindLinkOut is live (pure
// navigation).
package fixitdoctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// SessionStore is the narrow interface the service needs from the
// persistence layer — mirrors projectwizard.SessionStore's role
// exactly. persistence.FixItSessionRepository satisfies it by
// structural conformance.
type SessionStore interface {
	Get(ctx context.Context, id string) (*persistence.FixItSession, error)
	Insert(ctx context.Context, s *persistence.FixItSession) error
	Update(ctx context.Context, s *persistence.FixItSession) error
	Close(ctx context.Context, id, operatorID string) error
	ListByOperator(ctx context.Context, operatorID string, pageSize int) ([]*persistence.FixItSession, error)
}

// UsageRecorder is the narrow cost-attribution hook, mirroring
// projectwizard.UsageRecorder. Optional — nil disables billing rows.
type UsageRecorder interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// ProjectLookup resolves a project ID to its registry.Project, the
// same narrow shape *registry.ProjectRegistry satisfies. Used to
// gate LLM spend via budget.Check; nil (or a ref with no ProjectID,
// e.g. the daemon-scope failed_reload kind) simply skips the budget
// gate.
type ProjectLookup interface {
	GetProject(id string) *registry.Project
}

// TaskLLMUsageSourceFixItDoctor / RoleFixItDoctor are the cost-
// attribution role/source stamped on every usage row this service
// writes, so Fix-It Doctor spend is distinguishable on the spend
// dashboard from project_wizard / dispatcher / workflow_step rows.
const (
	RoleFixItDoctor   = "fix_it_doctor"
	SourceFixItDoctor = "fix_it_doctor"
)

// Turn is one message in a repair-chat transcript. Mirrors
// projectwizard.Turn's shape.
type Turn struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Envelope  *FixItEnvelope `json:"envelope,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Result is what Converse returns on a successful turn.
type Result struct {
	SessionID string
	Envelope  *FixItEnvelope
	// StatusPoll is populated only on a turn where the envelope's
	// Resolved flag was true — the objective state check the operator
	// sees instead of an auto-close.
	StatusPoll *StatusPollResult
}

// Errors mirroring projectwizard's Converse refusal taxonomy.
var (
	// ErrTooManySessions — the operator hit their concurrent open-
	// session cap.
	ErrTooManySessions = errors.New("fixitdoctor: too many active sessions")
	// ErrSessionClosed — the session is closed (operator-closed or
	// cascade-closed); further converse calls are refused.
	ErrSessionClosed = errors.New("fixitdoctor: session already closed")
	// ErrTurnsExhausted — the session hit MaxTurns.
	ErrTurnsExhausted = errors.New("fixitdoctor: session turn limit reached")
	// ErrFailureRefRequired — a new session (sessionID == "") needs a
	// non-empty FailureRef to bind to.
	ErrFailureRefRequired = errors.New("fixitdoctor: failure ref required for a new session")
)

// Service is the per-daemon repair-chat orchestrator. One instance
// shared across all operators; sessions are looked up by ID per call.
type Service struct {
	// Sessions persists transcript + envelope state across turns.
	Sessions SessionStore
	// Assembler builds (and re-builds, every turn) the grounding
	// bundle a FailureRef is repaired against. Required.
	Assembler *Assembler
	// Chat is the LLM provider.
	Chat chat.Provider
	// Model is the LLM model identifier passed via
	// chat.ModelOverridable ("fixit.model" config). Empty leaves the
	// router's default.
	Model string
	// Edition gates the ActionKind vocabulary (config_apply is
	// Enterprise-only, §5.3). Empty normalizes to Community (fail-safe
	// — see version.NormalizeEdition).
	Edition string
	// LLMUsage records one row per call for the spend dashboard, with
	// Role/Source = fix_it_doctor. Optional.
	LLMUsage UsageRecorder
	// BudgetRepo backs the budget.Check spend-cap gate. Optional — a
	// nil repo (or a ref with no ProjectID) skips the gate.
	BudgetRepo budget.Repo
	// Projects resolves ref.ProjectID -> *registry.Project for the
	// budget gate. Optional.
	Projects ProjectLookup
	// Metrics counts session + guardrail outcomes. Optional — nil is
	// no-op.
	Metrics *Metrics
	// MaxActiveSessions caps the number of OPEN sessions one operator
	// can hold at once. 0 -> 5 (mirrors the wizard's default).
	MaxActiveSessions int
	// MaxTurns caps the conversation at this many user turns. 0 -> 20.
	MaxTurns int
	// Timeout caps each LLM call. 0 -> 60s.
	Timeout time.Duration

	// --- Task 3.3: the deny-by-default action dispatcher (dispatch.go) ---
	// Each of the following is the execution seam for exactly one
	// ActionKind (fix-it-doctor-design.md §5.3). A nil seam does not
	// widen the vocabulary — Dispatch still validates kind+params first
	// and simply fails closed ("not configured on this deployment") for
	// an action whose pipeline isn't wired, the same fail-safe posture
	// every other optional Service dependency uses.

	// GatePipeline executes ActionKindConfigApplyGate (CE): resolves a
	// "key" param to a registered feature-doctor gate and applies via
	// featuredoctor.PlanEnable/ApplyEnable (backup/write/validate/
	// restore-on-fail already inside the pipeline).
	GatePipeline GatePipeline
	// ConfigProposals executes ActionKindConfigApply (EE-only — see
	// Edition/AllowedActionKinds). Files a ControlPlaneProposal with
	// ProposedBy=fix_it_doctor, then applies it with the human operator
	// as actor (proposer != approver, clearing ErrProposalSelfApprove
	// by design).
	ConfigProposals ConfigProposalPipeline
	// ActionTaskRetrier executes ActionKindRetryTask.
	ActionTaskRetrier TaskRetrier
	// IntegrationReprober executes ActionKindReprobeIntegration.
	IntegrationReprober IntegrationReprober
	// SecretSetter executes ActionKindSetSecret. The VALUE always comes
	// from Dispatch's caller-supplied secretValue argument, never from
	// the persisted ProposedAction.Params — see dispatch.go.
	SecretSetter SecretSetter
	// Audit records one AdminAuditEntry per successfully APPLIED action
	// (fix-it-doctor-design.md §5.6). Nil-safe: a missing audit sink
	// degrades to "not recorded" rather than blocking the apply — the
	// mutation already happened through its own pipeline (which has its
	// own ledger, e.g. ControlPlaneProposal), so a failed audit insert
	// must not make an already-applied fix look like it failed.
	Audit AuditRecorder

	// dispatchMu serialises the fresh-read→append→write of a session's
	// AppliedActions inside recordAppliedAction, so two concurrent applies
	// (double-click, future automation) can't lost-update the record
	// (review-20260716-d95b). It is held ONLY across that short window — never
	// across the pipeline/audit IO — so a slow apply can't stall other
	// dispatches (review-20260717-c377). A single coarse mutex is adequate for
	// this low-volume admin surface and avoids a per-session keyed-lock map that
	// would leak entries; a multi-daemon deployment would need a DB advisory
	// lock instead — deferred with the other multi-instance items.
	dispatchMu sync.Mutex
}

// Converse appends the operator's message to the session transcript,
// re-grounds via Assembler.Assemble, calls the LLM with the envelope
// schema, parses + validates the envelope (dropping any out-of-
// vocabulary or param-invalid action), persists the session, and
// returns the result.
//
// Pass sessionID="" with a populated ref to start a fresh session
// bound to that FailureRef. For a RESUMED session (sessionID != ""),
// ref is ignored entirely — the FailureRef ridden on the persisted
// session row is authoritative, so a caller can never redirect an
// existing session at a different (possibly out-of-scope) object by
// tampering with the request body. Callers that need to scope-gate a
// resumed session by project should load the session themselves first
// (Sessions.Get) and check its ProjectID before calling Converse.
func (s *Service) Converse(ctx context.Context, sessionID, operatorID string, ref FailureRef, userMessage string) (res *Result, retErr error) {
	userMessage = strings.TrimSpace(userMessage)
	if err := validateConverseInputs(s, operatorID, userMessage); err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	createdNew := sessionID == ""
	sessionInserted := false
	var session *persistence.FixItSession
	// Mirrors projectwizard.Converse's orphan-draft cleanup: a brand-
	// new session that fails its very first turn is closed so it
	// doesn't linger as an uncommitted-forever row counting against
	// the operator's session cap. A RESUMED session is left intact on
	// failure — its prior turns are real work.
	defer func() {
		if retErr != nil && createdNew && sessionInserted && session != nil {
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer ccancel()
			_ = s.Sessions.Close(cctx, session.ID, operatorID)
		}
	}()

	if sessionID == "" {
		// Validate the caller-supplied ref and budget-gate BEFORE creating the
		// session, so a malformed ref or a budget-blocked request never inserts
		// (and then immediately closes) an orphan session row
		// (review-20260716-d95b). A resumed session skips this: its ref is the
		// store's own, and no new row is created.
		if err := validateFailureRef(ref); err != nil {
			return nil, err
		}
		if blocked, reason := s.budgetBlocked(callCtx, ref.ProjectID); blocked {
			return nil, fmt.Errorf("fixitdoctor: budget exceeded: %s", reason)
		}
		newSession, err := s.startSession(callCtx, operatorID, ref)
		if err != nil {
			return nil, err
		}
		session = newSession
		sessionInserted = true
	} else {
		gotSession, gotRef, err := s.resumeSession(callCtx, sessionID, operatorID)
		if err != nil {
			return nil, err
		}
		session, ref = gotSession, gotRef
		// Resumed: ref is now the store's authoritative value — budget-gate here.
		if blocked, reason := s.budgetBlocked(callCtx, ref.ProjectID); blocked {
			return nil, fmt.Errorf("fixitdoctor: budget exceeded: %s", reason)
		}
	}

	// Cascade-close check: has the underlying object vanished since
	// this session was opened (or since the last turn)? A poll error
	// here is NOT itself proof the object is gone (transient DB/probe
	// hiccup), so only an explicit ErrNotFound-shaped signal closes
	// the session; any other poll error just degrades that turn's
	// state-changed comparison (handled below).
	if s.objectGone(callCtx, ref) {
		_ = s.Sessions.Close(callCtx, session.ID, operatorID)
		s.Metrics.recordSession(string(ref.Kind), SessionOutcomeClosed)
		return nil, ErrSessionClosed
	}

	transcript, err := decodeTranscript(session.Transcript)
	if err != nil {
		return nil, fmt.Errorf("fixitdoctor: decode transcript: %w", err)
	}
	if countUserTurns(transcript) >= s.maxTurns() {
		return nil, ErrTurnsExhausted
	}
	transcript = append(transcript, Turn{Role: "user", Content: userMessage, CreatedAt: time.Now().UTC()})

	envelope, resp, newSignal, err := s.runTurn(callCtx, ref, session, transcript)
	if err != nil {
		return nil, err
	}

	result := &Result{SessionID: session.ID, Envelope: envelope}
	if envelope.Resolved {
		if poll, perr := s.Assembler.PollResolvedStatus(callCtx, ref); perr == nil {
			result.StatusPoll = &poll
		}
		s.Metrics.recordSession(string(ref.Kind), SessionOutcomeResolved)
	}

	if err := s.persistTurn(callCtx, session, transcript, envelope, newSignal); err != nil {
		return result, err
	}

	s.recordUsage(ctx, resp, session.ID)
	return result, nil
}

// runTurn re-grounds (Assemble), computes the state-changed notice,
// calls the LLM with the envelope schema, parses the response, and
// guardrail-validates its proposed actions. Split out of Converse to
// keep the top-level orchestration function's cognitive complexity
// bounded — this is the "one LLM round-trip" unit.
func (s *Service) runTurn(ctx context.Context, ref FailureRef, session *persistence.FixItSession, transcript []Turn) (*FixItEnvelope, *chat.ChatResponse, string, error) {
	// RE-GROUND every turn (fix-it-doctor-design.md §5.2).
	bundle, err := s.Assembler.Assemble(ctx, ref)
	if err != nil {
		return nil, nil, "", fmt.Errorf("fixitdoctor: assemble grounding: %w", err)
	}

	newSignal := s.Assembler.statusSignal(ctx, ref)
	stateChangedNotice := stateChangedNoticeFor(session.StatusSignal, newSignal)

	msgs := s.buildChatMessages(bundle, stateChangedNotice, transcript)
	schemaCtx := chat.WithRequestResponseFormatStruct(ctx, EnvelopeResponseFormat(s.Edition))
	client := pickModel(s.Chat, s.Model)
	resp, err := client.Complete(schemaCtx, msgs)
	if err != nil {
		return nil, nil, newSignal, fmt.Errorf("fixitdoctor: chat: %w", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		return nil, nil, newSignal, errors.New("fixitdoctor: empty chat response")
	}
	envelope, err := ParseEnvelope(resp.Choices[0].Message.Content)
	if err != nil {
		return nil, nil, newSignal, fmt.Errorf("fixitdoctor: parse envelope: %w", err)
	}
	envelope.Actions = ValidateActions(envelope.Actions, s.Edition, s.Metrics)
	return envelope, resp, newSignal, nil
}

// stateChangedNoticeFor compares the status signal observed when the
// session last persisted against the freshly re-grounded one, per
// fix-it-doctor-design.md §5.2 ("if the underlying failure status
// changed since open, inject a notice"). Empty on the session's first
// turn (nothing to compare against yet) or when either signal is
// unavailable.
func stateChangedNoticeFor(previous, current string) string {
	if previous == "" || current == "" || previous == current {
		return ""
	}
	return fmt.Sprintf(
		"The underlying object's status has changed since this conversation started (was: %q, now: %q). Re-assess before proposing further actions.",
		previous, current,
	)
}

// persistTurn appends the assistant turn to the transcript and writes
// the session row back.
func (s *Service) persistTurn(ctx context.Context, session *persistence.FixItSession, transcript []Turn, envelope *FixItEnvelope, newSignal string) error {
	envelopeJSON, _ := json.Marshal(envelope)
	transcript = append(transcript, Turn{
		Role:      "assistant",
		Content:   envelope.Message,
		Envelope:  envelope,
		CreatedAt: time.Now().UTC(),
	})
	transcriptBytes, _ := json.Marshal(transcript)
	session.Transcript = transcriptBytes
	session.LastEnvelope = envelopeJSON
	if newSignal != "" {
		session.StatusSignal = newSignal
	}
	if err := s.Sessions.Update(ctx, session); err != nil {
		return fmt.Errorf("fixitdoctor: update session: %w", err)
	}
	return nil
}

func (s *Service) maxTurns() int {
	if s.MaxTurns > 0 {
		return s.MaxTurns
	}
	return 20
}

func (s *Service) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 60 * time.Second
}

// validateConverseInputs checks the Converse preconditions common to
// both the new-session and resumed-session paths. userMessage is
// expected already-trimmed.
func validateConverseInputs(s *Service, operatorID, userMessage string) error {
	if s == nil || s.Sessions == nil || s.Assembler == nil || s.Chat == nil {
		return errors.New("fixitdoctor: not fully wired")
	}
	if userMessage == "" {
		return errors.New("fixitdoctor: empty user message")
	}
	if operatorID == "" {
		return errors.New("fixitdoctor: operator id required")
	}
	return nil
}

// maxFailureRefFieldLen bounds a FailureRef's ID/ProjectID at the public
// boundary. These flow into grounding SQL lookups (parameterised) and the audit
// Target field; a real task/execution/integration id or feature key is short.
const maxFailureRefFieldLen = 256

// validateFailureRef bounds + character-set-checks the CALLER-supplied ref
// before a NEW session is created (review-20260716-d95b). A resumed session's
// ref comes from the store and is already trusted. The charset rule is
// deliberately a denylist (no control chars, no whitespace, valid UTF-8) rather
// than an allowlist, so it rejects abuse (oversized / binary / newline-injected
// ids) without over-blocking legitimate slug/id charsets we don't fully enumerate.
func validateFailureRef(ref FailureRef) error {
	if ref.Kind == "" || ref.ID == "" {
		return ErrFailureRefRequired
	}
	if !isKnownFailureKind(ref.Kind) {
		return fmt.Errorf("fixitdoctor: unknown failure ref kind %q", ref.Kind)
	}
	if err := validRefField("id", ref.ID); err != nil {
		return err
	}
	if ref.ProjectID != "" {
		if err := validRefField("project id", ref.ProjectID); err != nil {
			return err
		}
	}
	return nil
}

func validRefField(name, v string) error {
	if len(v) > maxFailureRefFieldLen {
		return fmt.Errorf("fixitdoctor: failure ref %s exceeds %d bytes", name, maxFailureRefFieldLen)
	}
	if !utf8.ValidString(v) {
		return fmt.Errorf("fixitdoctor: failure ref %s is not valid UTF-8", name)
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("fixitdoctor: failure ref %s contains an invalid character", name)
		}
	}
	return nil
}

// startSession allocates and inserts a brand-new session bound to
// ref, enforcing the per-operator active-session cap first. Split out
// of Converse to keep its cognitive complexity bounded.
func (s *Service) startSession(ctx context.Context, operatorID string, ref FailureRef) (*persistence.FixItSession, error) {
	if ref.Kind == "" || ref.ID == "" {
		return nil, ErrFailureRefRequired
	}
	maxActive := s.MaxActiveSessions
	if maxActive <= 0 {
		maxActive = 5
	}
	// Fail-open on a ListByOperator error (transient DB hiccup): a lookup
	// failure here must not block starting a new repair session — matches
	// the project-wizard's identical precedent for its own session cap.
	if existing, err := s.Sessions.ListByOperator(ctx, operatorID, maxActive*2+1); err == nil {
		active := 0
		for _, row := range existing {
			if row != nil && row.ClosedAt == nil {
				active++
			}
		}
		if active >= maxActive {
			s.Metrics.recordSession(string(ref.Kind), SessionOutcomeRejected)
			return nil, ErrTooManySessions
		}
	}
	session := &persistence.FixItSession{
		ID:           persistence.GenerateID("fix"),
		OperatorID:   operatorID,
		FailureKind:  string(ref.Kind),
		FailureRefID: ref.ID,
		ProjectID:    ref.ProjectID,
	}
	if err := s.Sessions.Insert(ctx, session); err != nil {
		s.Metrics.recordSession(string(ref.Kind), SessionOutcomeRejected)
		return nil, fmt.Errorf("fixitdoctor: insert session: %w", err)
	}
	s.Metrics.recordSession(string(ref.Kind), SessionOutcomeOpened)
	return session, nil
}

// resumeSession loads an existing session, enforcing the IDOR + closed
// checks, and returns the session's OWN persisted ref — authoritative
// over whatever the caller passed in (see Converse's doc comment).
func (s *Service) resumeSession(ctx context.Context, sessionID, operatorID string) (*persistence.FixItSession, FailureRef, error) {
	got, err := s.Sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, FailureRef{}, fmt.Errorf("fixitdoctor: load session: %w", err)
	}
	if got == nil {
		return nil, FailureRef{}, persistence.ErrNotFound
	}
	if got.OperatorID != operatorID {
		// IDOR guard — same "don't leak existence" convention as the
		// wizard's Cancel.
		return nil, FailureRef{}, persistence.ErrNotFound
	}
	if got.ClosedAt != nil {
		return nil, FailureRef{}, ErrSessionClosed
	}
	ref := FailureRef{Kind: FailureKind(got.FailureKind), ID: got.FailureRefID, ProjectID: got.ProjectID}
	return got, ref, nil
}

// objectGone reports whether the referenced failing object has been
// deleted since the session was opened. Only failed_task has a
// concrete, checkable backing row today (a task ID); the other three
// kinds either have no delete concept (featuredoctor features are a
// static registry; integrations aren't DB rows) or are daemon-scoped,
// so they never cascade-close via this path.
func (s *Service) objectGone(ctx context.Context, ref FailureRef) bool {
	if ref.Kind != FailureKindFailedTask || s.Assembler == nil || s.Assembler.Tasks == nil {
		return false
	}
	_, err := s.Assembler.Tasks.Get(ctx, ref.ID)
	return errors.Is(err, persistence.ErrNotFound)
}

func (s *Service) budgetBlocked(ctx context.Context, projectID string) (bool, string) {
	if s.BudgetRepo == nil || s.Projects == nil || projectID == "" {
		return false, ""
	}
	proj := s.Projects.GetProject(projectID)
	if proj == nil {
		return false, ""
	}
	decision, err := budget.Check(ctx, s.BudgetRepo, proj, time.Now().UTC())
	if err != nil {
		return false, ""
	}
	return decision.Blocked, decision.Reason
}

// buildChatMessages composes the system+conversation pair the chat
// router consumes. The system message is BuildSystemPrompt's fenced
// grounding; conversation is the transcript rendered as role/content
// pairs (the assistant's own prior envelopes collapse to their
// Message text — the model doesn't need its own past raw JSON echoed
// back to reconstruct the conversation).
func (s *Service) buildChatMessages(bundle GroundingBundle, stateChangedNotice string, transcript []Turn) []chat.Message {
	system := BuildSystemPrompt(bundle, s.Edition, stateChangedNotice)
	msgs := []chat.Message{{Role: "system", Content: system}}
	for _, t := range transcript {
		msgs = append(msgs, chat.Message{Role: t.Role, Content: t.Content})
	}
	return msgs
}

func (s *Service) recordUsage(ctx context.Context, resp *chat.ChatResponse, sessionID string) {
	if s == nil || s.LLMUsage == nil || resp == nil {
		return
	}
	prompt := resp.Usage.PromptTokens
	completion := resp.Usage.CompletionTokens
	if prompt == 0 && completion == 0 {
		return
	}
	row := &persistence.TaskLLMUsage{
		ID:               persistence.GenerateID("llm"),
		StepID:           sessionID,
		Role:             RoleFixItDoctor,
		Model:            firstNonEmpty(resp.Model, s.Model),
		PromptTokens:     int64(prompt),
		CompletionTokens: int64(completion),
		Iterations:       1,
		Source:           SourceFixItDoctor,
		RecordedAt:       time.Now().UTC(),
	}
	_ = s.LLMUsage.Record(ctx, row)
}

// decodeTranscript handles the empty-blob case explicitly, mirroring
// projectwizard's helper of the same name.
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
// supports it. Mirrors projectwizard's helper of the same name.
func pickModel(client chat.Provider, model string) chat.Provider {
	if model == "" {
		return client
	}
	if mo, ok := client.(chat.ModelOverridable); ok {
		return mo.WithModel(model)
	}
	return client
}
