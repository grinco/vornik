package service

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
)

// The series resolver at its seam (2026-09-16-retrieval-recency-design.md
// §5.1.1). The derivation rule itself is unit-tested in internal/memory; what is
// tested HERE is the wiring that the rule cannot see: reading the prompt out of
// an opaque task payload, reading the declared slugs off the project, and
// degrading to "not a series" rather than to an error on every missing piece.

type fakeSeriesTasks struct {
	persistence.TaskRepository
	task *persistence.Task
	err  error
}

func (f *fakeSeriesTasks) Get(context.Context, string) (*persistence.Task, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.task == nil {
		// The miss contract: an absent row is (nil, ErrNotFound), never
		// (nil, nil). A double that answers (nil, nil) lets a caller's
		// nil-check and its error-check both pass, which is how a "not found"
		// becomes a nil dereference two layers up.
		return nil, persistence.ErrNotFound
	}
	return f.task, nil
}

// withPrompt builds a present task carrying a prompt.
func withPrompt(p string) *fakeSeriesTasks {
	return &fakeSeriesTasks{task: &persistence.Task{Payload: promptPayload(p)}}
}

// The repo-double conformance gate: a double standing in for a repository must
// keep the same miss contract as the real one, or tests pass against behaviour
// production never exhibits.
func TestFakeSeriesTasks_KeepsTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "TaskRepository.Get", (&fakeSeriesTasks{}).Get)
}

// fakeFeedSource stands in for the registry. The real one populates its project
// map only from disk, so faking the lookup keeps these tests about the rule
// rather than about staging YAML.
type fakeFeedSource struct{ projects map[string]*registry.Project }

func (f *fakeFeedSource) GetProject(id string) *registry.Project { return f.projects[id] }

func feedProject(t *testing.T, id string, slugs ...string) *fakeFeedSource {
	t.Helper()
	p := &registry.Project{ID: id}
	for _, s := range slugs {
		p.Autonomy.Feeds = append(p.Autonomy.Feeds, registry.AutonomyFeed{Slug: s, Cadence: "12h"})
	}
	return &fakeFeedSource{projects: map[string]*registry.Project{id: p}}
}

func promptPayload(p string) []byte {
	return []byte(`{"context":{"prompt":` + quoteJSON(p) + `}}`)
}

func quoteJSON(s string) string {
	out := `"`
	for _, r := range s {
		if r == '"' || r == '\\' {
			out += `\`
		}
		out += string(r)
	}
	return out + `"`
}

func TestSeriesResolver_DerivesADeclaredSlug(t *testing.T) {
	r := newSeriesResolver(
		withPrompt("czech-news: Refresh project memory."),
		feedProject(t, "assistant", "czech-news", "prague-events"),
		zerolog.Nop())
	key, warn := r.SeriesKeyFor(context.Background(), "assistant", "task_1")
	if key != "czech-news" {
		t.Errorf("key = %q, want czech-news", key)
	}
	if warn {
		t.Error("a declared slug must not warn")
	}
}

// §5.1.1 row 4, and the one the live store makes expensive: 786 tasks across
// two projects run recurring series with no feeds block. They must be silent,
// or the warning that matters is buried.
func TestSeriesResolver_ProjectWithoutFeedsIsSilent(t *testing.T) {
	r := newSeriesResolver(
		withPrompt("linkedin-jobs-cz: scan postings."),
		feedProject(t, "janka"), // declares nothing
		zerolog.Nop())
	key, warn := r.SeriesKeyFor(context.Background(), "janka", "task_1")
	if key != "" || warn {
		t.Errorf("key=%q warn=%v — a project declaring no feeds must be silent", key, warn)
	}
}

func TestSeriesResolver_UndeclaredSlugWarns(t *testing.T) {
	r := newSeriesResolver(
		withPrompt("europe-relocation: refresh."),
		feedProject(t, "assistant", "czech-news"),
		zerolog.Nop())
	key, warn := r.SeriesKeyFor(context.Background(), "assistant", "task_1")
	if key != "" {
		t.Errorf("key = %q, want empty for an undeclared slug", key)
	}
	if !warn {
		t.Error("an undeclared slug on a feed-declaring project must warn — the scheduler " +
			"cannot see this case, so ranking is the only place it surfaces")
	}
}

// Every missing piece degrades to "not a series", never to an error. A chunk
// that cannot be attributed simply is not part of one, which is the right
// answer for the overwhelming majority of chunks.
func TestSeriesResolver_DegradesQuietly(t *testing.T) {
	reg := feedProject(t, "assistant", "czech-news")

	cases := map[string]struct {
		tasks   *fakeSeriesTasks
		reg     *fakeFeedSource
		project string
		taskID  string
	}{
		"task miss":           {&fakeSeriesTasks{err: persistence.ErrNotFound}, reg, "assistant", "t1"},
		"empty payload":       {&fakeSeriesTasks{task: &persistence.Task{}}, reg, "assistant", "t1"},
		"unparseable payload": {&fakeSeriesTasks{task: &persistence.Task{Payload: []byte("not json")}}, reg, "assistant", "t1"},
		"unknown project":     {withPrompt("czech-news: refresh."), reg, "no-such-project", "t1"},
		"no task id":          {withPrompt("czech-news: refresh."), reg, "assistant", ""},
	}
	for name, tc := range cases {
		r := newSeriesResolver(tc.tasks, tc.reg, zerolog.Nop())
		key, warn := r.SeriesKeyFor(context.Background(), tc.project, tc.taskID)
		if key != "" || warn {
			t.Errorf("%s: key=%q warn=%v, want silent", name, key, warn)
		}
	}
}

// An unwired dependency disables the feature rather than half-enabling it: a
// resolver that always answered "" would be indistinguishable from one that
// works and finds nothing.
func TestSeriesResolver_NilWhenUnwired(t *testing.T) {
	if newSeriesResolver(nil, feedProject(t, "assistant"), zerolog.Nop()) != nil {
		t.Error("no task store must yield no resolver")
	}
	if newSeriesResolver(&fakeSeriesTasks{}, nil, zerolog.Nop()) != nil {
		t.Error("no registry must yield no resolver")
	}
	// A typed-nil *registry.Registry is a non-nil interface value; it must be
	// caught too, or the first lookup panics.
	var typedNil *registry.Registry
	if newSeriesResolver(&fakeSeriesTasks{}, typedNil, zerolog.Nop()) != nil {
		t.Error("a typed-nil registry must yield no resolver")
	}
}

// The wiring itself, not the rule. A resolver that derives perfectly and is
// never attached to the Indexer is indistinguishable, in the store, from no
// feature — series_key stays NULL on every row and §5.3's supersession never
// fires. So the attachment gets its own test.
func TestWireSeriesResolver_OnlyWhenBothHalvesArePresent(t *testing.T) {
	idx := memory.NewIndexer(memory.Config{}, nil, nil, zerolog.Nop())

	// No repositories at all: the feature stays off, and asking must not panic
	// on the way there — this container shape is reachable on a daemon whose
	// storage init failed.
	c := &Container{Logger: zerolog.Nop()}
	if got := c.wireSeriesResolver(idx); got != nil {
		t.Errorf("no repos: wired %T, want nil", got)
	}

	// Task store present, registry absent — a typed-nil *registry.Registry,
	// which is the shape a half-built container actually holds.
	c = &Container{Logger: zerolog.Nop(), repos: &storage.Repositories{Tasks: &fakeSeriesTasks{}}}
	if got := c.wireSeriesResolver(idx); got != nil {
		t.Errorf("no registry: wired %T, want nil", got)
	}

	// Both halves present: the resolver must reach the Indexer.
	c = &Container{
		Logger:   zerolog.Nop(),
		repos:    &storage.Repositories{Tasks: &fakeSeriesTasks{}},
		Registry: registry.New(),
	}
	if got := c.wireSeriesResolver(idx); got == nil {
		t.Fatal("a container holding both a task store and a registry must wire a resolver; " +
			"without it series_key is never written and the column stays dead")
	}
}

// Scheduler stamping (2026-09-16, after the operator identified the daily
// briefing as a recurring DISPATCHER REMINDER rather than an autonomy tick).
//
// A reminder-fired task already carries `context.scheduled_reminder_id`, and a
// recurring reminder re-arms itself so that id is STABLE ACROSS FIRINGS —
// verified live: one row, rem_20260720192026_092619fbbbd7b940, 111 tasks from
// 2026-07-22 to 2026-09-16. That is a better series identity than any prompt
// prefix: written by the scheduler rather than by prose, so it cannot drift.

func reminderTask(id string) *fakeSeriesTasks {
	return &fakeSeriesTasks{task: &persistence.Task{
		Payload: []byte(`{"context":{"scheduled_reminder_id":` + quoteJSON(id) + `}}`),
	}}
}

func TestSeriesResolver_SchedulerStampBeatsThePromptParse(t *testing.T) {
	// A task carrying BOTH a stamp and a slug-shaped prompt prefix.
	tasks := &fakeSeriesTasks{task: &persistence.Task{Payload: []byte(
		`{"context":{"prompt":"czech-news: refresh","scheduled_reminder_id":"rem_abc"}}`)}}
	r := newSeriesResolver(tasks, feedProject(t, "assistant", "czech-news"), zerolog.Nop())
	key, warn := r.SeriesKeyFor(context.Background(), "assistant", "t1")
	if key != "reminder:rem_abc" {
		t.Errorf("key = %q, want reminder:rem_abc — a scheduler STAMP outranks a prompt parse; "+
			"one is written by the scheduler, the other is prose that has drifted twice", key)
	}
	if warn {
		t.Error("a stamped task must not warn")
	}
}

// The case that motivated this: the briefing's project need not declare feeds
// at all, and §5.1.1's row-4 "no registry, stay silent" rule must not block a
// stamp. A reminder id is self-identifying; it needs no registry to validate
// against.
func TestSeriesResolver_StampWorksWithoutAFeedRegistry(t *testing.T) {
	r := newSeriesResolver(reminderTask("rem_20260720192026_092619fbbbd7b940"),
		feedProject(t, "assistant"), // declares NO feeds
		zerolog.Nop())
	key, warn := r.SeriesKeyFor(context.Background(), "assistant", "t1")
	if key != "reminder:rem_20260720192026_092619fbbbd7b940" {
		t.Errorf("key = %q — a reminder series must not require a feeds[] registry", key)
	}
	if warn {
		t.Error("a stamped task in a registry-less project must be silent, not warned about")
	}
}

// Namespaced so a reminder id can never collide with a declared feed slug,
// whatever an operator names a feed.
func TestSeriesResolver_StampIsNamespaced(t *testing.T) {
	r := newSeriesResolver(reminderTask("czech-news"), // pathological: id equals a slug
		feedProject(t, "assistant", "czech-news"), zerolog.Nop())
	key, _ := r.SeriesKeyFor(context.Background(), "assistant", "t1")
	if key == "czech-news" {
		t.Error("an unnamespaced stamp collided with a declared feed slug; the two vocabularies " +
			"are independent and must not be able to merge two unrelated series")
	}
	if key != "reminder:czech-news" {
		t.Errorf("key = %q, want reminder:czech-news", key)
	}
}

// The autonomy path is unchanged when there is no stamp.
func TestSeriesResolver_FallsBackToTheFeedParseWithoutAStamp(t *testing.T) {
	r := newSeriesResolver(withPrompt("czech-news: refresh"),
		feedProject(t, "assistant", "czech-news"), zerolog.Nop())
	key, _ := r.SeriesKeyFor(context.Background(), "assistant", "t1")
	if key != "czech-news" {
		t.Errorf("key = %q, want czech-news — the feed parse must still work unstamped", key)
	}
}
