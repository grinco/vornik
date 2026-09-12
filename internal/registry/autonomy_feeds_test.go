package registry

import (
	"strings"
	"testing"
	"time"
)

// TestResolveFeeds_ParsesCadences pins the happy path: declared feeds
// resolve in declaration order with their cadence strings parsed to
// time.Duration. Task 3 (schedule-lag measurement) and Task 5 (the
// health table) both consume ResolveFeeds() rather than re-parsing
// AutonomyFeed.Cadence themselves, so this is the contract they build on.
func TestResolveFeeds_ParsesCadences(t *testing.T) {
	p := &Project{Autonomy: ProjectAutonomy{
		Enabled: true,
		Feeds: []AutonomyFeed{
			{Slug: "czech-news", Cadence: "12h"},
			{Slug: "cultural-events", Cadence: "1440h"},
		},
	}}
	got := p.ResolveFeeds()
	if len(got) != 2 {
		t.Fatalf("want 2 resolved feeds, got %d", len(got))
	}
	if got[0].Slug != "czech-news" || got[0].Cadence != 12*time.Hour {
		t.Errorf("feed 0 = %+v, want czech-news/12h", got[0])
	}
	if got[1].Cadence != 1440*time.Hour {
		t.Errorf("feed 1 cadence = %v, want 1440h", got[1].Cadence)
	}
}

// TestResolveFeeds_EmptyWhenUndeclared confirms a project that never
// mentions autonomy.feeds resolves to an empty (not nil-panicking)
// slice — the "not declared" case the health surface reports honestly
// rather than defaulting to a fabricated OK (design §6).
func TestResolveFeeds_EmptyWhenUndeclared(t *testing.T) {
	p := &Project{Autonomy: ProjectAutonomy{Enabled: true}}
	if got := p.ResolveFeeds(); len(got) != 0 {
		t.Errorf("undeclared feeds should resolve to empty, got %+v", got)
	}
}

// TestValidate_RefusesDuplicateSlug exercises the real load path (Stage +
// StripInvalidFromStaged), not a synthetic Validate() — see task-1 brief
// ruling 1: no such method exists on Config. Two feeds sharing a slug
// would make the per-slug lag metric ambiguous (two cadences, one
// series), so registry load must refuse the project rather than let a
// later consumer guess which cadence applies.
func TestValidate_RefusesDuplicateSlug(t *testing.T) {
	tmp := t.TempDir()
	mustWriteAll(t, tmp, minimalProjectTree(t, `
  feeds:
    - slug: czech-news
      cadence: "12h"
    - slug: czech-news
      cadence: "24h"`))

	r := New()
	if err := r.Stage(tmp); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	verr := r.StripInvalidFromStaged()
	if verr == nil {
		t.Fatal("duplicate slug must be refused at load")
	}
	if !strings.Contains(verr.Error(), "duplicate autonomy.feeds slug") {
		t.Errorf("error should name the duplicate, got: %v", verr)
	}
	if err := r.ActivateStaged(); err != nil {
		t.Fatalf("ActivateStaged: %v", err)
	}
	if r.GetProject("feeds-project") != nil {
		t.Error("feeds-project survived load despite the duplicate slug")
	}
}

// TestValidate_RefusesNonPositiveCadence covers every shape of a bad
// cadence string an operator typo could produce: empty, an explicit
// zero duration, a negative duration, and unparseable text. Any of
// these would make every future lag observation for that slug read as
// a breach, so all four are refused at load rather than surfacing in
// a dashboard later.
func TestValidate_RefusesNonPositiveCadence(t *testing.T) {
	for _, bad := range []string{"", "0s", "-4h", "banana"} {
		t.Run(bad, func(t *testing.T) {
			tmp := t.TempDir()
			mustWriteAll(t, tmp, minimalProjectTree(t, `
  feeds:
    - slug: s
      cadence: "`+bad+`"`))

			r := New()
			if err := r.Stage(tmp); err != nil {
				t.Fatalf("Stage: %v", err)
			}
			verr := r.StripInvalidFromStaged()
			if verr == nil {
				t.Errorf("cadence %q must be refused at load", bad)
			}
		})
	}
}

// TestValidate_RefusesEmptySlug covers the branch every other case in
// this file walked past: each cadence case uses the slug "s", so the
// empty-slug guard (registry.go) had no test at all. An empty slug makes
// feedSlugPrefix ": ", which prefix-matches nothing and would report the
// feed as never-run forever -- a declared feed that can never be
// observed is the "never examined" state wearing a declaration.
func TestValidate_RefusesEmptySlug(t *testing.T) {
	for name, slugYAML := range map[string]string{
		"absent": `    - cadence: "12h"`,
		"empty":  "    - slug: \"\"\n      cadence: \"12h\"",
		"blank":  "    - slug: \"   \"\n      cadence: \"12h\"",
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			mustWriteAll(t, tmp, minimalProjectTree(t, "\n  feeds:\n"+slugYAML))

			r := New()
			if err := r.Stage(tmp); err != nil {
				t.Fatalf("Stage: %v", err)
			}
			verr := r.StripInvalidFromStaged()
			if verr == nil {
				t.Fatal("an empty autonomy.feeds slug must be refused at load")
			}
			if !strings.Contains(verr.Error(), "empty slug") {
				t.Errorf("error should name the empty slug, got: %v", verr)
			}
			if err := r.ActivateStaged(); err != nil {
				t.Fatalf("ActivateStaged: %v", err)
			}
			if r.GetProject("feeds-project") != nil {
				t.Error("feeds-project survived load despite the empty slug")
			}
		})
	}
}

// TestValidate_AbsentFeedsLoadsUnchanged is the compatibility guard:
// a project that never mentions autonomy.feeds must load exactly as
// it did before this feature existed. autonomy.feeds is additive and
// optional (design §3), never a new required key.
func TestValidate_AbsentFeedsLoadsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	mustWriteAll(t, tmp, minimalProjectTree(t, ""))

	r := New()
	if err := r.Stage(tmp); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if verr := r.StripInvalidFromStaged(); verr != nil {
		t.Fatalf("a project with no feeds must load unchanged, got: %v", verr)
	}
	if err := r.ActivateStaged(); err != nil {
		t.Fatalf("ActivateStaged: %v", err)
	}
	if r.GetProject("feeds-project") == nil {
		t.Error("feeds-project was stripped despite declaring no feeds")
	}
}

// minimalProjectTree builds the smallest swarm+workflow+project config
// tree that survives every OTHER stripInvalidProjects check, with
// project "feeds-project" carrying an autonomy block of:
//
//	enabled: true
//	mode: llm
//	goal: g
//	<feedsYAML>
//
// spliced in verbatim, so the feeds-validation tests exercise the real
// registry.Load path (Stage + StripInvalidFromStaged) instead of a
// synthetic Config.Validate() — see task-1 brief ruling 1.
func minimalProjectTree(t *testing.T, feedsYAML string) map[string]string {
	t.Helper()
	return map[string]string{
		"swarms/feeds-swarm.md": `---
swarmId: "feeds-swarm"
roles:
  - name: "worker"
    runtime: { image: "x" }
---
`,
		"workflows/feeds-workflow.md": `---
workflowId: "feeds-workflow"
entrypoint: "run"
steps:
  run:
    type: "agent"
    role: "worker"
    prompt: "do work"
terminals:
  complete: { status: "COMPLETED" }
---
`,
		"projects/feeds-project.yaml": `projectId: "feeds-project"
swarmId: "feeds-swarm"
defaultWorkflowId: "feeds-workflow"
autonomy:
  enabled: true
  mode: llm
  goal: "g"` + feedsYAML + `
`,
	}
}
