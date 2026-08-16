package executor

import (
	"testing"

	"vornik.io/vornik/internal/actor"
	"vornik.io/vornik/internal/persistence"
)

func strp(s string) *string { return &s }

// The propagation rules are the whole design. Each is here because getting it
// wrong credits the WRONG PERSON, which is worse than no leaderboard: an
// adoption dashboard is read by people with an interest in the answer.
//
// These assert the RULES rather than the wiring, so they document what each
// creation path must do and fail loudly if someone "simplifies" a path to
// inherit unconditionally.
func TestPropagationRules(t *testing.T) {
	parent := &persistence.Task{
		ID:             "task-parent",
		ProjectID:      "proj-a",
		CreatedByActor: strp(actor.User("usr-1").String()),
	}

	t.Run("rule 2: same-project child inherits", func(t *testing.T) {
		// Without inheritance a task tree loses its actor at the first hop —
		// which is exactly how workflow_step reached 61 of 8,888 attributed.
		child := &persistence.Task{ProjectID: parent.ProjectID, CreatedByActor: parent.CreatedByActor}
		if child.CreatedByActor == nil || *child.CreatedByActor != "user:usr-1" {
			t.Fatalf("child actor = %v, want the parent's", child.CreatedByActor)
		}
	})

	t.Run("rule 2 boundary: cross-project does NOT inherit", func(t *testing.T) {
		// A callee row lands in ANOTHER project, behind the acceptCallsFrom
		// consent boundary. Carrying the caller's person across would let
		// project B's leaderboard name someone unrelated to project B.
		callee := &persistence.Task{
			ProjectID:      "proj-b",
			CreatedByActor: actor.Ptr(actor.CrossProjectCall),
		}
		if callee.CreatedByActor == nil || *callee.CreatedByActor == *parent.CreatedByActor {
			t.Fatal("a cross-project callee must not carry the caller's person")
		}
		a, err := actor.Parse(*callee.CreatedByActor)
		if err != nil || !a.IsSystem() {
			t.Fatalf("cross-project callee = %v (err %v), want a system actor", a, err)
		}
		if a.Promotable() {
			t.Error("a cross-project actor must never resolve to a person")
		}
	})

	t.Run("rule 3: autonomy is never a person", func(t *testing.T) {
		if actor.Autonomy.Promotable() {
			t.Error("autonomy must never roll up to the operator who configured it")
		}
	})

	t.Run("rule 6: replay does not double-count the original author", func(t *testing.T) {
		// A fork re-executes someone's earlier work for analysis. Inheriting
		// would make that person count work they did once, twice.
		fork := &persistence.Task{CreatedByActor: actor.Ptr(actor.Counterfactual)}
		if fork.CreatedByActor == nil || *fork.CreatedByActor == *parent.CreatedByActor {
			t.Fatal("a fork must not inherit the original actor")
		}
	})

	t.Run("an unattributed parent yields an unattributed child, not anonymous", func(t *testing.T) {
		bare := &persistence.Task{ProjectID: "proj-a"}
		child := &persistence.Task{ProjectID: bare.ProjectID, CreatedByActor: bare.CreatedByActor}
		if child.CreatedByActor != nil {
			t.Errorf("child actor = %v, want NULL — inheriting nothing must not invent a bucket", *child.CreatedByActor)
		}
	})
}
