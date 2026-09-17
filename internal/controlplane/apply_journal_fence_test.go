package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// revocableGate is an EpochVerifier a test can revoke at an exact moment,
// rather than after a counted number of calls. The fence defects this file
// covers happen at specific TRANSITIONS — the terminal ledger commit, a
// reversal — so the probe has to fire between them, not at call N.
type revocableGate struct {
	revoked bool
	calls   int
}

func (g *revocableGate) VerifyEpoch(context.Context) (bool, int64, error) {
	g.calls++
	if g.revoked {
		return false, 2, nil
	}
	return true, 1, nil
}

// Regression: re-audit 2026-09-15 CA-22 — "journal terminal commits and
// reversals bypass the writer fence".
//
// The fence was consulted at PREPARE and before each forward write, and
// nowhere else. A writer that lost the lease DURING Reload — the longest
// window in the whole apply — still ran MarkAppliedWithLedger, so a superseded
// process committed the authoritative ledger after another daemon had taken
// over as the config writer.
//
// THE SEAM, which is what this asserts: the fence protects FILE WRITES and not
// the AUTHORITATIVE TRANSITION. Every forward write can be correctly fenced and
// the ledger still ends up written by a process that is no longer the writer,
// so a test that only counts pre-write fence checks passes while the defect is
// live. The assertion is therefore on the ledger and journal state after a
// mid-reload takeover, not on the number of fence calls.
func TestJournal_LeaseLostDuringReload_DoesNotCommitApplied(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	gate := &revocableGate{}
	env.e.LeaderGate = gate
	env.e.WriterEpoch = func() int64 { return 1 }
	// The takeover lands while this process is reloading: writes are done,
	// the ledger is not.
	env.e.Reload = func() error {
		gate.revoked = true
		return nil
	}

	err := env.e.Apply(context.Background(), id, "vadim", false)
	if err == nil {
		t.Fatal("a writer that lost the lease during reload must not report a successful apply")
	}
	if !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("want ErrWriterFenced, got %v", err)
	}
	if st := env.proposalStatus(t, id); st == persistence.ProposalStatusApplied {
		t.Fatal("a superseded writer committed the authoritative ledger as APPLIED")
	}
	j := env.latestJournal(t, id)
	if j.State == persistence.JournalStateApplied {
		t.Fatalf("journal committed APPLIED after the lease was lost: %+v", j)
	}
}

// The same seam on the way back: a reversal WRITES FILES, so a superseded
// process must not be able to reverse content another writer now owns.
//
// Regression: re-audit 2026-09-15 CA-22 — "restoreOne and workspace-context
// Rollback likewise write without a fence, so a superseded process can reverse
// content after another writer takes over."
func TestJournal_LeaseLostBeforeRevert_DoesNotRestoreFiles(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	gate := &revocableGate{}
	env.e.LeaderGate = gate
	env.e.WriterEpoch = func() int64 { return 1 }
	// Reload rejects (which normally triggers a revert) AND the lease is
	// lost at the same moment — the revert must not write.
	env.e.Reload = func() error {
		gate.revoked = true
		return errors.New("bad yaml")
	}

	err := env.e.Apply(context.Background(), id, "vadim", false)
	if err == nil {
		t.Fatal("apply must fail when reload rejects")
	}
	j := env.latestJournal(t, id)
	if j.State == persistence.JournalStateReverted {
		t.Fatalf("a superseded writer performed a reversal: %+v", j)
	}
	if !strings.Contains(strings.ToLower(j.Failure), "fence") && !strings.Contains(strings.ToLower(j.Failure), "leader") && !strings.Contains(strings.ToLower(j.Failure), "epoch") {
		t.Fatalf("the row must record that the fence stopped the reversal, for the current leader to resolve: %+v", j)
	}
}

// What else rides this seam (tenet 4.5): recovery also writes and reverses.
// A daemon that lost the lease between reading the open row and acting on it
// must not roll a bundle forward.
func TestJournal_RecoveryRespectsTheFenceBeforeCommitting(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	gate := &revocableGate{}
	env.e.LeaderGate = gate
	env.e.WriterEpoch = func() int64 { return 1 }
	env.crashAt("write:config.yaml", true, nil)

	// Crash mid-bundle, leaving an open row.
	if err := env.e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("seeded crash should have failed the apply")
	}
	env.e.crashAfter = nil

	// A different daemon has taken the lease by the time we reconcile.
	gate.revoked = true
	err := env.e.Reconcile(context.Background())
	if err == nil {
		t.Fatal("reconcile must refuse to act without the writer lease")
	}
	j := env.latestJournal(t, id)
	if j.State == persistence.JournalStateApplied {
		t.Fatalf("a fenced-out daemon completed recovery: %+v", j)
	}
}

// Regression: re-audit 2026-09-15 CA-21 — "journal recovery ignores drift in
// read-only dependencies".
//
// The forward path validates Evidence.read_set twice (§4 passes 1 and 2), and
// PREPARE persists it onto the row precisely so recovery can re-check it. It
// never did: reconcileApplying redid the remaining writes and committed
// APPLIED against a dependency that had changed while the daemon was down.
//
// THE SEAM: the read-set check lives on the FORWARD path only, so every
// forward-path test passes while recovery — the path that runs after a crash,
// when an operator is most likely to have hand-edited something — skips it
// entirely. The proposal was computed from content that no longer exists, and
// recovery is exactly where "never overwrite a hand edit" (§1.4) has to hold.
func TestJournal_RecoveryRefusesWhenAReadDependencyChanged(t *testing.T) {
	env := newJournalEnv(t)
	// The bundle's read set names a swarm file it does NOT write.
	env.write(t, "swarms/dev.md", "---\nswarmId: dev\n---\n")
	dep := hashBytes([]byte("---\nswarmId: dev\n---\n"))
	id := env.seed(t, `{"read_set":{"swarms/dev.md":"`+dep+`"}}`)

	// Crash after the first rename: the row stays open, mid-bundle.
	env.crashAt("write:projects/a.yaml", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("seeded crash should have failed the apply")
	}
	env.e.crashAfter = nil

	// While the daemon was down, an operator edited the dependency.
	env.write(t, "swarms/dev.md", "---\nswarmId: dev\nleadRole: reviewer\n---\n")

	err := env.e.Reconcile(context.Background())
	if err == nil {
		t.Fatal("recovery completed a bundle whose read dependency had changed since it was computed")
	}
	j := env.latestJournal(t, id)
	if j.State == persistence.JournalStateApplied {
		t.Fatalf("recovery committed APPLIED against a changed dependency: %+v", j)
	}
	if st := env.proposalStatus(t, id); st == persistence.ProposalStatusApplied {
		t.Fatal("the ledger recorded APPLIED against a changed dependency")
	}
}

// The counterpart: recovery MUST still complete when the dependency is
// unchanged, or the check would turn every crash into a stuck bundle.
func TestJournal_RecoveryCompletesWhenDependenciesHold(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "swarms/dev.md", "---\nswarmId: dev\n---\n")
	dep := hashBytes([]byte("---\nswarmId: dev\n---\n"))
	id := env.seed(t, `{"read_set":{"swarms/dev.md":"`+dep+`"}}`)

	env.crashAt("write:projects/a.yaml", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("seeded crash should have failed the apply")
	}
	env.e.crashAfter = nil

	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("recovery must roll forward when every dependency still holds: %v", err)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApplied {
		t.Fatalf("proposal should be APPLIED after a clean recovery, got %s", st)
	}
}
