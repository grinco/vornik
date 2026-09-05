package forgereview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The coordinator holds the rules BOTH forge ingresses obey: the per-PR pause
// gate and the coalescing decision. It is keyed on forge.ForgeJob and contains
// no provider types, so a GitLab ingress inherits all of it the moment its
// ClassifyEvent lands.
//
// Design: https://docs.vornik.io
// §5.2, §7, §13.3.

type fakeState struct {
	row     *persistence.ForgePRReviewState
	getErr  error
	claimed string
	priorID string
	setTo   []string
	paused  []bool
}

func (f *fakeState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	return f.row, f.getErr
}
func (f *fakeState) ClaimOrSupersede(_ context.Context, _, _ string, _ int, sha string) (string, error) {
	f.claimed = sha
	return f.priorID, nil
}
func (f *fakeState) SetTask(_ context.Context, _, _ string, _ int, id string) error {
	f.setTo = append(f.setTo, id)
	return nil
}
func (f *fakeState) BeginClosing(context.Context, string, string, int, string, string) (persistence.ClosingOutcome, error) {
	return persistence.ClosingOutcome{Applied: true}, nil
}
func (f *fakeState) MarkReviewed(context.Context, string, string, int, string, time.Time) error {
	return nil
}
func (f *fakeState) SetPaused(_ context.Context, _, _ string, _ int, p bool) error {
	f.paused = append(f.paused, p)
	return nil
}

type fakeTasks struct {
	status  persistence.TaskStatus
	missing bool
	err     error
}

func (f *fakeTasks) TaskStatus(context.Context, string) (persistence.TaskStatus, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	if f.missing {
		return "", false, nil
	}
	return f.status, true, nil
}

func reviewJob() forgeapi.ForgeJob {
	return forgeapi.ForgeJob{
		Provider: forgeapi.ProviderGitHub, Repo: "acme/api", Number: 12,
		Action: "synchronize", IsChangeRequest: true, HeadSHA: "sha-new",
	}
}

// commandJob is reviewJob as an on-demand request from someone with standing.
// Every onDemand=true case uses it, because an on-demand job WITHOUT a trusted
// author is now refused outright — that is the gate, not an incidental detail
// of the fixture.
func commandJob() forgeapi.ForgeJob {
	j := reviewJob()
	j.Action = "created"
	j.OnDemand = true
	j.Command = "review"
	j.AuthorIsTrusted = true
	return j
}

func newCoord(st StateStore, tk TaskStatusReader) *Coordinator {
	return New(st, tk, zerolog.Nop())
}

// Nothing in flight → enqueue.
func TestDecide_NoClaim_Enqueues(t *testing.T) {
	st := &fakeState{}
	d := newCoord(st, &fakeTasks{}).Decide(context.Background(), "p1", reviewJob(), false)
	if d.Skip {
		t.Fatalf("Skip = true with no claim: %s", d.Reason)
	}
	if st.claimed != "sha-new" {
		t.Errorf("claimed %q, want the delivery's head sha-new", st.claimed)
	}
}

// A live claim absorbs the push.
func TestDecide_LiveClaim_Supersedes(t *testing.T) {
	st := &fakeState{priorID: "task-1"}
	d := newCoord(st, &fakeTasks{status: persistence.TaskStatusRunning}).
		Decide(context.Background(), "p1", reviewJob(), false)
	if !d.Skip {
		t.Fatal("Skip = false while a review is in flight — the burst is not coalescing")
	}
}

// A claim held by a task that can no longer review must NOT wedge the PR. The
// claim is derived, so a dead holder reads as absent.
func TestDecide_DeadOrMissingHolder_Enqueues(t *testing.T) {
	for name, tasks := range map[string]*fakeTasks{
		"failed":     {status: persistence.TaskStatusFailed},
		"cancelled":  {status: persistence.TaskStatusCancelled},
		"completed":  {status: persistence.TaskStatusCompleted},
		"closed":     {status: persistence.TaskStatusClosed},
		"missing":    {missing: true},
		"unreadable": {err: errors.New("db down")},
	} {
		t.Run(name, func(t *testing.T) {
			d := newCoord(&fakeState{priorID: "task-1"}, tasks).
				Decide(context.Background(), "p1", reviewJob(), false)
			if d.Skip {
				t.Fatalf("Skip = true for a %s holder — the PR is wedged until a daemon restart", name)
			}
		})
	}
}

// An explicit human request is never absorbed.
func TestDecide_OnDemand_NeverSkips(t *testing.T) {
	st := &fakeState{priorID: "task-1"}
	d := newCoord(st, &fakeTasks{status: persistence.TaskStatusRunning}).
		Decide(context.Background(), "p1", commandJob(), true)
	if d.Skip {
		t.Fatal("an explicit request was coalesced away")
	}
}

// Pause suppresses automatic triggers only.
func TestDecide_Paused(t *testing.T) {
	st := &fakeState{row: &persistence.ForgePRReviewState{AutoReviewPaused: true}}
	if d := newCoord(st, &fakeTasks{}).Decide(context.Background(), "p1", reviewJob(), false); !d.Skip {
		t.Fatal("an automatic trigger ran while the PR was paused")
	}
	if d := newCoord(st, &fakeTasks{}).Decide(context.Background(), "p1", commandJob(), true); d.Skip {
		t.Fatal("an explicit request was blocked by pause — the operator cannot escape it from the PR thread")
	}
}

// An unreadable pause flag must read as NOT paused: a phantom pause costs every
// review on the PR, a lost one costs a single unwanted review.
func TestDecide_PauseUnreadable_TreatedAsNotPaused(t *testing.T) {
	st := &fakeState{getErr: errors.New("db down")}
	if d := newCoord(st, &fakeTasks{}).Decide(context.Background(), "p1", reviewJob(), false); d.Skip {
		t.Fatal("an unreadable pause flag silenced the PR")
	}
}

// A non-review job (a labelled issue) is not the coordinator's business.
func TestDecide_NotAChangeRequest_Enqueues(t *testing.T) {
	job := reviewJob()
	job.IsChangeRequest = false
	st := &fakeState{priorID: "task-1"}
	d := newCoord(st, &fakeTasks{status: persistence.TaskStatusRunning}).
		Decide(context.Background(), "p1", job, false)
	if d.Skip {
		t.Fatal("an issue task was coalesced against PR review state")
	}
	if st.claimed != "" {
		t.Error("an issue event touched PR review state")
	}
}

// No store wired → degrade to always-enqueue, never to silence.
func TestDecide_NoStore_Enqueues(t *testing.T) {
	if d := newCoord(nil, nil).Decide(context.Background(), "p1", reviewJob(), false); d.Skip {
		t.Fatal("no state store must degrade to always-enqueue")
	}
}

// The double must obey the same miss contract as the real repositories, or the
// "no state yet" paths above exercise the wrong branch. ForgePRReviewState
// returns (nil, nil) for a PR it has never seen.
func TestFakeState_ObeysTheMissContract(t *testing.T) {
	f := &fakeState{}
	repotest.AssertMiss(t, "ForgePRReviewStateRepository.Get", func() (*persistence.ForgePRReviewState, error) {
		return f.Get(context.Background(), "p", "o/r", 1)
	})
}

// THE AUTHOR-TRUST FLOOR — regression for the 2026-09-03 four-week audit's P1.
//
// The refusal for a command from someone with no standing in the repository
// lived in ONE ingress (internal/api/webhook_handlers.go, 9ecedb2c) while the
// GitHub App channel dispatched the identical command grammar with no author
// gate at all. Both ingresses call Decide, so the rule belongs here: an ingress
// added later inherits it instead of having to remember it.
func TestDecide_OnDemand_FromAnUntrustedAuthor_Refuses(t *testing.T) {
	job := commandJob()
	job.AuthorIsTrusted = false

	d := newCoord(&fakeState{}, &fakeTasks{}).Decide(context.Background(), "p1", job, true)
	if !d.Skip {
		t.Fatal("a command from an author without repository standing was enqueued — denial-of-wallet on a public repo")
	}
	if d.Reason != "author_untrusted" {
		t.Errorf("Reason = %q, want author_untrusted", d.Reason)
	}
}

// AUTHORIZATION IS NOT COALESCING, so it does not inherit "every uncertainty
// resolves to enqueue". A deployment with no review state configured still must
// not review on a stranger's say-so — and a nil Coordinator is exactly the
// shape a CE deployment without the state store has.
func TestDecide_OnDemand_FromAnUntrustedAuthor_RefusesEvenWithNoState(t *testing.T) {
	job := commandJob()
	job.AuthorIsTrusted = false

	for name, c := range map[string]*Coordinator{
		"nil coordinator": nil,
		"nil state":       New(nil, nil, zerolog.Nop()),
	} {
		t.Run(name, func(t *testing.T) {
			if d := c.Decide(context.Background(), "p1", job, true); !d.Skip {
				t.Fatalf("%s enqueued an untrusted command; the missing-store early return must not open the gate", name)
			}
		})
	}
}

// The gate is on COMMANDS only. A push carries no author standing — there is no
// comment and nobody to vouch for — so gating automatic triggers on it would
// stop reviewing pull requests altogether.
func TestDecide_AutomaticTrigger_IsNotGatedOnAuthorTrust(t *testing.T) {
	job := reviewJob() // AuthorIsTrusted false, as every push job is
	if d := newCoord(&fakeState{}, &fakeTasks{}).Decide(context.Background(), "p1", job, false); d.Skip {
		t.Fatalf("an automatic trigger was refused as untrusted (%q) — no pull request would ever be reviewed", d.Reason)
	}
}
