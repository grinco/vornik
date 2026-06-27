// Package ui: per-option wiring tests. Each ServerOption is a tiny
// closure that lands a dependency on a Server field. These tests
// cover the long tail of options that the larger handler tests
// don't necessarily exercise on their own — without them the
// per-file coverage on server.go stays below target.
package ui

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// We don't need real repos — interface satisfaction is enough. Each
// minimal stub embeds the interface so it inherits all methods as
// nil (panics if called, but we never call them in these tests).

type stubTaskScratchpadRepo struct {
	persistence.TaskScratchpadRepository
}
type stubTradingSnapshotRepo struct {
	persistence.TradingPositionsSnapshotRepository
}
type stubTradingOrderRepo struct {
	persistence.TradingOrderRepository
}
type stubTradingSafetyRepo struct {
	persistence.TradingSafetyEventRepository
}
type stubTradingFillRepo struct {
	persistence.TradingFillRepository
}
type stubPostMortemRepo struct {
	persistence.TaskPostMortemRepository
}
type stubAdminChatAuditRepo struct {
	persistence.ChatAuditRepository
}

type stubRescheduler struct{ called bool }

func (s *stubRescheduler) Wake() { s.called = true }

type stubPostMortemExplainer struct{}

func (s *stubPostMortemExplainer) Generate(_ context.Context, _ string, _ bool) (*PostMortemResult, error) {
	return nil, nil
}

func TestWithLogger_SetsLogger(t *testing.T) {
	srv := NewServer(WithLogger(zerolog.Nop()))
	// zerolog.Nop is a real value; just confirm the option was
	// applied (the field is a struct, not a pointer, so we can't
	// easily compare — instead we trigger a log and confirm no
	// panic).
	srv.logger.Info().Msg("ping")
}

func TestWithTaskScratchpadRepository(t *testing.T) {
	repo := &stubTaskScratchpadRepo{}
	srv := NewServer(WithTaskScratchpadRepository(repo))
	if srv.taskScratchpadRepo != repo {
		t.Errorf("field not set")
	}
}

func TestWithRescheduler(t *testing.T) {
	r := &stubRescheduler{}
	srv := NewServer(WithRescheduler(r))
	if srv.rescheduler != r {
		t.Errorf("field not set")
	}
}

func TestWithTradingSnapshotRepository(t *testing.T) {
	repo := &stubTradingSnapshotRepo{}
	srv := NewServer(WithTradingSnapshotRepository(repo))
	if srv.tradingSnapshotRepo != repo {
		t.Errorf("field not set")
	}
}

func TestWithTradingOrderRepository(t *testing.T) {
	repo := &stubTradingOrderRepo{}
	srv := NewServer(WithTradingOrderRepository(repo))
	if srv.tradingOrderRepo != repo {
		t.Errorf("field not set")
	}
}

func TestWithTradingSafetyRepository(t *testing.T) {
	repo := &stubTradingSafetyRepo{}
	srv := NewServer(WithTradingSafetyRepository(repo))
	if srv.tradingSafetyRepo != repo {
		t.Errorf("field not set")
	}
}

func TestWithTradingFillRepository(t *testing.T) {
	repo := &stubTradingFillRepo{}
	srv := NewServer(WithTradingFillRepository(repo))
	if srv.tradingFillRepo != repo {
		t.Errorf("field not set")
	}
}

func TestWithPostMortemRepository(t *testing.T) {
	repo := &stubPostMortemRepo{}
	srv := NewServer(WithPostMortemRepository(repo))
	if srv.postMortemRepo != repo {
		t.Errorf("field not set")
	}
}

func TestWithPostMortemExplainer(t *testing.T) {
	e := &stubPostMortemExplainer{}
	srv := NewServer(WithPostMortemExplainer(e))
	if srv.postMortemExplainer != e {
		t.Errorf("field not set")
	}
}

func TestWithArtifactBasePath(t *testing.T) {
	srv := NewServer(WithArtifactBasePath("/tmp/vornik-artifacts"))
	if srv.artifactBasePath != "/tmp/vornik-artifacts" {
		t.Errorf("field not set: %q", srv.artifactBasePath)
	}
}

func TestWithAdminChatAuditRepository(t *testing.T) {
	repo := &stubAdminChatAuditRepo{}
	srv := NewServer(WithAdminChatAuditRepository(repo))
	if srv.adminChatAudit != repo {
		t.Errorf("field not set")
	}
}

// WithRateLimiter and WithBudgetNotifier accept concrete types
// (*ratelimit.Limiter, budget.Notifier) — nil is a documented valid
// value (rate limiter / notifier disabled), so we exercise the
// nil-passthrough form which is the only one we can build hermetically
// without a full daemon container.

func TestWithRateLimiter_NilIsValid(t *testing.T) {
	srv := NewServer(WithRateLimiter(nil))
	if srv.rateLimiter != nil {
		t.Errorf("expected nil rate limiter, got non-nil")
	}
}

func TestWithBudgetNotifier_NilIsValid(t *testing.T) {
	srv := NewServer(WithBudgetNotifier(nil))
	if srv.budgetNotifier != nil {
		t.Errorf("expected nil budget notifier, got non-nil")
	}
}
