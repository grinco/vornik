//go:build integration

package cli

// Integration coverage for the calibration harness's DB source
// (chatAuditTurnSource in memory_ned_calibrate.go): the DISTINCT-turn
// per-project counts and the deterministic, seed-stable stratum sample, driven
// against the live test Postgres via the dbcov* harness. The sampling/tally
// logic itself is unit-tested without a DB in memory_ned_calibrate_test.go; this
// asserts the SQL contract (window filter, DISTINCT, empty-project exclusion,
// determinism) that unit tests cannot.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIntegration_ChatAuditTurnSource_CountsSampleWindowAndDeterminism(t *testing.T) {
	db := dbcovSetup(t)
	ctx := context.Background()

	marker := fmt.Sprintf("nedcal-%d", time.Now().UnixNano())
	projA := "nedcal-a-" + marker
	projB := "nedcal-b-" + marker

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM chat_audit_log WHERE user_id = $1`, marker)
	})

	now := time.Now().UTC()
	inWindow := now.Add(-2 * 24 * time.Hour) // 2 days ago (inside a 30d window)
	oldTurn := now.Add(-60 * 24 * time.Hour) // 60 days ago (outside)
	since := now.Add(-30 * 24 * time.Hour)

	ins := func(project, msg string, ts time.Time) {
		t.Helper()
		id := fmt.Sprintf("cal-%d", time.Now().UnixNano())
		time.Sleep(time.Microsecond)
		if _, err := db.Exec(
			`INSERT INTO chat_audit_log (id, project_id, user_id, user_message, ts) VALUES ($1,$2,$3,$4,$5)`,
			id, project, marker, msg, ts); err != nil {
			t.Fatalf("seed chat turn: %v", err)
		}
	}

	// Project A: 5 distinct in-window texts, one DUPLICATE of an existing text
	// (must collapse under DISTINCT), and one OLD text (outside the window).
	for i := 0; i < 5; i++ {
		ins(projA, fmt.Sprintf("A message number %d", i), inWindow)
	}
	ins(projA, "A message number 0", inWindow) // duplicate content
	ins(projA, "A ancient message", oldTurn)   // outside window
	// Project B: 2 distinct in-window texts.
	ins(projB, "B first", inWindow)
	ins(projB, "B second", inWindow)
	// Empty project id: must be excluded from counts entirely.
	ins("", "orphan turn one", inWindow)
	ins("", "orphan turn two", inWindow)

	src := &chatAuditTurnSource{db: db}

	counts, err := src.AvailableCounts(ctx, since)
	if err != nil {
		t.Fatalf("AvailableCounts: %v", err)
	}
	if counts[projA] != 5 {
		t.Errorf("project A distinct in-window count = %d, want 5 (duplicate collapsed, old excluded)", counts[projA])
	}
	if counts[projB] != 2 {
		t.Errorf("project B count = %d, want 2", counts[projB])
	}
	if _, ok := counts[""]; ok {
		t.Errorf("empty project id must be excluded from counts; got %d", counts[""])
	}

	// Sample is capped at availability and returns DISTINCT texts.
	got, err := src.SampleStratum(ctx, projA, since, 3, "seed-1")
	if err != nil {
		t.Fatalf("SampleStratum: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("sample size = %d, want 3", len(got))
	}
	seen := map[string]bool{}
	for _, txt := range got {
		if seen[txt] {
			t.Errorf("sample returned a duplicate text: %q", txt)
		}
		seen[txt] = true
		if txt == "A ancient message" {
			t.Error("sample must not include an out-of-window turn")
		}
	}

	// Determinism: the SAME seed draws the SAME sample; a different seed may
	// differ but must still be valid.
	again, err := src.SampleStratum(ctx, projA, since, 3, "seed-1")
	if err != nil {
		t.Fatalf("SampleStratum (repeat): %v", err)
	}
	for i := range got {
		if got[i] != again[i] {
			t.Errorf("same seed produced a different sample at %d: %q vs %q", i, got[i], again[i])
		}
	}

	// Drawing more than available returns all distinct in-window texts (5), never
	// the old one.
	all, err := src.SampleStratum(ctx, projA, since, 100, "seed-1")
	if err != nil {
		t.Fatalf("SampleStratum (all): %v", err)
	}
	if len(all) != 5 {
		t.Errorf("over-draw returned %d, want 5 (distinct in-window)", len(all))
	}
}
