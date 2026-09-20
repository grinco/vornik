package registry

import (
	"fmt"
	"regexp"
	"strings"
)

// KeyRepair is one corrected key, for printing a diff an operator can review
// before it is written.
type KeyRepair struct {
	Line int    // 1-based line number in the original file
	From string // the key as it was written
	To   string // the key this binary knows
}

func (r KeyRepair) String() string {
	return fmt.Sprintf("line %d: %q → %q", r.Line, r.From, r.To)
}

// HealMisspelledKeys corrects keys whose ONLY defect is spelling — the narrow
// class §11.1 already separates, and the only one with an unambiguous correct
// end state (loader-validator agreement design §13.4).
//
// WHAT IT WILL NOT DO, and why each bound is load-bearing:
//
//   - A key unknown under EVERY spelling is left alone. That is a
//     deploy-ordering problem, and "fixing" it means deleting it — which
//     restores the project by silently disabling whatever the operator was
//     configuring, the thing §11 refused and still refuses.
//   - A key with no single suggestion is left alone. Ambiguous by definition,
//     and a guess here rewrites a customer's intent.
//   - Only the KEY is rewritten, at the position it is declared. A value that
//     happens to contain the misspelling is data, and rewriting it would
//     corrupt the file while claiming to repair it.
//   - Nothing else in the file moves. The repair is applied textually rather
//     than by re-marshalling, because a round-trip through a YAML library
//     reorders keys, drops comments and re-indents — producing a diff an
//     operator cannot review, for a change they did not ask for.
//
// It returns the healed bytes and the repairs made; callers write nothing when
// the repair list is empty.
func HealMisspelledKeys(data []byte, diag SkewDiagnosis) ([]byte, []KeyRepair, error) {
	if len(data) == 0 || len(diag.Suggestions) == 0 {
		return data, nil, nil
	}

	lines := strings.Split(string(data), "\n")
	var repairs []KeyRepair

	for _, wrong := range diag.UnknownKeys {
		right := diag.Suggestions[wrong]
		if right == "" || right == wrong {
			continue
		}
		// A key DECLARATION: optional indent, the key, optional space, a colon.
		// Anchored at the start so a value containing the same text is not a
		// match, and the key is quoted into the pattern so a name with regex
		// metacharacters cannot widen it.
		pat, err := regexp.Compile(`(?m)^(\s*)` + regexp.QuoteMeta(wrong) + `(\s*:)`)
		if err != nil {
			return data, nil, fmt.Errorf("heal: key %q is not matchable: %w", wrong, err)
		}
		for i, line := range lines {
			if !pat.MatchString(line) {
				continue
			}
			lines[i] = pat.ReplaceAllString(line, "${1}"+right+"${2}")
			repairs = append(repairs, KeyRepair{Line: i + 1, From: wrong, To: right})
		}
	}

	if len(repairs) == 0 {
		return data, nil, nil
	}
	return []byte(strings.Join(lines, "\n")), repairs, nil
}
