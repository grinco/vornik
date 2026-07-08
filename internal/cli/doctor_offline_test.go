package cli

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestMigrationHead: the offline doctor's head must equal the highest version
// in the binary's migration set (so "applied < head" is an accurate signal).
func TestMigrationHead(t *testing.T) {
	head := migrationHead()
	if head <= 0 {
		t.Fatalf("migrationHead must be positive, got %d", head)
	}
	wantMax := 0
	for _, m := range persistence.DefaultMigrations {
		if m.Version > wantMax {
			wantMax = m.Version
		}
	}
	if head != wantMax {
		t.Errorf("migrationHead = %d, want max(DefaultMigrations)=%d", head, wantMax)
	}
	// Control-plane migration 117 must be present (proposal ledger).
	if head < 117 {
		t.Errorf("expected head >= 117 (control_plane_proposals), got %d", head)
	}
}
