package narrator

import "fmt"

// triggerKind enumerates the narration trigger kinds the state
// machine recognizes (design §5.2). Distinct from the STORAGE kind
// column (persistence.ExecutionNarrationKind* — step|tool|milestone|
// completion): a couple of trigger kinds map onto the same storage
// kind (step_started and step_completed both persist as "step").
type triggerKind string

const (
	triggerStepStarted   triggerKind = "step_started"
	triggerToolHeartbeat triggerKind = "tool_heartbeat"
	triggerStepCompleted triggerKind = "step_completed"
	triggerCompletion    triggerKind = "completion"
	// triggerAttemptFailed narrates an execution that reached a terminal
	// FAILED/CANCELLED status while its task is still active (a retry or
	// recovery is coming). Distinct from triggerCompletion so the story
	// says "this attempt failed, retrying" rather than ending on the last
	// step's "Finished step N" (a false-success read) or the terminal
	// "task didn't complete" (which wrongly implies the task is done).
	triggerAttemptFailed triggerKind = "attempt_failed"
)

// allTriggerKinds is the exhaustiveness fixture (design §5.5 / §8):
// a unit test walks this list against every role in allKnownRoles
// (plus "" for unknown/system steps) and asserts fallbackTemplate
// never returns "".
var allTriggerKinds = []triggerKind{
	triggerStepStarted, triggerToolHeartbeat, triggerStepCompleted, triggerCompletion, triggerAttemptFailed,
}

// allKnownRoles is the same fixture's role axis — every role the
// templates special-case, plus a handful of unknown/adversarial
// values the DEFAULT branch must still cover.
var allKnownRoles = []string{
	"", "worker", "researcher", "coder", "engineer", "reviewer",
	"architect", "planner", "writer", "tester", "qa", "some_future_role",
}

// templateInput carries the STRUCTURED fields a template renders
// from — never a raw, untrusted string (design §6: step/tool names
// are labelled data, not instructions, and only ever reach the LLM
// path via composeLine's delimited prompt; the deterministic
// fallback path never echoes them at all except via humanizeTool,
// which is inert string formatting, not model input).
type templateInput struct {
	Role      string
	Tool      string
	StepIdx   int
	StepTotal int
	// Outcome is the step_completed outcome class ("ok", "error", ...).
	Outcome string
	// Success is set for triggerCompletion only.
	Success bool
}

// fallbackTemplate renders a deterministic, present-tense line for
// (kind, input) in the playbook register's plain vocabulary
// (playbook.HumanMessage — plain, non-technical, present tense). A
// DEFAULT branch guarantees a non-empty result for any kind not in
// the switch, so a future event kind can never yield a blank line
// (design §5.5 exhaustiveness guarantee).
func fallbackTemplate(kind triggerKind, in templateInput) string {
	switch kind {
	case triggerStepStarted:
		return roleVerb(in.Role) + stepOf(in) + "…"
	case triggerToolHeartbeat:
		return toolHeartbeatTemplate(in)
	case triggerStepCompleted:
		return stepCompletedTemplate(in)
	case triggerCompletion:
		return completionTemplate(in)
	case triggerAttemptFailed:
		return "That attempt ran into a problem — trying again."
	default:
		return defaultTemplate(in)
	}
}

// stepOf renders the "(step N of M)" / "(step N)" suffix. StepTotal
// is 0 ("unknown") whenever no workflow-step-count resolver is
// wired — see executionState's doc comment — so the "of M" clause is
// omitted gracefully rather than rendering "of 0".
func stepOf(in templateInput) string {
	if in.StepIdx <= 0 {
		return ""
	}
	if in.StepTotal > 0 {
		return fmt.Sprintf(" (step %d of %d)", in.StepIdx, in.StepTotal)
	}
	return fmt.Sprintf(" (step %d)", in.StepIdx)
}

// roleVerb maps a role archetype to a plain present-tense verb
// phrase, mirroring playbook.HumanMessage's register. Any role not
// in the map (including "", and any future/unknown role) falls back
// to a generic phrase — the same exhaustiveness discipline as
// fallbackTemplate's DEFAULT branch.
func roleVerb(role string) string {
	switch role {
	case "researcher":
		return "Researching"
	case "coder", "engineer":
		return "Writing code"
	case "reviewer":
		return "Reviewing the work"
	case "architect", "planner":
		return "Planning the approach"
	case "writer":
		return "Writing"
	case "tester", "qa":
		return "Testing"
	default:
		return "Working on the task"
	}
}

func toolHeartbeatTemplate(in templateInput) string {
	tool := humanizeTool(in.Tool)
	if tool == "" {
		return "Still working" + stepOf(in) + "…"
	}
	return fmt.Sprintf("Still using %s%s…", tool, stepOf(in))
}

func stepCompletedTemplate(in templateInput) string {
	switch in.Outcome {
	case "", "ok":
		return "Finished" + stepOf(in) + "."
	default:
		return "Ran into a problem" + stepOf(in) + "."
	}
}

func completionTemplate(in templateInput) string {
	if in.Success {
		return "All done — the task completed successfully."
	}
	return "The task didn't complete successfully."
}

// defaultTemplate is the DEFAULT branch guarantee (design §5.5): any
// trigger kind not explicitly handled above still renders a
// non-blank, generic "still working" line.
func defaultTemplate(in templateInput) string {
	idx := in.StepIdx
	if idx <= 0 {
		idx = 1
	}
	if in.StepTotal > 0 {
		return fmt.Sprintf("Working on step %d of %d…", idx, in.StepTotal)
	}
	return "Working on the task…"
}

// humanizeTool strips MCP-style prefixes (mcp__server__tool) and
// underscores from a tool name for display. Best-effort — an
// unrecognised shape is returned with underscores turned to spaces,
// never left raw-technical when avoidable, and never dropped
// (falling through to "" only for a genuinely empty tool name).
func humanizeTool(tool string) string {
	if tool == "" {
		return ""
	}
	name := tool
	// mcp__<server>__<tool> → last segment is the human-meaningful part.
	if idx := lastIndexAll(name, "__"); idx >= 0 {
		name = name[idx+2:]
	}
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '_' || r == '-' {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// lastIndexAll returns the index of the last occurrence of sep in s,
// or -1. Tiny local helper so this file doesn't need "strings" just
// for one call site beyond what's already imported.
func lastIndexAll(s, sep string) int {
	last := -1
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			last = i
		}
	}
	return last
}
