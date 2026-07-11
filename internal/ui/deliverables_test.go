package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// --- classifyArtifactType ------------------------------------------------

func TestClassifyArtifactType(t *testing.T) {
	mt := func(s string) *string { return &s }
	cases := []struct {
		name     string
		mimeType *string
		want     string
	}{
		{"photo.png", mt("image/png"), "Image"},
		{"report.pdf", mt("application/pdf"), "PDF"},
		{"data.csv", nil, "Spreadsheet"},
		{"data.xlsx", mt("application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"), "Spreadsheet"},
		{"archive.zip", nil, "Archive"},
		{"bundle.tar.gz", nil, "Archive"},
		{"main.go", nil, "Code"},
		{"config.yaml", nil, "Code"},
		{"data.json", mt("application/json"), "Code"},
		{"notes.md", nil, "Text"},
		{"plain.txt", mt("text/plain"), "Text"},
		{"mystery.bin", nil, "File"},
		{"noext", mt(""), "File"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyArtifactType(c.name, c.mimeType)
			assert.Equal(t, c.want, got)
		})
	}
}

// --- buildDeliverableCards ------------------------------------------------

func TestBuildDeliverableCards_FiltersToOutputClass(t *testing.T) {
	size := int64(1234)
	artifacts := []*persistence.Artifact{
		{ID: "a1", Name: "input.txt", ArtifactClass: persistence.ArtifactClassInput},
		{ID: "a2", Name: "scratch.tmp", ArtifactClass: persistence.ArtifactClassIntermediate},
		{ID: "a3", Name: "report.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: &size},
		nil,
	}
	cards := buildDeliverableCards(artifacts)
	require.Len(t, cards, 1, "only the OUTPUT-class artifact should become a card")
	assert.Equal(t, "a3", cards[0].ID)
	assert.Equal(t, "report.md", cards[0].Name)
	assert.Equal(t, "Text", cards[0].TypeLabel)
	assert.Equal(t, int64(1234), cards[0].SizeBytes)
	assert.Equal(t, "/ui/artifacts/a3", cards[0].DownloadURL)
}

func TestBuildDeliverableCards_EmptyInputEmptyOutput(t *testing.T) {
	assert.Empty(t, buildDeliverableCards(nil))
	assert.Empty(t, buildDeliverableCards([]*persistence.Artifact{}))
}

// --- DeliverableMetrics ----------------------------------------------------

func TestDeliverableMetrics_RecordSend(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewDeliverableMetrics(reg)
	m.RecordSend("email")
	m.RecordSend("email")
	m.RecordSend("telegram")

	got, err := reg.Gather()
	require.NoError(t, err)
	counts := map[string]float64{}
	for _, mf := range got {
		if mf.GetName() != "vornik_deliverable_sends_total" {
			continue
		}
		for _, metric := range mf.Metric {
			for _, l := range metric.Label {
				if l.GetName() == "channel" {
					counts[l.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	assert.Equal(t, 2.0, counts["email"])
	assert.Equal(t, 1.0, counts["telegram"])
}

func TestDeliverableMetrics_NilSafe(t *testing.T) {
	var m *DeliverableMetrics
	assert.NotPanics(t, func() { m.RecordSend("email") })
}

func TestDeliverableMetrics_RecordSend_EmptyChannelNoop(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewDeliverableMetrics(reg)
	m.RecordSend("")
	got, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range got {
		if mf.GetName() == "vornik_deliverable_sends_total" {
			assert.Empty(t, mf.Metric, "empty channel must not create a label series")
		}
	}
}

// --- parseDeliverableSendPath -----------------------------------------

func TestParseDeliverableSendPath(t *testing.T) {
	cases := []struct {
		path      string
		wantTask  string
		wantArt   string
		wantOK    bool
		caseLabel string
	}{
		{"/tasks/t1/artifacts/a1/send", "t1", "a1", true, "plain"},
		{"/ui/tasks/t1/artifacts/a1/send", "t1", "a1", true, "with /ui prefix"},
		{"/tasks/t1/artifacts/a1", "", "", false, "missing /send"},
		{"/tasks/t1/send", "", "", false, "missing artifacts segment"},
		{"/tasks//artifacts/a1/send", "", "", false, "empty task id"},
		{"/tasks/t1/artifacts//send", "", "", false, "empty artifact id"},
	}
	for _, c := range cases {
		t.Run(c.caseLabel, func(t *testing.T) {
			taskID, artifactID, ok := parseDeliverableSendPath(c.path)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.wantTask, taskID)
			assert.Equal(t, c.wantArt, artifactID)
		})
	}
}

// --- DeliverableSend handler --------------------------------------------

// fakeChannelDS is a minimal conversation.Channel test double for the
// deliverable-send handler tests.
type fakeChannelDS struct {
	name    string
	sent    []conversation.ChannelMessage
	sendErr error
}

func (f *fakeChannelDS) Name() string                                       { return f.name }
func (f *fakeChannelDS) Start(context.Context, conversation.Receiver) error { return nil }
func (f *fakeChannelDS) Stop() error                                        { return nil }
func (f *fakeChannelDS) Send(_ context.Context, m conversation.ChannelMessage) (string, error) {
	f.sent = append(f.sent, m)
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "sent-1", nil
}
func (f *fakeChannelDS) ListSessions(context.Context) ([]conversation.Session, error) {
	return nil, nil
}
func (f *fakeChannelDS) ResolveSpeaker(context.Context, string) (conversation.Speaker, error) {
	return conversation.Speaker{}, nil
}

type fakeResolverDS struct {
	byName map[string]conversation.Channel
}

func (f fakeResolverDS) ResolveChannel(name string) conversation.Channel {
	if f.byName == nil {
		return nil
	}
	return f.byName[name]
}

type fakeChatAuditDS struct {
	row *persistence.ChatAuditEntry
	err error
}

func (f fakeChatAuditDS) GetByID(context.Context, string) (*persistence.ChatAuditEntry, error) {
	return f.row, f.err
}

func strpDS(s string) *string { return &s }

func TestDeliverableSend_SuccessAttachesViaArtifactID_EmailChannel(t *testing.T) {
	taskID, artifactID := "task1", "art1"
	turnID := "chat_1"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", ChatTurnID: strpDS(turnID), Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == taskID {
				return task, nil
			}
			return nil, nil
		},
	}
	mime := "text/markdown"
	artifact := &persistence.Artifact{
		ID: artifactID, ProjectID: "p1", TaskID: &taskID, Name: "report.md",
		ArtifactClass: persistence.ArtifactClassOutput, MimeType: &mime,
	}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Artifact, error) {
			if id == artifactID {
				return artifact, nil
			}
			return nil, nil
		},
	}
	row := &persistence.ChatAuditEntry{ID: turnID, ChatID: "email:<thread@x.com>", UserID: "email:ops@x.com", ProjectID: "p1"}
	ch := &fakeChannelDS{name: "email"}
	reg := prometheus.NewRegistry()
	metrics := NewDeliverableMetrics(reg)

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
		WithChatAuditRepository(fakeChatAuditDS{row: row}),
		WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"email": ch}}),
		WithDeliverableMetrics(metrics),
	)

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, ch.sent, 1, "channel Send should have been called exactly once")
	msg := ch.sent[0]
	assert.Equal(t, "<thread@x.com>", msg.SessionID)
	require.Len(t, msg.Attachments, 1)
	assert.Equal(t, artifactID, msg.Attachments[0].ArtifactID, "attachment-capable channel must carry Attachment.ArtifactID")
	assert.Equal(t, "ops@x.com", msg.ChannelSpecific["to"])
	assert.Contains(t, rec.Body.String(), "email", "confirmation fragment should name the destination channel")

	got, err := reg.Gather()
	require.NoError(t, err)
	found := false
	for _, mf := range got {
		if mf.GetName() != "vornik_deliverable_sends_total" {
			continue
		}
		for _, m := range mf.Metric {
			for _, l := range m.Label {
				if l.GetName() == "channel" && l.GetValue() == "email" {
					found = true
					assert.Equal(t, 1.0, m.GetCounter().GetValue())
				}
			}
		}
	}
	assert.True(t, found, "metric should have incremented for channel=email")
}

func TestDeliverableSend_TextOnlyChannel_LinkInText(t *testing.T) {
	taskID, artifactID := "task2", "art2"
	turnID := "chat_2"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", ChatTurnID: strpDS(turnID), Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	artifact := &persistence.Artifact{ID: artifactID, ProjectID: "p1", TaskID: &taskID, Name: "out.csv", ArtifactClass: persistence.ArtifactClassOutput}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	row := &persistence.ChatAuditEntry{ID: turnID, ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelDS{name: "telegram"}

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
		WithChatAuditRepository(fakeChatAuditDS{row: row}),
		WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"telegram": ch}}),
		WithWebUIBaseURL("https://vornik.example.com"),
	)

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, ch.sent, 1)
	msg := ch.sent[0]
	// Telegram's Send ignores Attachments, so the text must carry a usable
	// link — same convention as chatpush.go's completionMessage.
	assert.Contains(t, msg.Text, "https://vornik.example.com/ui/artifacts/"+artifactID)
	require.Len(t, msg.Attachments, 1, "attachments are still built unconditionally; the channel decides whether to use them")
}

func TestDeliverableSend_NonChatOriginated_HandledGracefully(t *testing.T) {
	taskID, artifactID := "task3", "art3"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted} // no ChatTurnID
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	artifact := &persistence.Artifact{ID: artifactID, ProjectID: "p1", TaskID: &taskID, Name: "out.csv", ArtifactClass: persistence.ArtifactClassOutput}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() { srv.DeliverableSend(rec, req) })
	require.Equal(t, http.StatusOK, rec.Code, "non-chat-originated must be handled gracefully, not error")
	assert.Contains(t, rec.Body.String(), "chat channel")
}

func TestDeliverableSend_SendError_Surfaced(t *testing.T) {
	taskID, artifactID := "task4", "art4"
	turnID := "chat_4"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", ChatTurnID: strpDS(turnID), Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	artifact := &persistence.Artifact{ID: artifactID, ProjectID: "p1", TaskID: &taskID, Name: "out.csv", ArtifactClass: persistence.ArtifactClassOutput}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	row := &persistence.ChatAuditEntry{ID: turnID, ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelDS{name: "telegram", sendErr: assertErr{}}
	reg := prometheus.NewRegistry()
	metrics := NewDeliverableMetrics(reg)

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
		WithChatAuditRepository(fakeChatAuditDS{row: row}),
		WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"telegram": ch}}),
		WithDeliverableMetrics(metrics),
	)

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Send failed")

	got, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range got {
		if mf.GetName() == "vornik_deliverable_sends_total" {
			assert.Empty(t, mf.Metric, "a failed send must not increment the metric")
		}
	}
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }

func TestDeliverableSend_OutOfScopeCaller_Rejected(t *testing.T) {
	taskID, artifactID := "task5", "art5"
	turnID := "chat_5"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", ChatTurnID: strpDS(turnID), Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	artifact := &persistence.Artifact{ID: artifactID, ProjectID: "p1", TaskID: &taskID, Name: "out.csv", ArtifactClass: persistence.ArtifactClassOutput}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	ch := &fakeChannelDS{name: "telegram"}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
		WithChatAuditRepository(fakeChatAuditDS{row: &persistence.ChatAuditEntry{ID: turnID, ChatID: "555", ProjectID: "p1"}}),
		WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"telegram": ch}}),
	)

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	// Caller is scoped to a DIFFERENT project — must be rejected.
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "other-project"))
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "out-of-scope caller must be rejected (404, not leaking existence)")
	assert.Empty(t, ch.sent, "the channel must never be invoked for a rejected caller")
}

func TestDeliverableSend_ArtifactNotBelongingToTask_NotFound(t *testing.T) {
	taskID, artifactID := "task6", "art6"
	otherTaskID := "task-other"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", ChatTurnID: strpDS("chat_6"), Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	// Artifact belongs to a DIFFERENT task.
	artifact := &persistence.Artifact{ID: artifactID, ProjectID: "p1", TaskID: &otherTaskID, Name: "out.csv", ArtifactClass: persistence.ArtifactClassOutput}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithArtifactRepository(artifactRepo))

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeliverableSend_NonOutputArtifact_NotFound(t *testing.T) {
	taskID, artifactID := "task7", "art7"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", ChatTurnID: strpDS("chat_7"), Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	artifact := &persistence.Artifact{ID: artifactID, ProjectID: "p1", TaskID: &taskID, Name: "input.txt", ArtifactClass: persistence.ArtifactClassInput}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithArtifactRepository(artifactRepo))

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "INPUT/INTERMEDIATE artifacts must never be sendable as a deliverable")
}

func TestDeliverableSend_MissingRepos_ServiceUnavailable(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/tasks/t/artifacts/a/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestDeliverableSend_WrongMethod_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	req := httptest.NewRequest(http.MethodGet, "/tasks/t/artifacts/a/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestDeliverableSend_BadPath_BadRequest(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}), WithArtifactRepository(&mocks.MockArtifactRepository{}))
	req := httptest.NewRequest(http.MethodPost, "/tasks/onlyonesegment", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDeliverableSend_TaskNotFound(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return nil, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithArtifactRepository(&mocks.MockArtifactRepository{}))
	req := httptest.NewRequest(http.MethodPost, "/tasks/missing/artifacts/a/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// --- taskRouter dispatch --------------------------------------------------

func TestTaskRouter_DispatchesDeliverableSend(t *testing.T) {
	taskID, artifactID := "task8", "art8"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
	}
	artifact := &persistence.Artifact{ID: artifactID, ProjectID: "p1", TaskID: &taskID, Name: "out.csv", ArtifactClass: persistence.ArtifactClassOutput}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return artifact, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithArtifactRepository(artifactRepo))
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/artifacts/"+artifactID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// Non-chat-originated task → graceful 200, proving the router reached
	// DeliverableSend (a mis-route would 404 via TaskDetail's GET path or
	// method-not-allowed).
	assert.Equal(t, http.StatusOK, rec.Code)
}
