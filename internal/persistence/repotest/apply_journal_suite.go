package repotest

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// RunApplyJournalSuite is the backend-agnostic contract for
// persistence.ApplyJournalRepository (LLD 2026-09-13 config-apply-journal
// §2/§3/§7). It takes the proposal ledger too because MarkAppliedWithLedger
// spans both tables and its atomicity is the property under test. Both
// Postgres and SQLite run it.
func RunApplyJournalSuite(t *testing.T, journal persistence.ApplyJournalRepository, proposals persistence.ProposalRepository) {
	t.Helper()
	t.Run("Prepare_then_Get_round_trips", func(t *testing.T) { journalRoundTrip(t, journal) })
	t.Run("Prepare_second_open_row_is_ErrDuplicateKey", func(t *testing.T) { journalDuplicateOpen(t, journal) })
	t.Run("Prepare_rejects_oversized_row", func(t *testing.T) { journalOversized(t, journal) })
	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) {
		AssertMissRepo(t, "ApplyJournalRepository.Get", journal.Get)
	})
	t.Run("OpenForProposal_none_open_is_ErrNotFound", func(t *testing.T) { journalOpenForProposal(t, journal) })
	t.Run("ListOpen_excludes_terminal_rows", func(t *testing.T) { journalListOpen(t, journal) })
	t.Run("Transition_is_compare_and_set", func(t *testing.T) { journalTransitionCAS(t, journal) })
	t.Run("Transition_patch_columns", func(t *testing.T) { journalTransitionPatch(t, journal) })
	t.Run("AppendProgress_keeps_write_order", func(t *testing.T) { journalAppendProgress(t, journal) })
	t.Run("MarkAppliedWithLedger_commits_both", func(t *testing.T) { journalLedgerCommit(t, journal, proposals) })
	t.Run("MarkAppliedWithLedger_pending_restart", func(t *testing.T) { journalLedgerPendingRestart(t, journal, proposals) })
	t.Run("MarkAppliedWithLedger_refuses_unapproved_and_writes_nothing", func(t *testing.T) {
		journalLedgerNotApproved(t, journal, proposals)
	})
	t.Run("MarkAppliedWithLedger_refuses_non_verifying_and_writes_nothing", func(t *testing.T) {
		journalLedgerNotVerifying(t, journal, proposals)
	})
}

// newTestJournal builds a PREPARED-shaped row for proposalID with one create
// and one replace op, a pre-image for each, and a read dependency.
func newTestJournal(proposalID string) *persistence.ConfigApplyJournal {
	return &persistence.ConfigApplyJournal{
		ID:               uniqueID("caj"),
		ProposalID:       proposalID,
		RequestID:        "req-" + proposalID,
		OpDigest:         "sha256:ops",
		DeploymentRoot:   "/deploy/configs",
		AffectedProjects: []string{"assistant", "janka"},
		WriterEpoch:      7,
		Ops: []persistence.JournalOp{
			{Op: "create", Path: "projects/new.yaml", ContentSHA256: "aaaa"},
			{Op: "replace", Path: "config.yaml", ContentSHA256: "bbbb"},
		},
		PreImages: map[string]persistence.JournalPreImage{
			"projects/new.yaml": {Existed: false},
			"config.yaml":       {Existed: true, SHA256: "cccc", Content: "server:\n  address: :8080\n"},
		},
		ReadSet:          map[string]string{"swarms/shared.yaml": "dddd", "projects/gone.yaml": persistence.JournalReadSetAbsent},
		GenerationBefore: "gen-1",
	}
}

func mustPrepare(t *testing.T, repo persistence.ApplyJournalRepository, row *persistence.ConfigApplyJournal) *persistence.ConfigApplyJournal {
	t.Helper()
	if err := repo.Prepare(context.Background(), row); err != nil {
		t.Fatalf("Prepare(%s): %v", row.ID, err)
	}
	return row
}

func mustTransition(t *testing.T, repo persistence.ApplyJournalRepository, id, from, to string) {
	t.Helper()
	if err := repo.Transition(context.Background(), id, from, to, persistence.JournalPatch{}); err != nil {
		t.Fatalf("Transition(%s, %s→%s): %v", id, from, to, err)
	}
}

func journalRoundTrip(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	in := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	got, err := repo.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != persistence.JournalStatePrepared || got.TerminalAt != nil {
		t.Fatalf("Prepare must land PREPARED and open, got state=%s terminal_at=%v", got.State, got.TerminalAt)
	}
	if got.SchemaVersion != persistence.JournalSchemaVersion {
		t.Errorf("schema_version defaulted to %d, want %d", got.SchemaVersion, persistence.JournalSchemaVersion)
	}
	if got.ProposalID != in.ProposalID || got.RequestID != in.RequestID || got.OpDigest != in.OpDigest ||
		got.DeploymentRoot != in.DeploymentRoot || got.WriterEpoch != 7 || got.GenerationBefore != "gen-1" {
		t.Errorf("scalar columns did not round-trip: %+v", got)
	}
	if got.GenerationAfter != "" || got.Failure != "" {
		t.Errorf("unset nullable columns must read back empty: %+v", got)
	}
	if len(got.AffectedProjects) != 2 || got.AffectedProjects[0] != "assistant" {
		t.Errorf("affected_projects did not round-trip: %v", got.AffectedProjects)
	}
	if len(got.Ops) != 2 || got.Ops[1].Path != "config.yaml" || got.Ops[1].ContentSHA256 != "bbbb" {
		t.Errorf("ops did not round-trip: %+v", got.Ops)
	}
	if pre := got.PreImages["config.yaml"]; !pre.Existed || pre.Content != "server:\n  address: :8080\n" || pre.SHA256 != "cccc" {
		t.Errorf("pre_images did not round-trip: %+v", got.PreImages)
	}
	if pre := got.PreImages["projects/new.yaml"]; pre.Existed || pre.Content != "" {
		t.Errorf("expected-absence pre-image did not round-trip: %+v", pre)
	}
	if got.ReadSet["projects/gone.yaml"] != persistence.JournalReadSetAbsent || got.ReadSet["swarms/shared.yaml"] != "dddd" {
		t.Errorf("read_set did not round-trip: %v", got.ReadSet)
	}
	if len(got.Progress) != 0 {
		t.Errorf("fresh row must have empty progress, got %v", got.Progress)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not defaulted: %+v", got)
	}
}

// journalDuplicateOpen pins the "one open journal per proposal" constraint
// (§2): a second open row collides, and a row for the same proposal is
// accepted again only once the first is terminal.
func journalDuplicateOpen(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	pid := uniqueID("cpp")
	first := mustPrepare(t, repo, newTestJournal(pid))
	if err := repo.Prepare(ctx, newTestJournal(pid)); !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("second open row for one proposal must be ErrDuplicateKey, got %v", err)
	}
	if err := repo.Transition(ctx, first.ID, persistence.JournalStatePrepared, persistence.JournalStateFailed,
		persistence.JournalPatch{Failure: "abort", Terminal: true}); err != nil {
		t.Fatalf("close first: %v", err)
	}
	if err := repo.Prepare(ctx, newTestJournal(pid)); err != nil {
		t.Fatalf("a new row after the first went terminal must be accepted, got %v", err)
	}
}

func journalOversized(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	row := newTestJournal(uniqueID("cpp"))
	row.Ops = make([]persistence.JournalOp, persistence.JournalMaxOps+1)
	if err := repo.Prepare(ctx, row); !errors.Is(err, persistence.ErrJournalTooLarge) {
		t.Fatalf("over-cap ops must be ErrJournalTooLarge, got %v", err)
	}
	row = newTestJournal(uniqueID("cpp"))
	row.PreImages["config.yaml"] = persistence.JournalPreImage{Existed: true, Content: string(make([]byte, persistence.ProposalMaxContentBytes+1))}
	if err := repo.Prepare(ctx, row); !errors.Is(err, persistence.ErrJournalTooLarge) {
		t.Fatalf("over-cap pre-image must be ErrJournalTooLarge, got %v", err)
	}
}

func journalOpenForProposal(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	AssertMiss(t, "ApplyJournalRepository.OpenForProposal", func() (*persistence.ConfigApplyJournal, error) {
		return repo.OpenForProposal(ctx, uniqueID("absent"))
	})
	row := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	got, err := repo.OpenForProposal(ctx, row.ProposalID)
	if err != nil || got.ID != row.ID {
		t.Fatalf("OpenForProposal = (%v, %v), want the open row %s", got, err, row.ID)
	}
	if err := repo.Transition(ctx, row.ID, persistence.JournalStatePrepared, persistence.JournalStateFailed,
		persistence.JournalPatch{Terminal: true}); err != nil {
		t.Fatalf("close: %v", err)
	}
	AssertMiss(t, "ApplyJournalRepository.OpenForProposal", func() (*persistence.ConfigApplyJournal, error) {
		return repo.OpenForProposal(ctx, row.ProposalID)
	})
}

func journalListOpen(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	open1 := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	closed := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	open2 := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	if err := repo.Transition(ctx, closed.ID, persistence.JournalStatePrepared, persistence.JournalStateFailed,
		persistence.JournalPatch{Terminal: true}); err != nil {
		t.Fatalf("close: %v", err)
	}
	// DRIFT is open: it must be listed so a reconciler/doctor can see it.
	drift := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	mustTransition(t, repo, drift.ID, persistence.JournalStatePrepared, persistence.JournalStateReverting)
	if err := repo.Transition(ctx, drift.ID, persistence.JournalStateReverting, persistence.JournalStateDrift,
		persistence.JournalPatch{Failure: "config.yaml matches neither image"}); err != nil {
		t.Fatalf("drift: %v", err)
	}
	rows, err := repo.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	seen := map[string]int{}
	for i, r := range rows {
		seen[r.ID] = i + 1
		if r.TerminalAt != nil {
			t.Errorf("ListOpen returned terminal row %s", r.ID)
		}
	}
	if seen[closed.ID] != 0 {
		t.Errorf("closed row %s must not be listed", closed.ID)
	}
	for _, want := range []string{open1.ID, open2.ID, drift.ID} {
		if seen[want] == 0 {
			t.Errorf("open row %s missing from ListOpen", want)
		}
	}
	if seen[open1.ID] > seen[open2.ID] {
		t.Errorf("ListOpen must be oldest-first: %s before %s", open1.ID, open2.ID)
	}
}

func journalTransitionCAS(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	row := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	// Wrong source state → ErrInvalidTransition, row untouched.
	err := repo.Transition(ctx, row.ID, persistence.JournalStateApplying, persistence.JournalStateVerifying, persistence.JournalPatch{})
	if !errors.Is(err, persistence.ErrInvalidTransition) {
		t.Fatalf("CAS from the wrong state must be ErrInvalidTransition, got %v", err)
	}
	got, _ := repo.Get(ctx, row.ID)
	if got.State != persistence.JournalStatePrepared {
		t.Fatalf("a refused CAS must leave the state alone, got %s", got.State)
	}
	// Right source state → applied.
	mustTransition(t, repo, row.ID, persistence.JournalStatePrepared, persistence.JournalStateApplying)
	got, _ = repo.Get(ctx, row.ID)
	if got.State != persistence.JournalStateApplying || got.TerminalAt != nil {
		t.Fatalf("expected APPLYING and open, got %s / %v", got.State, got.TerminalAt)
	}
	// Unknown id → ErrNotFound, not ErrInvalidTransition.
	err = repo.Transition(ctx, uniqueID("absent"), persistence.JournalStatePrepared, persistence.JournalStateFailed, persistence.JournalPatch{})
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown id must be ErrNotFound, got %v", err)
	}
}

func journalTransitionPatch(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	row := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	mustTransition(t, repo, row.ID, persistence.JournalStatePrepared, persistence.JournalStateApplying)
	if err := repo.Transition(ctx, row.ID, persistence.JournalStateApplying, persistence.JournalStateReverting,
		persistence.JournalPatch{Failure: "write failed for config.yaml"}); err != nil {
		t.Fatalf("→REVERTING: %v", err)
	}
	got, _ := repo.Get(ctx, row.ID)
	if got.Failure != "write failed for config.yaml" || got.TerminalAt != nil {
		t.Fatalf("REVERTING must record failure and stay open: %+v", got)
	}
	if err := repo.Transition(ctx, row.ID, persistence.JournalStateReverting, persistence.JournalStateReverted,
		persistence.JournalPatch{GenerationAfter: "gen-restored", Terminal: true}); err != nil {
		t.Fatalf("→REVERTED: %v", err)
	}
	got, _ = repo.Get(ctx, row.ID)
	if got.State != persistence.JournalStateReverted || got.TerminalAt == nil || got.GenerationAfter != "gen-restored" {
		t.Fatalf("terminal patch not applied: %+v", got)
	}
	if got.Failure != "write failed for config.yaml" {
		t.Errorf("an empty patch field must leave the column alone, failure = %q", got.Failure)
	}
	if !got.UpdatedAt.After(got.CreatedAt) && !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Errorf("updated_at must be stamped: %v < %v", got.UpdatedAt, got.CreatedAt)
	}
}

func journalAppendProgress(t *testing.T, repo persistence.ApplyJournalRepository) {
	ctx := context.Background()
	row := mustPrepare(t, repo, newTestJournal(uniqueID("cpp")))
	for _, p := range []string{"projects/new.yaml", "config.yaml"} {
		if err := repo.AppendProgress(ctx, row.ID, p); err != nil {
			t.Fatalf("AppendProgress(%s): %v", p, err)
		}
	}
	got, _ := repo.Get(ctx, row.ID)
	if len(got.Progress) != 2 || got.Progress[0] != "projects/new.yaml" || got.Progress[1] != "config.yaml" {
		t.Fatalf("progress must keep write order, got %v", got.Progress)
	}
	if err := repo.AppendProgress(ctx, uniqueID("absent"), "x"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown id must be ErrNotFound, got %v", err)
	}
}

// approvedProposalFor seeds an APPROVED proposal the ledger commit can flip.
func approvedProposalFor(t *testing.T, proposals persistence.ProposalRepository) string {
	t.Helper()
	ctx := context.Background()
	id := uniqueID("cpp")
	p := &persistence.ControlPlaneProposal{
		ID: id, ProjectID: "assistant", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "journal " + id,
		ApplyTarget: "config.yaml", ApplyContent: "new", Status: persistence.ProposalStatusDraft,
		ProposedBy: "assistant",
	}
	if err := proposals.Create(ctx, p); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	if err := proposals.SetStatus(ctx, id, persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatalf("approve(%s): %v", id, err)
	}
	return id
}

// verifyingJournalFor drives a fresh row to VERIFYING for proposalID.
func verifyingJournalFor(t *testing.T, repo persistence.ApplyJournalRepository, proposalID string) *persistence.ConfigApplyJournal {
	t.Helper()
	row := mustPrepare(t, repo, newTestJournal(proposalID))
	mustTransition(t, repo, row.ID, persistence.JournalStatePrepared, persistence.JournalStateApplying)
	mustTransition(t, repo, row.ID, persistence.JournalStateApplying, persistence.JournalStateVerifying)
	return row
}

func journalLedgerCommit(t *testing.T, journal persistence.ApplyJournalRepository, proposals persistence.ProposalRepository) {
	ctx := context.Background()
	pid := approvedProposalFor(t, proposals)
	row := verifyingJournalFor(t, journal, pid)
	err := journal.MarkAppliedWithLedger(ctx, persistence.JournalLedgerCommit{
		JournalID: row.ID, To: persistence.JournalStateApplied, GenerationAfter: "gen-2",
		ProposalID: pid, AppliedBy: "operator", Snapshot: "SNAPSHOT",
	})
	if err != nil {
		t.Fatalf("MarkAppliedWithLedger: %v", err)
	}
	j, _ := journal.Get(ctx, row.ID)
	if j.State != persistence.JournalStateApplied || j.TerminalAt == nil || j.GenerationAfter != "gen-2" {
		t.Fatalf("journal not terminal APPLIED: %+v", j)
	}
	p, _ := proposals.GetByID(ctx, pid)
	if p.Status != persistence.ProposalStatusApplied || p.AppliedBy != "operator" || p.PreApplySnapshot != "SNAPSHOT" || p.AppliedAt == nil {
		t.Fatalf("ledger not APPLIED with the derived snapshot: %+v", p)
	}
	// Terminal → a second commit is refused (idempotent single-apply).
	err = journal.MarkAppliedWithLedger(ctx, persistence.JournalLedgerCommit{
		JournalID: row.ID, To: persistence.JournalStateApplied, ProposalID: pid, AppliedBy: "operator",
	})
	if !errors.Is(err, persistence.ErrInvalidTransition) {
		t.Fatalf("re-commit must be ErrInvalidTransition, got %v", err)
	}
}

func journalLedgerPendingRestart(t *testing.T, journal persistence.ApplyJournalRepository, proposals persistence.ProposalRepository) {
	ctx := context.Background()
	pid := approvedProposalFor(t, proposals)
	row := verifyingJournalFor(t, journal, pid)
	if err := journal.MarkAppliedWithLedger(ctx, persistence.JournalLedgerCommit{
		JournalID: row.ID, To: persistence.JournalStatePendingRestart, ProposalID: pid, AppliedBy: "operator", Snapshot: "S",
	}); err != nil {
		t.Fatalf("MarkAppliedWithLedger(PENDING_RESTART): %v", err)
	}
	j, _ := journal.Get(ctx, row.ID)
	p, _ := proposals.GetByID(ctx, pid)
	if j.State != persistence.JournalStatePendingRestart || j.TerminalAt == nil || p.Status != persistence.ProposalStatusApplied {
		t.Fatalf("PENDING_RESTART must be terminal with the ledger APPLIED: journal=%s proposal=%s", j.State, p.Status)
	}
	// Any other terminal state is not a ledger commit.
	other := verifyingJournalFor(t, journal, approvedProposalFor(t, proposals))
	err := journal.MarkAppliedWithLedger(ctx, persistence.JournalLedgerCommit{
		JournalID: other.ID, To: persistence.JournalStateReverted, ProposalID: pid, AppliedBy: "operator",
	})
	if !errors.Is(err, persistence.ErrInvalidTransition) {
		t.Fatalf("To=REVERTED must be ErrInvalidTransition, got %v", err)
	}
}

// journalLedgerNotApproved is the atomicity proof in the failing direction:
// the journal write succeeds first inside the transaction, the ledger write
// is refused, and the journal must still read back VERIFYING and open.
func journalLedgerNotApproved(t *testing.T, journal persistence.ApplyJournalRepository, proposals persistence.ProposalRepository) {
	ctx := context.Background()
	pid := uniqueID("cpp")
	draft := &persistence.ControlPlaneProposal{
		ID: pid, ProjectID: "assistant", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "draft " + pid,
		ApplyTarget: "config.yaml", ApplyContent: "new", Status: persistence.ProposalStatusDraft, ProposedBy: "assistant",
	}
	if err := proposals.Create(ctx, draft); err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := verifyingJournalFor(t, journal, pid)
	err := journal.MarkAppliedWithLedger(ctx, persistence.JournalLedgerCommit{
		JournalID: row.ID, To: persistence.JournalStateApplied, ProposalID: pid, AppliedBy: "operator", Snapshot: "S",
	})
	if !errors.Is(err, persistence.ErrProposalNotApproved) {
		t.Fatalf("un-approved proposal must be ErrProposalNotApproved, got %v", err)
	}
	j, _ := journal.Get(ctx, row.ID)
	if j.State != persistence.JournalStateVerifying || j.TerminalAt != nil {
		t.Fatalf("a refused ledger commit must roll the journal write back; got state=%s terminal_at=%v", j.State, j.TerminalAt)
	}
	p, _ := proposals.GetByID(ctx, pid)
	if p.Status != persistence.ProposalStatusDraft {
		t.Fatalf("proposal must be untouched, got %s", p.Status)
	}
}

func journalLedgerNotVerifying(t *testing.T, journal persistence.ApplyJournalRepository, proposals persistence.ProposalRepository) {
	ctx := context.Background()
	pid := approvedProposalFor(t, proposals)
	row := mustPrepare(t, journal, newTestJournal(pid)) // PREPARED, not VERIFYING
	err := journal.MarkAppliedWithLedger(ctx, persistence.JournalLedgerCommit{
		JournalID: row.ID, To: persistence.JournalStateApplied, ProposalID: pid, AppliedBy: "operator", Snapshot: "S",
	})
	if !errors.Is(err, persistence.ErrInvalidTransition) {
		t.Fatalf("non-VERIFYING journal must be ErrInvalidTransition, got %v", err)
	}
	p, _ := proposals.GetByID(ctx, pid)
	if p.Status != persistence.ProposalStatusApproved || p.AppliedAt != nil {
		t.Fatalf("ledger must be untouched when the journal refuses: %+v", p)
	}
}
