package service

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// seriesResolver derives a chunk's recurring-series key at ingest
// (2026-09-16-retrieval-recency-design.md §5.1.1). It lives here rather than in
// internal/memory because answering needs two things that package must not
// depend on: the task store (for the producing task's prompt) and the project
// registry (for the slugs that project declares).
//
// The derivation reads the autonomy scheduler's OWN convention —
// `AutonomyFeed.Slug` documents itself as "the `<slug>: ` prompt prefix
// identifying this feed's tasks", and the scheduler matches on it to decide
// which feed is due. That is why this adds no new fragile dependency: it reads
// a string that is already load-bearing for a mechanism that fails noisily,
// rather than inventing one that fails silently.
// projectFeedSource is the narrow slice of the registry this needs: one
// project lookup. *registry.Registry satisfies it. Narrow on purpose — the
// resolver has no business with swarms or workflows, and the registry's project
// map is only populated from disk, so a wider dependency would force every test
// to stage YAML for a rule that is pure string comparison.
type projectFeedSource interface {
	GetProject(id string) *registry.Project
}

type seriesResolver struct {
	tasks persistence.TaskRepository
	reg   projectFeedSource
	log   zerolog.Logger
}

func newSeriesResolver(tasks persistence.TaskRepository, reg projectFeedSource, log zerolog.Logger) memory.SeriesResolver {
	if tasks == nil || reg == nil || isNilFeedSource(reg) {
		// No task store or no registry means no derivation is possible.
		// Returning nil disables the feature rather than half-enabling it:
		// a resolver that always answers "" would be indistinguishable from
		// one that works and finds nothing.
		return nil
	}
	return &seriesResolver{tasks: tasks, reg: reg, log: log}
}

// SeriesKeyFor implements memory.SeriesResolver.
//
// Every failure returns ("", false) — no key, no warning. A chunk that cannot
// be attributed to a series simply is not one, which is the correct answer for
// the overwhelming majority of chunks. The one case that warns is §5.1.1's:
// the project declares feeds, the prompt carries a slug-shaped prefix, and that
// prefix is not among them. The scheduler cannot detect that case (it matches
// prompt-prefix against prompt-prefix and never reads the registry), so ranking
// is the only place it surfaces and it must not surface silently.
func (s *seriesResolver) SeriesKeyFor(ctx context.Context, projectID, taskID string) (string, bool) {
	if s == nil || taskID == "" || projectID == "" {
		return "", false
	}
	t, err := s.tasks.Get(ctx, taskID)
	if err != nil || t == nil {
		return "", false
	}

	// 1. THE SCHEDULER'S OWN STAMP WINS, and needs no registry.
	//
	// A recurring dispatcher reminder re-arms itself, so its id is stable
	// across firings — verified live: one row,
	// rem_20260720192026_092619fbbbd7b940, 111 tasks across two months. That
	// is a better series identity than any prompt prefix, because the
	// scheduler writes it rather than an LLM composing prose, and prose has
	// already drifted twice ("Run a czech-news refresh tick", "Daily
	// digest:"). It also reaches a producer the feed parse structurally
	// cannot: the briefing's project need not declare feeds at all, so the
	// registry check below must NOT gate this.
	//
	// Namespaced so the two vocabularies can never merge two unrelated
	// series, whatever an operator names a feed.
	if rid := scheduledReminderID(t.Payload); rid != "" {
		return seriesKeyReminderPrefix + rid, false
	}

	// 2. Otherwise fall back to the autonomy feed slug carried in the prompt.
	proj := s.reg.GetProject(projectID)
	if proj == nil {
		return "", false
	}
	declared := proj.Autonomy.Feeds
	if len(declared) == 0 {
		// §5.1.1 row 4: a project that declares nothing cannot have an
		// UNDECLARED slug, so there is no signal to emit. Measured: 786 live
		// tasks across two projects take this path, and warning on them would
		// bury the row that matters.
		return "", false
	}
	prompt := taskPromptText(t.Payload)
	if prompt == "" {
		return "", false
	}
	slugs := make([]string, 0, len(declared))
	for _, f := range declared {
		slugs = append(slugs, f.Slug)
	}
	key, warn := memory.ResolveSeriesKey("", prompt, slugs, true)
	if warn {
		s.log.Warn().
			Str("project_id", projectID).
			Str("task_id", taskID).
			Str("prefix", memory.SlugPrefix(prompt)).
			Msg("memory: task prompt carries an undeclared feed slug; its chunks get no series_key " +
				"and will not be demoted by a newer member — declare it in the project's feeds[] or " +
				"correct the prompt prefix")
	}
	return key, warn
}

// seriesKeyReminderPrefix namespaces scheduler-stamped keys away from feed
// slugs. Without it a feed named `rem_x` and reminder `rem_x` would merge into
// one series, which is the silent wrong-grouping the closed vocabulary exists
// to prevent — arriving from the one direction validation does not cover.
const seriesKeyReminderPrefix = "reminder:"

// scheduledReminderID reads the reminder id the runner stamps on every task it
// fires (container_reminders.go sets context.scheduled_reminder_id). Empty for
// every task that was not scheduled by a reminder, which is most of them.
func scheduledReminderID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Context struct {
			ReminderID string `json:"scheduled_reminder_id"`
		} `json:"context"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.Context.ReminderID
}

// taskPromptText pulls context.prompt out of a task payload. The payload is
// stored as opaque JSON, and only this one field matters here; an unmarshal
// failure returns "" rather than an error because a task whose payload cannot
// be read is simply not attributable to a series.
func taskPromptText(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.Context.Prompt
}

// isNilFeedSource catches a typed-nil *registry.Registry arriving through the
// interface, which is non-nil as an interface value and panics on use. The same
// trap the container hits elsewhere with typed-nil Registerers.
func isNilFeedSource(s projectFeedSource) bool {
	r, ok := s.(*registry.Registry)
	return ok && r == nil
}

// wireSeriesResolver attaches the recurring-series resolver to the memory
// indexer, and is the only place in the daemon that can: the derivation needs
// the task store for the producing task's prompt and the project registry for
// the slugs that project declares, and internal/memory has neither.
//
// Returns the resolver it attached, or nil when the feature stays off. The
// return value exists so the attachment is assertable — the Indexer's resolver
// field is unexported, and "derives correctly but was never wired" is exactly
// the failure that leaves series_key NULL on every row while every unit test
// stays green.
//
// Nil-safe at every layer: a container whose storage init failed, and an
// indexer that was never built, both leave the feature off rather than panic.
func (c *Container) wireSeriesResolver(idx *memory.Indexer) memory.SeriesResolver {
	if c == nil || c.repos == nil || idx == nil {
		return nil
	}
	resolver := newSeriesResolver(c.repos.Tasks, c.Registry,
		c.Logger.With().Str("component", "memory").Str("derive", "series_key").Logger())
	if resolver == nil {
		return nil
	}
	idx.SetSeriesResolver(resolver)
	return resolver
}
