package sqlite_test

import (
	"context"
	"testing"
)

// TestSupersessionEpochConsistencyCheck is the hardening regression
// (2026-06-15): a recorded superseded_in_epoch is only valid on a
// chunk whose validation_status is 'superseded' (mirrors postgres
// migration 100). A NULL epoch is always allowed — including on a
// superseded chunk (empty epochID / pre-migration history).
func TestSupersessionEpochConsistencyCheck(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	insert := func(id, status string, epoch any) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO project_memory_chunks (id, project_id, created_at, validation_status, superseded_in_epoch)
			VALUES (?, 'p1', '2026-06-15T00:00:00Z', ?, ?)`, id, status, epoch)
		return err
	}

	// Epoch set on a non-superseded chunk → rejected by the CHECK.
	if err := insert("c1", "unverified", "epoch-1"); err == nil {
		t.Error("epoch on a non-superseded chunk should be rejected")
	}
	// Epoch set on a superseded chunk → allowed.
	if err := insert("c2", "superseded", "epoch-1"); err != nil {
		t.Errorf("epoch on a superseded chunk should be allowed: %v", err)
	}
	// Superseded chunk with NULL epoch (empty epochID / legacy) → allowed.
	if err := insert("c3", "superseded", nil); err != nil {
		t.Errorf("superseded chunk with NULL epoch should be allowed: %v", err)
	}
	// Ordinary non-superseded chunk with NULL epoch → allowed.
	if err := insert("c4", "unverified", nil); err != nil {
		t.Errorf("non-superseded chunk with NULL epoch should be allowed: %v", err)
	}
}
