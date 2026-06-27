package dispatcher

// Coverage tests for the WithXxx options surface. Each option is a
// trivial setter, so the goal is just to pin "the option assigns the
// expected field" — that way a future rename / typo on the option
// side trips a test rather than silently dropping a wired
// dependency.

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
)

// stubInputArtifactStore is a minimal InputArtifactStore that lets us
// pin WithInputArtifactStore without dragging the real artifacts
// package into the test binary.
type stubInputArtifactStore struct{}

func (stubInputArtifactStore) StoreInput(context.Context, string, string, string) (*persistence.Artifact, error) {
	return nil, nil
}

func (stubInputArtifactStore) Retrieve(context.Context, string) ([]byte, error) {
	return nil, nil
}

// stubMCPExecutor — minimal MCPExecutor for WithMCPManager.
type stubMCPExecutor struct{}

func (stubMCPExecutor) Tools(string) []chat.Tool { return nil }
func (stubMCPExecutor) Execute(context.Context, string, string, string) (string, error) {
	return "", nil
}

func newOptionsAgent(opts ...AgentOption) *Agent {
	return NewAgent(nil, nil, nil, nil, nil, opts...)
}

func TestWithLogger_SetsLogger(t *testing.T) {
	want := zerolog.Nop().With().Str("test", "x").Logger()
	a := newOptionsAgent(WithLogger(want))
	if a.logger.GetLevel() != want.GetLevel() {
		t.Fatalf("logger level not propagated: got %v", a.logger.GetLevel())
	}
}

func TestWithMaxIterations_PositiveSets(t *testing.T) {
	a := newOptionsAgent(WithMaxIterations(42))
	if a.maxIterations != 42 {
		t.Fatalf("maxIterations = %d, want 42", a.maxIterations)
	}
}

func TestWithMaxIterations_NonPositiveIgnored(t *testing.T) {
	a := newOptionsAgent(WithMaxIterations(0))
	if a.maxIterations != 10 {
		t.Fatalf("maxIterations changed from default on 0: got %d, want 10", a.maxIterations)
	}
	a = newOptionsAgent(WithMaxIterations(-5))
	if a.maxIterations != 10 {
		t.Fatalf("maxIterations changed from default on -5: got %d, want 10", a.maxIterations)
	}
}

func TestWithTaskWatchFunc(t *testing.T) {
	fn := func(string, int64) {}
	a := newOptionsAgent(WithTaskWatchFunc(fn))
	if a.watchFunc == nil {
		t.Fatal("watchFunc not set")
	}
}

func TestWithOutputGuard(t *testing.T) {
	a := newOptionsAgent(WithOutputGuard(true))
	if a.outputGuard == nil || !a.outputGuard.RedactHigh {
		t.Fatalf("outputGuard = %+v, want redact=true", a.outputGuard)
	}
	a = newOptionsAgent(WithOutputGuard(false))
	if a.outputGuard == nil || a.outputGuard.RedactHigh {
		t.Fatalf("outputGuard = %+v, want redact=false", a.outputGuard)
	}
}

func TestWithHallucinationDetector(t *testing.T) {
	d := &hallucination.Detector{}
	a := newOptionsAgent(WithHallucinationDetector(d))
	if a.hallucinationDetector != d {
		t.Fatal("hallucinationDetector not set")
	}
}

func TestWithHallucinationMetrics(t *testing.T) {
	m := &hallucination.Metrics{}
	a := newOptionsAgent(WithHallucinationMetrics(m))
	if a.hallucinationMetrics != m {
		t.Fatal("hallucinationMetrics not set")
	}
}

func TestWithLLMUsageRepository(t *testing.T) {
	var repo persistence.TaskLLMUsageRepository
	a := newOptionsAgent(WithLLMUsageRepository(repo))
	// Nil-passthrough is the documented behaviour; just verify the
	// option doesn't panic and the field stays at its zero value.
	if a.llmUsageRepo != nil {
		t.Fatalf("llmUsageRepo = %v, want nil", a.llmUsageRepo)
	}
}

func TestWithRateLimiter(t *testing.T) {
	l := &ratelimit.Limiter{}
	a := newOptionsAgent(WithRateLimiter(l))
	if a.rateLimiter != l {
		t.Fatal("rateLimiter not set")
	}
}

func TestWithBudgetNotifier(t *testing.T) {
	// Reuse the package-shared stubBudgetNotifier from
	// tools_handlers_test.go.
	n := &stubBudgetNotifier{}
	a := newOptionsAgent(WithBudgetNotifier(n))
	if a.budgetNotifier == nil {
		t.Fatal("budgetNotifier not set")
	}
}

func TestWithDefaultModel(t *testing.T) {
	a := newOptionsAgent(WithDefaultModel("anthropic/claude-x"))
	if a.defaultModel != "anthropic/claude-x" {
		t.Fatalf("defaultModel = %q", a.defaultModel)
	}
}

func TestWithBillingProjectID(t *testing.T) {
	a := newOptionsAgent(WithBillingProjectID("assistant"))
	if a.billingProjectID != "assistant" {
		t.Fatalf("billingProjectID = %q", a.billingProjectID)
	}
}

func TestWithPricing(t *testing.T) {
	tbl := &pricing.Table{}
	a := newOptionsAgent(WithPricing(tbl))
	if a.pricing != tbl {
		t.Fatal("pricing not set")
	}
}

func TestWithAuditRepository(t *testing.T) {
	// Reuse the package-shared stubAuditRepo from
	// tools_handlers_test.go.
	r := &stubAuditRepo{}
	a := newOptionsAgent(WithAuditRepository(r))
	if a.auditRepo == nil {
		t.Fatal("auditRepo not set")
	}
}

func TestWithInputArtifactStore(t *testing.T) {
	s := stubInputArtifactStore{}
	a := newOptionsAgent(WithInputArtifactStore(s))
	if a.artifactStore == nil {
		t.Fatal("artifactStore not set")
	}
}

// TestNewAgent_DefaultMaxIterations pins the constant the WithMaxIterations
// guards land on — flipping the default elsewhere should require a
// deliberate test change rather than a silent shift.
func TestNewAgent_DefaultMaxIterations(t *testing.T) {
	a := newOptionsAgent()
	if a.maxIterations != 10 {
		t.Fatalf("default maxIterations = %d, want 10", a.maxIterations)
	}
}

func TestWithMCPManager(t *testing.T) {
	m := stubMCPExecutor{}
	a := newOptionsAgent(WithMCPManager(m))
	if a.mcpManager == nil {
		t.Fatal("mcpManager not set")
	}
}

// stubFollowupRegistrar — minimal FollowupRegistrar.
type stubFollowupRegistrar struct{}

func (stubFollowupRegistrar) RegisterFollowup(int64, string, string) {}

func TestWithFollowupRegistrar(t *testing.T) {
	r := stubFollowupRegistrar{}
	a := newOptionsAgent(WithFollowupRegistrar(r))
	if a.followupRegistrar == nil {
		t.Fatal("followupRegistrar not set")
	}
}

// stubMemorySearcher — minimal MemorySearcher.
type stubMemorySearcher struct{}

func (stubMemorySearcher) Search(context.Context, string, string, int) ([]memory.SearchResult, error) {
	return nil, nil
}

func TestWithMemorySearcher(t *testing.T) {
	m := stubMemorySearcher{}
	a := newOptionsAgent(WithMemorySearcher(m))
	if a.memory == nil {
		t.Fatal("memory searcher not set")
	}
}

// stubMemoryCorrector — minimal MemoryCorrector.
type stubMemoryCorrector struct{}

func (stubMemoryCorrector) RefuteByClaim(context.Context, string, string, int) ([]memory.RefutedChunk, error) {
	return nil, nil
}
func (stubMemoryCorrector) InsertCorrection(context.Context, string, string, string) (string, error) {
	return "", nil
}

func TestWithMemoryCorrector(t *testing.T) {
	c := stubMemoryCorrector{}
	a := newOptionsAgent(WithMemoryCorrector(c))
	if a.memoryCorrector == nil {
		t.Fatal("memoryCorrector not set")
	}
}

func TestWithGroundingTaskRepo(t *testing.T) {
	var repo persistence.TaskRepository
	a := newOptionsAgent(WithGroundingTaskRepo(repo))
	// nil-passthrough — just verify the option doesn't panic and
	// the field is reachable.
	if a.taskRepoForGrounding != nil {
		t.Fatalf("taskRepoForGrounding = %v, want nil", a.taskRepoForGrounding)
	}
}

func TestSetMetrics_AssignsField(t *testing.T) {
	a := newOptionsAgent()
	m := &Metrics{}
	a.SetMetrics(m)
	if a.metrics != m {
		t.Fatalf("SetMetrics did not assign the field")
	}
	// nil receiver must not panic.
	var aNil *Agent
	aNil.SetMetrics(m)
}

func TestSetHallucinationMetrics_AssignsField(t *testing.T) {
	a := newOptionsAgent()
	m := &hallucination.Metrics{}
	a.SetHallucinationMetrics(m)
	if a.hallucinationMetrics != m {
		t.Fatalf("SetHallucinationMetrics did not assign the field")
	}
	// nil receiver must not panic.
	var aNil *Agent
	aNil.SetHallucinationMetrics(m)
}
