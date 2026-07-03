package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestJudgeVerdict_ListRecentSince — the Since-bounded query (E1, audit
// 2026-07-03) applies recorded_at >= since in SQL, so rows older than the
// window are not returned (previously fetched to a 5000 cap and dropped in Go).
func TestJudgeVerdict_ListRecentSince(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskJudgeVerdictRepository(db.DB)
	now := time.Now().UTC()

	rows := []*persistence.TaskJudgeVerdict{
		{ID: "v-fresh1", ProjectID: "p1", TaskID: "t1", Role: "r", Model: "m", Verdict: "pass", RecordedAt: now.Add(-1 * time.Hour)},
		{ID: "v-fresh2", ProjectID: "p1", TaskID: "t2", Role: "r", Model: "m", Verdict: "fail", RecordedAt: now.Add(-2 * time.Hour)},
		{ID: "v-stale", ProjectID: "p1", TaskID: "t3", Role: "r", Model: "m", Verdict: "pass", RecordedAt: now.Add(-72 * time.Hour)},
	}
	for _, v := range rows {
		if err := repo.Record(ctx, v); err != nil {
			t.Fatalf("Record %s: %v", v.ID, err)
		}
	}

	since := now.Add(-24 * time.Hour)
	got, err := repo.ListRecentSince(ctx, "p1", since, 5000)
	if err != nil {
		t.Fatalf("ListRecentSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 in-window verdicts, got %d", len(got))
	}
	for _, v := range got {
		if v.ID == "v-stale" {
			t.Errorf("stale verdict outside window must be excluded by SQL")
		}
	}
}
