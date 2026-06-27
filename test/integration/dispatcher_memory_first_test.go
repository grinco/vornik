//go:build integration
// +build integration

package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/registry"
)

// TestDispatcherAnswersFromMemoryWithoutSchedulingTask is the
// narrative-level test for the "Telegram should behave like an
// assistant" change. It runs a real dispatcher against:
//
//   - the live daemon's postgres (task + artifact + memory repos)
//   - the configured embeddings endpoint
//   - the configured chat endpoint
//
// and sends a question whose answer is already in project memory.
// The assertion is the behavior the user asked for: memory_search
// gets called, create_task does not.
//
// Required env:
//
//	VORNIK_INTEGRATION=1                        opt-in, skipped otherwise
//	CHAT_ENDPOINT / CHAT_API_KEY / CHAT_MODEL   same values the daemon uses
//	POSTGRES_HOST / POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB
//	                                            same DB the daemon writes to
//	MEM_PROJECT                                 project id that has memory
//	                                            populated (default "assistant")
//	MEM_QUERY                                   user question (default
//	                                            "who is Vadim Grinco?")
func TestDispatcherAnswersFromMemoryWithoutSchedulingTask(t *testing.T) {
	if os.Getenv("VORNIK_INTEGRATION") == "" {
		t.Skip("VORNIK_INTEGRATION not set — skipping dispatcher live integration")
	}
	// This test drives a real LLM endpoint; skip cleanly when the
	// chat env isn't configured so a default `make test-integration`
	// run doesn't have to choose between failing-on-unconfigured-LLM
	// and skipping the whole suite. VORNIK_INTEGRATION is the on-ramp;
	// CHAT_* are the live-LLM requirements layered on top of it.
	for _, key := range []string{"CHAT_ENDPOINT", "CHAT_API_KEY", "CHAT_MODEL"} {
		if os.Getenv(key) == "" {
			t.Skipf("env %s not set — skipping live-LLM dispatcher integration", key)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Memory manager talking to the same schema + endpoint the daemon uses.
	chatEndpoint := mustGetenv(t, "CHAT_ENDPOINT")
	chatAPIKey := mustGetenv(t, "CHAT_API_KEY")
	chatModel := mustGetenv(t, "CHAT_MODEL")

	memCfg := memory.Config{
		Enabled:            true,
		EmbeddingModel:     "text-embedding-3-small",
		EmbeddingDimension: 1536,
		EmbeddingEndpoint:  chatEndpoint,
		EmbeddingAPIKey:    chatAPIKey,
		ChunkTokens:        512,
		ChunkOverlap:       64,
		WorkerConcurrency:  1,
	}
	memMgr, err := memory.New(memCfg, db, zerolog.Nop())
	require.NoError(t, err, "construct memory manager")

	// Load the real registry so list_projects / switch_project behave
	// sensibly. Path mirrors the daemon's VORNIK_CONFIGS_DIR / default.
	configDir := mustEnvOr("VORNIK_CONFIGS_DIR",
		"/opt/vornik/.config/vornik/configs")
	reg := registry.New()
	require.NoError(t, reg.Load(configDir), "load registry from %s", configDir)
	project := mustEnvOr("MEM_PROJECT", "assistant")

	chatClient := chat.NewClient(chatEndpoint, chatAPIKey, chatModel)

	// Capture tool calls via an audit writer that shoves every invocation
	// into a slice — no DB dependency.
	audit := &capturingAudit{}

	agent := dispatcher.NewAgent(
		chatClient,
		postgres.NewTaskRepository(db),
		postgres.NewExecutionRepository(db),
		postgres.NewArtifactRepository(db),
		reg,
		dispatcher.WithMaxIterations(15),
		dispatcher.WithAuditRepository(audit),
		dispatcher.WithMemorySearcher(memMgr.Searcher),
	)

	question := mustEnvOr("MEM_QUERY", "Who is Vadim Grinco? Give me a brief answer based on project memory.")

	req := dispatcher.Request{
		Messages: []chat.Message{{Role: "user", Content: question}},
		Project:  project,
	}

	result := agent.Process(ctx, req)
	require.NoError(t, result.Err, "dispatcher.Process")
	require.NotEmpty(t, result.Text, "dispatcher produced an empty reply")

	t.Logf("assistant reply:\n%s", result.Text)
	t.Logf("tool calls (in order):")
	for _, c := range audit.calls {
		t.Logf("  - %s", c.tool)
	}

	// The core assertion: memory_search fires and create_task doesn't.
	var sawMemorySearch, sawCreateTask bool
	for _, c := range audit.calls {
		switch c.tool {
		case "memory_search":
			sawMemorySearch = true
		case "create_task":
			sawCreateTask = true
		}
	}
	require.True(t, sawMemorySearch,
		"expected memory_search to be called; the dispatcher should search memory first, not reach for create_task")
	require.False(t, sawCreateTask,
		"create_task was called for an information request that memory could satisfy")

	// The reply should mention something that only comes from the stored
	// CV / research about Vadim, not a generic Vadim. A weak grounding
	// check — any of a handful of specific markers is enough.
	specifics := []string{"HeadMatch", "Prague", "ote_rate", "HASS-coral", "binaural", "PipeWire"}
	matched := ""
	for _, s := range specifics {
		if strings.Contains(result.Text, s) {
			matched = s
			break
		}
	}
	require.NotEmptyf(t, matched,
		"reply does not appear to be grounded in project memory (none of %v found). Reply was: %s",
		specifics, result.Text)
	t.Logf("grounded marker found: %q", matched)
}

type captured struct {
	tool    string
	input   string
	project string
}

// capturingAudit implements dispatcher.AuditRepository and records every
// tool call the dispatcher makes so the test can assert on the shape of
// the tool call sequence.
type capturingAudit struct {
	calls []captured
}

func (c *capturingAudit) Log(_ context.Context, entry *persistence.ToolAuditEntry) error {
	if entry == nil {
		return nil
	}
	c.calls = append(c.calls, captured{
		tool:    entry.ToolName,
		input:   entry.ToolInput,
		project: entry.ProjectID,
	})
	return nil
}

func mustGetenv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("env %s is required for this integration test", key)
	}
	return v
}

func mustEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
