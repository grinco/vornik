package postgres

import (
	"strings"
	"testing"
)

// TestRenewLeaseSQL_HasLoadBearingGuards locks the lease-renewal
// SQL's load-bearing predicates in place. Removing or weakening
// any of these guards has, historically, surfaced as silent lease
// loss bugs that take time-on-the-clock to track down — most
// recently the chat-proxy QueueHooks regression on 2026-05-10
// (T-6f55) where a `task.status = QUEUED` flip mid-execution
// orphaned the lease and forced terminal FAILED after 3 renewal
// strikes. See https://docs.vornik.io §4.6 for the
// contract; this test is its enforcement.
//
// If you NEED to change the SQL — fine, but read the LLD first
// and update both. A failing test here means a code path can now
// renew a lease against a task that's QUEUED (in scheduler
// territory), CANCELLED (terminal), or held by a different
// lease_id (race with another claimer). All three are bugs.
func TestRenewLeaseSQL_HasLoadBearingGuards(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		// task_id match — anchors the row.
		{"id_match", "id = $1"},
		// lease_id match — refuses to touch rows that have been
		// re-leased to a different executor since this caller
		// claimed the lease.
		{"lease_id_match", "lease_id = $2"},
		// status guard — refuses to renew a lease when the task
		// has been moved out of the active set. This is the
		// guard the chat-proxy bug violated.
		{"status_active_set", "status IN ('LEASED', 'RUNNING', 'WAITING_FOR_CHILDREN')"},
		// Extension expressed via interval string (NOT a hard-
		// coded duration). Keeps lease windows tunable per call.
		{"extend_seconds_param", "$3 || ' seconds'"},
		// Updated_at bump on every renewal so observability
		// queries can see "this task was alive recently".
		{"updates_updated_at", "updated_at = NOW()"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(renewLeaseSQL, c.want) {
				t.Errorf("renewLeaseSQL missing required clause %q\nactual SQL:\n%s",
					c.want, renewLeaseSQL)
			}
		})
	}
}

// TestRenewLeaseSQL_RejectsForbiddenPatterns guards against
// well-meaning rewrites that loosen the contract — e.g. dropping
// the lease_id check (so any caller can renew any task), or
// replacing the active-set status guard with a broader filter.
func TestRenewLeaseSQL_RejectsForbiddenPatterns(t *testing.T) {
	forbidden := []struct {
		name    string
		pattern string
		why     string
	}{
		{
			name:    "no_unconditional_id_only",
			pattern: "WHERE id = $1\n",
			why:     "WHERE id = $1 alone (no lease_id check) lets any caller renew any lease",
		},
		{
			name:    "no_terminal_status_in_renewal",
			pattern: "'COMPLETED'",
			why:     "renewing a COMPLETED task would resurrect it — terminal states must never appear in the renewal status guard",
		},
		{
			name:    "no_queued_in_renewal",
			pattern: "'QUEUED'",
			why:     "a QUEUED task is unowned by definition — renewing one would re-create the chat-proxy QueueHooks lease-loss bug",
		},
	}

	for _, f := range forbidden {
		t.Run(f.name, func(t *testing.T) {
			if strings.Contains(renewLeaseSQL, f.pattern) {
				t.Errorf("renewLeaseSQL must NOT contain %q: %s\nactual SQL:\n%s",
					f.pattern, f.why, renewLeaseSQL)
			}
		})
	}
}
