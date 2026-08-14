package agentbench

import (
	"context"
	"sort"
	"strings"
)

// Tool-use probe (§3.2, third probe).
//
// DISTINCT FROM THE TOOL-GRANT PROBE, which asks whether the lead handed the
// step the right tools. This asks whether the agent then USED them correctly:
// valid arguments, real tool names, and a call budget spent rather than
// exhausted. A step can score perfectly on grants and still fail because the
// agent called a tool that does not exist, or called a real one with arguments
// its schema rejects.
//
// This is the closest thing in the design to the industry's function-calling
// benchmarks, and it is measured the same way they measure it: name validity
// and argument validity against the advertised schema, counted per call.
//
// Substrate is tool_audit_log (one row per invocation, with tool_input and
// tool_output) plus the effective_tool_budget / tool_calls_used columns already
// on execution_step_outcomes. No new instrumentation, same as every other probe.

// ToolCall is one recorded invocation.
type ToolCall struct {
	Name string `json:"name"`
	// Role is the swarm role that made the call, resolved from the step. Carried
	// so validity can be reported per role as well as per call.
	Role string `json:"role,omitempty"`
	// Failed reports a call that returned an error rather than a result.
	Failed bool `json:"failed"`
	// ErrorText is the recorded failure, used to separate an agent's mistake
	// from a tool's own outage.
	ErrorText string `json:"errorText,omitempty"`
}

// unknownToolMarkers identify a call to a tool that was never advertised —
// the agent inventing a name. Kept separate from ordinary call failures
// because the fix is different: a hallucinated tool name is a prompt or
// advertisement problem, not a flaky dependency.
var unknownToolMarkers = []string{
	"unknown tool",
	"tool not found",
	"no such tool",
	"not advertised",
	"is not available",
}

// argumentErrorMarkers identify a real tool called with arguments its schema
// rejected — the function-calling failure proper.
var argumentErrorMarkers = []string{
	"invalid arguments",
	"invalid parameters",
	"validation failed",
	"required property",
	"cannot unmarshal",
	"invalid character",
	"missing required",
}

// ToolUseVerdict is the tool-use probe's vector.
type ToolUseVerdict struct {
	Probe       string `json:"probe"`
	ExecutionID string `json:"executionId"`
	TaskID      string `json:"taskId"`

	Calls  int `json:"calls"`
	Failed int `json:"failed"`

	// UnknownTool counts invented tool names; ArgumentError counts real tools
	// called with arguments their schema rejected. Split because they have
	// different fixes, and blending them would hide which one is happening.
	UnknownTool   int `json:"unknownTool"`
	ArgumentError int `json:"argumentError"`
	// OtherFailure is a call that failed for reasons that are not the agent's
	// doing — the dependency was down. Excluded from call validity, because
	// scoring an agent on a third party's outage measures the wrong thing.
	OtherFailure int `json:"otherFailure"`

	// CallValidity is the headline: calls that were neither an invented name
	// nor a schema-rejected argument set, over calls where the agent's own
	// choice was what was tested.
	CallValidity        float64 `json:"callValidity"`
	CallValidityDefined bool    `json:"callValidityDefined"`

	// BudgetUtilisation is used/budget. Both over- and under-provisioning are
	// findings: near 1.0 with failures suggests a cap that is too tight, while
	// a persistently tiny fraction is budget nobody needed.
	BudgetUtilisation        float64 `json:"budgetUtilisation"`
	BudgetUtilisationDefined bool    `json:"budgetUtilisationDefined"`
	// BudgetExhausted reports the step hitting its iteration cap, which is the
	// terminal form of under-provisioning.
	BudgetExhausted bool `json:"budgetExhausted"`

	// RepeatedIdenticalCalls counts a call made more than once with the same
	// name in the same execution beyond the first — the cheapest available
	// signal for a degenerate loop, and a direct token cost.
	RepeatedCalls int `json:"repeatedCalls"`

	// ByRole is call validity per role.
	//
	// WHY IT EXISTS. CallValidity is weighted by CALL COUNT, which answers "what
	// fraction of calls conformed" — not "what fraction of roles called
	// correctly". Round-4 review found the consequence: a role making 100 calls
	// at 95% validity and a role making 1 call that is wrong every time
	// aggregate to ~95%, and the second role's total failure is invisible. Both
	// questions are legitimate; the aggregate answers the first, and this
	// answers the second, which is the one that catches a single broken role.
	ByRole map[string]RoleCallValidity `json:"byRole,omitempty"`
}

// RoleCallValidity is one role's tool-call record.
type RoleCallValidity struct {
	Calls         int     `json:"calls"`
	Attributable  int     `json:"attributable"`
	Invalid       int     `json:"invalid"`
	Validity      float64 `json:"validity"`
	ValidityValid bool    `json:"validityDefined"`
}

// ToolUseProbe scores how well an agent used the tools it was given.
type ToolUseProbe struct{}

// Name implements Probe.
func (ToolUseProbe) Name() string { return "tool-use" }

// ScoreToolUse scores one execution's tool use.
//
// Like the schema probe it needs no Gold: a tool call is valid or not against
// the tool's own advertised schema, which is a fact about the call rather than
// something a prior run has to establish.
func (p ToolUseProbe) ScoreToolUse(trace Trace, task TaskRef) ToolUseVerdict {
	v := ToolUseVerdict{
		Probe:       p.Name(),
		ExecutionID: trace.ExecutionID,
		TaskID:      task.ID,
		Calls:       len(trace.Calls),
	}

	seen := map[string]int{}
	byRole := map[string]RoleCallValidity{}
	for _, call := range trace.Calls {
		seen[call.Name]++
		if seen[call.Name] > 1 {
			v.RepeatedCalls++
		}

		rc := byRole[call.Role]
		rc.Calls++
		kind := callFailureOther
		if call.Failed {
			v.Failed++
			kind = classifyCallFailure(call.ErrorText)
		}
		switch {
		case !call.Failed:
			rc.Attributable++
		case kind == callFailureUnknownTool:
			v.UnknownTool++
			rc.Attributable++
			rc.Invalid++
		case kind == callFailureArgument:
			v.ArgumentError++
			rc.Attributable++
			rc.Invalid++
		default:
			v.OtherFailure++
		}
		byRole[call.Role] = rc
	}
	for role, rc := range byRole {
		if rc.Attributable > 0 {
			rc.Validity = float64(rc.Attributable-rc.Invalid) / float64(rc.Attributable)
			rc.ValidityValid = true
		}
		if v.ByRole == nil {
			v.ByRole = map[string]RoleCallValidity{}
		}
		v.ByRole[role] = rc
	}

	// The denominator excludes third-party outages: the agent's choice was not
	// what failed there, and including them would let a flaky dependency read
	// as an agent that cannot call functions.
	attributable := v.Calls - v.OtherFailure
	if attributable > 0 {
		bad := v.UnknownTool + v.ArgumentError
		v.CallValidity = float64(attributable-bad) / float64(attributable)
		v.CallValidityDefined = true
	}

	if trace.ToolBudget > 0 {
		v.BudgetUtilisation = float64(trace.ToolCallsUsed) / float64(trace.ToolBudget)
		v.BudgetUtilisationDefined = true
	}
	for _, o := range trace.Outcomes {
		if o.Outcome == OutcomeIterationExhausted {
			v.BudgetExhausted = true
			break
		}
	}
	return v
}

// Score implements Probe.
func (p ToolUseProbe) Score(_ context.Context, task TaskRef, _ Gold, trace Trace) (Verdict, error) {
	t := p.ScoreToolUse(trace, task)
	return Verdict{
		Probe:       p.Name(),
		ExecutionID: trace.ExecutionID,
		TaskID:      task.ID,
		ToolUse:     &t,
	}, nil
}

type callFailureKind int

const (
	callFailureOther callFailureKind = iota
	callFailureUnknownTool
	callFailureArgument
)

// classifyCallFailure separates the agent's mistakes from everything else.
//
// Unknown-tool is checked first: a call to a name that does not exist often
// also reports an argument problem downstream, and the invented name is the
// finding.
func classifyCallFailure(errText string) callFailureKind {
	msg := strings.ToLower(strings.TrimSpace(errText))
	if msg == "" {
		return callFailureOther
	}
	for _, m := range unknownToolMarkers {
		if strings.Contains(msg, m) {
			return callFailureUnknownTool
		}
	}
	for _, m := range argumentErrorMarkers {
		if strings.Contains(msg, m) {
			return callFailureArgument
		}
	}
	return callFailureOther
}

// ToolsByFailureCount lists the tools an execution failed to call correctly,
// worst first — the actionable output when call validity drops.
func (v ToolUseVerdict) ToolsByFailureCount(trace Trace) []string {
	counts := map[string]int{}
	for _, call := range trace.Calls {
		if call.Failed && classifyCallFailure(call.ErrorText) != callFailureOther {
			counts[call.Name]++
		}
	}
	out := make([]string, 0, len(counts))
	for name := range counts {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
