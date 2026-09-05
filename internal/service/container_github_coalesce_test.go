package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forgereview"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// Phase 2 of https://docs.vornik.io:
// coalescing (§5, §5.2).
//
// Phase 1 made every push trigger a review. On an active PR that is one full
// review per push — precisely the behaviour §2 records the original author
// correctly declining to ship. These tests are the reason phase 1 must not
// reach a release without phase 2.

// fakeReviewState is an in-memory ForgePRReviewStateRepository.
type fakeReviewState struct {
	mu   sync.Mutex
	rows map[string]*persistence.ForgePRReviewState
}

func newFakeReviewState() *fakeReviewState {
	return &fakeReviewState{rows: map[string]*persistence.ForgePRReviewState{}}
}

func (f *fakeReviewState) key(p, r string, n int) string {
	return p + "|" + r + "|" + string(rune(n))
}

func (f *fakeReviewState) Get(_ context.Context, p, r string, n int) (*persistence.ForgePRReviewState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[f.key(p, r, n)], nil
}

func (f *fakeReviewState) ClaimOrSupersede(_ context.Context, p, r string, n int, sha string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(p, r, n)
	row := f.rows[k]
	if row == nil {
		row = &persistence.ForgePRReviewState{ProjectID: p, Repo: r, Number: n}
		f.rows[k] = row
	}
	prior := row.TaskID
	row.PendingHeadSHA = sha
	return prior, nil
}

func (f *fakeReviewState) SetTask(_ context.Context, p, r string, n int, taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(p, r, n)
	if f.rows[k] == nil {
		f.rows[k] = &persistence.ForgePRReviewState{ProjectID: p, Repo: r, Number: n}
	}
	f.rows[k].TaskID = taskID
	return nil
}

// BeginClosing models the real backends' COMPARE-AND-SET on pending_head_sha.
// A double that applied unconditionally would be looser than production and
// would certify the very hole the 2026-09-03 audit found.
func (f *fakeReviewState) BeginClosing(_ context.Context, p, r string, n int, sha, expectedPending string) (persistence.ClosingOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(p, r, n)
	if f.rows[k] == nil {
		f.rows[k] = &persistence.ForgePRReviewState{ProjectID: p, Repo: r, Number: n}
	}
	if f.rows[k].PendingHeadSHA != expectedPending {
		return persistence.ClosingOutcome{PendingHeadSHA: f.rows[k].PendingHeadSHA}, nil
	}
	f.rows[k].TaskID = ""
	f.rows[k].ReviewingHeadSHA = sha
	return persistence.ClosingOutcome{Applied: true}, nil
}

func (f *fakeReviewState) MarkReviewed(_ context.Context, p, r string, n int, sha string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(p, r, n)
	if f.rows[k] == nil {
		f.rows[k] = &persistence.ForgePRReviewState{ProjectID: p, Repo: r, Number: n}
	}
	f.rows[k].LastReviewedHeadSHA = sha
	f.rows[k].LastReviewedAt = &at
	f.rows[k].TaskID = ""
	return nil
}

func (f *fakeReviewState) SetPaused(_ context.Context, p, r string, n int, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(p, r, n)
	if f.rows[k] == nil {
		f.rows[k] = &persistence.ForgePRReviewState{ProjectID: p, Repo: r, Number: n}
	}
	f.rows[k].AutoReviewPaused = paused
	return nil
}

func syncEvent(delivery, headSHA string) github.TaskCreationEvent {
	return github.TaskCreationEvent{
		Kind:           "pull_request.synchronize",
		SessionID:      "acme/api#pulls/12",
		Title:          "PR title",
		SenderLogin:    "alice",
		Repo:           "acme/api",
		Number:         12,
		HeadSHA:        headSHA,
		InstallationID: 9001,
		IdempotencyKey: "github-app:" + delivery,
	}
}

// A push burst must produce ONE review, not one per push. This is the test that
// fails if coalescing is removed, and the cost multiplier it prevents is the
// whole reason synchronize was originally dropped.
func TestCoalesce_PushBurstWhileReviewing_ProducesOneTask(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	for i, sha := range []string{"sha-1", "sha-2", "sha-3", "sha-4"} {
		if err := g.Create(context.Background(), syncEvent(string(rune('a'+i)), sha)); err != nil {
			t.Fatalf("Create(%s): %v", sha, err)
		}
	}

	if got := len(repo.snapshotTasks()); got != 1 {
		t.Fatalf("4 pushes produced %d tasks, want 1 — coalescing is not holding", got)
	}
	// The in-flight review must end up pointed at the NEWEST head, not the one
	// that happened to create the task.
	st, _ := state.Get(context.Background(), "p-rr", "acme/api", 12)
	if st.PendingHeadSHA != "sha-4" {
		t.Errorf("PendingHeadSHA = %q, want sha-4 — a supersede must advance the head", st.PendingHeadSHA)
	}
}

// The claim is a POINTER, not a boolean: once the task holding it is terminal,
// the next push must enqueue rather than supersede into a finished review.
func TestCoalesce_AfterTheReviewFinishes_NextPushEnqueues(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	if err := g.Create(context.Background(), syncEvent("d1", "sha-1")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	tasks[0].Status = persistence.TaskStatusCompleted

	if err := g.Create(context.Background(), syncEvent("d2", "sha-2")); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 2 {
		t.Fatalf("tasks = %d, want 2 — a push after the review finished must enqueue", got)
	}
}

// THE DROPPED-PUSH FAILURE. An ABSORBING task that dies without releasing its
// claim must not wedge the PR: every later push would supersede into a corpse
// and the PR would never be reviewed again until a daemon restart. §5.2 makes
// the claim derived, so a dead task reads as absent.
func TestCoalesce_DeadAbsorbingTaskDoesNotWedgeThePR(t *testing.T) {
	for _, st := range []persistence.TaskStatus{
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
	} {
		t.Run(string(st), func(t *testing.T) {
			repo := newRecordingTaskRepo()
			state := newFakeReviewState()
			g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
			g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

			if err := g.Create(context.Background(), syncEvent("d1", "sha-1")); err != nil {
				t.Fatalf("first Create: %v", err)
			}
			// The task dies WITHOUT anything clearing the claim.
			repo.snapshotTasks()[0].Status = st

			if err := g.Create(context.Background(), syncEvent("d2", "sha-2")); err != nil {
				t.Fatalf("second Create: %v", err)
			}
			if got := len(repo.snapshotTasks()); got != 2 {
				t.Fatalf("tasks = %d, want 2 — a dead claim holder wedged the PR", got)
			}
		})
	}
}

// A claim naming a task that no longer exists at all (pruned by retention) must
// read as absent for the same reason.
func TestCoalesce_ClaimNamingAMissingTaskEnqueues(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	// A claim pointing at a task id the store has never heard of.
	if err := state.SetTask(context.Background(), "p-rr", "acme/api", 12, "task-vanished"); err != nil {
		t.Fatalf("SetTask: %v", err)
	}
	if err := g.Create(context.Background(), syncEvent("d1", "sha-1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 1 {
		t.Fatalf("tasks = %d, want 1 — a claim naming a vanished task must not absorb", got)
	}
}

// Coalescing is for the AUTOMATIC triggers. A first review (opened) must still
// create its task, and must take the claim so the pushes that follow coalesce.
func TestCoalesce_OpenedTakesTheClaim(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	ev := syncEvent("d-open", "sha-1")
	ev.Kind = "pull_request.opened"
	if err := g.Create(context.Background(), ev); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	st, _ := state.Get(context.Background(), "p-rr", "acme/api", 12)
	if st == nil || st.TaskID != tasks[0].ID {
		t.Fatalf("claim = %+v, want it pointing at the created task %s", st, tasks[0].ID)
	}
}

// An issue task must not touch PR review state at all — it has no PR.
func TestCoalesce_IssueTaskDoesNotTouchReviewState(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		SessionID:      "acme/api#issues/7",
		Title:          "Issue",
		Labels:         []string{"vornik-task"},
		SenderLogin:    "alice",
		Repo:           "acme/api",
		Number:         7,
		InstallationID: 9001,
		IdempotencyKey: "github-app:d-issue",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if st, _ := state.Get(context.Background(), "p-rr", "acme/api", 7); st != nil {
		t.Fatalf("an issue event created PR review state: %+v", st)
	}
}

// Without a state repository wired, coalescing must DEGRADE TO PHASE 1 (every
// trigger enqueues) rather than dropping reviews. Failing closed here would
// silently stop reviewing PRs on any deployment that had not wired it.
func TestCoalesce_NoStateRepository_StillCreatesTasks(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	// reviewState deliberately nil.
	if err := g.Create(context.Background(), syncEvent("d1", "sha-1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 1 {
		t.Fatalf("tasks = %d, want 1 — no state repo must degrade to always-enqueue", got)
	}
}

// The double must obey the same miss contract as the real repositories, or the
// coalescing tests above prove something about a fiction. ForgePRReviewState
// returns (nil, nil) for an absent PR — if the double returned an error instead,
// every "no claim yet" path here would be exercising the wrong branch.
func TestFakeReviewState_ObeysTheMissContract(t *testing.T) {
	f := newFakeReviewState()
	repotest.AssertMiss(t, "ForgePRReviewStateRepository.Get", func() (*persistence.ForgePRReviewState, error) {
		return f.Get(context.Background(), "p-absent", "acme/absent", 4242)
	})
}

// The claim must point at the SPECIFIC task just created, and the next push must
// absorb onto that same task. TestCoalesce_OpenedTakesTheClaim proves the first
// half and the burst test proves the second, but nothing tied them together —
// a claim recording the wrong id would still coalesce, just onto the wrong
// review, and both existing tests would pass.
func TestCoalesce_ClaimPointsAtTheCreatedTaskAndAbsorbsOntoIt(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	if err := g.Create(context.Background(), syncEvent("d1", "sha-1")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	created := repo.snapshotTasks()
	if len(created) != 1 {
		t.Fatalf("tasks = %d, want 1", len(created))
	}
	st, _ := state.Get(context.Background(), "p-rr", "acme/api", 12)
	if st == nil || st.TaskID != created[0].ID {
		t.Fatalf("claim = %v, want it naming the created task %s", st, created[0].ID)
	}

	// The second push must be absorbed BY THAT TASK — no new task, and the
	// claim still naming the same one.
	if err := g.Create(context.Background(), syncEvent("d2", "sha-2")); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 1 {
		t.Fatalf("tasks = %d after the second push, want 1", got)
	}
	st, _ = state.Get(context.Background(), "p-rr", "acme/api", 12)
	if st.TaskID != created[0].ID {
		t.Errorf("claim moved to %q, want it still naming %s", st.TaskID, created[0].ID)
	}
	if st.PendingHeadSHA != "sha-2" {
		t.Errorf("PendingHeadSHA = %q, want sha-2", st.PendingHeadSHA)
	}
}

// A command is a review request and must route like one. Without this it lands
// in the else-branch and gets answered by the CONVERSATIONAL workflow — the
// §1.3 trap, reached by a different door.
func TestCommand_RoutesToTheReviewWorkflow(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())

	ev := syncEvent("d-cmd", "sha-1")
	ev.Kind = "pull_request.comment_command"
	ev.OnDemand = true
	// A trusted author, stated rather than defaulted: an on-demand job whose
	// author has no standing in the repository is REFUSED by the coordinator
	// (2026-09-03 audit), so a command fixture that omits this is testing a
	// request that would never have been allowed to run.
	ev.AuthorAssociation = "OWNER"
	if err := g.Create(context.Background(), ev); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	var payload map[string]any
	_ = json.Unmarshal(tasks[0].Payload, &payload)
	if got := payload["workflowId"]; got != "github-review" {
		t.Errorf("workflowId = %v, want github-review", got)
	}
	if got := payload["taskType"]; got != pullRequestReviewTaskType {
		t.Errorf("taskType = %v, want %q", got, pullRequestReviewTaskType)
	}
}

// A human asking a second time is asking for a fresh answer. Coalescing a
// command away would look broken (§5).
func TestCommand_IsNeverCoalescedAway(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	// A push takes the claim...
	if err := g.Create(context.Background(), syncEvent("d1", "sha-1")); err != nil {
		t.Fatalf("push Create: %v", err)
	}
	// ...and a command arrives while that review is still running.
	cmd := syncEvent("d2", "sha-1")
	cmd.Kind = "pull_request.comment_command"
	cmd.OnDemand = true
	cmd.AuthorAssociation = "OWNER"
	if err := g.Create(context.Background(), cmd); err != nil {
		t.Fatalf("command Create: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 2 {
		t.Fatalf("tasks = %d, want 2 — an explicit request was silently absorbed", got)
	}
}

// pause suppresses the AUTOMATIC triggers only. A human asking explicitly must
// still get a review, or pause becomes a trap the operator cannot escape from
// the PR thread.
func TestPause_SuppressesPushesButNotCommands(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	if err := g.SetAutoReviewPaused(context.Background(), "acme/api", 12, true); err != nil {
		t.Fatalf("SetAutoReviewPaused: %v", err)
	}
	if err := g.Create(context.Background(), syncEvent("d1", "sha-1")); err != nil {
		t.Fatalf("push Create: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 0 {
		t.Fatalf("tasks = %d while paused, want 0", got)
	}

	cmd := syncEvent("d2", "sha-2")
	cmd.Kind = "pull_request.comment_command"
	cmd.OnDemand = true
	cmd.AuthorAssociation = "OWNER"
	if err := g.Create(context.Background(), cmd); err != nil {
		t.Fatalf("command Create: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 1 {
		t.Fatalf("tasks = %d after an explicit command while paused, want 1", got)
	}

	// And resume restores the automatic path. The command's review must be
	// finished first: otherwise the push correctly COALESCES onto it, and this
	// would be testing coalescing rather than resume.
	repo.snapshotTasks()[0].Status = persistence.TaskStatusCompleted
	if err := g.SetAutoReviewPaused(context.Background(), "acme/api", 12, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := g.Create(context.Background(), syncEvent("d3", "sha-3")); err != nil {
		t.Fatalf("push after resume: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 2 {
		t.Fatalf("tasks = %d after resume, want 2 — resume did not restore the automatic path", got)
	}
}

// forgeJobFromEvent hard-coded pull_request.opened as the only change-request
// kind, so every re-review trigger built a job with IsChangeRequest=false and an
// EMPTY action. The forge handlers read those, so a synchronize review would
// have run against a job that did not describe a pull request at all. Third
// instance of the §1.3 class, in a place the first two fixes did not reach.
func TestForgeJob_EveryReviewKindIsAChangeRequest(t *testing.T) {
	for _, kind := range []string{
		"pull_request.opened",
		"pull_request.reopened",
		"pull_request.ready_for_review",
		"pull_request.synchronize",
		"pull_request.comment_command",
	} {
		t.Run(kind, func(t *testing.T) {
			job := forgeJobFromEvent(github.TaskCreationEvent{
				Kind: kind, Repo: "acme/api", Number: 12, HeadSHA: "sha-1",
			})
			if !job.IsChangeRequest {
				t.Errorf("IsChangeRequest = false for %s — the forge handlers will not treat it as a PR", kind)
			}
			if job.Action == "" {
				t.Errorf("Action is empty for %s", kind)
			}
			if job.HeadSHA != "sha-1" {
				t.Errorf("HeadSHA = %q, want sha-1 — the incremental baseline cannot work without it", job.HeadSHA)
			}
		})
	}
}

// The full-review flag has to survive into the job, or the command does nothing.
func TestForgeJob_CarriesTheFullReviewFlag(t *testing.T) {
	job := forgeJobFromEvent(github.TaskCreationEvent{
		Kind: "pull_request.comment_command", Repo: "acme/api", Number: 12, FullReview: true,
	})
	if !job.FullReview {
		t.Error("FullReview did not survive into the forge job")
	}
}

// THE AUTHORIZATION INPUTS — regression for the 2026-09-03 four-week audit's P1.
//
// The shared coordinator refuses an OnDemand job whose author has no standing
// in the repository, and it reads BOTH facts off the job. This function set
// neither, so every command arriving through the GitHub App channel reached the
// coordinator looking like an automatic trigger and was reviewed regardless of
// who asked.
func TestForgeJob_CarriesTheAuthorizationInputs(t *testing.T) {
	for assoc, wantTrusted := range map[string]bool{
		"OWNER":        true,
		"MEMBER":       true,
		"COLLABORATOR": true,
		"collaborator": true,
		// CONTRIBUTOR describes the past, not permission to spend.
		"CONTRIBUTOR": false,
		"NONE":        false,
		// Absent / unrecognised fails closed.
		"":       false,
		"FUTURE": false,
	} {
		t.Run(assoc, func(t *testing.T) {
			job := forgeJobFromEvent(github.TaskCreationEvent{
				Kind: "pull_request.comment_command", Repo: "acme/api", Number: 12,
				OnDemand: true, AuthorAssociation: assoc,
			})
			if !job.OnDemand {
				t.Fatal("OnDemand did not survive into the forge job — the gate only applies to on-demand jobs, " +
					"so losing this flag disables the gate entirely")
			}
			if job.AuthorIsTrusted != wantTrusted {
				t.Errorf("AuthorIsTrusted = %v for author_association=%q, want %v", job.AuthorIsTrusted, assoc, wantTrusted)
			}
		})
	}
}

// A push has no comment and no author standing. It must stay OnDemand=false, or
// the author gate would refuse every automatic review.
func TestForgeJob_PushIsNotOnDemand(t *testing.T) {
	job := forgeJobFromEvent(github.TaskCreationEvent{
		Kind: "pull_request.synchronize", Repo: "acme/api", Number: 12, HeadSHA: "sha-1",
	})
	if job.OnDemand {
		t.Fatal("a synchronize job is OnDemand — it would be gated on an author standing it can never carry")
	}
	if job.AuthorIsTrusted {
		t.Error("a synchronize job claims a trusted author; there is no comment behind it")
	}
}

// Two humans asking at once must BOTH be served, while a redelivery of one
// request must not run twice. Both properties rest on the same thing: the
// idempotency key is "github-app:" + the webhook delivery id, and GitHub gives
// every delivery its own id.
//
// Worth pinning because the two failure directions are very different. If
// distinct commands collided on a key, the second human would ask and nothing
// would happen — the exact silence this feature exists to remove. If a
// redelivery did NOT collide, GitHub's retries would post duplicate reviews.
func TestCommand_DistinctDeliveriesBothRun_RedeliveryDoesNot(t *testing.T) {
	repo := newRecordingTaskRepo()
	state := newFakeReviewState()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	g.review = forgereview.New(state, taskStatusReader{tasks: repo}, zerolog.Nop())

	cmd := func(delivery string) github.TaskCreationEvent {
		ev := syncEvent(delivery, "sha-1")
		ev.Kind = "pull_request.comment_command"
		ev.OnDemand = true
		ev.AuthorAssociation = "OWNER"
		return ev
	}

	// Two people, two deliveries: both get a review.
	if err := g.Create(context.Background(), cmd("alice-delivery")); err != nil {
		t.Fatalf("first command: %v", err)
	}
	if err := g.Create(context.Background(), cmd("bob-delivery")); err != nil {
		t.Fatalf("second command: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 2 {
		t.Fatalf("tasks = %d for two distinct commands, want 2 — one human was silently ignored", got)
	}

	// GitHub redelivering Alice's comment must NOT produce a third.
	if err := g.Create(context.Background(), cmd("alice-delivery")); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := len(repo.snapshotTasks()); got != 2 {
		t.Fatalf("tasks = %d after a redelivery, want 2 — GitHub's retries would post duplicate reviews", got)
	}
}
