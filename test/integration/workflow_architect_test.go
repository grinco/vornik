//go:build integration
// +build integration

package integration_test

// End-to-end integration test for the memetic architect against
// real postgres. Wires:
//   - filesystem WorkflowSource (temp dir + WORKFLOW.md fixture)
//   - SQL ExecutionLookup (real executions×tasks join)
//   - workflowtelemetry.Service (real DB)
//   - persistence WorkflowProposalRepository (real DB)
//   - fake chat.Provider returning a canned JSON proposal
//
// The unit tests in internal/memetic + internal/persistence/postgres
// cover the per-method semantics. THIS test covers the seams:
//   - evidence-lookup SQL actually filters by tasks.workflow_id
//   - inserted row reads back with every field threaded
//   - partial unique index blocks a second pending proposal
//     end-to-end (not just at the repo layer)

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// fakeChatProvider returns whatever JSON we configure. Satisfies
// chat.Provider via the same shape the dispatcher tests use.
type fakeChatProvider struct {
	content string
}

func (f *fakeChatProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Index: 0, Message: chat.Message{Role: "assistant", Content: f.content}, FinishReason: "stop"},
		},
	}, nil
}
func (f *fakeChatProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return f.Complete(context.TODO(), nil)
}
func (f *fakeChatProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return f.Complete(context.TODO(), nil)
}
func (f *fakeChatProvider) Model() string              { return "fake-architect-model" }
func (f *fakeChatProvider) SetMetrics(_ *chat.Metrics) {}

// Mirrors the service-package fsWorkflowSource + sqlExecutionLookup
// inline so this test doesn't import the service package (would
// pull in a heavy dependency graph the integration test doesn't
// need). Same logic shape; production wiring is tested
// separately in internal/service.
type fsWorkflowSourceIT struct {
	workflowsDir string
}

func (s *fsWorkflowSourceIT) Load(_ context.Context, workflowID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.workflowsDir, workflowID+".md"))
}

type sqlExecutionLookupIT struct {
	db *sql.DB
}

func (l *sqlExecutionLookupIT) BelongsTo(ctx context.Context, workflowID string, ids []string) ([]string, bool, error) {
	if len(ids) == 0 {
		return nil, true, nil
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT e.id
		FROM executions e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.id = ANY($1) AND t.workflow_id = $2`,
		pq.Array(ids), workflowID,
	)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var valid []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		valid = append(valid, id)
	}
	return valid, len(valid) == len(ids), rows.Err()
}

// validWorkflowMD is a minimal but registry-validating WORKFLOW.md
// content. Sufficient for the architect's YAML validator gate.
const validWorkflowMD = `---
workflowId: "%s"
displayName: "IT Test Workflow"
description: "Integration-test workflow used by the memetic architect end-to-end test; never executes."
version: "1.0.0"
maxStepVisits: 3
maxWallClock: "30m"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "lead"
    on_success: "step2"
    on_fail: "failed"
    timeout: "10m"
  step2:
    type: "agent"
    role: "coder"
    on_fail: "failed"
    timeout: "20m"
    gates:
      - condition: "step2.approved == true"
        target: "complete"
terminals:
  complete:
    status: "COMPLETED"
    message: "ok"
  failed:
    status: "FAILED"
    message: "bad"
  cancelled:
    status: "CANCELLED"
    message: "cancelled"
---

# IT Test

## Prompts

### step1
Do the first step.

### step2
Do the second step.
`

func writeWorkflowFile(t *testing.T, dir, workflowID string) {
	t.Helper()
	body := fmt.Sprintf(validWorkflowMD, workflowID)
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowID+".md"), []byte(body), 0o644))
}

// seedExecutions writes N tasks + N executions tied to the
// given workflow. Returns the execution IDs.
func seedExecutions(t *testing.T, db *sql.DB, projectID, workflowID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	var execIDs []string
	for i := 0; i < n; i++ {
		suffix := fmt.Sprintf("%d-%d", time.Now().UnixNano(), i)
		taskID := "wfarch-task-" + suffix
		execID := "wfarch-exec-" + suffix
		_, err := db.ExecContext(ctx, `
			INSERT INTO tasks (id, project_id, workflow_id, status, priority,
			                   creation_source, attempt, max_attempts,
			                   created_at, updated_at)
			VALUES ($1, $2, $3, 'COMPLETED', 5, 'USER', 1, 3, NOW(), NOW())`,
			taskID, projectID, workflowID)
		require.NoError(t, err, "insert task")
		_, err = db.ExecContext(ctx, `
			INSERT INTO executions (id, task_id, project_id, workflow_id,
			                        workflow_revision, status,
			                        started_at, completed_at)
			VALUES ($1, $2, $3, $4, 'rev-1', 'COMPLETED', NOW(), NOW())`,
			execID, taskID, projectID, workflowID)
		require.NoError(t, err, "insert execution")
		execIDs = append(execIDs, execID)
	}
	return execIDs
}

func TestArchitect_E2E_HappyPath(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	projectID := "wfarch-" + suffix
	workflowID := "wfarch-wf-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
		_, _ = db.Exec(`DELETE FROM executions WHERE project_id = $1`, projectID)
		_, _ = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, projectID)
	})

	wfDir := t.TempDir()
	writeWorkflowFile(t, wfDir, workflowID)

	execIDs := seedExecutions(t, db, projectID, workflowID, 3)

	// Build the canned LLM output that cites the seeded execution
	// IDs as evidence. Confidence above the 0.6 floor.
	output := memetic.ArchitectOutput{
		WorkflowID:     workflowID,
		ProposedYAML:   fmt.Sprintf(validWorkflowMD, workflowID),
		Motivation:     "across 3 runs, step2's reviewer rejected 2/3 outputs — suggest tightening step1's gate",
		EvidenceRunIDs: execIDs,
		Confidence:     0.78,
	}
	outputJSON, err := json.MarshalIndent(output, "", "  ")
	require.NoError(t, err)

	repo := postgres.NewWorkflowProposalRepository(db)
	arch := memetic.New(
		&fakeChatProvider{content: string(outputJSON)},
		&typedTelemetrySource{svc: workflowtelemetry.NewService(db)},
		&fsWorkflowSourceIT{workflowsDir: wfDir},
		&sqlExecutionLookupIT{db: db},
		repo,
		memetic.DefaultConfig(),
	)

	got, err := arch.Propose(context.Background(), workflowID)
	require.NoError(t, err, "Propose should succeed end-to-end")
	require.NotNil(t, got)
	require.Equal(t, workflowID, got.WorkflowID)
	require.Equal(t, persistence.WorkflowProposalStatusPending, got.Status)
	require.Equal(t, "fake-architect-model", got.ArchitectModel)
	require.Len(t, got.EvidenceRunIDs, 3)

	// Round-trip: persistence layer wrote everything we passed in.
	roundTrip, err := repo.Get(context.Background(), got.ID)
	require.NoError(t, err)
	require.Equal(t, got.ID, roundTrip.ID)
	require.Equal(t, output.Motivation, roundTrip.Motivation)
	require.InDelta(t, 0.78, roundTrip.Confidence, 0.01)
}

func TestArchitect_E2E_EvidenceFromOtherWorkflow_Rejected(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	projectID := "wfarch-otherwf-" + suffix
	targetWorkflow := "wfarch-target-" + suffix
	otherWorkflow := "wfarch-other-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id IN ($1, $2)`, targetWorkflow, otherWorkflow)
		_, _ = db.Exec(`DELETE FROM executions WHERE project_id = $1`, projectID)
		_, _ = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, projectID)
	})

	wfDir := t.TempDir()
	writeWorkflowFile(t, wfDir, targetWorkflow)

	// Seed executions under a DIFFERENT workflow.
	otherIDs := seedExecutions(t, db, projectID, otherWorkflow, 3)

	output := memetic.ArchitectOutput{
		WorkflowID:     targetWorkflow,
		ProposedYAML:   fmt.Sprintf(validWorkflowMD, targetWorkflow),
		Motivation:     "smuggled evidence",
		EvidenceRunIDs: otherIDs, // wrong workflow
		Confidence:     0.9,
	}
	outputJSON, _ := json.MarshalIndent(output, "", "  ")

	repo := postgres.NewWorkflowProposalRepository(db)
	arch := memetic.New(
		&fakeChatProvider{content: string(outputJSON)},
		&typedTelemetrySource{svc: workflowtelemetry.NewService(db)},
		&fsWorkflowSourceIT{workflowsDir: wfDir},
		&sqlExecutionLookupIT{db: db},
		repo,
		memetic.DefaultConfig(),
	)

	_, err := arch.Propose(context.Background(), targetWorkflow)
	require.Error(t, err)
	require.True(t, errors.Is(err, memetic.ErrEvidenceInvalid),
		"want ErrEvidenceInvalid, got %v", err)
}

func TestArchitect_E2E_RateLimit_BlocksSecondPropose(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	projectID := "wfarch-rl-" + suffix
	workflowID := "wfarch-rlwf-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
		_, _ = db.Exec(`DELETE FROM executions WHERE project_id = $1`, projectID)
		_, _ = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, projectID)
	})

	wfDir := t.TempDir()
	writeWorkflowFile(t, wfDir, workflowID)
	execIDs := seedExecutions(t, db, projectID, workflowID, 3)

	output := memetic.ArchitectOutput{
		WorkflowID:     workflowID,
		ProposedYAML:   fmt.Sprintf(validWorkflowMD, workflowID),
		Motivation:     "first proposal",
		EvidenceRunIDs: execIDs,
		Confidence:     0.7,
	}
	outputJSON, _ := json.MarshalIndent(output, "", "  ")

	repo := postgres.NewWorkflowProposalRepository(db)
	arch := memetic.New(
		&fakeChatProvider{content: string(outputJSON)},
		&typedTelemetrySource{svc: workflowtelemetry.NewService(db)},
		&fsWorkflowSourceIT{workflowsDir: wfDir},
		&sqlExecutionLookupIT{db: db},
		repo,
		memetic.DefaultConfig(),
	)

	_, err := arch.Propose(context.Background(), workflowID)
	require.NoError(t, err, "first Propose should succeed")

	// Second propose with a different motivation but same
	// workflow → blocked by the partial unique index on
	// (workflow_id) WHERE status='pending'.
	output.Motivation = "second proposal"
	outputJSON, _ = json.MarshalIndent(output, "", "  ")
	arch2 := memetic.New(
		&fakeChatProvider{content: string(outputJSON)},
		&typedTelemetrySource{svc: workflowtelemetry.NewService(db)},
		&fsWorkflowSourceIT{workflowsDir: wfDir},
		&sqlExecutionLookupIT{db: db},
		repo,
		memetic.DefaultConfig(),
	)
	_, err = arch2.Propose(context.Background(), workflowID)
	require.Error(t, err)
	require.True(t, errors.Is(err, persistence.ErrProposalRateLimited),
		"want ErrProposalRateLimited propagated, got %v", err)
}

// typedTelemetrySource is the inline equivalent of the
// service-package memeticTelemetrySource. Kept in this test file
// so the integration test doesn't drag in the service package.
type typedTelemetrySource struct {
	svc *workflowtelemetry.Service
}

func (s *typedTelemetrySource) ForWorkflow(ctx context.Context, workflowID string, since time.Time) (*workflowtelemetry.Rollup, error) {
	return s.svc.ForWorkflow(ctx, workflowID, since)
}
