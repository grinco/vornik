package executor

import (
	"errors"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// A job that names no pull request or issue is a different finding from a
// target that is gone. Both are permanent — neither comes good by retrying —
// but the remedies point at different people: FORGE_TARGET_UNAVAILABLE sends
// the operator to the repository, the App installation and its permissions;
// a job with no target is the WORKFLOW or its trigger, and nothing on the
// forge side is wrong. review-20260909-f95b (companion, on the §18 fix) found
// the first class doing double duty; headmatch
// task_20260909150605_13cc14a3d079dc09 is the incident.
//
// The discriminator is the type's Status: 0 means permanence was decided from
// the payload, no request was made (permanent-failure design D1, amended
// 2026-09-09).
func TestClassifyExecutionFailure_PayloadDecidedPermanenceHasItsOwnClass(t *testing.T) {
	noTarget := &forge.PermanentError{Op: "forge.fetch_diff", Detail: "forge job must be either issue-driven (repo + number>0) or backlog-origin (repo + kind=backlog + slug)"}
	if got := ClassifyExecutionFailure(fmt.Errorf("system step failed: %w", noTarget), ""); got != persistence.TaskFailureClassForgeJobNoTarget {
		t.Errorf("payload-decided permanence classified as %q, want %q", got, persistence.TaskFailureClassForgeJobNoTarget)
	}
	gone := &forge.PermanentError{Op: "forge.fetch_diff", Status: 404, Detail: "Not Found", Err: errors.New("404")}
	if got := ClassifyExecutionFailure(gone, ""); got != persistence.TaskFailureClassForgeTargetUnavailable {
		t.Errorf("an HTTP-decided permanence must keep its class, got %q", got)
	}
}
