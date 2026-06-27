package registry

import "strings"

// gateConditionPaths extracts the LHS dotted paths from a gate
// condition string. The executor's evaluateGateCondition supports
// `<path> == <value>` and `&&`-joined sub-conditions; this helper
// mirrors that grammar in reverse, returning every path the
// condition would dereference at runtime.
//
// A condition like:
//
//	"review.approved == true && review.all_done == true"
//
// returns ["review.approved", "review.all_done"]. Sub-terms that
// don't parse as path-comparisons (empty, malformed) are skipped —
// the executor's evaluator already errors loudly on those at runtime,
// and surfacing them here would double-flag the same config bug.
//
// Used by stripInvalidProjects to refuse configs where a gate
// references a path the role's schema can't produce. Item 11 of
// https://docs.vornik.io
func gateConditionPaths(condition string) []string {
	if condition == "" {
		return nil
	}
	var out []string
	for _, sub := range strings.Split(condition, "&&") {
		sub = strings.TrimSpace(sub)
		if sub == "" {
			continue
		}
		eq := strings.SplitN(sub, "==", 2)
		if len(eq) != 2 {
			// Not a path-comparison shape; the gate evaluator will
			// fail at runtime. Don't double-report here.
			continue
		}
		path := strings.TrimSpace(eq[0])
		if path == "" {
			continue
		}
		out = append(out, path)
	}
	return out
}
