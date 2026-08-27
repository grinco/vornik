package registry

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Step frontmatter is decoded STRICTLY: a key that matches no field is an
// error, not a silent drop.
//
// Ten shipped workflows carried a per-step `retry:` block whose four keys
// matched nothing. Lenient decoding meant the daemon, the validator and the
// operator all agreed the config was fine while it did nothing, and the
// comments above those blocks asserted they were tuned from production
// telemetry. A config that looks like a control and is discarded without a
// word is the defect this closes.
//
// Scoped to STEPS rather than the whole document, deliberately. The
// frontmatter also carries the SKILL.md envelope and keys owned by other
// subsystems; rejecting everything unrecognised at document level would fail
// files that are fine. Steps are where the demonstrated harm is.

// stepFieldNames is the set of yaml keys WorkflowStep actually models, derived
// from the struct tags so it cannot drift from the type it describes.
var stepFieldNames = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(WorkflowStep{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			out[name] = true
		}
	}
	return out
})

// UnmarshalYAML decodes a step and rejects keys it does not model.
//
// The alias type is what stops this recursing: decoding into a type without
// this method uses the default struct decoding.
func (s *WorkflowStep) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		known := stepFieldNames()
		var unknown []string
		// A mapping node's Content alternates key, value, key, value…
		for i := 0; i+1 < len(value.Content); i += 2 {
			if key := value.Content[i].Value; !known[key] {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("line %d: unknown step field(s) %s%s",
				value.Line, strings.Join(quoteAll(unknown), ", "), removedKeyHint(unknown))
		}
	}
	type stepAlias WorkflowStep
	var alias stepAlias
	if err := value.Decode(&alias); err != nil {
		return err
	}
	*s = WorkflowStep(alias)
	return nil
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// removedKeyHint turns "unknown field" into a fix for keys we deliberately
// removed.
//
// `retryPolicy` was removed rather than deprecated because it never reached
// the executor — there was no working behaviour to preserve. But an operator
// who had it set still deserves the replacement, not just the rejection: a key
// that never worked is exactly the one whose removal looks arbitrary.
func removedKeyHint(unknown []string) string {
	for _, k := range unknown {
		if k == "retryPolicy" {
			return "\n  hint: did you mean \"retry\"? retryPolicy was never read by the" +
				" executor and has been removed.\n" +
				"  replace with:  retry: { max_attempts: <n>, backoff: <s> }"
		}
	}
	return ""
}
