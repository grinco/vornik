package api

// Integration test for the per-project task-creation rate limit
// (Tier 3, https://docs.vornik.io). The per-project sliding-window limiter
// in internal/ratelimit is SHARED — by design — between the REST
// POST /tasks handler (via taskcreate.Creator), the dispatcher's
// create_task tool, and autonomy, so a project can't burst past its
// cap by routing through a different entry point.
//
// The existing unit tests cover narrower angles in isolation:
//   - ratelimit.TestCheck_IsolatedPerProject pins per-project
//     isolation at the limiter primitive (direct Check/Record).
//   - api.TestServer_CreateTask_ViaCreator_RateLimited pins a single
//     project's 2nd POST /tasks -> 429 (cap=1, no isolation angle).
//
// This file adds the *wired* angles those don't: a multi-request
// burst through the real HTTP handler that crosses the cap and
// returns 429, a DIFFERENT project that stays unaffected (isolation
// observed end-to-end through the handler, not just the primitive),
// and proof that the SAME limiter instance the API path saturates is
// the one the dispatcher's create_task gate then consults via the
// shared ratelimit.ProjectLimiter — so neither path can bypass the
// shared counter. The dispatcher path is also driven end-to-end in
// dispatcher.TestRateLimit_CreateTask_SharedLimiterBlocks.
//
// Determinism: the Creator's clock is frozen via WithNowFunc so the
// whole burst lands at one instant of the rolling minute window. No
// sleeps.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// makeSharedLimiterRegistry builds two projects (burst + other) that
// share a swarm/workflow, each with the same per-minute task cap so
// the isolation assertion is symmetric. cap is the tasks_per_minute
// applied to both projects.
func makeSharedLimiterRegistry(t *testing.T, perMinute int) *registry.Registry {
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
	projYAML := func(id string) string {
		return fmt.Sprintf(`projectId: %q
displayName: %q
swarmId: "shared-swarm"
defaultWorkflowId: "build"
defaultPriority: 50
rate_limit:
  tasks_per_minute: %d
`, id, id, perMinute)
	}
	write("projects/burst.yaml", projYAML("burst"))
	write("projects/other.yaml", projYAML("other"))

	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

// postCreateTask fires one POST /projects/{id}/tasks and returns the
// recorder so the caller can assert on status + body.
func postCreateTask(t *testing.T, server *Server, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"taskType":"research","context":{"prompt":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/projects/"+projectID+"/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)
	return rec
}

// TestRateLimit_SharedLimiter_BurstAndIsolation is the wired
// burst->429 + per-project isolation journey through the REST POST
// /tasks handler, plus a same-instance proof that the dispatcher's
// create_task gate would observe the identical saturated counter.
//
// With a per-minute cap of 3 on project "burst":
//
//  1. The first 3 POST /tasks are accepted (202).
//  2. The 4th+ POST /tasks exceed the window -> 429 RATE_LIMITED,
//     and no Retry-After header (this path leaves it unset by design).
//  3. A DIFFERENT project ("other") is unaffected: its first POST
//     still returns 202 even though "burst" is saturated.
//  4. The SAME limiter instance, consulted exactly as the dispatcher
//     create_task gate does (Check(proj, now)), reports Blocked for
//     "burst" and not for "other" — proving no creation path can
//     bypass the shared counter.
//
// Characterization: expected to PASS. A failure of step 2 or 4 would
// mean a creation path bypasses the shared limiter — a real bug per
// the ratelimit package doc's "can't burst past its cap by routing
// through a different entry point" invariant.
func TestRateLimit_SharedLimiter_BurstAndIsolation(t *testing.T) {
	const perMinute = 3
	reg := makeSharedLimiterRegistry(t, perMinute)

	// One limiter instance, shared by the API server/Creator and (in
	// the same-instance check below) the dispatcher gate.
	sharedLimiter := ratelimit.New()

	// Frozen clock for the API path so the whole burst lands in one
	// instant of the rolling minute window.
	frozen := time.Now()

	taskRepo := &mocks.MockTaskRepository{}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
		taskcreate.WithRateLimiter(sharedLimiter),
		taskcreate.WithNowFunc(func() time.Time { return frozen }),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
		WithRateLimiter(sharedLimiter),
	)

	// --- Step 1: fill the cap with accepted creations for "burst". ---
	for i := 0; i < perMinute; i++ {
		rec := postCreateTask(t, server, "burst")
		require.Equalf(t, http.StatusAccepted, rec.Code,
			"creation %d within cap must be accepted; body=%s", i+1, rec.Body.String())
	}
	require.Equal(t, perMinute, taskRepo.CallCount.Create,
		"exactly cap tasks should have been persisted")

	// --- Step 2: burst beyond the cap -> 429 RATE_LIMITED. ---
	for i := 0; i < 2; i++ {
		rec := postCreateTask(t, server, "burst")
		assert.Equalf(t, http.StatusTooManyRequests, rec.Code,
			"over-cap creation %d must be 429; body=%s", i+1, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "RATE_LIMITED",
			"429 body must carry the RATE_LIMITED code")
		// The task-creation limiter path does NOT emit a Retry-After
		// header (only the trading key-scoped CheckKey path sets
		// Decision.RetryAfter; this path leaves it zero — see the
		// Decision.RetryAfter doc in ratelimit.go). Pin that so a
		// future change that starts emitting it is a conscious one.
		assert.Empty(t, rec.Header().Get("Retry-After"),
			"task-creation 429 path does not set Retry-After")
	}
	assert.Equal(t, perMinute, taskRepo.CallCount.Create,
		"no new task should be persisted while the project is rate-limited")

	// --- Step 3: a DIFFERENT project is unaffected (isolation). ---
	rec := postCreateTask(t, server, "other")
	assert.Equalf(t, http.StatusAccepted, rec.Code,
		"a different project must not inherit burst's saturated window; body=%s", rec.Body.String())
	assert.Equal(t, perMinute+1, taskRepo.CallCount.Create,
		"the other-project creation should have persisted")

	// --- Step 4: same-instance no-bypass proof. ---
	// The dispatcher's create_task gate calls
	// te.rateLimiter.Check(proj, time.Now()) on the very same
	// ProjectLimiter instance. Reproduce that exact call here against
	// sharedLimiter and confirm the verdict the dispatcher would get:
	// "burst" blocked, "other" not. This pins that the instance the
	// API path saturated is the instance the dispatcher consults.
	burstProj := reg.GetProject("burst")
	otherProj := reg.GetProject("other")
	require.NotNil(t, burstProj)
	require.NotNil(t, otherProj)
	assert.True(t, sharedLimiter.Check(burstProj, frozen).Blocked,
		"the shared limiter (dispatcher's view) must report burst as rate-limited")
	assert.False(t, sharedLimiter.Check(otherProj, frozen).Blocked,
		"the shared limiter must NOT report the isolated other project as rate-limited")
}
