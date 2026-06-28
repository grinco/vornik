package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/telegram"
	"vornik.io/vornik/internal/ui"
)

func TestAgentCallbackURL(t *testing.T) {
	assert.Equal(t, "http://127.0.0.1:8080", agentCallbackURL(""))
	assert.Equal(t, "http://host.containers.internal:8080", agentCallbackURL("0.0.0.0:8080"))
	assert.Equal(t, "https://vornik.internal", agentCallbackURL("https://vornik.internal"))
}

// TestHealthChecker_* removed alongside the HealthChecker type — health
// + readiness probes now route through api.Server.{Healthz,Readyz},
// which already covered the DB-ping behaviour these tests pinned. See
// internal/api/handlers_test.go TestServer_Readyz for the live tests.

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		def      time.Duration
	}{
		{"10s", 10 * time.Second, time.Minute},
		{"5m", 5 * time.Minute, time.Minute},
		{"1h", time.Hour, time.Minute},
		{"", time.Minute, time.Minute},        // uses default
		{"invalid", time.Minute, time.Minute}, // uses default
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseDuration(tt.input, tt.def)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestEffectiveServerWriteTimeout(t *testing.T) {
	tests := []struct {
		name     string
		writeRaw string
		chatRaw  string
		want     time.Duration
	}{
		{
			name:     "empty write timeout follows default chat timeout plus headroom",
			writeRaw: "",
			chatRaw:  "",
			want:     chat.DefaultTimeout + time.Minute,
		},
		{
			name:     "raises explicit write timeout below chat budget",
			writeRaw: "30s",
			chatRaw:  "300s",
			want:     6 * time.Minute,
		},
		{
			name:     "keeps explicit write timeout above chat budget",
			writeRaw: "20m",
			chatRaw:  "300s",
			want:     20 * time.Minute,
		},
		{
			name:     "uses configured long chat timeout",
			writeRaw: "360s",
			chatRaw:  "1800s",
			want:     31 * time.Minute,
		},
		{
			name:     "invalid write timeout is ignored",
			writeRaw: "bad",
			chatRaw:  "90s",
			want:     150 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, effectiveServerWriteTimeout(tt.writeRaw, tt.chatRaw))
		})
	}
}

func TestResolveDispatchTimeout(t *testing.T) {
	assert.Equal(t, 20*time.Minute, resolveDispatchTimeout("20m", "5m"))
	assert.Equal(t, 15*time.Minute, resolveDispatchTimeout("", "2m"))
	assert.Equal(t, 30*time.Minute, resolveDispatchTimeout("", "10m"))
	assert.Equal(t, time.Duration(0), resolveDispatchTimeout("bad", "also-bad"))
}

func TestFallbackNonEmpty(t *testing.T) {
	assert.Equal(t, "fallback", fallbackNonEmpty("", "fallback"))
	assert.Equal(t, "configured", fallbackNonEmpty("configured", "fallback"))
}

func TestBrokerHeadersFor(t *testing.T) {
	assert.Nil(t, brokerHeadersFor(nil, "broker"))
	assert.Nil(t, brokerHeadersFor(&registry.Project{ID: "proj-a"}, "gmail"))

	project := &registry.Project{
		ID: "proj-a",
		Trading: registry.ProjectTrading{
			KillSwitch: true,
			Caps: registry.TradingCaps{
				MaxPositionUSD:            1000,
				MaxDailyTurnoverUSD:       5000,
				MaxOrdersPerHour:          12,
				MaxOrdersPerMinute:        3,
				DrawdownCircuitBreakerPct: 8.5,
			},
		},
	}
	headers := brokerHeadersFor(project, "broker")
	require.Equal(t, "proj-a", headers["X-Project-ID"])

	var caps map[string]any
	require.NoError(t, json.Unmarshal([]byte(headers["X-Project-Caps"]), &caps))
	assert.Equal(t, float64(1000), caps["max_position_usd"])
	assert.Equal(t, float64(5000), caps["max_daily_turnover_usd"])
	assert.Equal(t, float64(12), caps["max_orders_per_hour"])
	assert.Equal(t, float64(3), caps["max_orders_per_minute"])
	assert.Equal(t, 8.5, caps["drawdown_circuit_breaker_pct"])
	assert.Equal(t, true, caps["kill_switch"])
}

// Audit T4 + T1 + T2: the per-project daily-loss breaker pct and the
// project's declared mode must flow through X-Project-Caps, and the
// shared secret must ride as a Bearer Authorization header when
// VORNIK_BROKER_INTERNAL_KEY is set.
func TestBrokerHeadersFor_DailyLossModeAndAuth(t *testing.T) {
	project := &registry.Project{
		ID: "proj-a",
		Trading: registry.ProjectTrading{
			Mode: "paper",
			Caps: registry.TradingCaps{
				MaxPositionUSD:             2500,
				DailyLossCircuitBreakerPct: 3.0,
			},
		},
	}

	t.Setenv("VORNIK_BROKER_INTERNAL_KEY", "")
	headers := brokerHeadersFor(project, "broker")
	var caps map[string]any
	require.NoError(t, json.Unmarshal([]byte(headers["X-Project-Caps"]), &caps))
	assert.Equal(t, 3.0, caps["daily_loss_circuit_breaker_pct"], "audit T4: per-project daily-loss must be plumbed")
	assert.Equal(t, "paper", caps["mode"], "audit T1: project mode must be plumbed")
	assert.Empty(t, headers["Authorization"], "no auth header without the shared secret")

	t.Setenv("VORNIK_BROKER_INTERNAL_KEY", "s3cr3t")
	headers = brokerHeadersFor(project, "broker")
	assert.Equal(t, "Bearer s3cr3t", headers["Authorization"], "audit T2: shared secret rides as Bearer")
}

func TestResolvePricingPath(t *testing.T) {
	t.Setenv("VORNIK_PRICING_PATH", "")
	t.Setenv("VORNIK_CONFIGS_DIR", "")

	base := t.TempDir()
	configPath := filepath.Join(base, "vornik.yaml")
	configPricing := filepath.Join(base, "configs", "pricing.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPricing), 0o755))
	require.NoError(t, os.WriteFile(configPricing, []byte("models: []\n"), 0o644))

	assert.Equal(t, configPricing, resolvePricingPath(configPath))

	envPricing := filepath.Join(base, "env-pricing.yaml")
	require.NoError(t, os.WriteFile(envPricing, []byte("models: []\n"), 0o644))
	t.Setenv("VORNIK_PRICING_PATH", envPricing)
	assert.Equal(t, envPricing, resolvePricingPath(configPath))
}

func TestDefaultRouterRoutesUseSubscriptionProvidersWhenEnabled(t *testing.T) {
	// All subscription-kind providers enabled → prefer them.
	subs := map[string]chat.Provider{
		"claude-subscription": nil,
		"claude-cli":          nil,
		"codex-subscription":  nil,
		"codex-cli":           nil,
	}
	routes := defaultRouterRoutesForSubs(subs)

	want := map[string]string{
		"claude-": "claude-subscription",
		"gpt-":    "codex-subscription",
		"o3-":     "codex-subscription",
		"o4-":     "codex-subscription",
		"codex":   "codex-subscription",
	}
	got := make(map[string]string, len(routes))
	for _, route := range routes {
		got[route.Prefix] = route.Kind
	}
	assert.Equal(t, want, got)
}

func TestDefaultRouterRoutesFallBackToCLIWhenSubscriptionDisabled(t *testing.T) {
	// Legacy CLI-only deployment: subscription providers disabled.
	// claude-cli and codex-cli still serve their prefixes.
	subs := map[string]chat.Provider{
		"claude-cli": nil,
		"codex-cli":  nil,
	}
	routes := defaultRouterRoutesForSubs(subs)

	want := map[string]string{
		"claude-": "claude-cli",
		"gpt-":    "codex-cli",
		"o3-":     "codex-cli",
		"o4-":     "codex-cli",
		"codex":   "codex-cli",
	}
	got := make(map[string]string, len(routes))
	for _, route := range routes {
		got[route.Prefix] = route.Kind
	}
	assert.Equal(t, want, got)
}

// TestDefaultRouterRoutesIncludeVertexWhenEnabled verifies that enabling
// the Vertex sub-provider extends the default route table with the
// `gemini-` and `google/` prefixes. Without this the router silently
// drops Vertex traffic to the fallback, which would route to whatever
// unrelated provider (Claude / OpenAI) the operator picked as default.
func TestDefaultRouterRoutesIncludeVertexWhenEnabled(t *testing.T) {
	subs := map[string]chat.Provider{
		"claude-subscription": nil,
		"codex-subscription":  nil,
		"vertex":              nil,
	}
	routes := defaultRouterRoutesForSubs(subs)

	got := make(map[string]string, len(routes))
	for _, route := range routes {
		got[route.Prefix] = route.Kind
	}
	assert.Equal(t, "vertex", got["gemini-"])
	assert.Equal(t, "vertex", got["google/"])
}

// TestDefaultRouterRoutesIncludeBedrockPrefixes — B-11 expansion.
// Bedrock supports many publisher namespaces (openai.*, amazon.*,
// anthropic.*, minimax.*, meta.*, …); each gets a default route so
// fresh installs don't trip on the "Malformed publisher model" 400
// that Vertex returns when un-routed Bedrock IDs fall through to it.
// Reproduced 2026-05-28 — sparse operator routes (just google/+gemini-)
// sent openai.gpt-oss-120b-1:0 to Vertex and broke companion-rag-ingest.
func TestDefaultRouterRoutesIncludeBedrockPrefixes(t *testing.T) {
	subs := map[string]chat.Provider{"bedrock": nil}
	routes := defaultRouterRoutesForSubs(subs)
	got := make(map[string]string, len(routes))
	for _, route := range routes {
		got[route.Prefix] = route.Kind
	}
	// Every Bedrock publisher prefix in the registry should land on bedrock.
	for _, prefix := range []string{
		"openai.", "amazon.", "anthropic.", "cohere.", "deepseek.",
		"meta.", "minimax.", "mistral.", "moonshot.", "moonshotai.",
		"nvidia.", "global.", "qwen.", "stability.", "writer.", "zai.",
	} {
		assert.Equal(t, "bedrock", got[prefix],
			"%s should route to bedrock by default when bedrock is enabled", prefix)
	}
}

// TestMergeWithDefaultRoutes_OperatorWins — when an operator
// configures a prefix explicitly (e.g. routing openai. to vertex
// intentionally), the merge must preserve that route. Defaults only
// fill gaps the operator left open.
func TestMergeWithDefaultRoutes_OperatorWins(t *testing.T) {
	user := []config.ChatRouteConfig{
		{Prefix: "openai.", Kind: "vertex"}, // operator's deliberate choice
		{Prefix: "google/", Kind: "vertex"},
	}
	defaults := []config.ChatRouteConfig{
		{Prefix: "openai.", Kind: "bedrock"}, // would conflict
		{Prefix: "amazon.", Kind: "bedrock"}, // gap-fill
		{Prefix: "gemini-", Kind: "vertex"},  // gap-fill
	}
	merged := mergeWithDefaultRoutes(user, defaults)

	require.Len(t, merged, 4, "user routes + non-overlapping defaults")
	// User's openai.→vertex must survive
	assert.Equal(t, "openai.", merged[0].Prefix)
	assert.Equal(t, "vertex", merged[0].Kind,
		"operator's explicit openai.→vertex must NOT be overridden by the default openai.→bedrock")
	// Gap-fills get appended in order
	prefixes := make([]string, len(merged))
	for i, r := range merged {
		prefixes[i] = r.Prefix
	}
	assert.Contains(t, prefixes, "amazon.")
	assert.Contains(t, prefixes, "gemini-")
}

// TestMergeWithDefaultRoutes_EmptyUser — empty operator list should
// surface all defaults verbatim (this is the existing
// "no routes configured at all" path; the merge function should
// pass it through unchanged).
func TestMergeWithDefaultRoutes_EmptyUser(t *testing.T) {
	defaults := []config.ChatRouteConfig{
		{Prefix: "openai.", Kind: "bedrock"},
		{Prefix: "gemini-", Kind: "vertex"},
	}
	merged := mergeWithDefaultRoutes(nil, defaults)
	assert.Equal(t, defaults, merged)
}

// TestRouterFallbackHonorsChatModel verifies the chat.model promotion:
// under provider=router, a top-level chat.model must be honored as the
// fallback sub-provider's effective default. Pre-fix, chat.model was
// silently ignored under router (see commit "chat: honor chat.model
// under provider=router") and the fallback ran whatever
// router.<sub>.model happened to be, surprising operators.
func TestRouterFallbackHonorsChatModel(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Model:    "promoted-model", // top-level — should reach the fallback
		Router: config.ChatRouterConfig{
			Default: "http",
			HTTP: config.ChatHTTPSubConfig{
				Enabled:  true,
				Endpoint: "http://example.invalid",
				APIKey:   "ignored-in-test",
				Model:    "sub-default-model", // would have won pre-fix
			},
		},
	}

	require.NoError(t, c.initChatRouter(c.Config.Chat))
	require.NotNil(t, c.ChatClient)
	assert.Equal(t, "promoted-model", c.ChatClient.Model(),
		"router.fallback.Model() must reflect chat.model when set")
}

// TestRouterFallbackUsesSubModelWhenChatModelEmpty verifies the inverse:
// when chat.model is empty, the fallback's build-time default
// (router.<sub>.model) wins. This keeps router.<sub>.model meaningful as
// a per-sub-provider baseline for deployments that prefer setting models
// per backend instead of one global chat.model.
func TestRouterFallbackUsesSubModelWhenChatModelEmpty(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Model:    "", // unset on purpose
		Router: config.ChatRouterConfig{
			Default: "http",
			HTTP: config.ChatHTTPSubConfig{
				Enabled:  true,
				Endpoint: "http://example.invalid",
				APIKey:   "ignored-in-test",
				Model:    "sub-default-model",
			},
		},
	}

	require.NoError(t, c.initChatRouter(c.Config.Chat))
	require.NotNil(t, c.ChatClient)
	assert.Equal(t, "sub-default-model", c.ChatClient.Model(),
		"fallback should use router.<sub>.model when chat.model is empty")
}

func TestBuildVertexEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		location  string
		want      string
	}{
		{
			name:      "global default",
			projectID: "my-proj",
			location:  "",
			want:      "https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/endpoints/openapi",
		},
		{
			name:      "explicit global",
			projectID: "my-proj",
			location:  "global",
			want:      "https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/endpoints/openapi",
		},
		{
			name:      "us-central1 regional",
			projectID: "my-proj",
			location:  "us-central1",
			want:      "https://us-central1-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-central1/endpoints/openapi",
		},
		{
			name:      "europe-west4 regional",
			projectID: "my-proj",
			location:  "europe-west4",
			want:      "https://europe-west4-aiplatform.googleapis.com/v1/projects/my-proj/locations/europe-west4/endpoints/openapi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildVertexEndpoint(tt.projectID, tt.location)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUISubtreeHandler_ServesDashboardPaths(t *testing.T) {
	uiHandler := uiSubtreeHandler(zerolog.Nop(), ui.NewServer().Handler())

	mux := http.NewServeMux()
	mux.Handle("/ui", uiHandler)
	mux.Handle("/ui/", uiHandler)
	mux.Handle("/", http.NotFoundHandler())

	for _, path := range []string{"/ui", "/ui/"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), "Dashboard")
		})
	}
}

func TestOnboardingSecretsDir_DerivesSecretsSubdir(t *testing.T) {
	got := onboardingSecretsDir("/etc/vornik/config.yaml")
	assert.Equal(t, "/etc/vornik/secrets", got)
}

func TestUIAuthChain_EnforcesProjectScope(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := api.ProjectAuthMiddleware()(next)
	handler = api.AuthMiddleware(api.AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"scoped-key": {"proj-a"}},
	})(handler)

	req := httptest.NewRequest(http.MethodGet, "/ui/projects/proj-b/artifacts", nil)
	req.Header.Set("X-API-Key", "scoped-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestResolveRegistryConfigDir(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "vornik.yaml")
	configsDir := filepath.Join(tmpDir, "configs")

	for _, name := range []string{"projects", "swarms", "workflows"} {
		err := os.MkdirAll(filepath.Join(configsDir, name), 0o755)
		assert.NoError(t, err)
	}

	assert.Equal(t, configsDir, resolveRegistryConfigDir(configPath))
}

func TestBuildObservabilityConfig(t *testing.T) {
	cfg := &config.Config{
		Metrics: config.MetricsConfig{
			Enabled: true,
			Addr:    ":9090",
		},
		Tracing: config.TracingConfig{
			Enabled:  true,
			Endpoint: "otel:4317",
		},
	}

	obsCfg := buildObservabilityConfig(cfg)
	assert.Equal(t, ":9090", obsCfg.MetricsAddr)
	assert.True(t, obsCfg.TracingEnabled)
	assert.Equal(t, "otel:4317", obsCfg.TracingEndpoint)
}

func TestObservabilityRegistry(t *testing.T) {
	t.Run("returns nil without observability", func(t *testing.T) {
		c := &Container{}
		assert.Nil(t, c.observabilityRegistry())
	})

	t.Run("returns nil when tracing only", func(t *testing.T) {
		c := &Container{
			Observability: &observability.Observability{},
		}
		assert.Nil(t, c.observabilityRegistry())
	})

	t.Run("returns registry when metrics enabled", func(t *testing.T) {
		obs, err := observability.New(observability.Config{MetricsAddr: ":9090"}, zerolog.Nop())
		require.NoError(t, err)

		c := &Container{Observability: obs}
		assert.NotNil(t, c.observabilityRegistry())
	})
}

func TestValidateRegistryActivationBlocksChangedProjectInFlightTask(t *testing.T) {
	tmpDir := t.TempDir()
	writeServiceRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"projects/test.yaml": `projectId: "test-project"
displayName: "Original Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
	})

	reg := registry.New()
	require.NoError(t, reg.Load(tmpDir))

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(`projectId: "test-project"
displayName: "Updated Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`), 0o644))
	require.NoError(t, reg.Stage(tmpDir))
	require.NoError(t, reg.ValidateStaged())

	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			if filter.Status != nil && *filter.Status == persistence.TaskStatusRunning {
				return []*persistence.Task{{
					ID:        "task-1",
					ProjectID: "test-project",
					Status:    persistence.TaskStatusRunning,
				}}, nil
			}
			return nil, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{}

	c := &Container{Registry: reg}
	err := c.validateRegistryActivation(taskRepo, execRepo)
	require.Error(t, err)

	var blockedErr *config.ActivationBlockedError
	require.True(t, errors.As(err, &blockedErr))
	assert.Contains(t, blockedErr.Error(), "task task-1 references changed project test-project")
}

func TestValidateRegistryActivationAllowsUnreferencedChanges(t *testing.T) {
	tmpDir := t.TempDir()
	writeServiceRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"projects/test.yaml": `projectId: "test-project"
displayName: "Original Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
	})

	reg := registry.New()
	require.NoError(t, reg.Load(tmpDir))

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(`projectId: "test-project"
displayName: "Updated Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`), 0o644))
	require.NoError(t, reg.Stage(tmpDir))
	require.NoError(t, reg.ValidateStaged())

	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, nil
		},
	}

	c := &Container{Registry: reg}
	require.NoError(t, c.validateRegistryActivation(taskRepo, execRepo))
}

// TestValidateRegistryActivation_SkipsOrphanPausedExec covers the
// 2026-05-22 fix: PAUSED executions whose parent task has reached a
// terminal status are orphans (the adaptive-route flow creates a
// continuation execution rather than resuming, leaving the original
// PAUSED row dangling). They must NOT block a config reload — the
// reload safety check filters them out.
func TestValidateRegistryActivation_SkipsOrphanPausedExec(t *testing.T) {
	tmpDir := t.TempDir()
	writeServiceRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"projects/test.yaml": `projectId: "test-project"
displayName: "Original"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
	})

	reg := registry.New()
	require.NoError(t, reg.Load(tmpDir))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(`projectId: "test-project"
displayName: "Updated"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`), 0o644))
	require.NoError(t, reg.Stage(tmpDir))
	require.NoError(t, reg.ValidateStaged())

	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			// Orphan parent task — already COMPLETED, so the
			// PAUSED execution that references it is dead.
			return &persistence.Task{
				ID:        "task-orphan",
				ProjectID: "test-project",
				Status:    persistence.TaskStatusCompleted,
			}, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			if filter.Status != nil && *filter.Status == persistence.ExecutionStatusPaused {
				return []*persistence.Execution{{
					ID:        "exec-orphan",
					TaskID:    "task-orphan",
					ProjectID: "test-project",
					Status:    persistence.ExecutionStatusPaused,
				}}, nil
			}
			return nil, nil
		},
	}

	c := &Container{Registry: reg}
	if err := c.validateRegistryActivation(taskRepo, execRepo); err != nil {
		t.Fatalf("expected orphan-paused exec to be skipped, got: %v", err)
	}
}

// TestValidateRegistryActivation_BlocksLivePausedExec is the
// counterpart: a PAUSED execution whose task is still RUNNING (e.g.
// genuinely awaiting children, not an orphan) MUST still block the
// reload so we don't yank config out from under live work.
func TestValidateRegistryActivation_BlocksLivePausedExec(t *testing.T) {
	tmpDir := t.TempDir()
	writeServiceRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"projects/test.yaml": `projectId: "test-project"
displayName: "Original"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
	})
	reg := registry.New()
	require.NoError(t, reg.Load(tmpDir))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(`projectId: "test-project"
displayName: "Updated"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`), 0o644))
	require.NoError(t, reg.Stage(tmpDir))
	require.NoError(t, reg.ValidateStaged())

	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-live",
				ProjectID: "test-project",
				Status:    persistence.TaskStatusWaitingForChildren,
			}, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			if filter.Status != nil && *filter.Status == persistence.ExecutionStatusPaused {
				return []*persistence.Execution{{
					ID:        "exec-live",
					TaskID:    "task-live",
					ProjectID: "test-project",
					Status:    persistence.ExecutionStatusPaused,
				}}, nil
			}
			return nil, nil
		},
	}

	c := &Container{Registry: reg}
	err := c.validateRegistryActivation(taskRepo, execRepo)
	if err == nil {
		t.Fatal("expected live PAUSED exec to block reload")
	}
	if !strings.Contains(err.Error(), "exec-live") {
		t.Errorf("expected error to mention exec-live: %v", err)
	}
}

func writeServiceRegistryFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, subdir), 0o755))
	}

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
	}
}

// TestWireComponentMetrics verifies that wireComponentMetrics registers the
// expected Prometheus metric families into the observability registry.
func TestWireComponentMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()

	// Build a minimal container with a custom observability registry.
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	require.NoError(t, err)

	c := &Container{
		Logger:        zerolog.Nop(),
		Observability: obs,
		ChatClient:    chat.NewClient("http://test", "key", "test-model"),
	}

	// Override the internal registry with our test one.
	// observabilityRegistry() delegates to Observability.Metrics.Registry().
	// Since we can't inject a custom registry into Metrics directly,
	// we test the public API: SetMetrics on the chat client.
	chatMetrics := chat.NewMetrics(reg)
	c.ChatClient.SetMetrics(chatMetrics)

	// Gather all registered metric families.
	families, err := reg.Gather()
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}

	// Chat metrics should be registered. Counters are initialized at zero
	// by SetMetrics; histograms only appear after the first observation.
	assert.True(t, names["vornik_chat_requests_total"], "chat requests_total missing")
	assert.True(t, names["vornik_chat_errors_total"], "chat errors_total missing — should be initialized at zero")

	// Dispatcher metrics
	dispatcherMetrics := dispatcher.NewMetrics(reg)
	assert.NotNil(t, dispatcherMetrics.ToolCallsTotal)
	dispatcherMetrics.ToolCallsTotal.WithLabelValues("test_tool").Inc()
	families, err = reg.Gather()
	require.NoError(t, err)
	names = make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}
	assert.True(t, names["vornik_dispatcher_tool_calls_total"], "dispatcher tool_calls_total missing")

	// Telegram metrics (can create without a bot instance)
	telegramMetrics := telegram.NewMetrics(reg)
	assert.NotNil(t, telegramMetrics)
}
