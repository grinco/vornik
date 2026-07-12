package forge

import (
	"errors"
	"fmt"
	"strings"
)

// PushRejectionKind classifies WHY a forge rejected a branch push, so the
// publish step can decide whether the change is un-pushable pending operator
// action (park + hand the operator a patch) rather than a transient failure to
// retry.
type PushRejectionKind int

const (
	// PushRejectionNone means the failure is not a remote rejection (e.g. a
	// network error or a local git problem) — retry semantics unchanged.
	PushRejectionNone PushRejectionKind = iota
	// PushRejectionPermission — the credential lacks a permission the push
	// needs. Operator-fixable: grant it and retry. Example: a GitHub App
	// without the `workflows` permission editing a file under
	// .github/workflows/, or without `contents:write` at all.
	PushRejectionPermission
	// PushRejectionProtected — a branch-protection / org policy structurally
	// forbids the push. Not fixable by a permission grant; the operator must
	// apply the change another way (hence the mail-in patch).
	PushRejectionProtected
	// PushRejectionOther — the remote rejected the push for a reason we don't
	// classify further (still a rejection: retrying as-is won't help).
	PushRejectionOther
)

func (k PushRejectionKind) String() string {
	switch k {
	case PushRejectionPermission:
		return "permission"
	case PushRejectionProtected:
		return "protected"
	case PushRejectionOther:
		return "other"
	default:
		return "none"
	}
}

// Remediation returns a short operator-facing hint for the rejection kind.
func (k PushRejectionKind) Remediation() string {
	switch k {
	case PushRejectionPermission:
		return "grant the forge credential the missing permission (e.g. the GitHub App's " +
			"`workflows` or `contents:write` permission — the org/repo owner must accept the " +
			"upgraded permission), then resume the task; or apply the attached patch manually."
	case PushRejectionProtected:
		return "the target branch is protected / blocked by org policy — apply the attached patch " +
			"manually (open the change from a branch the policy allows), then close the task."
	default:
		return "the remote rejected the push — inspect the rejection detail, then apply the attached " +
			"patch manually or resume once the cause is cleared."
	}
}

// PushRejectedError is returned by ForgeProvider.PushBranch when the remote
// rejected the push (as opposed to a transient/local failure). It carries the
// classification and the token-free git output so the publish step can park the
// task and hand the operator a submittable patch instead of failing.
type PushRejectedError struct {
	Branch string
	Kind   PushRejectionKind
	Output string // trimmed git stderr, token-free
	Err    error  // underlying error (e.g. *exec.ExitError)
}

func (e *PushRejectedError) Error() string {
	return fmt.Sprintf("push rejected (%s) for branch %s: %s", e.Kind, e.Branch, e.Output)
}

func (e *PushRejectedError) Unwrap() error { return e.Err }

// AsPushRejected reports whether err is (or wraps) a *PushRejectedError.
func AsPushRejected(err error) (*PushRejectedError, bool) {
	var pre *PushRejectedError
	if errors.As(err, &pre) {
		return pre, true
	}
	return nil, false
}

// ClassifyPushOutput inspects git's combined push output and reports whether it
// is a remote rejection and of what kind. Returns PushRejectionNone when the
// output does not look like a remote rejection (caller keeps the plain error).
func ClassifyPushOutput(output string) PushRejectionKind {
	lo := strings.ToLower(output)
	// Permission-class: GitHub App missing a scoped permission.
	if strings.Contains(lo, "without `workflows` permission") ||
		strings.Contains(lo, "without workflows permission") ||
		strings.Contains(lo, "refusing to allow a github app") ||
		strings.Contains(lo, "refusing to allow an oauth app") ||
		strings.Contains(lo, "resource not accessible by integration") ||
		strings.Contains(lo, "permission to") && strings.Contains(lo, "denied") {
		return PushRejectionPermission
	}
	// Protected-branch / org policy.
	if strings.Contains(lo, "protected branch") ||
		strings.Contains(lo, "gh006") ||
		strings.Contains(lo, "push declined due to") ||
		strings.Contains(lo, "cannot force-update") {
		return PushRejectionProtected
	}
	// Generic remote rejection (still not retry-fixable as-is).
	if strings.Contains(lo, "[remote rejected]") ||
		strings.Contains(lo, "remote rejected") ||
		strings.Contains(lo, "! [rejected]") {
		return PushRejectionOther
	}
	return PushRejectionNone
}
