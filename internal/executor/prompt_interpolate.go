package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// errPromptRefUnresolved marks a ${outputs.<step>.<field>} reference in an
// agent step prompt that could not be resolved. Callers match it with
// errors.Is and fail the step; it is never substituted away.
var errPromptRefUnresolved = errors.New("prompt reference unresolved")

// interpolatePromptRefs resolves every ${outputs.<step>.<field>} reference in
// an agent step's prompt against the execution's per-step result mirror. It
// shares outputRefRegex, resolveStepField and scalarStringForm with the
// payload path (outputs_interpolate.go) — one resolver, two callers.
//
// It differs from interpolateOutputs in EXACTLY ONE respect, and deliberately:
// an unresolved reference is an ERROR here, where the payload path resolves it
// to "". A payload field is a structured value some envelope schema validates
// downstream, so "" surfaces as an obviously-broken value and is caught. A
// prompt has no downstream schema: an empty substitution would hand the model
// "the analyst pinned exactly these case ids: " — an empty contract it can
// satisfy by reporting nothing, scoring zero, and looking indistinguishable
// from the defect this exists to prevent. §4 of CLAUDE.md — a control that
// cannot tell "examined and clean" from "never examined" reports the first and
// means the second.
//
// Design: 2026-08-13-agent-quality-benchmark-design.md, amendment 2026-09-18,
// D1 and D2.
func interpolatePromptRefs(prompt string, stepResults map[string]json.RawMessage) (string, error) {
	matches := outputRefRegex.FindAllStringSubmatchIndex(prompt, -1)
	if len(matches) == 0 {
		// The overwhelming majority of step prompts carry no reference. They
		// pass through byte-identical and can never fail for this reason.
		return prompt, nil
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(prompt[last:m[0]])
		step := prompt[m[2]:m[3]]
		field := prompt[m[4]:m[5]]
		val := resolveStepField(stepResults, step, field)
		if val == nil {
			return "", fmt.Errorf(
				"%w: ${outputs.%s.%s} — step %q has no readable %q in this execution's step results",
				errPromptRefUnresolved, step, field, step, field)
		}
		b.WriteString(scalarStringForm(val))
		last = m[1]
	}
	b.WriteString(prompt[last:])
	return b.String(), nil
}
