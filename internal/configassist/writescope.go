package configassist

import (
	"sort"
	"strings"
)

// sharedPrefixes are the parts of the deployed tree that belong to the
// DEPLOYMENT rather than to any one project: every project may ground on
// them, and an edit to one reaches every project that does.
//
// This list is the single definition of "shared" for three decisions that
// previously each had their own idea: what a project-scoped request may
// write (GateWriteScope), what a proposal's announced blast radius is
// (blastRadius) and what belongs in a proposal's read set.
var sharedPrefixes = []string{
	"swarms/",
	"workflows/",
	"role-library/",
	"project-templates/",
}

// sharedFiles are shared paths that are files rather than directories.
var sharedFiles = []string{"pricing.yaml"}

// IsSharedConfigPath reports whether rel names deployment-wide config. It
// accepts the path with or without the apply prefix, because the same
// question is asked on both sides of applyPath.
func IsSharedConfigPath(rel string) bool {
	p := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(rel, "./"), "configs/"))
	for _, f := range sharedFiles {
		if p == f {
			return true
		}
	}
	for _, pre := range sharedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// GateWriteScope is the server's own write allowlist (audit 2026-09-15
// CA-08).
//
// The snapshot authorizer decides what a caller may READ, and it
// deliberately admits shared context — pricing, the role library, project
// templates — so a project-scoped request can ground on them. Nothing
// enforced that "read-only" on the way out: the only thing standing between
// the model and a shared file was GateDeclaredScope, which checks the ops
// against the model's OWN declared PLAN. A plan that declares a shared file
// authorises itself.
//
// writable is supplied by the engine from the project's own grounding, never
// from anything the model said.
func GateWriteScope(writable func(rel string) bool, ops []Op) *Refusal {
	if writable == nil {
		return nil
	}
	var out []string
	for _, op := range ops {
		if writable(op.Path) {
			continue
		}
		reason := op.Path + " is readable context for this request, not writable by it"
		if IsSharedConfigPath(op.Path) {
			reason = op.Path + " is shared deployment config: readable by this project, writable only by a deployment-scoped change"
		}
		out = append(out, reason)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return &Refusal{
		Code:     RefuseDiffScope,
		Message:  "the edit touched files outside this request's write scope",
		Findings: out,
	}
}
