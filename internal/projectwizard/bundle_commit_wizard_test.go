package projectwizard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/persistence"
)

// TestCommit_Bundle_ConcurrentSameID_OnlyOneLands is the review-20260716-8f22
// Finding-1 regression: two sessions committing bundles with the SAME project id
// concurrently must NOT both land (the collision-check→rename was a non-atomic
// read-then-write). Wizard.commitMu serialises the check-and-land, so exactly
// one wins and the other fails its (now-populated) collision check.
func TestCommit_Bundle_ConcurrentSameID_OnlyOneLands(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	sessA := pinReadyBundleSession(t, store, validComposedBundle())
	sessB := pinReadyBundleSession(t, store, validComposedBundle()) // same project id

	ids := []string{sessA, sessB}
	results := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range ids {
		go func(i int) {
			defer wg.Done()
			_, results[i] = w.Commit(context.Background(), ids[i], "op_1")
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("expected exactly ONE concurrent commit to succeed, got %d (results: %v)", ok, results)
	}
	if _, statErr := os.Stat(filepath.Join(liveDir, "projects", "ai-news-digest.yaml")); statErr != nil {
		t.Fatalf("the winning commit's project file must be present: %v", statErr)
	}
}

// pinReadyBundleSession seeds a session carrying a persisted tier-3
// bundle (the JSON shape applyBundle stores on session.Bundle once a
// turn composes + guardrails + staged-validates cleanly), mirroring
// pinReadySession/pinReadySessionWithComposition's seeding for the
// legacy/v2 paths.
func pinReadyBundleSession(t *testing.T, store *fakeSessionStore, bundle *ComposedBundle) string {
	t.Helper()
	session := &persistence.ProjectWizardSession{
		ID:            persistence.GenerateID("pw"),
		OperatorID:    "op_1",
		ReadyToCommit: true,
	}
	b, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	session.Bundle = b
	if err := store.Insert(context.Background(), session); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return session.ID
}

// wizardForBundleCommit returns a Wizard wired the way commitBundleSession
// needs: a temp LiveConfigDir (NEVER the real deployed tree — global
// constraint) and the role library validComposedBundle()'s roles
// resolve against.
func wizardForBundleCommit(t *testing.T) (*Wizard, *fakeSessionStore, string) {
	t.Helper()
	w, store, _ := newWizardForTest()
	w.RoleLibrary = testArchetypes()
	w.LiveConfigDir = t.TempDir()
	return w, store, w.LiveConfigDir
}

func TestCommit_Bundle_HappyPath(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if result.ProjectID != "ai-news-digest" {
		t.Errorf("project id = %q, want ai-news-digest", result.ProjectID)
	}
	if result.URL != "/ui/projects/ai-news-digest/setup" {
		t.Errorf("url = %q, want the project doctor/setup redirect", result.URL)
	}

	for _, rel := range []string{
		"projects/ai-news-digest.yaml",
		"swarms/ai-news-digest-swarm.md",
		"workflows/research-digest.md",
	} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); statErr != nil {
			t.Errorf("expected %s to land in the live tree: %v", rel, statErr)
		}
	}

	if _, statErr := os.Stat(stagingDirFor(liveDir, sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected the staging dir cleaned up, stat err = %v", statErr)
	}

	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID == nil || *stored.CommittedProjectID != "ai-news-digest" {
		t.Errorf("session not stamped: %+v", stored.CommittedProjectID)
	}
}

func TestCommit_Bundle_MetricsRecordCreated(t *testing.T) {
	w, store, _ := wizardForBundleCommit(t)
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := composerCommitsMetricValue(t, metrics, composerCommitTier3, composerCommitResultCreated); got != 1 {
		t.Errorf("composer commits created = %.0f, want 1", got)
	}
}

// TestCommit_Bundle_IdempotentRecommit: a session whose bundle commit
// already succeeded (CommittedProjectID stamped) must short-circuit to
// the existing project's URL WITHOUT re-running the pipeline or
// touching the live tree again — same idempotent contract the legacy/
// composition paths already guarantee, now exercised on the Bundle
// branch. This assertion also proves the Bundle-branch dispatch sits
// AFTER the idempotent check in Commit, not before it.
func TestCommit_Bundle_IdempotentRecommit(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())
	committed := "already-committed-elsewhere"
	stored, _ := store.Get(context.Background(), sessionID)
	stored.CommittedProjectID = &committed
	_ = store.Update(context.Background(), stored)

	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("idempotent commit should succeed: %v", err)
	}
	if result.ProjectID != committed {
		t.Errorf("expected prior project id, got %q", result.ProjectID)
	}
	// Nothing should have been written under the live config dir for
	// THIS bundle — the idempotent branch never reaches
	// commitBundleSession/stageAndCommitBundle.
	if _, statErr := os.Stat(filepath.Join(liveDir, "projects", "ai-news-digest.yaml")); !os.IsNotExist(statErr) {
		t.Error("idempotent re-click must not land files again")
	}
}

// TestCommit_Bundle_IdempotentRecommit_ProposerWired is the regression
// for the slice-i review's Important finding: production EE wires the
// ScaffoldProposer whenever c.repos.Proposals != nil
// (project_wizard_adapter.go), independently of whether the composer
// is wired — so a bundle session's idempotent re-click must NOT be
// routed to controlPlaneProposalsURL just because w.Proposer is
// non-nil. A bundle commit never uses the Proposer/ledger
// (commitBundleSession lands files directly to the live tree via
// stageAndCommitBundle), so the project is already live: the
// idempotent branch must return its doctor/setup URL exactly like the
// no-proposer case, checked AHEAD of the w.Proposer != nil branch.
func TestCommit_Bundle_IdempotentRecommit_ProposerWired(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	w.Proposer = &fakeProposer{proposalID: "cpp_should_not_be_used", url: controlPlaneProposalsURL}
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())
	committed := "already-committed-elsewhere"
	stored, _ := store.Get(context.Background(), sessionID)
	stored.CommittedProjectID = &committed
	_ = store.Update(context.Background(), stored)

	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("idempotent commit should succeed: %v", err)
	}
	wantURL := composerDoctorURL(committed)
	if result.URL != wantURL {
		t.Errorf("expected doctor/setup URL %q, got %q (must NOT be %q)", wantURL, result.URL, controlPlaneProposalsURL)
	}
	if result.URL == controlPlaneProposalsURL {
		t.Error("bundle idempotent re-click must not be routed to the control-plane proposals tab")
	}
	if _, statErr := os.Stat(filepath.Join(liveDir, "projects", "ai-news-digest.yaml")); !os.IsNotExist(statErr) {
		t.Error("idempotent re-click must not land files again")
	}
}

// TestCommit_Bundle_LiveCollision_NoBypass is the design's "registry
// drift between last turn and commit" failure row (§7) / the brief's
// no-bypass test: a live project/swarm/workflow with the same id as
// the bundle's — introduced, e.g., by a concurrent creation — must
// refuse the commit with ZERO files landed, the session left
// resumable (Bundle intact, commit-failure marker stamped), and a
// failed metric recorded. LoadFromPaths' later-path-wins semantics
// mean staged validation ALONE would not catch this (a same-ID rename
// would silently clobber the live file) — this is exactly why
// commitBundleSession re-runs the live-collision check up front.
func TestCommit_Bundle_LiveCollision_NoBypass(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	// A concurrently-created project now occupies the same id.
	if err := os.MkdirAll(filepath.Join(liveDir, "projects"), 0o700); err != nil {
		t.Fatalf("seed live dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "projects", "ai-news-digest.yaml"),
		[]byte("projectId: \"ai-news-digest\"\ndisplayName: \"Other\"\nswarmId: \"other-swarm\"\ndefaultWorkflowId: \"other-wf\"\n"),
		0o600); err != nil {
		t.Fatalf("seed colliding project: %v", err)
	}

	sessionID := pinReadyBundleSession(t, store, validComposedBundle())
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics

	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected the live collision to refuse the commit")
	}
	if !errors.Is(err, ErrBundleCommitFailed) {
		t.Errorf("expected ErrBundleCommitFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected a plain-language collision message, got %v", err)
	}

	// Zero NEW files landed: the swarm/workflow must not exist, and the
	// pre-existing (colliding) project file must be untouched.
	for _, rel := range []string{"swarms/ai-news-digest-swarm.md", "workflows/research-digest.md"} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); !os.IsNotExist(statErr) {
			t.Errorf("no-bypass violated: %s must not exist, stat err = %v", rel, statErr)
		}
	}
	untouched, err := os.ReadFile(filepath.Join(liveDir, "projects", "ai-news-digest.yaml"))
	if err != nil || !strings.Contains(string(untouched), "Other") {
		t.Errorf("the pre-existing colliding project file must be left untouched, got %q err=%v", untouched, err)
	}

	// Session left resumable: Bundle intact, failure marker stamped.
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.Bundle == nil {
		t.Error("session.Bundle must stay intact for a resumable retry")
	}
	if stored.CommittedProjectID != nil {
		t.Error("session must not be stamped committed on a refused commit")
	}
	if stored.BundleCommitFailedAt == nil || stored.BundleCommitError == "" {
		t.Errorf("expected the commit-failed-resumable marker stamped, got %+v", stored)
	}

	if got := composerCommitsMetricValue(t, metrics, composerCommitTier3, composerCommitResultFailed); got != 1 {
		t.Errorf("composer commits failed = %.0f, want 1", got)
	}
}

// TestCommit_Bundle_GuardrailViolation_NoBypass: a bundle whose role
// declares an allowedTools entry outside its archetype's allowlist
// (tool_overreach, guardrail.go) must fail at commit time — never
// silently stripped, never landed.
func TestCommit_Bundle_GuardrailViolation_NoBypass(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	bundle := validComposedBundle()
	roles := bundle.Swarm["roles"].([]any)
	roles[0] = map[string]any{
		"name":         "researcher",
		"archetypeId":  "researcher",
		"allowedTools": []any{"forge.post_review"}, // outside the researcher archetype's allowlist
	}
	bundle.Swarm["roles"] = roles

	sessionID := pinReadyBundleSession(t, store, bundle)
	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected the tool-overreach guardrail violation to refuse the commit")
	}
	if !errors.Is(err, ErrBundleCommitFailed) {
		t.Errorf("expected ErrBundleCommitFailed, got %v", err)
	}

	entries, _ := os.ReadDir(liveDir)
	if len(entries) != 0 {
		t.Errorf("no-bypass violated: expected nothing written to the live tree, got %v", entries)
	}
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.Bundle == nil {
		t.Error("session.Bundle must stay intact for a resumable retry")
	}
}

// TestCommit_Bundle_ScheduleNotConfirmed_NoBypass: autonomy enabled
// but the session never recorded a schedule confirmation — the
// structural gate (§5.4) must refuse the commit even though
// ready_to_commit was true on the session (the wizard's own
// applyBundle only sets ready_to_commit=false in-turn; a directly
// seeded test session skips that, so this proves Commit enforces the
// gate independently, not just Converse).
func TestCommit_Bundle_ScheduleNotConfirmed_NoBypass(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	bundle := validComposedBundle()
	bundle.Project["autonomy"] = map[string]any{"enabled": true, "pollInterval": "24h"}

	session := &persistence.ProjectWizardSession{
		ID:            persistence.GenerateID("pw"),
		OperatorID:    "op_1",
		ReadyToCommit: true,
	}
	b, _ := json.Marshal(bundle)
	session.Bundle = b
	if err := store.Insert(context.Background(), session); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	_, err := w.Commit(context.Background(), session.ID, "op_1")
	if err == nil {
		t.Fatal("expected an unconfirmed schedule to refuse the commit")
	}
	if !strings.Contains(err.Error(), "schedule") {
		t.Errorf("expected a schedule-confirmation error, got %v", err)
	}
	entries, _ := os.ReadDir(liveDir)
	if len(entries) != 0 {
		t.Errorf("no-bypass violated: expected nothing written, got %v", entries)
	}
}

// TestCommit_Bundle_ScheduleConfirmed_Succeeds is the positive
// counterpart: a matching, confirmed schedule must commit cleanly.
func TestCommit_Bundle_ScheduleConfirmed_Succeeds(t *testing.T) {
	w, store, _ := wizardForBundleCommit(t)
	bundle := validComposedBundle()
	bundle.Project["autonomy"] = map[string]any{"enabled": true, "pollInterval": "24h"}

	now := time.Now().UTC()
	session := &persistence.ProjectWizardSession{
		ID:                    persistence.GenerateID("pw"),
		OperatorID:            "op_1",
		ReadyToCommit:         true,
		ScheduleConfirmedAt:   &now,
		ScheduleConfirmedCron: "24h",
	}
	b, _ := json.Marshal(bundle)
	session.Bundle = b
	if err := store.Insert(context.Background(), session); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	result, err := w.Commit(context.Background(), session.ID, "op_1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if result.ProjectID != "ai-news-digest" {
		t.Errorf("project id = %q", result.ProjectID)
	}
}

// TestCommit_Bundle_StagedValidationFailure_NoBypass: the bundle
// materializes and passes guardrails, but the swarm's leadRole doesn't
// match any declared role — a staged-registry-validation failure
// (registry.Swarm.Validate) distinct from both the live-collision
// check and the guardrail pass. Zero files must land.
func TestCommit_Bundle_StagedValidationFailure_NoBypass(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	bundle := validComposedBundle()
	bundle.Swarm["leadRole"] = "ghost-lead"

	sessionID := pinReadyBundleSession(t, store, bundle)
	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected staged validation to refuse the commit")
	}
	if !errors.Is(err, ErrBundleCommitFailed) {
		t.Errorf("expected ErrBundleCommitFailed, got %v", err)
	}
	entries, _ := os.ReadDir(liveDir)
	if len(entries) != 0 {
		t.Errorf("no-bypass violated: expected nothing written, got %v", entries)
	}
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID != nil {
		t.Error("session must not be stamped committed")
	}
	if stored.Bundle == nil {
		t.Error("session.Bundle must stay intact for a resumable retry")
	}
}

// TestCommit_Bundle_MaterializeFailure_NoBypass: the role library no
// longer has the archetype the bundle references (e.g. the operator
// edited the library between the turn and the commit click) — a
// materialization failure, the earliest possible rejection point.
func TestCommit_Bundle_MaterializeFailure_NoBypass(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	w.RoleLibrary = nil // archetype library gone
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected materialization failure to refuse the commit")
	}
	entries, _ := os.ReadDir(liveDir)
	if len(entries) != 0 {
		t.Errorf("no-bypass violated: expected nothing written, got %v", entries)
	}
}

// TestCommit_Bundle_UnreadableBundle: a corrupt session.Bundle blob
// (shouldn't happen, but the JSON decode step must fail closed) is
// resumable, not a panic.
func TestCommit_Bundle_UnreadableBundle(t *testing.T) {
	w, store, _ := wizardForBundleCommit(t)
	session := &persistence.ProjectWizardSession{
		ID:            persistence.GenerateID("pw"),
		OperatorID:    "op_1",
		ReadyToCommit: true,
		Bundle:        []byte("{not json"),
	}
	if err := store.Insert(context.Background(), session); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	_, err := w.Commit(context.Background(), session.ID, "op_1")
	if !errors.Is(err, ErrBundleCommitFailed) {
		t.Errorf("expected ErrBundleCommitFailed, got %v", err)
	}
}

// TestCommit_Bundle_RenameFailure_RollbackAndResumable exercises the
// brief's "force a failure on the project rename" rollback scenario at
// the full Wizard.Commit level (bundle_commit_test.go's
// TestStageAndCommitBundle_RollbackRemovesLandedFiles already covers
// stageAndCommitBundle in isolation) — this proves the SAME guarantee
// end-to-end: all previously-landed journal files are removed, the
// staging dir is gone, the session is left resumable (Bundle intact,
// commit-failure marker stamped), and the live tree is back to its
// pre-commit state (empty).
func TestCommit_Bundle_RenameFailure_RollbackAndResumable(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	origRename := renameFn
	defer func() { renameFn = origRename }()
	renameFn = func(oldpath, newpath string) error {
		if strings.Contains(newpath, string(filepath.Separator)+"projects"+string(filepath.Separator)) {
			return errInjectedRenameFailure
		}
		return origRename(oldpath, newpath)
	}

	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected the injected rename failure to refuse the commit")
	}
	if !errors.Is(err, ErrBundleCommitFailed) {
		t.Errorf("expected ErrBundleCommitFailed, got %v", err)
	}

	for _, rel := range []string{"workflows/research-digest.md", "swarms/ai-news-digest-swarm.md", "projects/ai-news-digest.yaml"} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); !os.IsNotExist(statErr) {
			t.Errorf("rollback must remove %s, stat err = %v", rel, statErr)
		}
	}
	if _, statErr := os.Stat(stagingDirFor(liveDir, sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected staging dir removed after rollback, stat err = %v", statErr)
	}

	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID != nil {
		t.Error("session must not be stamped committed")
	}
	if stored.Bundle == nil {
		t.Error("session.Bundle must stay intact for a resumable retry")
	}
	if stored.BundleCommitFailedAt == nil || stored.BundleCommitError == "" {
		t.Error("expected the commit-failed-resumable marker stamped")
	}
	if got := composerCommitsMetricValue(t, metrics, composerCommitTier3, composerCommitResultFailed); got != 1 {
		t.Errorf("composer commits failed = %.0f, want 1", got)
	}
}

// TestCommit_Bundle_SessionStampFailure_DoesNotUnwind: files land
// successfully but the CommitTo stamp fails afterward — mirrors the
// legacy/composition paths' identical choice (commit.go's own Commit)
// to NOT unwind a successful on-disk commit just because the
// session-stamp update failed. The operator's next commit click hits
// the idempotent branch once the stamp eventually succeeds (or the
// operator is told the project exists via the doctor page).
func TestCommit_Bundle_SessionStampFailure_DoesNotUnwind(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())
	store.errOn = "CommitTo"

	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected the CommitTo failure to propagate")
	}
	if result == nil || result.ProjectID != "ai-news-digest" {
		t.Fatalf("expected a non-nil result carrying the landed project id, got %+v", result)
	}
	if _, statErr := os.Stat(filepath.Join(liveDir, "projects", "ai-news-digest.yaml")); statErr != nil {
		t.Errorf("files must have landed despite the stamp failure: %v", statErr)
	}
}

func TestOrderedRelPaths_UnknownPrefixSortsFirstNeverDropped(t *testing.T) {
	files := map[string]string{
		"projects/x.yaml":   "proj",
		"workflows/a.md":    "wf",
		"role-library/misc": "stray",
	}
	order := orderedRelPaths(files)
	if len(order) != 3 {
		t.Fatalf("expected all 3 entries preserved, got %v", order)
	}
	if order[0] != "role-library/misc" {
		t.Errorf("unrecognised-prefix entry must sort first (never silently dropped), got order=%v", order)
	}
}
