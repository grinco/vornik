package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// approvedProposalLive seeds an APPROVED, applyable config proposal with the
// given blast radius and LiveApply flag, and writes the target with oldContent.
func approvedProposalLive(t *testing.T, scope string, live bool) (*ApplyEngine, string, string) {
	t.Helper()
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte(oldContent), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	ctx := context.Background()
	p := &persistence.ControlPlaneProposal{
		ID:   persistence.GenerateID("cpp"), // daemon-scope ⇒ ProjectID ""
		Kind: persistence.ProposalKindConfig, BlastRadius: scope,
		Title: "MCP: add server", ApplyTarget: "config.yaml", ApplyContent: newContent,
		Status: persistence.ProposalStatusDraft, ProposedBy: "operator-ui",
		LiveApply: live,
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Reload: func() error { return nil }, Logger: zerolog.Nop()}
	return e, p.ID, file
}

// TestApply_LiveApplySkipsBusyGate is the fix for the 2026-07-08 MCP
// "homeassistant" apply failure: a daemon-scope config proposal was
// un-appliable in production because ApplyEngine gates on HasActiveTasks("")
// across ALL projects, and autonomy loops keep a task RUNNING continuously so
// the gate never opens. A LiveApply proposal skips ONLY that gate.
func TestApply_LiveApplySkipsBusyGate(t *testing.T) {
	e, id, file := approvedProposalLive(t, persistence.ProposalScopeDaemon, true)
	// A task is running in every project — the pre-fix ErrBusy trigger.
	e.HasActiveTasks = func(_ context.Context, _ string) (bool, error) { return true, nil }
	if err := e.Apply(context.Background(), id, "vadim", true /* ackDaemon */); err != nil {
		t.Fatalf("live-apply must apply despite running tasks, got %v", err)
	}
	if got := readFile(t, file); got != newContent {
		t.Errorf("live-apply should have written the new config, got %q", got)
	}
}

// TestApply_NonLiveDaemonStillBusyRefused is the control: the SAME daemon-scope
// change WITHOUT LiveApply still refuses when busy (default behavior unchanged).
func TestApply_NonLiveDaemonStillBusyRefused(t *testing.T) {
	e, id, file := approvedProposalLive(t, persistence.ProposalScopeDaemon, false)
	e.HasActiveTasks = func(_ context.Context, _ string) (bool, error) { return true, nil }
	if err := e.Apply(context.Background(), id, "vadim", true); !errors.Is(err, ErrBusy) {
		t.Fatalf("non-live daemon change must still refuse when busy, got %v", err)
	}
	if got := readFile(t, file); got != oldContent {
		t.Errorf("busy refusal must not touch the file")
	}
}

// TestApply_LiveApplyStillNeedsDaemonAck confirms LiveApply narrows ONLY the
// busy gate — the daemon-scope blast-radius acknowledgment is still required.
func TestApply_LiveApplyStillNeedsDaemonAck(t *testing.T) {
	e, id, _ := approvedProposalLive(t, persistence.ProposalScopeDaemon, true)
	e.HasActiveTasks = func(_ context.Context, _ string) (bool, error) { return true, nil }
	if err := e.Apply(context.Background(), id, "vadim", false /* no ack */); !errors.Is(err, ErrDaemonAckRequired) {
		t.Fatalf("live-apply must still require daemon-ack, got %v", err)
	}
}
