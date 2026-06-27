package ui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestLoadAncestors_WalksToRoot exercises the parent-chain walker
// used by the detail page breadcrumb. The walker stops at the root
// and the returned slice runs root → parent.
func TestLoadAncestors_WalksToRoot(t *testing.T) {
	root := &persistence.Task{ID: "task_root"}
	mid := &persistence.Task{ID: "task_mid", ParentTaskID: stringPtr("task_root")}
	leaf := &persistence.Task{ID: "task_leaf", ParentTaskID: stringPtr("task_mid")}

	mockRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			switch id {
			case "task_root":
				return root, nil
			case "task_mid":
				return mid, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(mockRepo))
	got := srv.loadAncestors(context.Background(), leaf)
	if len(got) != 2 {
		t.Fatalf("want 2 ancestors, got %d (%+v)", len(got), got)
	}
	if got[0].ID != "task_root" || got[1].ID != "task_mid" {
		t.Errorf("expected root → mid ordering, got %s → %s", got[0].ID, got[1].ID)
	}
}

// TestLoadAncestors_RootHasNone confirms the no-parent case returns
// an empty slice rather than panicking on the nil dereference.
func TestLoadAncestors_RootHasNone(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	got := srv.loadAncestors(context.Background(), &persistence.Task{ID: "task_root"})
	if len(got) != 0 {
		t.Errorf("root should have no ancestors, got %+v", got)
	}
}

// TestLoadAncestors_CycleSafe ensures a malformed cycle in the data
// can't trap the walker into an unbounded fetch loop — the visited
// set + depth cap kick in.
func TestLoadAncestors_CycleSafe(t *testing.T) {
	a := &persistence.Task{ID: "task_a", ParentTaskID: stringPtr("task_b")}
	b := &persistence.Task{ID: "task_b", ParentTaskID: stringPtr("task_a")}
	mockRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			if id == "task_a" {
				return a, nil
			}
			return b, nil
		},
	}
	srv := NewServer(WithTaskRepository(mockRepo))
	// Either the visited check or the depth cap must fire — we just
	// need the call to return in finite time without exploding.
	got := srv.loadAncestors(context.Background(), a)
	if len(got) > 10 {
		t.Errorf("cycle should be bounded; got len=%d", len(got))
	}
}

// TestLoadAncestors_NilRepoSafe handles the rare case where the
// detail page renders without a task repo wired — returns nil
// rather than panicking on the nil deref.
func TestLoadAncestors_NilRepoSafe(t *testing.T) {
	srv := NewServer()
	if got := srv.loadAncestors(context.Background(), &persistence.Task{ID: "x"}); got != nil {
		t.Errorf("nil repo should return nil ancestors, got %+v", got)
	}
}

// TestTaskDetail_RendersChildrenAndBreadcrumb confirms the detail
// handler surfaces both the ancestor breadcrumb and the subtasks
// list in the rendered HTML.
func TestTaskDetail_RendersChildrenAndBreadcrumb(t *testing.T) {
	root := &persistence.Task{ID: "task_root", ProjectID: "p1", Status: persistence.TaskStatusCompleted}
	mid := &persistence.Task{ID: "task_mid", ProjectID: "p1", Status: persistence.TaskStatusRunning, ParentTaskID: stringPtr("task_root")}
	leafID := "task_leaf"
	leaf := &persistence.Task{ID: leafID, ProjectID: "p1", Status: persistence.TaskStatusQueued, ParentTaskID: stringPtr("task_mid")}
	subA := &persistence.Task{ID: "task_subA", ProjectID: "p1", Status: persistence.TaskStatusCompleted, ParentTaskID: stringPtr(leafID)}

	mockRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			switch id {
			case leafID:
				return leaf, nil
			case "task_mid":
				return mid, nil
			case "task_root":
				return root, nil
			}
			return nil, nil
		},
		GetChildrenFunc: func(ctx context.Context, parent string) ([]*persistence.Task, error) {
			if parent == leafID {
				return []*persistence.Task{subA}, nil
			}
			return nil, nil
		},
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			// Sibling-nav helper hits this; an empty slice is fine.
			return nil, nil
		},
	}

	srv := NewServer(WithTaskRepository(mockRepo))
	req := httptest.NewRequest("GET", "/tasks/"+leafID, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Breadcrumb shows the lineage label and both ancestor task IDs.
	if !strings.Contains(body, "Lineage") {
		t.Errorf("expected Lineage label in body")
	}
	if !strings.Contains(body, "task_root") || !strings.Contains(body, "task_mid") {
		t.Errorf("expected both ancestors in body")
	}
	// Subtasks panel shows the single child.
	if !strings.Contains(body, "Subtasks (1)") {
		t.Errorf("expected Subtasks (1) panel header")
	}
	if !strings.Contains(body, "task_subA") {
		t.Errorf("expected child task_subA in subtasks panel")
	}
}

// TestIsChangelogArtifact_LegacyAndDisambig covers both the
// pre-disambig name ("CHANGELOG.md") and the post-disambig form
// ("CHANGELOG-YYYYMMDD-XXXX.md"). Other variants — Quarter tags,
// trailing date, wrong shape — must NOT trip the inline-render
// path or operator-named CHANGELOG-2026-Q2.md files would render
// in the wrong slot.
func TestIsChangelogArtifact_LegacyAndDisambig(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Match — accepted.
		{"CHANGELOG.md", true},
		{"changelog.md", true},
		{"CHANGELOG-20260516-1a2b.md", true},
		{"changelog-20260516-1a2b.md", true},
		// No match — rejected.
		{"", false},
		{"NOTES.md", false},
		{"CHANGELOG", false},                   // missing .md
		{"CHANGELOG.txt", false},               // wrong ext
		{"CHANGELOG-2026-Q2.md", false},        // operator-named, wrong shape
		{"CHANGELOG-20260516-XYZW.md", false},  // non-hex id
		{"CHANGELOG-XXXXXXXX-1a2b.md", false},  // non-digit date
		{"CHANGELOG-1234-1a2b.md", false},      // date too short
		{"OLDCHANGELOG.md", false},             // wrong prefix
		{"CHANGELOG-20260516-1a2b3.md", false}, // id too long
	}
	for _, c := range cases {
		got := isChangelogArtifact(c.name)
		if got != c.want {
			t.Errorf("isChangelogArtifact(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
