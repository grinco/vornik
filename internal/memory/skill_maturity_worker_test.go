package memory

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func TestSkillShouldPromote(t *testing.T) {
	cfg := SkillMaturityConfig{PromoteWorked: 5, RetireCorrected: 3, StaleDays: 90}
	active := func(w, c int64) *persistence.Skill {
		return &persistence.Skill{Maturity: persistence.SkillMaturityActive, UsageWorked: w, UsageCorrected: c}
	}
	if !skillShouldPromote(active(5, 0), cfg) {
		t.Error("5 worked / 0 corrected should promote")
	}
	if skillShouldPromote(active(4, 0), cfg) {
		t.Error("4 worked is below threshold")
	}
	if skillShouldPromote(active(9, 1), cfg) {
		t.Error("any correction blocks promotion")
	}
	if skillShouldPromote(&persistence.Skill{Maturity: persistence.SkillMaturityDraft, UsageWorked: 9}, cfg) {
		t.Error("draft never auto-promotes")
	}
	if skillShouldPromote(&persistence.Skill{Maturity: persistence.SkillMaturityTrusted, UsageWorked: 9}, cfg) {
		t.Error("already-trusted is not re-promoted")
	}
}

func TestSkillShouldRetire(t *testing.T) {
	cfg := SkillMaturityConfig{PromoteWorked: 5, RetireCorrected: 3, StaleDays: 90}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * 24 * time.Hour)
	fresh := now.Add(-1 * 24 * time.Hour)

	if !skillShouldRetire(&persistence.Skill{Maturity: persistence.SkillMaturityActive, UsageCorrected: 3}, now, cfg) {
		t.Error("3 corrections should retire an active skill")
	}
	if !skillShouldRetire(&persistence.Skill{Maturity: persistence.SkillMaturityTrusted, UsageCorrected: 3}, now, cfg) {
		t.Error("3 corrections should retire a trusted skill too")
	}
	if !skillShouldRetire(&persistence.Skill{Maturity: persistence.SkillMaturityActive, LastFiredAt: &stale}, now, cfg) {
		t.Error("stale active (never trusted) should retire")
	}
	if skillShouldRetire(&persistence.Skill{Maturity: persistence.SkillMaturityTrusted, LastFiredAt: &stale}, now, cfg) {
		t.Error("trusted is not retired on staleness alone")
	}
	if skillShouldRetire(&persistence.Skill{Maturity: persistence.SkillMaturityActive, LastFiredAt: &fresh}, now, cfg) {
		t.Error("fresh active must not retire")
	}
	if skillShouldRetire(&persistence.Skill{Maturity: persistence.SkillMaturityDraft, UsageCorrected: 9}, now, cfg) {
		t.Error("draft is never retired by the worker")
	}
}

func TestSkillMaturityWorker_Tick(t *testing.T) {
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := sqlite.NewSkillRepository(db.DB)
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * 24 * time.Hour)

	mk := func(id, name, maturity string, worked, corrected int64, last *time.Time) {
		if err := repo.Create(ctx, &persistence.Skill{
			ID: id, ProjectID: "p1", RepoScope: "github.com/x/m", Name: name,
			Description: "d", Body: "b", BodySHA256: "h", Maturity: maturity,
			UsageWorked: worked, UsageCorrected: corrected, LastFiredAt: last,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("w-promote", "promote-me", persistence.SkillMaturityActive, 5, 0, &now)
	mk("w-retire-c", "retire-corrected", persistence.SkillMaturityActive, 2, 3, &now)
	mk("w-retire-s", "retire-stale", persistence.SkillMaturityActive, 1, 0, &stale)
	mk("w-stay", "stay-active", persistence.SkillMaturityActive, 2, 0, &now)

	w := &SkillMaturityWorker{Skills: repo, Now: func() time.Time { return now }}
	w.tick(ctx)

	assertMaturity := func(id, want string) {
		got, _ := repo.GetByID(ctx, id)
		if got.Maturity != want {
			t.Errorf("%s: maturity = %s, want %s", id, got.Maturity, want)
		}
	}
	assertMaturity("w-promote", persistence.SkillMaturityTrusted)
	assertMaturity("w-retire-c", persistence.SkillMaturityRetired)
	assertMaturity("w-retire-s", persistence.SkillMaturityRetired)
	assertMaturity("w-stay", persistence.SkillMaturityActive)
}
