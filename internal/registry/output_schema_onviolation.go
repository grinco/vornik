package registry

import "fmt"

// ViolationMode is what the validator does with a value outside a node's enum.
type ViolationMode string

const (
	// ViolationFail rejects the step: INVALID_OUTPUT, into the shape-retry
	// ladder. The default, because an enum that tolerates anything is the
	// control CLAUDE.md §4 forbids — it reports "constrained" and means
	// "whatever arrived".
	ViolationFail ViolationMode = "fail"
	// ViolationWarn logs and counts but does not fail the step, for an
	// advisory node whose out-of-range values a downstream fallback already
	// resolves. See OnViolation's doc for why analysis.complexity is one.
	ViolationWarn ViolationMode = "warn"
)

// ViolationMode resolves the node's declared mode, defaulting to
// ViolationFail. An unrecognised spelling also resolves to fail rather than
// to the permissive mode: ValidateOnViolation rejects it at load, and if it
// ever reached here anyway, the safe reading of a typo is the binding one.
func (s *OutputSchema) ViolationMode() ViolationMode {
	if s == nil {
		return ViolationFail
	}
	if ViolationMode(s.OnViolation) == ViolationWarn {
		return ViolationWarn
	}
	return ViolationFail
}

// ValidateOnViolation reports whether this node's onViolation declaration is
// usable. Called for every node at config load, so a typo is a refused config
// rather than a constraint that silently stopped applying.
func (s *OutputSchema) ValidateOnViolation() error {
	if s == nil || s.OnViolation == "" {
		return nil
	}
	switch ViolationMode(s.OnViolation) {
	case ViolationFail, ViolationWarn:
	default:
		return fmt.Errorf("onViolation: %q is not one of %q, %q",
			s.OnViolation, ViolationFail, ViolationWarn)
	}
	if len(s.Enum) == 0 {
		return fmt.Errorf("onViolation: %q declared on a node with no enum — it constrains nothing there",
			s.OnViolation)
	}
	return nil
}

// ValidateOnViolationTree walks the whole schema and reports the first
// unusable declaration, naming the path so an operator can find it.
func (s *OutputSchema) ValidateOnViolationTree(path string) error {
	if s == nil {
		return nil
	}
	if err := s.ValidateOnViolation(); err != nil {
		if path == "" {
			return err
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	for name, child := range s.Properties {
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		if err := child.ValidateOnViolationTree(childPath); err != nil {
			return err
		}
	}
	if s.Items != nil {
		itemPath := path + "[]"
		if err := s.Items.ValidateOnViolationTree(itemPath); err != nil {
			return err
		}
	}
	return nil
}
