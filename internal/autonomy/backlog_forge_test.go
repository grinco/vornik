package autonomy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/backlogfile"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

// T8b: tickBacklog stamps a synthetic backlog-origin forge job into the
// task payload when the project can mint a GitHub token AND resolves an
// unambiguous outbound repo — and stamps nothing otherwise.

// backlogForgePayload extracts the top-level forge_job the stamping path
// writes (same location the github channel / webhook intake use).
func backlogForgePayload(t *testing.T, payload []byte) *forgeapi.ForgeJob {
	t.Helper()
	var pl struct {
		ForgeJob *forgeapi.ForgeJob `json:"forge_job"`
	}
	require.NoError(t, json.Unmarshal(payload, &pl))
	return pl.ForgeJob
}

func newBacklogForgeManager(t *testing.T, content string) (*Manager, *mockTaskRepo) {
	t.Helper()
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	abs := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil,
		WithWorkspacePath(ws), WithBacklogStore(backlogfile.NewStore()))
	return m, repo
}

func TestTickBacklog_StampsForgeJob_GithubBlockOnly(t *testing.T) {
	// T8b fix wave: the primary consumer configures ONLY the `github:`
	// block (token creds + repo) — no github_app channel at all. The stamp
	// must key entirely off project.GitHub, the same struct whose
	// Enabled() gates injectGitHubToken.
	m, repo := newBacklogForgeManager(t, "- [ ] Fix the parser bug\n")
	project := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{Enabled: true, Mode: registry.AutonomyModeBacklog},
		// GitHub token-minting creds + outbound repo, all on `github:`.
		GitHub: registry.ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k.pem", Repo: "o/r"},
	}

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	job := backlogForgePayload(t, tasks[0].Payload)
	require.NotNil(t, job, "a github:-only project with a repo must stamp a forge job")
	assert.Equal(t, "o/r", job.Repo)
	assert.Equal(t, "backlog", job.Kind)
	assert.Equal(t, "fix-the-parser-bug", job.Slug)
	assert.Equal(t, "Fix the parser bug", job.Title)
	assert.Equal(t, "Fix the parser bug", job.Body, "body carries the raw item text")
}

func TestTickBacklog_StampsForgeJob_SlugAndTitleFromCleanedItem(t *testing.T) {
	// A deposit-rendered item: "**[kind]** title — detail…". BOTH the title
	// AND the slug key off the cleaned string (marker stripped, cut at the
	// em-dash) — the slug must not carry marker punctuation or detail text.
	// The body is still the whole raw item text.
	m, repo := newBacklogForgeManager(t,
		"- [ ] **[bug]** Fix the parser — it crashes on empty input\n")
	project := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{Enabled: true, Mode: registry.AutonomyModeBacklog},
		GitHub:   registry.ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k.pem", Repo: "o/r"},
	}

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	job := backlogForgePayload(t, tasks[0].Payload)
	require.NotNil(t, job)
	assert.Equal(t, "Fix the parser", job.Title)
	assert.Equal(t, "fix-the-parser", job.Slug,
		"slug keys off the cleaned title — no marker punctuation, no detail text")
	assert.Contains(t, job.Body, "**[bug]** Fix the parser — it crashes on empty input")
}

func TestTickBacklog_OmitsForgeJob_WhenNoRepoResolvable(t *testing.T) {
	cases := []struct {
		name    string
		project *registry.Project
	}{
		{
			name: "github token creds but no repo configured",
			project: &registry.Project{
				ID:       "p1",
				Autonomy: registry.ProjectAutonomy{Enabled: true, Mode: registry.AutonomyModeBacklog},
				GitHub:   registry.ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k.pem"},
			},
		},
		{
			name: "repo set but no github token creds",
			project: &registry.Project{
				ID:       "p1",
				Autonomy: registry.ProjectAutonomy{Enabled: true, Mode: registry.AutonomyModeBacklog},
				GitHub:   registry.ProjectGitHub{Repo: "o/r"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, repo := newBacklogForgeManager(t, "- [ ] do the thing\n")
			require.NoError(t, m.tickBacklog(context.Background(), tc.project, time.Now()))
			tasks := repo.createdTasks()
			require.Len(t, tasks, 1, "the backlog item still dispatches without a forge job")
			assert.Nil(t, backlogForgePayload(t, tasks[0].Payload),
				"no resolvable repo (or no token creds) must stamp no forge job")
		})
	}
}

func TestSlugifyBacklogTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "Fix the parser bug", "fix-the-parser-bug"},
		{"punctuation collapses", "Fix the cache!! (urgent)", "fix-the-cache-urgent"},
		{"leading/trailing separators trimmed", "  --Hello, World--  ", "hello-world"},
		{"unicode non-ascii becomes separators", "café déjà vu", "caf-d-j-vu"},
		{"empty falls back to item", "", "item"},
		{"all punctuation falls back to item", "!!! @@@ ###", "item"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slugifyBacklogTitle(tc.in)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len(got), 40)
			// Determinism.
			assert.Equal(t, got, slugifyBacklogTitle(tc.in))
		})
	}

	// Truncation caps at 40 chars (all-alnum input → no boundary dash).
	t.Run("truncation caps at 40 chars", func(t *testing.T) {
		in := strings.Repeat("a", 50)
		got := slugifyBacklogTitle(in)
		assert.Equal(t, strings.Repeat("a", 40), got)
		assert.Len(t, got, 40)
	})

	// Truncation trims a dash left at the 40-char boundary: 39 alnum + a
	// separator lands a '-' at index 39, which TrimRight removes.
	t.Run("truncation trims boundary dash", func(t *testing.T) {
		in := strings.Repeat("a", 39) + " more words here"
		got := slugifyBacklogTitle(in)
		assert.Equal(t, strings.Repeat("a", 39), got, "trailing dash after truncation must be trimmed")
	})

	// Rendered-item shape: the stamp path composes the two helpers —
	// slugifyBacklogTitle(backlogItemTitle(raw)) — so a deposit-rendered
	// item's slug carries neither the "**[kind]**" marker nor the detail.
	t.Run("rendered item slug excludes marker and detail", func(t *testing.T) {
		raw := "**[bug]** Fix the parser — it crashes on empty input"
		assert.Equal(t, "fix-the-parser", slugifyBacklogTitle(backlogItemTitle(raw)))
	})
}

func TestBacklogItemTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain no separator", "Fix the parser bug", "Fix the parser bug"},
		{"cut at em-dash", "Fix the parser — it crashes", "Fix the parser"},
		{"strip kind marker", "**[bug]** Fix the parser", "Fix the parser"},
		{"strip marker and cut", "**[feature]** Add export — CSV + JSON", "Add export"},
		{"whitespace trimmed", "   groom the backlog   ", "groom the backlog"},
		// M2 (final review, 2026-07-09): a deposited title containing an
		// em-dash is normalized to a plain hyphen by the deposit handler
		// before render (internal/api/backlog_deposit_handlers.go), so
		// the rendered title/detail separator this parser cuts on is
		// unambiguous — the hyphen must NOT be mistaken for the
		// separator and must not truncate the title early.
		{"hyphen in title (post-normalization) is not mistaken for the em-dash separator",
			"**[bug]** cache - rebuild loop — root cause was a missing lock", "cache - rebuild loop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, backlogItemTitle(tc.in))
		})
	}
}
