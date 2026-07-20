package dispatcher

// Regression tests for Task 8 (scheduled-task-notifications plan):
// set_reminder gaining kind/cron/project + a per-operator task cap.
// Each test below failed before the kind/cron/project branch existed
// in tool_set_reminder.go (setReminderArgs had no such fields, so the
// JSON simply didn't parse into anything meaningful and the tool ran
// the text/fire_at path, producing the wrong error or a text-kind
// insert instead of a task-kind one).

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestSetReminderTaskKindRequiresProject: a task-kind scheduled update
// needs somewhere to run. No active project + no explicit project arg
// must be refused with a clear "need a project" message, not silently
// defaulted or inserted with an empty ProjectID.
func TestSetReminderTaskKindRequiresProject(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest"}`,
		42, "" /* no active project */, nil)
	if !strings.Contains(res.Content, "project") {
		t.Fatalf("expected project-required error, got: %s", res.Content)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo should not have accepted the insert; got %d rows", len(repo.rows))
	}
}

// TestSetReminderTaskKindValidatesCron: a malformed cron expression on
// a task-kind reminder must be rejected before insert, with an error
// that names "cron" so the LLM can retry with a corrected expression.
func TestSetReminderTaskKindValidatesCron(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"not a cron","content":"digest"}`,
		42, "news", nil)
	if !strings.Contains(strings.ToLower(res.Content), "cron") {
		t.Fatalf("expected cron validation error, got: %s", res.Content)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo should not have accepted the insert; got %d rows", len(repo.rows))
	}
}

// TestSetReminderTaskKindInsertsRecurring: the happy path — a valid
// cron + project produces a task-kind, recurring row (non-empty
// CronExpr) in the right project, and a success confirmation.
func TestSetReminderTaskKindInsertsRecurring(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"Daily news digest"}`,
		42, "news", nil)
	if !strings.Contains(res.Content, "scheduled") && !strings.Contains(res.Content, "set") {
		t.Fatalf("expected success confirmation, got: %s", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 inserted row, got %d", len(repo.rows))
	}
	r := repo.rows[0]
	if r.Kind != persistence.ReminderKindTask {
		t.Errorf("kind = %q, want %q", r.Kind, persistence.ReminderKindTask)
	}
	if r.CronExpr != "0 7 * * *" {
		t.Errorf("cron_expr = %q, want %q", r.CronExpr, "0 7 * * *")
	}
	if !r.IsRecurring() {
		t.Errorf("expected IsRecurring() true for a cron row")
	}
	if r.ProjectID != "news" {
		t.Errorf("project_id = %q, want %q", r.ProjectID, "news")
	}
	if r.Content != "Daily news digest" {
		t.Errorf("content = %q", r.Content)
	}
}

// TestSetReminderTaskKindUsesActiveProjectFallback: an explicit
// `project` arg is not required if the session already has an active
// project — the tool should default to it rather than erroring.
func TestSetReminderTaskKindUsesActiveProjectFallback(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest"}`,
		42, "assistant", nil)
	if strings.Contains(res.Content, "needs a") {
		t.Fatalf("should not require an explicit project when one is active; got: %s", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 inserted row, got %d", len(repo.rows))
	}
	if repo.rows[0].ProjectID != "assistant" {
		t.Errorf("project_id = %q, want active project %q", repo.rows[0].ProjectID, "assistant")
	}
}

// TestSetReminderTaskKindEnforcesTaskCap: once an operator already has
// reminderMaxTaskPerOperator task-kind reminders, a further one is
// refused (distinct from — and additional to — the general pending
// cap) so a mis-fired cron can't uncontrollably consume executor
// slots.
func TestSetReminderTaskKindEnforcesTaskCap(t *testing.T) {
	repo := &stubReminderRepo{taskCount: defaultReminderMaxTaskPerOperator}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest"}`,
		42, "news", nil)
	if !strings.Contains(res.Content, "cap") {
		t.Fatalf("expected task-cap rejection, got: %s", res.Content)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo should not have accepted the insert; got %d rows", len(repo.rows))
	}
}

// TestSetReminderTaskCapEnvOverride: VORNIK_REMINDERS_MAX_TASK_PER_OPERATOR
// tightens (or loosens) the task-kind cap without a code change (backlog:
// "wire the task caps to env vars"). At the env cap the next request is
// refused; one below it is accepted.
func TestSetReminderTaskCapEnvOverride(t *testing.T) {
	t.Setenv(maxTaskPerOperatorEnvVar, "2")

	// At the env cap → rejected.
	repoAt := &stubReminderRepo{taskCount: 2}
	teAt := newSetReminderExecutor(repoAt, nil)
	resAt := teAt.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest"}`, 42, "news", nil)
	if !strings.Contains(resAt.Content, "cap=2") {
		t.Fatalf("expected rejection citing env cap=2, got: %s", resAt.Content)
	}

	// One below the env cap → accepted (would have been allowed under the
	// default 20 too, but this proves the env value is the one in force).
	repoUnder := &stubReminderRepo{taskCount: 1}
	teUnder := newSetReminderExecutor(repoUnder, nil)
	resUnder := teUnder.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest"}`, 42, "news", nil)
	if strings.Contains(resUnder.Content, "cap") {
		t.Fatalf("expected acceptance under env cap, got rejection: %s", resUnder.Content)
	}
}

// TestResolveMaxTaskPerOperator_InvalidFallsBack: a non-positive or
// unparseable override falls back to the default rather than disabling
// the cap.
func TestResolveMaxTaskPerOperator_InvalidFallsBack(t *testing.T) {
	for _, bad := range []string{"", "0", "-3", "lots"} {
		t.Setenv(maxTaskPerOperatorEnvVar, bad)
		if got := resolveMaxTaskPerOperator(); got != defaultReminderMaxTaskPerOperator {
			t.Errorf("resolveMaxTaskPerOperator(%q) = %d, want default %d", bad, got, defaultReminderMaxTaskPerOperator)
		}
	}
}

// TestSetReminderTextKindUnaffectedByTaskArgs: kind defaults to "text"
// when omitted, and the pre-existing fire_at/fire_in_seconds behavior
// (including its exact success-string shape) is unchanged — Task 8
// must not regress the shipped text-reminder path.
func TestSetReminderTextKindUnaffectedByTaskArgs(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"fire_in_seconds": 60, "content": "x"}`,
		1, "p", nil)
	if !strings.Contains(res.Content, "Reminder rem_") {
		t.Errorf("expected unchanged text-kind confirmation; got %q", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("rows: %d", len(repo.rows))
	}
	if repo.rows[0].Kind != persistence.ReminderKindText && repo.rows[0].Kind != "" {
		t.Errorf("kind = %q, want text or empty default", repo.rows[0].Kind)
	}
}

// TestSetReminderTaskKindRefusesDisallowedProject is the regression
// test for the final-review CRITICAL finding: set_reminder kind=task
// let a session schedule a recurring task in a project outside its
// `allowedProjects` ACL — every sibling task tool (create_task,
// list_tasks, ...) enforces this via resolveProjectAllowed, but
// set_reminder never received allowedProjects at all. An explicit
// `project` arg naming a project not in the session's allowlist must
// be refused with the same "not permitted for this session" wording
// resolveProjectAllowed produces elsewhere, and — critically — must
// NOT insert a row.
func TestSetReminderTaskKindRefusesDisallowedProject(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest","project":"secret-project"}`,
		42, "news", []string{"news", "assistant"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Fatalf("expected ACL-refusal message, got: %s", res.Content)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo should not have accepted the insert for a disallowed project; got %d rows", len(repo.rows))
	}
}

// TestSetReminderTaskKindRefusesDisallowedActiveProject covers the
// same gap via the active-project fallback (no explicit `project`
// arg) — a session whose active project isn't in its own allowlist
// (a misconfiguration, or a stale active project after an allowlist
// tightened) must still be refused rather than silently scheduling
// into it.
func TestSetReminderTaskKindRefusesDisallowedActiveProject(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest"}`,
		42, "news", []string{"assistant"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Fatalf("expected ACL-refusal message, got: %s", res.Content)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo should not have accepted the insert for a disallowed active project; got %d rows", len(repo.rows))
	}
}

// TestSetReminderTaskKindAllowsPermittedProject: the positive case —
// an explicit `project` that IS in allowedProjects succeeds exactly
// as before the ACL gate was added.
func TestSetReminderTaskKindAllowsPermittedProject(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest","project":"news"}`,
		42, "news", []string{"news", "assistant"})
	if strings.Contains(res.Content, "not permitted") {
		t.Fatalf("expected success for an allowed project, got: %s", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 inserted row, got %d", len(repo.rows))
	}
	if repo.rows[0].ProjectID != "news" {
		t.Errorf("project_id = %q, want %q", repo.rows[0].ProjectID, "news")
	}
}

// TestSetReminderTaskKindAllowsEmptyAllowlist: an empty/nil
// allowedProjects means "no restriction" (matches projectAllowed's
// documented semantics, preserving backward compatibility for the
// dev-mode single-operator path) — task-kind scheduling must still
// work when the session has no ACL configured at all.
func TestSetReminderTaskKindAllowsEmptyAllowlist(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest","project":"anything"}`,
		42, "news", nil)
	if strings.Contains(res.Content, "not permitted") {
		t.Fatalf("expected success with an empty allowlist, got: %s", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 inserted row, got %d", len(repo.rows))
	}
}

// TestSetReminderTaskKindAllowsWildcardAllowlist: a "*" entry in
// allowedProjects is the documented all-access wildcard — verifies
// the new gate honors it identically to every sibling task tool.
func TestSetReminderTaskKindAllowsWildcardAllowlist(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(context.Background(),
		`{"kind":"task","cron":"0 7 * * *","content":"digest","project":"anything"}`,
		42, "news", []string{"*"})
	if strings.Contains(res.Content, "not permitted") {
		t.Fatalf("expected success with a wildcard allowlist, got: %s", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 inserted row, got %d", len(repo.rows))
	}
}
