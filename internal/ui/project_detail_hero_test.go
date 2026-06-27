// Tests for the per-project homepage hero (2026.6.0 SaaS-readiness,
// F4 slice). Verifies the new hero block renders the
// human-readable description, the next-autonomy-tick badge, and the
// project-scoped recent activity strip. Each sub-block is
// independently conditional so older projects without these fields
// don't show a sad-empty card.

package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubAutonomyEvalRepo is the per-test stand-in for the
// AutonomyEvaluationRepository. ListFunc lets each test script
// the recent-eval lookup; the other methods are nil-safe stubs
// because ProjectDetail only hits List on the homepage hero path.
type stubAutonomyEvalRepo struct {
	ListFunc func(ctx context.Context, f persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error)
}

func (s *stubAutonomyEvalRepo) Record(context.Context, *persistence.AutonomyEvaluation) error {
	return nil
}
func (s *stubAutonomyEvalRepo) List(ctx context.Context, f persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	if s.ListFunc != nil {
		return s.ListFunc(ctx, f)
	}
	return nil, nil
}
func (s *stubAutonomyEvalRepo) CountByOutcome(context.Context, string, time.Time, time.Time) (map[string]int64, error) {
	return nil, nil
}

// TestProjectDetail_HeroRendersDescription pins the human-readable
// description surfacing path. Without this the per-project homepage
// is YAML-only and unwelcoming to non-technical users.
func TestProjectDetail_HeroRendersDescription(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:          "demo",
			DisplayName: "Demo",
			Description: "A friendly assistant for daily research tasks.",
			SwarmID:     "swarm-1",
		},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "A friendly assistant for daily research tasks.") {
		t.Errorf("hero must render Project.Description verbatim — got body without it")
	}
}

// TestProjectDetail_TaskOriginBadgesRender pins the post-cleanup
// task-table contract: every task row renders its CreationSource
// as a small colored pill so an operator can tell autonomy /
// manual / delegated apart at a glance. The standalone "Autonomy"
// panel was removed in favour of these per-row badges.
func TestProjectDetail_TaskOriginBadgesRender(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo", SwarmID: "swarm-1"},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
		Tasks: []*persistence.Task{
			{ID: "task_1", Status: persistence.TaskStatusCompleted, CreationSource: persistence.TaskCreationSourceUser},
			{ID: "task_2", Status: persistence.TaskStatusRunning, CreationSource: persistence.TaskCreationSourceAutonomous},
			{ID: "task_3", Status: persistence.TaskStatusQueued, CreationSource: persistence.TaskCreationSourceDelegation},
			{ID: "task_4", Status: persistence.TaskStatusPending, CreationSource: persistence.TaskCreationSourceRoute},
			{ID: "task_5", Status: persistence.TaskStatusFailed, CreationSource: persistence.TaskCreationSourceCheckpoint},
		},
		TaskTotal: 5,
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		">manual<",     // USER → manual pill
		">autonomy<",   // AUTONOMOUS → autonomy pill
		">delegated<",  // DELEGATION → delegated pill
		">routed<",     // ROUTE → routed pill
		">checkpoint<", // CHECKPOINT → checkpoint pill
	} {
		if !strings.Contains(out, want) {
			t.Errorf("task origin badge column missing %q from rendered output", want)
		}
	}
}

// TestProjectDetail_HeroRendersFullAutonomySummary pins the
// post-regression-fix layout: when autonomy is enabled, the hero
// MUST render the goal + poll interval / max tasks-per-hour /
// approval grid + next-tick countdown + last outcome — same
// information the 2026.5.6 Config + Autonomy panel surfaced.
// Burying any of these in <details> or removing them entirely
// is a regression the 2026-05-15 cleanup tripped over.
func TestProjectDetail_HeroRendersFullAutonomySummary(t *testing.T) {
	s := NewServer()
	future := time.Now().UTC().Add(2 * time.Minute)
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:      "demo",
			SwarmID: "swarm-1",
			Autonomy: registry.ProjectAutonomy{
				Enabled:         true,
				PollInterval:    "5m",
				MaxTasksPerHour: 6,
				RequireApproval: true,
				Goal:            "Watch the news every morning and summarise.",
			},
		},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
		NextAutonomyTickAt:   future,
		NextAutonomyTickETA:  "in 1m 50s",
		NextAutonomyTickISO:  future.Format(time.RFC3339),
		LastAutonomyOutcome:  "CREATED",
		LastAutonomyAgoLabel: "3m 24s ago",
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Autonomy enabled", // header pill
		"Goal",             // section label
		"Watch the news every morning and summarise.", // goal body — visible, not collapsed
		"Poll Interval",
		"Max Tasks/Hour",
		"Require Approval",
		"5m",
		"next tick",
		"in 1m 50s",
		"CREATED",
		"3m 24s ago",
		"Yes", // RequireApproval=true
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hero autonomy summary missing %q from rendered output", want)
		}
	}
	if !strings.Contains(out, `data-next-eval-at="`+future.Format(time.RFC3339)+`"`) {
		t.Errorf("expected data-next-eval-at attribute on the ETA span for the client-side ticker")
	}
	// The goal body MUST NOT be wrapped in <details>/<summary>
	// — that's how the regression cleanup hid the goal. Lock it
	// in so future refactors don't re-collapse it. Scoped to the
	// region around the goal text (the 2026.5.8 Stack section
	// elsewhere on the page legitimately uses <details>; this
	// check is about THIS panel, not the whole page).
	goalIdx := strings.Index(out, "Watch the news every morning and summarise.")
	if goalIdx < 0 {
		t.Fatal("goal text not found in render — earlier assertion should have caught this")
	}
	// Look in a 2000-char window around the goal for a <details>
	// or <summary> ancestor. Tight enough to catch a wrapping
	// collapsible while ignoring the Stack section a thousand
	// lines below.
	start := goalIdx - 1000
	if start < 0 {
		start = 0
	}
	end := goalIdx + 1000
	if end > len(out) {
		end = len(out)
	}
	window := out[start:end]
	if strings.Contains(window, "<summary") || strings.Contains(window, "<details") {
		t.Error("goal must render as a visible block, not behind a collapsed <details> — regression seen on 2026-05-15")
	}
}

// TestProjectDetail_HeroHidesAutonomyWhenDisabled — projects
// without autonomy enabled render the hero with just the
// description (if set) and no empty autonomy card. Disabled
// projects without a description render no hero at all.
func TestProjectDetail_HeroHidesAutonomyWhenDisabled(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:      "demo",
			SwarmID: "swarm-1",
		},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	out := buf.String()
	for _, forbidden := range []string{
		"Autonomy enabled",
		"Poll Interval",
		"Max Tasks/Hour",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("autonomy summary must be hidden when disabled; rendered output contains %q", forbidden)
		}
	}
}

// TestProjectDetail_HeroRendersWhenAutonomyOnEvenWithoutDescription
// — locks in the regression fix: projects with autonomy enabled
// but no human-written description still get a visible hero card
// with the autonomy summary. Before the fix the hero gated solely
// on description and disappeared entirely for autonomy-only
// projects (the user's projects in production fit this profile).
func TestProjectDetail_HeroRendersWhenAutonomyOnEvenWithoutDescription(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:      "demo",
			SwarmID: "swarm-1",
			// Intentionally no Description set.
			Autonomy: registry.ProjectAutonomy{
				Enabled:      true,
				PollInterval: "10m",
				Goal:         "Run the daily ingest every morning.",
			},
		},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Run the daily ingest every morning.") {
		t.Error("hero must render the autonomy goal even when Project.Description is empty — regression seen on 2026-05-15")
	}
	if !strings.Contains(out, "Autonomy enabled") {
		t.Error("hero must render the autonomy pill when autonomy is on, even without a description")
	}
}

// TestPopulateProjectHomepageAutonomy_NilRepoOrDisabledNoop —
// every safe-degradation branch returns without mutating data.
// Pins the contract that the hero badge silently disappears
// rather than spamming logs / showing stale fields.
func TestPopulateProjectHomepageAutonomy_NilRepoOrDisabledNoop(t *testing.T) {
	cases := []struct {
		name    string
		srv     *Server
		project *registry.Project
	}{
		{
			"no repo wired",
			NewServer(),
			&registry.Project{ID: "x", Autonomy: registry.ProjectAutonomy{Enabled: true}},
		},
		{
			"autonomy disabled",
			NewServer(WithAutonomyEvaluationRepository(&stubAutonomyEvalRepo{})),
			&registry.Project{ID: "x", Autonomy: registry.ProjectAutonomy{Enabled: false}},
		},
		{
			"nil project (defensive)",
			NewServer(WithAutonomyEvaluationRepository(&stubAutonomyEvalRepo{})),
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data ProjectDetailData
			tc.srv.populateProjectHomepageAutonomy(context.Background(), tc.project, "x", &data)
			if data.NextAutonomyTickETA != "" {
				t.Errorf("expected empty NextAutonomyTickETA for %q; got %q", tc.name, data.NextAutonomyTickETA)
			}
		})
	}
}

// TestPopulateProjectHomepageAutonomy_HappyPath — autonomy enabled,
// a recent eval row exists; the hero fields populate using the
// declared PollInterval (parsed from string) on top of CreatedAt.
func TestPopulateProjectHomepageAutonomy_HappyPath(t *testing.T) {
	createdAt := time.Now().UTC().Add(-1 * time.Minute)
	stub := &stubAutonomyEvalRepo{
		ListFunc: func(_ context.Context, f persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
			if f.ProjectID == nil || *f.ProjectID != "alpha" {
				t.Fatalf("expected project-scoped lookup, got %+v", f)
			}
			return []*persistence.AutonomyEvaluation{
				{ID: "e1", ProjectID: "alpha", Outcome: "CREATED", CreatedAt: createdAt},
			}, nil
		},
	}
	srv := NewServer(WithAutonomyEvaluationRepository(stub))
	project := &registry.Project{
		ID:       "alpha",
		Autonomy: registry.ProjectAutonomy{Enabled: true, PollInterval: "2m"},
	}
	var data ProjectDetailData
	srv.populateProjectHomepageAutonomy(context.Background(), project, "alpha", &data)
	if data.NextAutonomyTickETA == "" {
		t.Fatal("ETA must be populated after a successful eval lookup")
	}
	if data.NextAutonomyTickISO == "" {
		t.Error("ISO timestamp must be populated for the client-side ticker")
	}
	if data.LastAutonomyOutcome != "CREATED" {
		t.Errorf("LastAutonomyOutcome = %q, want CREATED", data.LastAutonomyOutcome)
	}
	if data.LastAutonomyAgoLabel == "" {
		t.Error("LastAutonomyAgoLabel must be pre-rendered handler-side")
	}
}

// TestPopulateProjectHomepageAutonomy_PollIntervalFallback — when
// PollInterval is empty or unparseable, the helper falls back to
// the daemon's 5m default. Pins the contract so a project YAML
// without an explicit interval still renders the badge.
func TestPopulateProjectHomepageAutonomy_PollIntervalFallback(t *testing.T) {
	createdAt := time.Now().UTC()
	stub := &stubAutonomyEvalRepo{
		ListFunc: func(context.Context, persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
			return []*persistence.AutonomyEvaluation{
				{ID: "e1", ProjectID: "alpha", Outcome: "NO_ACTION", CreatedAt: createdAt},
			}, nil
		},
	}
	srv := NewServer(WithAutonomyEvaluationRepository(stub))
	project := &registry.Project{
		ID:       "alpha",
		Autonomy: registry.ProjectAutonomy{Enabled: true}, // PollInterval empty
	}
	var data ProjectDetailData
	srv.populateProjectHomepageAutonomy(context.Background(), project, "alpha", &data)
	// With PollInterval empty the fallback is 5m → ETA ≈ createdAt+5m
	// minus the wall clock at compute time, so within a small margin
	// the next-tick ISO should be ~5m after CreatedAt.
	if data.NextAutonomyTickAt.Sub(createdAt) != 5*time.Minute {
		t.Errorf("default fallback should be 5m; got NextAutonomyTickAt-CreatedAt = %s", data.NextAutonomyTickAt.Sub(createdAt))
	}
}

// TestPopulateProjectHomepageAutonomy_EmptyEvalsNoop — autonomy
// enabled but never run (fresh project); helper must NOT populate
// hero fields so the template hides the badge.
func TestPopulateProjectHomepageAutonomy_EmptyEvalsNoop(t *testing.T) {
	stub := &stubAutonomyEvalRepo{
		ListFunc: func(context.Context, persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithAutonomyEvaluationRepository(stub))
	project := &registry.Project{
		ID:       "alpha",
		Autonomy: registry.ProjectAutonomy{Enabled: true, PollInterval: "5m"},
	}
	var data ProjectDetailData
	srv.populateProjectHomepageAutonomy(context.Background(), project, "alpha", &data)
	if data.NextAutonomyTickETA != "" {
		t.Errorf("expected empty ETA when no evaluations recorded yet; got %q", data.NextAutonomyTickETA)
	}
}

// TestPopulateProjectHomepageAutonomy_RepoErrorNoop — the lookup
// can fail (e.g. DB connection blip); the helper logs at debug
// and degrades to a hidden badge rather than 500'ing the whole
// page.
func TestPopulateProjectHomepageAutonomy_RepoErrorNoop(t *testing.T) {
	stub := &stubAutonomyEvalRepo{
		ListFunc: func(context.Context, persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
			return nil, errors.New("simulated db error")
		},
	}
	srv := NewServer(WithAutonomyEvaluationRepository(stub))
	project := &registry.Project{
		ID:       "alpha",
		Autonomy: registry.ProjectAutonomy{Enabled: true, PollInterval: "5m"},
	}
	var data ProjectDetailData
	srv.populateProjectHomepageAutonomy(context.Background(), project, "alpha", &data)
	if data.NextAutonomyTickETA != "" {
		t.Errorf("repo error must not populate ETA; got %q", data.NextAutonomyTickETA)
	}
}
