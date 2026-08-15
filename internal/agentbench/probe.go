// Package agentbench measures the decisions Vornik's control logic makes
// (https://docs.vornik.io).
//
// L2 — decision quality — is what lives here: verifiable probes over persisted
// execution traces, scored against a ground truth that is a RECORDING rather
// than an authored opinion. No judge is involved, which is why this layer can
// gate at a modest task count where a judged completion score never could.
package agentbench

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// TaskRef identifies the benchmark task an execution was running.
type TaskRef struct {
	ID   string
	Name string
}

// Trace is one execution's recorded decisions, assembled from
// execution_tool_grants (requested / accepted / refused / escalations) and
// tool_audit_log (invoked). Both tables already exist and are written on the
// executor path, so nothing here needs new instrumentation.
type Trace struct {
	ExecutionID string
	StepID      string
	Role        string

	// Requested is what the lead asked for; Accepted what the ceiling allowed;
	// Refused what it denied. A refused request is recorded because a rejected
	// privilege request is exactly what an audit trail is for.
	Requested []string
	Accepted  []string
	Refused   []string

	// Escalations counts mid-execution grant escalations.
	Escalations int

	// Invoked is what the agent actually called.
	Invoked []string

	// Stalled reports an execution that failed with a tool-unavailable
	// signature — under-granting, terminally.
	Stalled bool

	// Outcomes are the execution's per-step results from
	// execution_step_outcomes, which the schema probe scores.
	Outcomes []StepOutcome

	// Calls are the individual tool invocations from tool_audit_log, which the
	// tool-use probe scores. Invoked above is the deduplicated name set; this
	// keeps each call with its result.
	Calls []ToolCall

	// ToolBudget is the step's EffectiveToolBudget and ToolCallsUsed what it
	// actually spent, both already recorded on execution_step_outcomes.
	ToolBudget    int
	ToolCallsUsed int
}

// Gold is the recorded ground truth for one task.
//
// Paths holds the per-run invoked set from each PASSING run of the
// unrestricted-ceiling arm. It is a disjunction, not a set: a task with two
// valid solution routes has two paths, and a grant is adequate when it covers
// at least one of them (§3.2). Earlier drafts modelled this as a single set —
// first a union, then an intersection — and both were wrong in opposite
// directions.
type Gold struct {
	TaskID string     `json:"taskId"`
	Paths  [][]string `json:"paths"`

	// Excluded marks a task the unrestricted arm could not pass. No ground
	// truth exists for it, so it leaves the benchmark set rather than being
	// scored — keeping it would measure the model's ceiling and report the
	// result as a policy finding.
	Excluded       bool   `json:"excluded,omitempty"`
	ExcludedReason string `json:"excludedReason,omitempty"`
}

// Core is the set of tools every observed path needed. Missing one is a hard
// failure rather than a fractional loss, because no demonstrated route exists
// without it.
func (g Gold) Core() []string {
	if len(g.Paths) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, path := range g.Paths {
		for _, tool := range normaliseTools(path) {
			counts[tool]++
		}
	}
	var core []string
	for tool, n := range counts {
		if n == len(g.Paths) {
			core = append(core, tool)
		}
	}
	sort.Strings(core)
	return core
}

// Union is every tool any observed path used. It is the denominator for
// request precision: a lead asking for something no demonstrated route ever
// needed is asking for too much.
func (g Gold) Union() []string {
	seen := map[string]bool{}
	for _, path := range g.Paths {
		for _, tool := range path {
			seen[tool] = true
		}
	}
	out := make([]string, 0, len(seen))
	for tool := range seen {
		out = append(out, tool)
	}
	sort.Strings(out)
	return out
}

// Verdict is a probe's output: a vector, never a score.
//
// The components trade against each other and the trade IS the finding, so
// nothing here is averaged into a single number. Two ratios carry a Defined
// flag because "no denominator" and "scored zero" are different facts and
// collapsing them drags averages toward a failure that did not happen.
type Verdict struct {
	Probe       string `json:"probe"`
	ExecutionID string `json:"executionId"`
	TaskID      string `json:"taskId"`

	// PathCoverage is maxᵢ count(Sᵢ ∩ accepted) / count(Sᵢ) — how completely the
	// grant covers the best-covered demonstrated route.
	PathCoverage float64 `json:"pathCoverage"`
	// CoreMiss reports a tool every path needed that was not granted.
	CoreMiss bool `json:"coreMiss"`
	// CoreSubstitutions records core tools met by an EQUIVALENT granted tool
	// rather than by themselves, as core→substitute. A pass earned by a
	// substitute is a different fact from a pass earned by the tool itself, and
	// a metric that hides which one happened cannot be audited later.
	CoreSubstitutions map[string]string `json:"coreSubstitutions,omitempty"`
	// CoreMissing NAMES those tools. A boolean says the hard-fail class fired
	// but not what fired it, and a rollup may never re-query the ledger to find
	// out (§6.2: verdicts are journaled rather than referenced precisely so a
	// retention window cannot erase the evidence). Diagnosing the first five
	// core misses this benchmark ever produced meant reaching into
	// execution_tool_grants by hand while the traces happened to still exist —
	// which is exactly the situation the journal exists to prevent.
	CoreMissing []string `json:"coreMissing,omitempty"`

	GrantPrecision        float64 `json:"grantPrecision"`
	GrantPrecisionDefined bool    `json:"grantPrecisionDefined"`

	// RequestPrecision is a DIAGNOSTIC, never an optimisation target: it
	// improves when the lead asks for less, so it is read only against
	// Escalations and Stalled, and §4 excludes it from the efficiency rollup.
	RequestPrecision        float64 `json:"requestPrecision"`
	RequestPrecisionDefined bool    `json:"requestPrecisionDefined"`

	Escalations        int     `json:"escalations"`
	RefusalRate        float64 `json:"refusalRate"`
	RefusalRateDefined bool    `json:"refusalRateDefined"`
	Stalled            bool    `json:"stalled"`

	// Schema and ToolUse carry the other probes' full vectors. A probe's
	// verdict is a vector by design, so the shared shape holds each one whole
	// rather than flattening it into fields that would collide.
	Schema  *SchemaVerdict  `json:"schema,omitempty"`
	ToolUse *ToolUseVerdict `json:"toolUse,omitempty"`
}

// Probe scores one execution's decisions against a recorded ground truth.
//
// One implementation ships (GrantProbe). Confidence-based retrieval routing and
// the testing.passed claim check fit the same shape and are named in the design
// as unbuilt siblings — the interface costs one type, and it is validated by the
// second probe that lands rather than by argument.
type Probe interface {
	Name() string
	Score(ctx context.Context, task TaskRef, gold Gold, trace Trace) (Verdict, error)
}

// grantProbeName identifies the tool-grant probe in a journal and in a rollup.
// A constant rather than a call, so a verdict can be matched without
// constructing the probe.
const grantProbeName = "tool-grant"

// GrantProbe scores the lead's per-execution tool-grant decisions.
type GrantProbe struct{}

// Name implements Probe.
func (GrantProbe) Name() string { return grantProbeName }

// Score implements Probe.
func (p GrantProbe) Score(_ context.Context, task TaskRef, gold Gold, trace Trace) (Verdict, error) {
	if gold.Excluded {
		return Verdict{}, fmt.Errorf("task %q is excluded from gold (%s): scoring it would "+
			"measure the model's ceiling and report it as a policy result",
			task.ID, gold.ExcludedReason)
	}
	if len(gold.Paths) == 0 {
		return Verdict{}, fmt.Errorf("task %q has no recorded paths: an empty ground truth "+
			"cannot be satisfied, and treating it as satisfied would pass every policy", task.ID)
	}
	// A trace with NO grant activity is not under-granting — it is not using the
	// feature. Scoring it 0.0 with a core miss drags every real measurement to
	// zero, which is exactly what the first validated run produced: path coverage
	// 0.000 across six steps, five of which never called grant_step_tools.
	if len(trace.Requested) == 0 && len(trace.Accepted) == 0 {
		return Verdict{}, fmt.Errorf("execution %q made no grant request: there is no grant "+
			"decision to score", trace.ExecutionID)
	}

	accepted := set(normaliseTools(trace.Accepted))
	v := Verdict{
		Probe:       p.Name(),
		ExecutionID: trace.ExecutionID,
		TaskID:      task.ID,
		Escalations: trace.Escalations,
		Stalled:     trace.Stalled,
	}

	// Path coverage: the best-covered demonstrated route. Max, not min — the
	// question is whether the grant supports SOME route the agent has been shown
	// to take, not whether it provisions every route it might have taken.
	for _, path := range gold.Paths {
		tools := normaliseTools(path)
		if len(tools) == 0 {
			continue
		}
		hit := 0
		for _, tool := range tools {
			if accepted[tool] {
				hit++
			}
		}
		if c := float64(hit) / float64(len(tools)); c > v.PathCoverage {
			v.PathCoverage = c
		}
	}

	// No early break: the SECOND missing core tool is as diagnostic as the
	// first, and stopping at one would have hidden whether the five misses in
	// the first baseline were five problems or one repeated.
	scoreCore(&v, gold.Core(), accepted)

	if invoked := normaliseTools(trace.Invoked); len(trace.Accepted) > 0 {
		acceptedNames := normaliseTools(trace.Accepted)
		used := 0
		for _, tool := range acceptedNames {
			if contains(invoked, tool) {
				used++
			}
		}
		v.GrantPrecision = float64(used) / float64(len(acceptedNames))
		v.GrantPrecisionDefined = true
	}

	if requested := normaliseTools(trace.Requested); len(requested) > 0 {
		union := normaliseTools(gold.Union())
		wanted := 0
		for _, tool := range requested {
			if contains(union, tool) {
				wanted++
			}
		}
		v.RequestPrecision = float64(wanted) / float64(len(requested))
		v.RequestPrecisionDefined = true

		v.RefusalRate = float64(len(normaliseTools(trace.Refused))) / float64(len(requested))
		v.RefusalRateDefined = true
	}

	return v, nil
}

// normaliseTool reduces a tool name to the bare form both sides can be compared
// in, mirroring the daemon's own ceiling check (mcpRoleToolAllowed).
//
// WHY THE METRIC NEEDS IT. Gold records what was INVOKED, which tool_audit_log
// stores bare ("git_status"). Grants record what the model ASKED FOR, which is
// whatever spelling it used ("functions.git_status"). Comparing those literally
// under-reports coverage: the first validated execution had 4 of 5 gold tools
// granted and scored 2 of 5, because two were spelled with the function-namespace
// prefix. A metric that depends on how a model spells a name is not measuring the
// grant decision.
func normaliseTool(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func normaliseTools(items []string) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, normaliseTool(i))
	}
	return dedupe(out)
}

func set(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, i := range items {
		out[i] = true
	}
	return out
}

func dedupe(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, i := range items {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// scoreCore records, for every tool in the core, whether the grant covers it —
// by itself, by an equivalent, or not at all.
//
// Extracted from Score so the substitution rule can be read on its own: it is
// the half of the verdict most likely to be argued with, and the first
// baseline's five core misses were all this rule misfiring.
func scoreCore(v *Verdict, core []string, accepted map[string]bool) {
	for _, tool := range core {
		via, ok := coreSatisfiedBy(tool, accepted)
		if !ok {
			// No early exit: the SECOND missing core tool is as diagnostic as
			// the first, and stopping at one hides whether several misses are
			// several problems or one repeated.
			v.CoreMiss = true
			v.CoreMissing = append(v.CoreMissing, tool)
			continue
		}
		if via == tool {
			continue
		}
		if v.CoreSubstitutions == nil {
			v.CoreSubstitutions = map[string]string{}
		}
		v.CoreSubstitutions[tool] = via
	}
}
