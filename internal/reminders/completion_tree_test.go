package reminders

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// A scheduled reminder must deliver what its task TREE produced, not what its
// own task row happens to own.
//
// Measured 2026-08-22 on the daily-briefing reminder: the fired task ran a
// router workflow that DELEGATED, so the reminder's own task held only the
// router's two `route-response` transcripts (886 B) while the child held the
// 3.9 KB morning briefing. The operator received the scaffolding and never the
// briefing. The design's "one fire spawns exactly one task" is true of the
// FIRE and false of the work.

// treeFixture wires a parent→child task tree with artifacts on both.
func treeFixture(t *testing.T) (*captureReminderFileSender, *CompletionNotifier, *persistence.Task) {
	t.Helper()
	older := time.Date(2026, 8, 22, 10, 3, 0, 0, time.UTC)
	newer := older.Add(2 * time.Minute)

	parent := &persistence.Task{ID: "task_router"}
	child := &persistence.Task{ID: "task_writer"}

	artifactsByTask := map[string][]*persistence.Artifact{
		"task_router": {
			// Router scaffolding — an agent step transcript, not a deliverable.
			{ID: "a_route", Name: "route-response-20260822-a1f8.md",
				ArtifactClass: persistence.ArtifactClassOutput, CreatedAt: older},
			{ID: "a_retry", Name: "route_route_retry-response-20260822-a1f8.md",
				ArtifactClass: persistence.ArtifactClassOutput, CreatedAt: older},
		},
		"task_writer": {
			{ID: "a_brief", Name: "morning-briefing-2026-08-22-20260822-68ea.md",
				ArtifactClass: persistence.ArtifactClassOutput, CreatedAt: newer},
			// Another step transcript, from the child this time.
			{ID: "a_write", Name: "write-response-20260822-68ea.md",
				ArtifactClass: persistence.ArtifactClassOutput, CreatedAt: newer},
			// Intermediate must never leave the task.
			{ID: "a_scratch", Name: "scratch.tmp",
				ArtifactClass: persistence.ArtifactClassIntermediate, CreatedAt: newer},
		},
	}

	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			if f.TaskID == nil {
				return nil, nil
			}
			return artifactsByTask[*f.TaskID], nil
		},
	}
	files := &captureReminderFileSender{}
	repo := newStubRepo()
	repo.claim = &persistence.Reminder{
		ID: "rem_digest", Kind: persistence.ReminderKindTask, Channel: "slack",
		ChannelRef: "T1/C1", Content: "Daily digest",
	}
	ch := &stubChannel{}

	n := NewCompletionNotifier(
		repo,
		&stubResolver{channels: map[string]conversation.Channel{"slack": ch}},
		nil, zerolog.Nop(), time.Now,
		WithArtifactDelivery(
			artifactRepo,
			stubReminderArtifactReader{
				"a_brief": []byte("briefing"), "a_route": []byte("route"),
				"a_retry": []byte("retry"), "a_write": []byte("write"),
			},
			stubReminderFileResolver{sender: files},
			stubChildLister{"task_router": {child}},
		),
	)
	return files, n, parent
}

func TestDeliverOutputArtifacts_ReachesTheChildTasksDeliverable(t *testing.T) {
	files, n, parent := treeFixture(t)

	n.NotifyTaskCompleted(context.Background(), parent, true, "done")

	got := files.names
	if len(got) != 1 || got[0] != "morning-briefing-2026-08-22-20260822-68ea.md" {
		t.Fatalf("delivered %v; want exactly the child's briefing — the deliverable "+
			"lives on the delegated task, and the router's own artifacts are transcripts", got)
	}
}

// Widening to the tree without excluding transcripts would have made the bug
// worse: today's digest would have shipped four files instead of two, three of
// them agent step transcripts the operator explicitly did not want.
func TestDeliverOutputArtifacts_ExcludesStepTranscripts(t *testing.T) {
	files, n, parent := treeFixture(t)

	n.NotifyTaskCompleted(context.Background(), parent, true, "done")

	for _, name := range files.names {
		switch name {
		case "route-response-20260822-a1f8.md",
			"route_route_retry-response-20260822-a1f8.md",
			"write-response-20260822-68ea.md":
			t.Errorf("%s is a per-step transcript and must not be delivered", name)
		case "scratch.tmp":
			t.Error("intermediate artifacts must stay private to the task")
		}
	}
}

// A missing child lister must degrade to the old single-task behaviour rather
// than dropping delivery entirely — an unwired dependency should cost reach,
// not the whole feature.
func TestDeliverOutputArtifacts_WithoutChildListerStillSendsTheOwnTask(t *testing.T) {
	older := time.Date(2026, 8, 22, 10, 3, 0, 0, time.UTC)
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{{ID: "a_doc", Name: "digest.md",
				ArtifactClass: persistence.ArtifactClassOutput, CreatedAt: older}}, nil
		},
	}
	files := &captureReminderFileSender{}
	soloRepo := newStubRepo()
	soloRepo.claim = &persistence.Reminder{
		ID: "rem_solo", Kind: persistence.ReminderKindTask, Channel: "slack",
		ChannelRef: "T1/C1", Content: "Daily digest"}
	n := NewCompletionNotifier(
		soloRepo,
		&stubResolver{channels: map[string]conversation.Channel{"slack": &stubChannel{}}},
		nil, zerolog.Nop(), time.Now,
		WithArtifactDelivery(artifactRepo,
			stubReminderArtifactReader{"a_doc": []byte("doc")},
			stubReminderFileResolver{sender: files}, nil),
	)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_solo"}, true, "done")

	if len(files.names) != 1 || files.names[0] != "digest.md" {
		t.Errorf("a nil child lister must still deliver the task's own outputs, got %v", files.names)
	}
}

// A cycle in parentage must not spin. It should not occur, but data heals
// imperfectly and the walk is on the delivery path.
func TestCollectTreeOutputs_TerminatesOnACycle(t *testing.T) {
	a := &persistence.Task{ID: "task_a"}
	b := &persistence.Task{ID: "task_b"}
	n := &CompletionNotifier{
		logger: zerolog.Nop(),
		artifactRepo: &mocks.MockArtifactRepository{
			ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
				return nil, nil
			},
		},
		childLister: stubChildLister{"task_a": {b}, "task_b": {a}},
	}

	done := make(chan int, 1)
	go func() { done <- len(n.collectTreeOutputs(context.Background(), a)) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the descendant walk did not terminate on a parentage cycle")
	}
}
