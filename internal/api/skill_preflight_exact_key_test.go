package api

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Regression, 2026-08-20. An operator-approved skill (`rag-first`) was
// DESTROYED through the guard that exists to prevent duplicates.
//
// LLD 2026-07-07 §12.2 distinguishes two operations explicitly:
//
//   - `supersedes` — "this DIFFERENT skill replaces that one" (two names, one
//     survives). Writes a NEW row, retires the target, never overwrites in place.
//   - exact-key re-propose — "this is a NEW VERSION of the same skill" (one
//     name, one identity). Updates the row in place, archiving the prior body.
//
// The preflight did not honour the distinction. `findSkillDuplicates` skipped
// only `ex.ID == candidate.ID`, so a same project+scope+name proposal — a
// version bump, not a duplicate — scored ~1.0 and soft-blocked. Both offered
// dispositions are semantically wrong for that case, so answering the block
// meant asserting something untrue either way.
//
// Answering it with `supersedes: <the exact-key row's own id>` then combined
// two correct-in-isolation behaviours into a destructive one: `Upsert` updated
// the row in place (right, for an exact key), and the supersede step retired
// that same row (right, for a different skill). Net: no active row, no draft,
// and — because UNIQUE (project_id, repo_scope, name) still holds the name —
// no way to write a replacement. The name was unrecoverable through MCP.
//
// §0.1 already flagged this class as an open integrity loss: "any caller with
// SkillWrite can re-propose an existing name and destroy an operator-approved
// body."

func seedSkill(t *testing.T, s *Server, sk *persistence.Skill) {
	t.Helper()
	if err := s.skillStore.Create(context.Background(), sk); err != nil {
		t.Fatalf("seed %s: %v", sk.Name, err)
	}
}

// An exact-key re-propose is a version bump. It must NOT be soft-blocked as a
// near-duplicate: there is nothing to disambiguate, and neither disposition
// describes it truthfully.
func TestPreflight_exactKeyReproposeIsNotADuplicate(t *testing.T) {
	s := newSkillTestServer(t)
	key := skillKey("proj-a", true, true)
	seedSkill(t, s, &persistence.Skill{
		ID: "sk-same", ProjectID: "proj-a", RepoScope: "github.com/acme/mine",
		Name: "rag-first", Description: "recall the design before reading code",
		Body: "# RAG-first\n## Do\n- recall first", Maturity: persistence.SkillMaturityActive, Version: 1,
	})

	out := mustPropose(t, s, key, map[string]any{
		"name":        "rag-first",
		"description": "recall the design before reading code",
		"body":        "# RAG-first\n## Do\n- recall first\n## New section\n- more",
		"repo_scope":  "github.com/acme/mine",
	})

	if blocked, matches := preflightBlocked(t, out); blocked {
		t.Fatalf("an exact-key re-propose was blocked as a near-duplicate; §12.2 routes it "+
			"to the in-place version bump. matches=%+v", matches)
	}
}

// A RETIRED row on the SAME natural key must not block — that is the only path
// back for a name whose live row was retired, and UNIQUE (project, scope, name)
// means no replacement row can be written while it holds the key.
//
// Deliberately narrow. TestPreflightSurfacesRetiredResurrection asserts the
// opposite for a DIFFERENT name, and it is right: retiring is an operator
// decision, so re-authoring that knowledge under a new name must stay visible.
// The distinction is identity, not maturity — which is why the fix lives in
// sameSkillIdentity and not in a maturity filter. Excluding retired rows
// wholesale was the first attempt here and it broke that test, correctly.
func TestPreflight_retiredRowOnTheSameKeyDoesNotBlockItsOwnRestore(t *testing.T) {
	s := newSkillTestServer(t)
	seedSkill(t, s, &persistence.Skill{
		ID: "sk-retired-same", ProjectID: "proj-a", RepoScope: "github.com/acme/mine",
		Name: "rag-first", Description: "recall the design before reading code",
		Body: "# RAG-first\n## Do\n- recall first", Maturity: persistence.SkillMaturityRetired, Version: 2,
	})

	out := mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name":        "rag-first",
		"description": "recall the design before reading code",
		"body":        "# RAG-first\n## Do\n- recall first\n## Restored",
		"repo_scope":  "github.com/acme/mine",
	})

	if blocked, matches := preflightBlocked(t, out); blocked {
		t.Fatalf("a retired row blocked the restore of its OWN name; the name is then "+
			"permanently unwritable. matches=%+v", matches)
	}
}

// The destructive combination itself: `supersedes` naming a row whose natural
// key equals the candidate's is not a supersede at all. Refuse it, and say what
// to do instead, rather than performing an in-place update AND a retire.
func TestPropose_selfSupersedeIsRefused(t *testing.T) {
	s := newSkillTestServer(t)
	key := skillKey("proj-a", true, true)
	seedSkill(t, s, &persistence.Skill{
		ID: "sk-self", ProjectID: "proj-a", RepoScope: "github.com/acme/mine",
		Name: "rag-first", Description: "recall the design before reading code",
		Body: "# RAG-first\n## Do\n- recall first", Maturity: persistence.SkillMaturityActive, Version: 1,
	})

	_, err := s.companionToolSkillPropose(context.Background(), key, rawArgs(t, map[string]any{
		"name":        "rag-first",
		"description": "recall the design before reading code",
		"body":        "# RAG-first\n## Do\n- recall first\n## More",
		"repo_scope":  "github.com/acme/mine",
		"supersedes":  "sk-self",
	}))

	if err == nil {
		t.Fatal("a self-supersede was accepted; it updates the row in place and then retires it, " +
			"destroying the skill and leaving the name unwritable")
	}
	if !strings.Contains(err.Error(), "same skill") {
		t.Errorf("refusal does not explain that this is a version bump, not a supersede: %v", err)
	}

	// And the seeded skill must survive untouched.
	got, gerr := s.skillStore.GetByID(context.Background(), "sk-self")
	if gerr != nil {
		t.Fatalf("get after refused propose: %v", gerr)
	}
	if got.Maturity != persistence.SkillMaturityActive {
		t.Errorf("maturity = %q after a refused self-supersede, want active — the refusal must "+
			"not have side effects", got.Maturity)
	}
}
