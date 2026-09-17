package config

import (
	"fmt"
	"strings"
	"time"
)

// AssistantConfig tunes the configuration and troubleshooting
// assistant in the control plane — the in-process, file-editing agent that
// turns a natural-language intent into a reviewable, rollbackable proposal
// against the deployed config tree. Design of record:
// https://docs.vornik.io (§6 classes,
// §7.3 models, §8.2 kill switches) and the normative review amendments
// (R6 durable apply, R7 egress, R8 budgets).
//
// Off by default. Enabling it is a featuredoctor act (`vornikctl doctor
// feature enable config-assistant`) so the durability contract and the
// different-family model check run BEFORE the first door opens.
type AssistantConfig struct {
	// Enabled opens the operator doors (REST, vornikctl, console). The chat
	// and agent doors have their own gates below and their own
	// prerequisites (identity.enabled; the selfops decision).
	Enabled bool `yaml:"enabled" doc:"Turn the configuration assistant on (operator doors). Off by default."`

	// Model is the assistant's model id — the most capable open-weight
	// model available (design §7.3). Empty falls back to chat.model, which
	// the doctor reports as a prerequisite failure when it would make the
	// assistant and the judge the same family.
	Model string `yaml:"model" doc:"Assistant model id (the file-editing loop). Empty = chat.model."`

	// JudgeModel is the smaller judge's model id and MUST resolve to a
	// different provider family from Model (design §7.3, test 37). A
	// same-family pair is refused by the doctor and by the request path;
	// there is no silent fall back to self-judging.
	JudgeModel string `yaml:"judge_model" doc:"Judge model id. Must be a different provider family from model — a same-family pair is refused."`

	// Paused is the LEVEL-1 kill switch (design §8.2): every request is
	// refused by name before any model call.
	Paused bool `yaml:"paused" doc:"Global pause: refuse every assistant request before any model call (kill switch level 1)."`

	// DisabledClasses is the LEVEL-3 kill switch: blast-radius classes
	// (A, B1, B2, C, D, E) the assistant refuses to propose at all.
	DisabledClasses []string `yaml:"disabled_classes" doc:"Blast-radius classes (A, B1, B2, C, D, E) refused outright (kill switch level 3)."`

	// MaxOutputBytes caps the model's emitted overlay; a completion past it
	// is a refusal, never a truncation (design §8.2, test 17).
	MaxOutputBytes int `yaml:"max_output_bytes" doc:"Cap on the bytes the assistant may write into its overlay per request; exceeding it refuses the request (default 262144)."`

	// MaxToolTurns caps the loop's tool-call iterations per request (R8).
	MaxToolTurns int `yaml:"max_tool_turns" doc:"Per-request cap on tool-call turns in the editing loop (default 40)."`

	// RequestTimeout is the per-request wall-clock deadline (R8).
	RequestTimeout string `yaml:"request_timeout" doc:"Per-request deadline as a duration string (default 5m)."`

	// MaxConcurrentPerProject bounds admission per project (R8).
	MaxConcurrentPerProject int `yaml:"max_concurrent_per_project" doc:"Concurrent assistant requests admitted per project (default 1)."`

	// ClassEDailyCap bounds class-E (authority) proposals per credential per
	// UTC day. Persisted in the deployment database, never in memory
	// (design §6.2 item 5, test 36).
	ClassEDailyCap int `yaml:"class_e_daily_cap" doc:"Class-E (authority) proposals allowed per credential per UTC day; refused loudly past it (default 5)."`

	// AutoApply is the per-project opt-in for auto-applying class A/B
	// proposals after a judge `pass`. Keyed by project id; absent = off.
	// Class C, D and E never auto-apply whatever this says (test 19), and
	// the chat and agent doors never auto-apply (test 22).
	AutoApply map[string]AssistantAutoApply `yaml:"auto_apply" doc:"Per-project opt-in for auto-applying judged class A/B proposals. C, D and E never auto-apply."`

	// Consult is the opt-in architect consultation over A2A (plan §8).
	Consult AssistantConsult `yaml:"consult" doc:"Opt-in architect consultation over A2A. Use requires a subscription; enabling creates an auditable record."`

	// ChatEntrypoint opens the chat-channel entrypoint (classes A and B2 only). Requires
	// identity.enabled — the door refuses to serve while account
	// resolution is unavailable (test 23).
	ChatEntrypoint bool `yaml:"chat_entrypoint" doc:"Open the chat-channel entrypoint (classes A and B2 only). Requires identity.enabled."`
	// AgentEntrypoint opens the agent entrypoint: an agent may PROPOSE a
	// configuration change through mcp__vornik__propose_config (classes A and
	// B2 only, never auto-applied, always human-reviewed).
	//
	// This is the surface design §6.3.2 calls the most dangerous of the four:
	// an agent's prompt carries third-party text, so it is a path from
	// untrusted input to a proposed change to the deployment. The mitigations
	// CONFINE that; they do not detect it. Off by default, and an operator who
	// turns it on has taken an act rather than drifted into one.
	AgentEntrypoint bool `yaml:"agent_entrypoint" doc:"Let agents PROPOSE config changes (classes A and B2 only, never auto-applied, always human-reviewed). The most exposed entrypoint: an agent prompt carries third-party text. Off by default."`
}

// AssistantAutoApply is one project's auto-apply opt-in.
type AssistantAutoApply struct {
	// Classes lists the classes that may auto-apply for this project. Only
	// A, B1 and B2 are honoured; anything else is a validation error so an
	// operator cannot believe they enabled class C auto-apply.
	Classes []string `yaml:"classes" doc:"Classes that may auto-apply for this project: any of A, B1, B2."`
}

// AssistantConsult is the architect-consultation opt-in.
type AssistantConsult struct {
	// Enabled turns consultation on. USE REQUIRES A SUBSCRIPTION — the gate
	// is the service contract, not a licence check in code (operator
	// decision 2026-09-13). Enabling writes an admin_audit event and every
	// consultation is recorded before dispatch; disabling writes one too.
	Enabled bool `yaml:"enabled" doc:"Opt in to architect consultation over A2A. Use requires a subscription; enabling and every use are recorded in admin_audit."`

	// Peer is the a2a.peers key of the architect. Required when Enabled.
	Peer string `yaml:"peer" doc:"The a2a.peers entry to consult (the architect). Required when enabled."`

	// MaxQuestionBytes caps the minimized question sent to the peer (R8).
	MaxQuestionBytes int `yaml:"max_question_bytes" doc:"Cap on the sanitized question sent to the peer (default 8192)."`

	// MaxAnswerBytes caps the accepted answer (R8 response-size cap).
	MaxAnswerBytes int `yaml:"max_answer_bytes" doc:"Cap on the accepted peer answer (default 32768)."`

	// Timeout bounds one consult (R8 deadline).
	Timeout string `yaml:"timeout" doc:"Per-consult deadline as a duration string (default 2m)."`
}

// Config-assistant defaults (design §8.2, review R8).
const (
	ConfigAssistantDefaultMaxOutputBytes          = 256 * 1024
	ConfigAssistantDefaultMaxToolTurns            = 40
	ConfigAssistantDefaultRequestTimeout          = "5m"
	ConfigAssistantDefaultMaxConcurrentPerProject = 1
	ConfigAssistantDefaultClassEDailyCap          = 5
	ConfigAssistantDefaultConsultMaxQuestionBytes = 8 * 1024
	ConfigAssistantDefaultConsultMaxAnswerBytes   = 32 * 1024
	ConfigAssistantDefaultConsultTimeout          = "2m"
)

// ConfigAssistantClasses is the blast-radius class vocabulary (design §6).
var ConfigAssistantClasses = []string{"A", "B1", "B2", "C", "D", "E"}

func isConfigAssistantClass(s string) bool {
	for _, c := range ConfigAssistantClasses {
		if c == strings.ToUpper(strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

// applyDefaults fills zero fields with the shipped defaults.
func (c *AssistantConfig) applyDefaults() {
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = ConfigAssistantDefaultMaxOutputBytes
	}
	if c.MaxToolTurns <= 0 {
		c.MaxToolTurns = ConfigAssistantDefaultMaxToolTurns
	}
	if strings.TrimSpace(c.RequestTimeout) == "" {
		c.RequestTimeout = ConfigAssistantDefaultRequestTimeout
	}
	if c.MaxConcurrentPerProject <= 0 {
		c.MaxConcurrentPerProject = ConfigAssistantDefaultMaxConcurrentPerProject
	}
	if c.ClassEDailyCap <= 0 {
		c.ClassEDailyCap = ConfigAssistantDefaultClassEDailyCap
	}
	if c.Consult.MaxQuestionBytes <= 0 {
		c.Consult.MaxQuestionBytes = ConfigAssistantDefaultConsultMaxQuestionBytes
	}
	if c.Consult.MaxAnswerBytes <= 0 {
		c.Consult.MaxAnswerBytes = ConfigAssistantDefaultConsultMaxAnswerBytes
	}
	if strings.TrimSpace(c.Consult.Timeout) == "" {
		c.Consult.Timeout = ConfigAssistantDefaultConsultTimeout
	}
}

// EffectiveRequestTimeout returns the per-request deadline.
func (c AssistantConfig) EffectiveRequestTimeout() time.Duration {
	return parseDurationOr(c.RequestTimeout, 5*time.Minute)
}

// EffectiveTimeout returns the per-consult deadline.
func (c AssistantConsult) EffectiveTimeout() time.Duration {
	return parseDurationOr(c.Timeout, 2*time.Minute)
}

// ClassDisabled reports whether class is in DisabledClasses (level-3 switch).
func (c AssistantConfig) ClassDisabled(class string) bool {
	for _, d := range c.DisabledClasses {
		if strings.EqualFold(strings.TrimSpace(d), class) {
			return true
		}
	}
	return false
}

// AutoApplyAllows reports whether the project opted in to auto-applying the
// class. It is NOT the whole auto-apply decision: the door, the judge
// verdict and the class ceiling (C, D, E never) are checked by the engine.
func (c AssistantConfig) AutoApplyAllows(projectID, class string) bool {
	opt, ok := c.AutoApply[projectID]
	if !ok {
		return false
	}
	for _, cl := range opt.Classes {
		if strings.EqualFold(strings.TrimSpace(cl), class) {
			return true
		}
	}
	return false
}

// Validate checks the block. An empty block is valid (feature off).
func (c AssistantConfig) Validate() error {
	for _, d := range c.DisabledClasses {
		if !isConfigAssistantClass(d) {
			return fmt.Errorf("config_assistant.disabled_classes: %q is not one of %s", d, strings.Join(ConfigAssistantClasses, "|"))
		}
	}
	for project, opt := range c.AutoApply {
		for _, cl := range opt.Classes {
			u := strings.ToUpper(strings.TrimSpace(cl))
			switch u {
			case "A", "B1", "B2":
			case "C", "D", "E":
				// Test 19: there exists NO config key that enables class C/D/E
				// auto-apply. Refusing the value at validation is what makes
				// that true of the schema rather than of one default.
				return fmt.Errorf("config_assistant.auto_apply.%s: class %s can never auto-apply (only A, B1, B2 may opt in)", project, u)
			default:
				return fmt.Errorf("config_assistant.auto_apply.%s: %q is not one of A|B1|B2", project, cl)
			}
		}
	}
	if v := strings.TrimSpace(c.RequestTimeout); v != "" {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("config_assistant.request_timeout: %w", err)
		}
	}
	if v := strings.TrimSpace(c.Consult.Timeout); v != "" {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("config_assistant.consult.timeout: %w", err)
		}
	}
	if c.Consult.Enabled && strings.TrimSpace(c.Consult.Peer) == "" {
		return fmt.Errorf("config_assistant.consult.peer: required when consult.enabled is true")
	}
	if c.MaxOutputBytes < 0 || c.MaxToolTurns < 0 || c.MaxConcurrentPerProject < 0 || c.ClassEDailyCap < 0 {
		return fmt.Errorf("config_assistant: numeric limits must be >= 0")
	}
	return nil
}
