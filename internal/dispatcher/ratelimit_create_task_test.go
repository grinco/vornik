package dispatcher

// Wired rate-limit test for the dispatcher's create_task tool path
// (Tier 3, https://docs.vornik.io). Companion to
// api.TestRateLimit_SharedLimiter_BurstAndIsolation: that test drives
// the burst->429 + isolation journey through the REST POST /tasks
// handler; this one proves the SAME ratelimit.ProjectLimiter, once
// saturated, also blocks the dispatcher's create_task tool — so the
// chat/dispatcher entry point cannot bypass the shared per-project
// counter. Together the two tests pin the limiter as the single
// chokepoint across ≥2 creation paths.
//
// The dispatcher's createTask reads time.Now() directly (no clock
// seam), but the per-minute window is a full minute wide, so the
// sub-millisecond drift between the Record() instant and the
// create_task Check() instant never crosses the window boundary. No
// sleeps.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// rateLimitDispatcherRegistry builds a single project "burst" with a
// per-minute task cap so the create_task gate has a limit to enforce.
func rateLimitDispatcherRegistry(t *testing.T, perMinute int) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	write("swarms/coder.md", `---
swarmId: "shared-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`)
	write("workflows/build.md", `---
workflowId: "build"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "do it"
terminals:
  done:
    status: "COMPLETED"
---
`)
	write("projects/burst.yaml", fmt.Sprintf(`projectId: "burst"
displayName: "Burst"
swarmId: "shared-swarm"
defaultWorkflowId: "build"
defaultPriority: 50
rate_limit:
  tasks_per_minute: %d
`, perMinute))
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

// TestRateLimit_CreateTask_SharedLimiterBlocks confirms the
// dispatcher's create_task tool consults the shared per-project
// limiter and refuses once the project's window is saturated — the
// no-bypass guarantee for the chat/dispatcher entry point.
//
// We saturate the limiter to the per-minute cap by recording cap
// samples (the same Record() call every accepted creation makes),
// then dispatch create_task for that project and assert the gate
// refuses with the rate-limit message — before any task-type,
// budget, or persistence work (none of which is wired here).
//
// Characterization: expected to PASS. A failure would mean
// create_task does not honour the shared limiter — a real bug.
func TestRateLimit_CreateTask_SharedLimiterBlocks(t *testing.T) {
	const perMinute = 3
	reg := rateLimitDispatcherRegistry(t, perMinute)
	limiter := ratelimit.New()

	// Saturate "burst" to its per-minute cap, exactly as cap accepted
	// creations would have via Creator.Record / createTask's Record.
	now := time.Now()
	for i := 0; i < perMinute; i++ {
		limiter.Record("burst", now)
	}
	// Sanity: the limiter is now blocking for this project.
	require.True(t, limiter.Check(reg.GetProject("burst"), now).Blocked,
		"precondition: limiter must be saturated for burst")

	te := &ToolExecutor{
		rateLimiter: limiter,
		registry:    reg,
		logger:      zerolog.Nop(),
	}

	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name:      "create_task",
		Arguments: `{"project_id":"burst","type":"research","prompt":"y"}`,
	}}
	// allowedProjects=["*"] -> wildcard, so resolveProjectAllowed
	// permits the named project.
	res := te.Execute(context.Background(), tc, "burst", []string{"*"}, 0, nil)

	assert.Contains(t, res.Content, "Cannot create task",
		"create_task must refuse the saturated project via the shared limiter; got %q", res.Content)
	assert.Contains(t, strings.ToLower(res.Content), "rate limit",
		"refusal must cite the rate-limit reason; got %q", res.Content)
}
