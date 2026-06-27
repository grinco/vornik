package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// awaitingApprovalServer builds a pause/resume-style server with API metrics
// wired, holding one AWAITING_APPROVAL task.
func awaitingApprovalServer(t *testing.T, projectID, taskID string) *Server {
	t.Helper()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: projectID, Status: persistence.TaskStatusAwaitingApproval}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	s := buildPauseResumeServer(taskRepo, &stubTaskMessageRepo{}, &mockPauseResumeExecutor{})
	s.apiMetrics = NewAPIMetrics(prometheus.NewRegistry())
	return s
}

func TestApproveTask_IncrementsApprovalsMetric(t *testing.T) {
	const projectID, taskID = "proj-m", "task-approve-m"
	s := awaitingApprovalServer(t, projectID, taskID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/approve", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	s.ApproveTask(httptest.NewRecorder(), req)

	if got := testutil.ToFloat64(s.apiMetrics.ApprovalsTotal.WithLabelValues(projectID, "approved")); got != 1 {
		t.Fatalf("approved counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.apiMetrics.ApprovalsTotal.WithLabelValues(projectID, "rejected")); got != 0 {
		t.Fatalf("rejected counter = %v, want 0", got)
	}
}

func TestRejectTask_IncrementsApprovalsMetric(t *testing.T) {
	const projectID, taskID = "proj-m", "task-reject-m"
	s := awaitingApprovalServer(t, projectID, taskID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/reject", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	s.RejectTask(httptest.NewRecorder(), req)

	if got := testutil.ToFloat64(s.apiMetrics.ApprovalsTotal.WithLabelValues(projectID, "rejected")); got != 1 {
		t.Fatalf("rejected counter = %v, want 1", got)
	}
}
