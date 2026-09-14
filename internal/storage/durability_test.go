package storage

import (
	"context"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/config"
)

// Review R6 / plan §7g: the SQLite branch must open with synchronous=FULL so
// the apply journal's PREPARED row is durable before the first file write.
// Before 2026-09-13 the DSN set only WAL, busy_timeout and foreign_keys,
// leaving synchronous at the driver default (NORMAL under WAL) — this test
// fails against that DSN.
func TestSQLiteOpensWithSynchronousFull(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, config.DatabaseConfig{Driver: "sqlite", Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	rep := b.ProbeDurability(ctx)
	if !rep.OK {
		t.Fatalf("sqlite backend must report a durable commit contract, got %+v", rep)
	}
	if rep.Driver != "sqlite" {
		t.Fatalf("driver = %q", rep.Driver)
	}
}

func TestProbeDurability_NilAndUnknown(t *testing.T) {
	if rep := ProbeDurability(context.Background(), "sqlite", nil); rep.OK {
		t.Fatal("nil db must not be durable")
	}
	if rep := (*Backend)(nil).ProbeDurability(context.Background()); rep.OK {
		t.Fatal("nil backend must not be durable")
	}
	if rep := ProbeDurability(context.Background(), "oracle", nil); rep.OK {
		t.Fatal("unknown driver must not be durable")
	}
}
