package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Regression: 2026-09-16 stale-news incident.
//
// A chat session summarised an 11-day-old czech-news digest and presented it
// as "what's in the news today". The feeds were healthy and the current
// digests were in the store; the assistant listed a task's artifacts, picked
// one, read it, and had NO WAY to know it was from 5 September.
//
// The cause is the same omission in three places: `list_artifacts`,
// `read_artifact` and `recall` all strip the timestamp. The repository
// ORDERS BY created_at — the field is right there and is simply never shown.
//
// THE SEAM, and why this test is on the tool's rendered output: the
// repository behaves correctly, so a repository test passes. What the MODEL
// receives is the thing that was wrong, and a model cannot caveat an age it
// was never told.
func TestListArtifacts_ShowsHowOldEachArtifactIs(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-11 * 24 * time.Hour)
	tid := "t1"
	taskPtr := &tid
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	artRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{ID: "a1", TaskID: taskPtr, Name: "czech-news-20260905.md", ArtifactClass: "output", CreatedAt: old},
				{ID: "a2", TaskID: taskPtr, Name: "czech-news-latest.md", ArtifactClass: "output", CreatedAt: now},
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(taskRepo), withArtifactRepo(artRepo))
	res := te.listArtifacts(context.Background(), `{"task_id":"t1"}`, []string{"snake"})

	if !strings.Contains(res.Content, old.Format("2006-01-02")) {
		t.Errorf("the listing does not show WHEN each artifact was produced, so an 11-day-old digest is indistinguishable from today's:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, now.Format("2006-01-02")) {
		t.Errorf("the newest artifact's date is missing too:\n%s", res.Content)
	}
}

// Reading one must say when it was produced, because that is the moment the
// content enters the model's answer.
func TestReadArtifact_HeaderCarriesTheProductionTime(t *testing.T) {
	old := time.Now().UTC().Add(-11 * 24 * time.Hour)
	tid := "t1"
	taskPtr := &tid
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "czech-news.md")
	if err := os.WriteFile(path, []byte("# Czech news\n\nOld headlines.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{ID: "a1", TaskID: taskPtr, Name: "czech-news.md", ArtifactClass: "output", CreatedAt: old, StoragePath: path},
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(taskRepo), withArtifactRepo(artRepo))
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"czech-news.md"}`, []string{"snake"})

	if !strings.Contains(res.Content, old.Format("2006-01-02")) {
		t.Fatalf("read_artifact does not say when the content was produced — the model cannot tell 11-day-old news from today's:\n%s", res.Content)
	}
}
