// Package projectwizard implements the conversational
// project-setup wizard — Feature #2 of the operator-UX arc.
// See https://docs.vornik.io
//
// Phase A (current) covers the converse loop: a chat-style
// API that accepts the operator's natural-language description,
// produces a structured WizardEnvelope via the chat router, and
// persists the conversation to project_wizard_sessions. Phase B
// adds the commit endpoint that turns a ready-to-commit envelope
// into a real project under ~/.config/vornik/configs/projects/.
package projectwizard

import (
	"encoding/json"
	"time"
)

// Envelope is the LLM-emitted structured output for every wizard
// turn. The chat call is constrained to this shape via
// response_format=json_schema so the operator's UI can render the
// preview deterministically.
type Envelope struct {
	// Message is the assistant's chat bubble text — what the
	// operator sees in the transcript. Always populated.
	Message string `json:"message"`

	// Proposal is the LLM's current best-effort project YAML.
	// Nil on the very first turn while the LLM is gathering
	// requirements; populated on every subsequent turn once the
	// shape is clear enough to draft.
	Proposal *ProjectYAML `json:"proposal,omitempty"`

	// ReadyToCommit is the LLM's signal that the proposal is
	// complete + the operator looks ready to press "commit". The
	// server-side validator gets the final say — if the proposal
	// doesn't pass, the wizard force-resets this to false before
	// returning.
	ReadyToCommit bool `json:"ready_to_commit"`

	// SuggestedTemplate is the slug of the closest match in the
	// `configs/project-templates/` gallery. Empty when no template
	// fits and the LLM is composing from scratch.
	SuggestedTemplate string `json:"suggested_template,omitempty"`

	// OpenQuestions surfaces in the UI as suggested-reply chips.
	// Short list ("yes", "every 6 hours", "no human approval")
	// the operator can click instead of typing free text.
	OpenQuestions []string `json:"open_questions,omitempty"`
}

// ProjectYAML carries the proposed project configuration. It's a
// loose map so the wizard can iterate the project schema without
// recompiling — the validator (internal/registry) is the source
// of truth for what shapes are accepted.
type ProjectYAML struct {
	// Raw is the proposed YAML as a generic map. The wizard
	// marshals it to YAML for the preview pane and validates via
	// internal/registry before the commit endpoint accepts it.
	Raw map[string]any `json:"raw"`
}

// MarshalJSON serialises ProjectYAML as just its Raw map so the
// LLM's emitted JSON {"raw": {...}} round-trips losslessly. The
// outer Raw field is preserved in JSON because that's the shape
// the schema constrains the LLM to.
func (p *ProjectYAML) MarshalJSON() ([]byte, error) {
	if p == nil {
		return []byte("null"), nil
	}
	return json.Marshal(struct {
		Raw map[string]any `json:"raw"`
	}{Raw: p.Raw})
}

// UnmarshalJSON accepts either {"raw": {...}} (the LLM's emitted
// shape, schema-enforced) or the bare object (defensive — older
// transcripts might have skipped the wrapper).
func (p *ProjectYAML) UnmarshalJSON(b []byte) error {
	var wrapper struct {
		Raw map[string]any `json:"raw"`
	}
	if err := json.Unmarshal(b, &wrapper); err == nil && wrapper.Raw != nil {
		p.Raw = wrapper.Raw
		return nil
	}
	// Fallback: bare object — treat the whole blob as the raw map.
	var bare map[string]any
	if err := json.Unmarshal(b, &bare); err != nil {
		return err
	}
	p.Raw = bare
	return nil
}

// Turn is one conversation message inside the transcript. Roles
// mirror the chat conversation: "user" / "assistant". On assistant
// turns, Envelope carries the full LLM emission; on user turns
// it's nil.
type Turn struct {
	Role      string    `json:"role"`               // "user" | "assistant"
	Content   string    `json:"content"`            // operator-visible text
	Envelope  *Envelope `json:"envelope,omitempty"` // assistant turns only
	CreatedAt time.Time `json:"created_at"`
}

// Result is the wizard service's per-Converse return value. The
// session ID is echoed so the UI can include it on the next turn
// (handler creates a fresh session when the operator passes "").
type Result struct {
	SessionID string    `json:"session_id"`
	Envelope  *Envelope `json:"envelope"`
}
