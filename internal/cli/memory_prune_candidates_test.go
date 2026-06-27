package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type stubPruneRepo struct {
	ids      []string
	err      error
	gotProj  string
	gotSince time.Time
	gotLimit int
}

func (s *stubPruneRepo) UnretrievedChunkIDs(_ context.Context, projectID string, since time.Time, limit int) ([]string, error) {
	s.gotProj = projectID
	s.gotSince = since
	s.gotLimit = limit
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

func TestDoMemoryPruneCandidates_EmptyList(t *testing.T) {
	repo := &stubPruneRepo{ids: nil}
	w, read := captureStdout(t)
	err := doMemoryPruneCandidates(context.Background(), repo, "p", 30*24*time.Hour, 100, false, w)
	if err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "no auto-prune candidates") {
		t.Fatalf("missing empty message: %s", got)
	}
}

func TestDoMemoryPruneCandidates_PrintsTable(t *testing.T) {
	repo := &stubPruneRepo{ids: []string{"chunk-a", "chunk-b", "chunk-c"}}
	w, read := captureStdout(t)
	if err := doMemoryPruneCandidates(context.Background(), repo, "p", 7*24*time.Hour, 50, false, w); err != nil {
		t.Fatal(err)
	}
	got := read()
	for _, want := range []string{"3 chunks", "chunk-a", "chunk-b", "chunk-c"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
}

func TestDoMemoryPruneCandidates_JSON(t *testing.T) {
	repo := &stubPruneRepo{ids: []string{"c1", "c2"}}
	w, read := captureStdout(t)
	if err := doMemoryPruneCandidates(context.Background(), repo, "assistant", 14*24*time.Hour, 25, true, w); err != nil {
		t.Fatal(err)
	}
	raw := read()
	var out struct {
		Project        string   `json:"project"`
		CandidateCount int      `json:"candidate_count"`
		ChunkIDs       []string `json:"chunk_ids"`
		Limit          int      `json:"limit"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if out.Project != "assistant" || out.CandidateCount != 2 || out.Limit != 25 {
		t.Fatalf("envelope: %+v", out)
	}
	if len(out.ChunkIDs) != 2 || out.ChunkIDs[0] != "c1" {
		t.Fatalf("ids: %+v", out.ChunkIDs)
	}
}

func TestDoMemoryPruneCandidates_DefaultsClamped(t *testing.T) {
	repo := &stubPruneRepo{ids: nil}
	w, _ := captureStdout(t)
	// window=0 + limit=0 → defaults 30d / 100.
	_ = doMemoryPruneCandidates(context.Background(), repo, "p", 0, 0, false, w)
	if repo.gotLimit != 100 {
		t.Fatalf("limit default not applied: %d", repo.gotLimit)
	}
	expectedSince := time.Now().Add(-30 * 24 * time.Hour)
	if delta := repo.gotSince.Sub(expectedSince); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("since default not applied: delta=%v", delta)
	}
}

func TestDoMemoryPruneCandidates_RepoError(t *testing.T) {
	repo := &stubPruneRepo{err: errors.New("db down")}
	w, _ := captureStdout(t)
	if err := doMemoryPruneCandidates(context.Background(), repo, "p", 0, 0, false, w); err == nil {
		t.Fatal("want err")
	}
}
