package projectwizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
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

%s

When your description matches a template in the gallery above, ALWAYS
set suggested_template — the project is built from that template, so
the proposal only needs to carry the template's key fields and the
operator gets the same vetted swarm + workflow the gallery ships.

The proposal's "raw" object must include at minimum: projectId (slug),
displayName, and a topic (a short phrase naming what to track or do).
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
	LLMUsage UsageRecorder
	// Writer commits the final proposal as a project on disk
	// (Phase B). Optional — without it, Commit returns
	// ErrWriterUnwired. Converse works fine without a writer; the
	// wizard just can't finalise the session.
	Writer ProjectWriter
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
				w.Metrics.recordTurn(turnOutcomeRejected)
				return nil, ErrTooManySessions
			}
		}
		session = &persistence.ProjectWizardSession{
			ID:         persistence.GenerateID("pw"),
			OperatorID: operatorID,
		}
		if err := w.Sessions.Insert(callCtx, session); err != nil {
			w.Metrics.recordTurn(turnOutcomeRejected)
			return nil, fmt.Errorf("projectwizard: insert session: %w", err)
		}
		sessionInserted = true
	} else {
		got, err := w.Sessions.Get(callCtx, sessionID)
		if err != nil {
			w.Metrics.recordTurn(turnOutcomeRejected)
			return nil, fmt.Errorf("projectwizard: load session: %w", err)
		}
		if got == nil {
			w.Metrics.recordTurn(turnOutcomeRejected)
			return nil, persistence.ErrNotFound
		}
		if got.CommittedProjectID != nil {
			w.Metrics.recordTurn(turnOutcomeRejected)
			return nil, ErrSessionCommitted
		}
		if got.CancelledAt != nil {
			w.Metrics.recordTurn(turnOutcomeRejected)
			return nil, ErrSessionCancelled
		}
		if got.OperatorID != operatorID {
			w.Metrics.recordTurn(turnOutcomeRejected)
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
		w.Metrics.recordTurn(turnOutcomeRejected)
		return nil, ErrTurnsExhausted
	}
	transcript = append(transcript, Turn{
		Role:      "user",
		Content:   userMessage,
		CreatedAt: time.Now().UTC(),
	})

	// Build LLM messages.
	msgs := w.buildChatMessages(transcript)

	// Apply response_format=json_schema via context so the chat
	// router enforces the envelope shape (Bedrock injects a
	// synthetic tool whose schema is the envelope, forces
	// tool_choice; Anthropic emits a tool call we unwrap).
	schemaCtx := chat.WithRequestResponseFormatStruct(callCtx, envelopeResponseFormat())

	client := pickModel(w.Chat, w.Model)
	resp, err := client.Complete(schemaCtx, msgs)
	if err != nil {
		w.Metrics.recordTurn(turnOutcomeLLMError)
		return nil, fmt.Errorf("projectwizard: chat: %w", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		w.Metrics.recordTurn(turnOutcomeLLMError)
		return nil, errors.New("projectwizard: empty chat response")
	}
	rawContent := resp.Choices[0].Message.Content
	envelope, err := parseEnvelope(rawContent)
	if err != nil {
		w.Metrics.recordTurn(turnOutcomeLLMError)
		return nil, fmt.Errorf("projectwizard: parse envelope: %w", err)
	}

	// Validate proposal. Failure appends the reason to the
	// message and resets ready_to_commit so the UI doesn't show
	// an enabled commit button on an invalid draft.
	turnOutcome := turnOutcomeAssistantReply
	if envelope.Proposal != nil {
		// Prefer this turn's template hint; fall back to the value
		// stickied on the session from an earlier turn.
		slug := envelope.SuggestedTemplate
		if slug == "" {
			slug = session.SuggestedTemplate
		}
		if verr := w.validateProposal(envelope.Proposal, slug); verr != nil {
			envelope.Message = envelope.Message + "\n\n(validation: " + verr.Error() + ")"
			envelope.ReadyToCommit = false
			turnOutcome = turnOutcomeValidationError
		}
	}
	if envelope.Proposal == nil {
		envelope.ReadyToCommit = false
	}
	w.Metrics.recordTurn(turnOutcome)

	// Append assistant turn + persist.
	envelopeJSON, _ := json.Marshal(envelope)
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

	// Cost row.
	w.recordUsage(ctx, resp, session.ID, envelopeJSON)
	_ = envelopeJSON

	return &Result{SessionID: session.ID, Envelope: envelope}, nil
}

// buildChatMessages composes the system+conversation pair the
// chat router consumes. System message reuses the template (with
// priors spliced); conversation is the transcript rendered as
// role/content pairs.
func (w *Wizard) buildChatMessages(transcript []Turn) []chat.Message {
	system := fmt.Sprintf(systemPromptTemplate, RenderPriors(w.Priors))
	msgs := []chat.Message{
		{Role: "system", Content: system},
	}
	for _, t := range transcript {
		msgs = append(msgs, chat.Message{Role: t.Role, Content: t.Content})
	}
	return msgs
}

func (w *Wizard) recordUsage(ctx context.Context, resp *chat.ChatResponse, sessionID string, _ []byte) {
	if w == nil || w.LLMUsage == nil || resp == nil {
		return
	}
	prompt := resp.Usage.PromptTokens
	completion := resp.Usage.CompletionTokens
	if prompt == 0 && completion == 0 {
		return
	}
	row := &persistence.TaskLLMUsage{
		ID:               persistence.GenerateID("llm"),
		ProjectID:        "", // no project yet; wizard precedes project creation
		StepID:           sessionID,
		Role:             "project_wizard",
		Model:            firstNonEmpty(resp.Model, w.Model),
		PromptTokens:     int64(prompt),
		CompletionTokens: int64(completion),
		Iterations:       1,
		Source:           "project_wizard",
		RecordedAt:       time.Now().UTC(),
	}
	_ = w.LLMUsage.Record(ctx, row)
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
