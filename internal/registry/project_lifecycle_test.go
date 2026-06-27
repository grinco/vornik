package registry

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestProjectLifecycle_YAMLRoundTrip pins the YAML field names
// the archive UI writes against — operators (and the archive
// sweeper) read lifecycle.status / archivedAt /
// scheduledDeleteAt, so a silent rename here would break the
// archive flow.
func TestProjectLifecycle_YAMLRoundTrip(t *testing.T) {
	in := []byte(`projectId: foo
displayName: Foo
swarmId: foo-swarm
defaultWorkflowId: foo-wf
lifecycle:
  status: archived
  archivedAt: 2026-05-23T12:00:00Z
  scheduledDeleteAt: 2026-05-30T12:00:00Z
  reason: end of contract
  archivedBy: ops@example.com
`)
	var p Project
	if err := yaml.Unmarshal(in, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.IsArchived() {
		t.Errorf("IsArchived = false, want true")
	}
	if p.Lifecycle.Reason != "end of contract" {
		t.Errorf("Reason = %q", p.Lifecycle.Reason)
	}
	if p.Lifecycle.ArchivedBy != "ops@example.com" {
		t.Errorf("ArchivedBy = %q", p.Lifecycle.ArchivedBy)
	}
	at, ok := p.ScheduledDeletion()
	if !ok {
		t.Fatalf("ScheduledDeletion returned !ok")
	}
	want := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	if !at.Equal(want) {
		t.Errorf("ScheduledDeletion = %v, want %v", at, want)
	}
}

// TestProjectLifecycle_ActiveByDefault confirms a project with no
// lifecycle block is treated as active — the most-common shape on
// disk for every pre-archive project.
func TestProjectLifecycle_ActiveByDefault(t *testing.T) {
	in := []byte(`projectId: bar
displayName: Bar
swarmId: bar-swarm
defaultWorkflowId: bar-wf
`)
	var p Project
	if err := yaml.Unmarshal(in, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.IsArchived() {
		t.Errorf("IsArchived = true on a project with no lifecycle block")
	}
	if _, ok := p.ScheduledDeletion(); ok {
		t.Errorf("ScheduledDeletion returned ok for an active project")
	}
}

// TestProjectLifecycle_DeletionDue covers the time-comparison
// helper the sweeper consults each tick.
func TestProjectLifecycle_DeletionDue(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name string
		p    Project
		now  time.Time
		want bool
	}{
		{"active project, no lifecycle", Project{ID: "a"}, time.Now(), false},
		{"archived but future-dated", Project{ID: "a", Lifecycle: ProjectLifecycle{Status: "archived", ScheduledDeleteAt: future}}, time.Now(), false},
		{"archived and past-dated", Project{ID: "a", Lifecycle: ProjectLifecycle{Status: "archived", ScheduledDeleteAt: past}}, time.Now(), true},
		{"archived but missing schedule", Project{ID: "a", Lifecycle: ProjectLifecycle{Status: "archived"}}, time.Now(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.DeletionDue(tc.now); got != tc.want {
				t.Errorf("DeletionDue = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestListActiveProjects_FiltersArchived covers the registry helpers
// the scheduler and task-create surfaces use to skip archived rows.
// Operates directly on the in-memory map so the test doesn't depend
// on the YAML/MD loader's swarm-frontmatter contract — the lifecycle
// filter is what matters here.
func TestListActiveProjects_FiltersArchived(t *testing.T) {
	r := New()
	r.active.projects = map[string]*Project{
		"active": {ID: "active"},
		"archived": {ID: "archived", Lifecycle: ProjectLifecycle{
			Status:            "archived",
			ArchivedAt:        "2026-05-23T12:00:00Z",
			ScheduledDeleteAt: "2026-05-30T12:00:00Z",
		}},
		"also-active": {ID: "also-active"},
	}
	r.projects = r.active.projects

	all := r.ListProjects()
	if len(all) != 3 {
		t.Fatalf("ListProjects: want 3, got %d", len(all))
	}
	active := r.ListActiveProjects()
	if got := projIDs(active); !equalStringSlice(got, []string{"active", "also-active"}) {
		t.Fatalf("ListActiveProjects: want [active also-active], got %v", got)
	}
	archived := r.ListArchivedProjects()
	if got := projIDs(archived); !equalStringSlice(got, []string{"archived"}) {
		t.Fatalf("ListArchivedProjects: want [archived], got %v", got)
	}
}

func projIDs(ps []*Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p == nil {
			continue
		}
		out = append(out, p.ID)
	}
	return out
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
