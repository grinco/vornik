package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forgereview"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// §13.3: the generic webhook path must obey the SAME pause and coalescing rules
// as the GitHub App channel, by calling the same coordinator. These tests are
// what stop the two ingresses drifting — the failure mode that let phases 1-4
// ship into an ingress this deployment does not use.

type stubClassifier struct{ job forgeapi.ForgeJob }

func (s stubClassifier) ClassifyWebhook(context.Context, string, []byte) (json.RawMessage, string, bool) {
	raw, _ := json.Marshal(s.job)
	return raw, "review it", true
}

type stubCoordinator struct {
	decision forgereview.Decision
	claimed  []string
	paused   []bool
	sawJob   forgeapi.ForgeJob
	sawOnDem bool
}

func (s *stubCoordinator) Decide(_ context.Context, _ string, job forgeapi.ForgeJob, onDemand bool) forgereview.Decision {
	s.sawJob, s.sawOnDem = job, onDemand
	return s.decision
}
func (s *stubCoordinator) Claim(_ context.Context, _ string, _ forgeapi.ForgeJob, taskID string) {
	s.claimed = append(s.claimed, taskID)
}
func (s *stubCoordinator) SetPaused(_ context.Context, _, _ string, _ int, p bool) error {
	s.paused = append(s.paused, p)
	return nil
}

func reviewForgeJob() forgeapi.ForgeJob {
	return forgeapi.ForgeJob{
		Provider: forgeapi.ProviderGitHub, Repo: "acme/api", Number: 12,
		Action: "synchronize", IsChangeRequest: true, HeadSHA: "sha-new",
	}
}

func serveWebhook(t *testing.T, job forgeapi.ForgeJob, coord ForgeReviewCoordinator) (*httptest.ResponseRecorder, *mocks.MockTaskRepository) {
	t.Helper()
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(&mockWebhookEventRepo{}),
		WithForgeClassifier(stubClassifier{job: job}),
		WithForgeReviewCoordinator(coord),
	)
	project := reg.GetProject("project-1")
	source, ok := findWebhookSource(project, "github")
	require.True(t, ok)

	rec := httptest.NewRecorder()
	server.enqueueVerifiedWebhook(context.Background(), rec, project, source,
		[]byte(`{"id":"evt-1","action":"synchronize"}`), "d-1")
	return rec, taskRepo
}

// A push while a review is in flight must NOT create a second task here either.
// Without this the generic path re-introduces the one-review-per-push cost the
// whole feature exists to remove.
func TestGenericPath_CoordinatorSkip_CreatesNoTask(t *testing.T) {
	coord := &stubCoordinator{decision: forgereview.Decision{Skip: true, Reason: "superseded"}}
	rec, taskRepo := serveWebhook(t, reviewForgeJob(), coord)

	assert.Equal(t, http.StatusOK, rec.Code, "a coalesced delivery must be acked, not retried")
	assert.Equal(t, 0, taskRepo.CallCount.Create, "the generic path enqueued a duplicate review")
	assert.Contains(t, rec.Body.String(), "superseded")
}

// The ordinary case still creates a task and TAKES the claim, so the pushes that
// follow coalesce onto it.
func TestGenericPath_CoordinatorAllows_CreatesTaskAndClaims(t *testing.T) {
	coord := &stubCoordinator{}
	rec, taskRepo := serveWebhook(t, reviewForgeJob(), coord)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
	require.Len(t, coord.claimed, 1, "the created task did not take the review claim")
	assert.NotEmpty(t, coord.claimed[0])
}

// An on-demand job must be passed through as such, or it would be coalesced away
// and a human's explicit request would silently do nothing.
func TestGenericPath_OnDemandFlagReachesTheCoordinator(t *testing.T) {
	job := reviewForgeJob()
	job.OnDemand = true
	job.Action = "comment_command"
	job.AuthorIsTrusted = true // a maintainer; the untrusted case is its own test
	coord := &stubCoordinator{}
	_, _ = serveWebhook(t, job, coord)
	assert.True(t, coord.sawOnDem, "OnDemand did not reach the coordinator")
}

// THE LOOP GUARD on this path. A bot-authored command must never create a
// review: our own review is posted as a comment.
func TestGenericPath_BotAuthoredCommand_CreatesNoTask(t *testing.T) {
	job := reviewForgeJob()
	job.OnDemand, job.AuthorIsBot, job.Action = true, true, "comment_command"
	rec, taskRepo := serveWebhook(t, job, &stubCoordinator{})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create, "a bot-authored command triggered a review — the self-trigger loop is open")
}

// pause/resume are state operations: they must not create a task.
func TestGenericPath_PauseCommand_SetsStateNotTask(t *testing.T) {
	job := reviewForgeJob()
	job.OnDemand, job.Action, job.Command = true, "comment_command", "pause"
	job.AuthorIsTrusted = true // a maintainer; the untrusted case is its own test
	coord := &stubCoordinator{}
	rec, taskRepo := serveWebhook(t, job, coord)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create, "pause created a review task")
	require.Len(t, coord.paused, 1)
	assert.True(t, coord.paused[0])
}

// No coordinator wired → degrade to always-enqueue, never to silence.
func TestGenericPath_NoCoordinator_StillCreatesTask(t *testing.T) {
	rec, taskRepo := serveWebhook(t, reviewForgeJob(), nil)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

// A non-forge delivery must not reach the coordinator at all.
func TestGenericPath_NonChangeRequest_SkipsTheCoordinator(t *testing.T) {
	job := forgeapi.ForgeJob{Provider: forgeapi.ProviderGitHub, Repo: "acme/api", Number: 7, Action: "labeled"}
	coord := &stubCoordinator{}
	_, taskRepo := serveWebhook(t, job, coord)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
	assert.Empty(t, coord.claimed, "an issue task took a PR review claim")
}

var _ = persistence.TaskStatusRunning

// THE DENIAL-OF-WALLET GUARD on the generic path. This ingress has no sender
// allowlist of its own — unlike the GitHub App channel, which checks
// resolveSpeakerForInstallation — so without this check any commenter on a
// PUBLIC repository could spend the project's review budget by typing
// "@vornik review". The provider supplies the trust signal; the rule lives here.
func TestGenericPath_UntrustedCommandAuthor_CreatesNoTask(t *testing.T) {
	job := reviewForgeJob()
	job.OnDemand, job.Action = true, "comment_command"
	job.AuthorIsTrusted = false // a stranger on a public repo

	rec, taskRepo := serveWebhook(t, job, &stubCoordinator{})
	assert.Equal(t, http.StatusOK, rec.Code, "a declined command must be acked, not retried")
	assert.Equal(t, 0, taskRepo.CallCount.Create,
		"an untrusted commenter triggered a review — this is a denial-of-wallet primitive on a public repo")
}

// A trusted maintainer's command still works.
func TestGenericPath_TrustedCommandAuthor_CreatesTask(t *testing.T) {
	job := reviewForgeJob()
	job.OnDemand, job.Action = true, "comment_command"
	job.AuthorIsTrusted = true

	rec, taskRepo := serveWebhook(t, job, &stubCoordinator{})
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

// pause/resume are cheap, but they change what the project does. They are
// commands too, so they need the same standing.
func TestGenericPath_UntrustedPause_IsRefused(t *testing.T) {
	job := reviewForgeJob()
	job.OnDemand, job.Action, job.Command = true, "comment_command", "pause"
	job.AuthorIsTrusted = false

	coord := &stubCoordinator{}
	rec, _ := serveWebhook(t, job, coord)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, coord.paused, "an untrusted commenter silenced review on this PR")
}

// The trust rule applies to COMMANDS only. A push is not a command: it carries
// no author standing, and gating it would stop reviewing pull requests entirely.
func TestGenericPath_PushIsNotSubjectToAuthorTrust(t *testing.T) {
	job := reviewForgeJob() // synchronize, OnDemand=false, AuthorIsTrusted=false
	rec, taskRepo := serveWebhook(t, job, &stubCoordinator{})
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 1, taskRepo.CallCount.Create, "a normal push was blocked by the command trust rule")
}
