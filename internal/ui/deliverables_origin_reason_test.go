package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestDeliverableSend_LostOriginRecord_DoesNotClaimNonChatOrigin pins the fix
// for the 2026-08-05 misleading-message defect on janka-companion.
//
// The task WAS Slack-originated. Its chat_audit_log row had been lost (the
// dispatcher's audit write ran on an expired turn context), so
// chatorigin.Resolve returned false — and DeliverableSend rendered its single
// failure message: "This task wasn't started from a chat channel". That is a
// factual claim about the task, and it was false. The operator believed
// parent-channel inheritance was missing and went looking for a feature that
// already existed, instead of at the missing row.
//
// A lost origin record must read as a fault, name the channel it can no longer
// reach, and hand over the download link so the deliverable is still
// recoverable by hand.
func TestDeliverableSend_LostOriginRecord_DoesNotClaimNonChatOrigin(t *testing.T) {
	taskID, artifactID := "task_20260805170522_ab6d9a44d42779b5", "art_report"
	parentID := "task_20260805170431_955e423704153971"
	parentTurn := "chat_20260805170412_9dc37555d22e8c76"

	// Child carries no turn id; the parent does. The lineage walk finds it.
	child := &persistence.Task{
		ID: taskID, ProjectID: "companion-janka",
		ParentTaskID: &parentID, Status: persistence.TaskStatusCompleted,
	}
	parent := &persistence.Task{
		ID: parentID, ProjectID: "companion-janka",
		ChatTurnID: strpDS(parentTurn), Status: persistence.TaskStatusCompleted,
	}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			switch id {
			case taskID:
				return child, nil
			case parentID:
				return parent, nil
			}
			return nil, nil
		},
	}
	artifact := &persistence.Artifact{
		ID: artifactID, ProjectID: "companion-janka", TaskID: &taskID,
		Name: "report-20260805-9bac.html", ArtifactClass: persistence.ArtifactClassOutput,
	}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
		// The audit row the parent's chat_turn_id points at is GONE.
		WithChatAuditRepository(fakeChatAuditDS{err: persistence.ErrNotFound}),
		WithChannelResolver(fakeResolverDS{}),
		WithWebUIBaseURL("https://vornik.example.com"),
	)

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Substring deliberately avoids the apostrophe: the template HTML-escapes
	// it to &#39;, so matching on "wasn't" would pass vacuously.
	assert.NotContains(t, strings.ToLower(body), "started from a chat channel",
		"the task WAS chat-originated — claiming otherwise sent the operator hunting for a missing feature")
	assert.Contains(t, strings.ToLower(body), "origin",
		"the message must name the real problem: the chat origin record is missing")
	assert.Contains(t, body, "https://vornik.example.com/ui/artifacts/"+artifactID,
		"an unroutable deliverable must still be recoverable by hand — give the download link")
}

// TestDeliverableSend_GenuinelyNonChatOriginated_StillSaysSo guards against
// over-correcting: a task that truly never came from a chat must keep its
// honest, final message.
func TestDeliverableSend_GenuinelyNonChatOriginated_StillSaysSo(t *testing.T) {
	taskID, artifactID := "task_api_only", "art_api"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted} // no ChatTurnID, no parent
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	artifact := &persistence.Artifact{
		ID: artifactID, ProjectID: "p1", TaskID: &taskID,
		Name: "out.csv", ArtifactClass: persistence.ArtifactClassOutput,
	}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	// Apostrophe-free substring: the template escapes ' to &#39;.
	assert.Contains(t, rec.Body.String(), "started from a chat channel")
}
