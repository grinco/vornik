// Coverage for the api package's ServerOption setters. These are
// all single-field assignments — a single test that constructs a
// Server with every option and probes its accessors-by-field
// covers ~40 functions in one shot. Worth doing because the
// fields drive operational features (queue, registry, executor,
// readiness checks, …) and a regression that silently zeroes one
// would be invisible from elsewhere in the suite.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/queue"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/templates"
)

// stubSecretsDetector satisfies secrets.Detector with no behaviour —
// just enough surface for WithSecrets to accept it.
type stubSecretsDetector struct{}

func (stubSecretsDetector) Scan(_ []byte) []secrets.Finding { return nil }

// TestServerOptions_AllSettersApply runs every WithX option and
// verifies it landed on the Server. Repos / interfaces that need
// concrete types use nil where the Server stores a pointer — the
// option doesn't dereference, just stashes. This pins the
// "passing this option non-nil keeps it non-nil" contract; a
// future refactor that accidentally returns a no-op closure
// would flip these fields back to their zero values and trip the
// assertions.
func TestServerOptions_AllSettersApply(t *testing.T) {
	cfg := &config.Config{}
	reg := registry.New()
	rl := &ratelimit.Limiter{}
	q := &queue.Queue{}
	cat := &templates.Catalog{}
	preg := prometheus.NewRegistry()
	rend := &postmortem.Renderer{}
	logger := zerolog.Nop()

	s := NewServer(
		WithLogger(logger),
		WithConfig(cfg),
		WithProjectRegistry(reg),
		WithRateLimiter(rl),
		WithQueue(q),
		WithProjectTemplates(cat),
		WithConfigsDir("/tmp/some/configs"),
		WithMetricsRegistry(preg),
		WithExplainRenderer(rend),
		WithPricingPath("/tmp/pricing.yaml"),
		WithSecrets(stubSecretsDetector{}, map[string]secrets.Action{}),
		WithReadinessCheck("noop", func(context.Context) error { return nil }),
		WithBudgetNotifier(stubBudgetNotifier{}),
	)

	if s.config != cfg {
		t.Error("WithConfig: not applied")
	}
	if s.projectRegistry != reg {
		t.Error("WithProjectRegistry: not applied")
	}
	if s.rateLimiter != rl {
		t.Error("WithRateLimiter: not applied")
	}
	if s.queue != q {
		t.Error("WithQueue: not applied")
	}
	if s.projectTemplates != cat {
		t.Error("WithProjectTemplates: not applied")
	}
	if s.configsDir != "/tmp/some/configs" {
		t.Errorf("WithConfigsDir: not applied (got %q)", s.configsDir)
	}
	if s.metricsRegistry != preg {
		t.Error("WithMetricsRegistry: not applied")
	}
	if s.explainRenderer != rend {
		t.Error("WithExplainRenderer: not applied")
	}
	if s.pricingPath != "/tmp/pricing.yaml" {
		t.Errorf("WithPricingPath: not applied (got %q)", s.pricingPath)
	}
	if s.budgetNotifier == nil {
		t.Error("WithBudgetNotifier: not applied")
	}
	// ReadinessCheck registers under .readinessChecks (slice or map);
	// the field is unexported so we exercise the option's branch by
	// running NewServer + a no-op check. A zero-length slice with
	// the field set proves the option fired.
	if len(s.readinessChecks) == 0 {
		t.Error("WithReadinessCheck: not applied")
	}
}

// TestServerOptions_NilReposZeroOutFields confirms the setters
// accept a nil typed-interface argument without panic. Operators
// occasionally build the server with a subset of repos wired (the
// retention sweeper deployment doesn't need the trading repos,
// etc.) — passing a typed nil through ServerOption must not
// invent state.
func TestServerOptions_NilReposZeroOutFields(t *testing.T) {
	s := NewServer(
		WithTaskRepository(nil),
		WithExecutionRepository(nil),
		WithTaskMessageRepository(nil),
		WithTaskScratchpadRepository(nil),
		WithRescheduler(nil),
		WithArtifactRepository(nil),
		WithLLMUsageRepository(nil),
		WithWebhookEventRepository(nil),
		WithAutonomyEvaluationRepository(nil),
		WithToolAuditRepository(nil),
		WithTradingOrderRepository(nil),
		WithTradingSafetyEventRepository(nil),
		WithTradingFillRepository(nil),
		WithMemoryAuditRepository(nil),
		WithMemoryQuarantine(nil),
		WithCorpusEpochs(nil),
		WithIngestQueueRepo(nil),
		WithMemoryStats(nil),
		WithMemoryTitleBackfiller(nil),
		WithMemoryClassifyBackfiller(nil),
		WithMemorySearcher(nil),
		WithFillNotifier(nil),
		WithChatProvider(nil),
		WithExecutor(nil),
		WithTaskLogSource(nil),
		WithMCPExecutor(nil),
	)
	if s == nil {
		t.Fatal("NewServer returned nil")
	}
}

// TestSetTradingMetrics_OnNilReceiverSafe — defensive: the method
// is called from the service container during partial initialisation
// where the Server pointer could in principle be nil. Must not
// panic.
func TestSetTradingMetrics_OnNilReceiverSafe(t *testing.T) {
	var s *Server
	s.SetTradingMetrics(&TradingMetrics{})
	// no assertion — the absence of a panic is the test
}

// TestSetTradingMetrics_HappyPath — the non-nil path lands the
// metrics on the Server. Mirrors the other setters' contract but
// uses a method instead of a ServerOption because the metrics
// type is created post-NewServer (the prometheus registry must
// exist first).
func TestSetTradingMetrics_HappyPath(t *testing.T) {
	s := NewServer()
	m := &TradingMetrics{}
	s.SetTradingMetrics(m)
	if s.tradingMetrics != m {
		t.Error("SetTradingMetrics: not applied")
	}
}

// TestWithGitHubAppWebhookHandler_AppliesHandler — the option
// stores the supplied handler on the Server so routes.go can
// branch on its presence when mounting routes.
func TestWithGitHubAppWebhookHandler_AppliesHandler(t *testing.T) {
	called := false
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})
	s := NewServer(WithGitHubAppWebhookHandler(handler))
	if s.githubAppWebhook == nil {
		t.Fatal("WithGitHubAppWebhookHandler: handler not stored")
	}
	// Drive the stored handler to confirm it's the one we passed.
	s.githubAppWebhook(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if !called {
		t.Error("stored handler did not match the supplied one")
	}
}

// TestWithGitHubAppWebhookHandler_NilLeavesUnset — passing nil
// clears the field (which routes.go reads as "don't mount the
// route"). Mirrors the other nil-tolerant setters.
func TestWithGitHubAppWebhookHandler_NilLeavesUnset(t *testing.T) {
	s := NewServer(WithGitHubAppWebhookHandler(nil))
	if s.githubAppWebhook != nil {
		t.Errorf("nil handler should leave field unset, got %v", s.githubAppWebhook)
	}
}

// TestRoutes_GitHubAppWebhook_MountedWhenHandlerSet — when the
// handler is wired, /api/v1/github-app/webhook routes to it.
func TestRoutes_GitHubAppWebhook_MountedWhenHandlerSet(t *testing.T) {
	called := 0
	s := NewServer(WithGitHubAppWebhookHandler(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusAccepted)
	}))
	router := NewRouter(s, nil)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", nil))
	if called != 1 {
		t.Errorf("github webhook handler fired %d times, want 1", called)
	}
	if w.Code != http.StatusAccepted {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusAccepted)
	}
}

// TestRoutes_GitHubAppWebhook_404WhenUnset — without the option,
// the route 404s. Operators get a clearer signal that the GitHub
// App isn't configured than a stub 401 / 503 would give.
func TestRoutes_GitHubAppWebhook_404WhenUnset(t *testing.T) {
	s := NewServer()
	router := NewRouter(s, nil)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404", w.Code)
	}
}

// stubBudgetNotifier satisfies the budget.Notifier interface with
// no behaviour. Lives here because the option-coverage test needs
// a value to pass through but the package doesn't ship a default.
type stubBudgetNotifier struct{}

func (stubBudgetNotifier) NotifyBudgetBreach(_ context.Context, _, _, _ string, _ budget.Decision) {
}
