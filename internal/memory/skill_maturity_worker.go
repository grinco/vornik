package memory

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Knowledge-skill maturity engine (LLD 2026-07-07-knowledge-skill-
// learning-loop-design §D.3). A leader-gated ticker promotes
// operator-approved skills active→trusted once they've accumulated
// enough clean "worked" signals, and decays active/trusted skills to
// retired when they keep getting corrected or go stale. It NEVER
// touches drafts (only the human gate promotes a draft) and never
// auto-activates anything.

// Default maturity thresholds (conservative). Tunable via
// SkillMaturityConfig; zero fields fall back to these.
const (
	defaultSkillPromoteWorked   = 5
	defaultSkillRetireCorrected = 3
	defaultSkillStaleDays       = 90
)

// SkillMaturityConfig holds the promotion/decay thresholds.
type SkillMaturityConfig struct {
	// PromoteWorked is the number of clean "worked" signals an active
	// skill needs before promotion to trusted.
	PromoteWorked int
	// RetireCorrected is the number of "corrected" signals that retires
	// an active/trusted skill.
	RetireCorrected int
	// StaleDays retires an active (never-trusted) skill that hasn't
	// fired in this many days.
	StaleDays int
}

func (c SkillMaturityConfig) withDefaults() SkillMaturityConfig {
	if c.PromoteWorked <= 0 {
		c.PromoteWorked = defaultSkillPromoteWorked
	}
	if c.RetireCorrected <= 0 {
		c.RetireCorrected = defaultSkillRetireCorrected
	}
	if c.StaleDays <= 0 {
		c.StaleDays = defaultSkillStaleDays
	}
	return c
}

// skillShouldPromote reports whether an active skill has earned trusted.
// Strict form: any correction blocks promotion. (The design's
// alternative worked/(worked+corrected) ratio was deliberately dropped
// for v1 — stricter is safer for content that injects as trusted
// directives; revisit if promotion proves too conservative.)
func skillShouldPromote(s *persistence.Skill, cfg SkillMaturityConfig) bool {
	return s.Maturity == persistence.SkillMaturityActive &&
		s.UsageCorrected == 0 && s.UsageWorked >= int64(cfg.PromoteWorked)
}

// skillShouldRetire reports whether a skill has decayed out. Corrections
// retire either state; staleness retires only an active (never-trusted)
// skill.
func skillShouldRetire(s *persistence.Skill, now time.Time, cfg SkillMaturityConfig) bool {
	if s.Maturity != persistence.SkillMaturityActive && s.Maturity != persistence.SkillMaturityTrusted {
		return false
	}
	if s.UsageCorrected >= int64(cfg.RetireCorrected) {
		return true
	}
	if s.Maturity == persistence.SkillMaturityActive && s.LastFiredAt != nil &&
		now.Sub(*s.LastFiredAt) > time.Duration(cfg.StaleDays)*24*time.Hour {
		return true
	}
	return false
}

// SkillMaturityWorker runs the periodic promote/decay pass.
type SkillMaturityWorker struct {
	Skills   persistence.SkillRepository
	Interval time.Duration
	Cfg      SkillMaturityConfig
	Logger   zerolog.Logger
	// LeaderGate gates the tick to the elected leader so two daemons
	// don't double-transition. Nil-safe.
	LeaderGate LeaderGate
	// Now is injectable for tests; nil → time.Now.
	Now func() time.Time

	stopped chan struct{}
}

func (w *SkillMaturityWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// Run drives the periodic loop until ctx is cancelled or the worker is
// structurally disabled (nil Skills repo or non-positive Interval).
func (w *SkillMaturityWorker) Run(ctx context.Context) {
	if w == nil || w.Skills == nil {
		return
	}
	if w.Interval <= 0 {
		w.Logger.Debug().Msg("skill-maturity worker disabled by config")
		return
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)
	w.Logger.Info().Dur("interval", w.Interval).Msg("skill-maturity worker started")
	defer w.Logger.Info().Msg("skill-maturity worker stopped")

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	if w.LeaderGate == nil || w.LeaderGate.IsLeader() {
		w.tick(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.LeaderGate != nil && !w.LeaderGate.IsLeader() {
				continue
			}
			w.tick(ctx)
		}
	}
}

// Stopped returns a channel closed when Run exits (test sync).
func (w *SkillMaturityWorker) Stopped() <-chan struct{} {
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	return w.stopped
}

// tick evaluates every active/trusted skill once. Per-skill failure is
// logged but never halts the pass.
func (w *SkillMaturityWorker) tick(ctx context.Context) {
	cfg := w.Cfg.withDefaults()
	now := w.now()
	skills, err := w.Skills.ListForMaturityScan(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("skill-maturity: scan failed")
		return
	}
	var promoted, retired int
	for _, s := range skills {
		switch {
		case skillShouldRetire(s, now, cfg):
			if err := w.Skills.SetMaturity(ctx, s.ID, persistence.SkillMaturityRetired); err != nil {
				w.Logger.Warn().Err(err).Str("skill_id", s.ID).Msg("skill-maturity: retire failed")
				continue
			}
			retired++
			w.Logger.Info().Str("skill_id", s.ID).Str("name", s.Name).
				Int64("corrected", s.UsageCorrected).Msg("skill-maturity: retired")
		case skillShouldPromote(s, cfg):
			if err := w.Skills.SetMaturity(ctx, s.ID, persistence.SkillMaturityTrusted); err != nil {
				w.Logger.Warn().Err(err).Str("skill_id", s.ID).Msg("skill-maturity: promote failed")
				continue
			}
			promoted++
			w.Logger.Info().Str("skill_id", s.ID).Str("name", s.Name).
				Int64("worked", s.UsageWorked).Msg("skill-maturity: promoted to trusted")
		}
	}
	if promoted > 0 || retired > 0 {
		w.Logger.Info().Int("promoted", promoted).Int("retired", retired).
			Msg("skill-maturity: tick complete")
	}
}
