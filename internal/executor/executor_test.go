package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

type MockRuntime struct {
	mu          sync.Mutex
	startErr    error
	startCalls  int
	waitErr     error
	waitCode    int
	stopCalls   int
	removeCalls int
	outputJSON  string
	// outputJSONSequence, when non-empty, supplies a different result.json
	// payload per StartContainer call (first call → element 0, second
	// → element 1, etc.). Once exhausted the mock falls back to
	// outputJSON. Used by route-step corrective-retry tests where the
	// lead's first answer is intentionally bad and the second must
	// recover. Default nil mimics the legacy single-output behaviour.
	outputJSONSequence []string
	lastConfig         *runtime.ContainerConfig
	// waitGate, when non-nil, causes WaitForExit to block on it so tests
	// can observe the executor's state while a run is mid-flight.
	waitGate          chan struct{}
	waitEntered       chan struct{}
	ignoreWaitContext bool
	// stopReleasesWaitGate makes StopContainer close waitGate exactly once,
	// modelling production where pauseWithReason's StopContainer is what makes
	// the in-flight WaitForExit return — so the run goroutine observes
	// shuttingDown (set before pauseWithReason) and bails, instead of racing a
	// manual close(gate). Used by the graceful-shutdown drain test.
	stopReleasesWaitGate bool
	stopGateClosed       bool
	// liveContainers simulates podman's view of containers keyed by task ID.
	// Recover()'s orphan sweep calls GetContainerByTask to decide whether a
	// RUNNING execution row is genuinely resumable or a zombie left by a
	// SIGKILL. Tests opt in by populating this map; default empty mimics a
	// crashed daemon whose agent containers were also killed.
	liveContainers map[string]*runtime.Container
	// inspectByID lets a test supply InspectContainer results per container
	// ID (re-attach reconcile). Default nil → InspectContainer returns
	// (nil, nil) as before (container not found).
	inspectByID map[string]*runtime.Container
	// artifactFiles, when non-empty, makes StartContainer write each
	// name→content entry into <WorkspaceDir>/artifacts/out/ so the real
	// persistArtifacts harvest path (artifacts.go source #1) turns them
	// into persistence.Artifact rows. Default nil mimics an agent that
	// produced no deliverables — preserving the legacy result.json-only
	// behaviour. Used by the full-lifecycle characterization test to
	// assert artifacts are persisted end-to-end through Execute().
	artifactFiles map[string]string
	// artifactFilesSequence, when non-empty, supplies a different
	// name→content map per StartContainer call (first call → element 0,
	// etc.). The popped map is written into <WorkspaceDir>/artifacts/out/
	// in place of artifactFiles; once the sequence is exhausted the mock
	// falls back to artifactFiles. Lets a multi-step e2e drive distinct
	// per-step deliverables — e.g. step 1 (researcher) emits research.md
	// while step 2 (writer) emits nothing. Default nil preserves the
	// legacy single-map artifactFiles behaviour. Added for the e9a5
	// cross-step artifact-handoff regression test.
	artifactFilesSequence []map[string]string
	// stagedInputsSeen captures, per StartContainer call, every regular
	// file present in <WorkspaceDir>/artifacts/out/ at the TOP of the
	// call — i.e. BEFORE the mock writes its own artifactFiles. Those
	// files are exactly what the executor's stageInputArtifacts copied
	// into the step's ephemeral workspace before launch, so this is the
	// observation point for the cross-step handoff (task e9a5): the
	// writer step's entry must contain the researcher's research.md.
	// Each element is a name→content map for one StartContainer call, in
	// call order. Default nil → no capture (existing tests unaffected).
	// Read via StagedInputsSeen() under the mock's lock.
	stagedInputsSeen []map[string]string
	// llmModelPerStart records the VORNIK_LLM_MODEL env value captured on
	// each StartContainer call, in call order. Lets a test assert WHICH
	// model the executor actually launched the container with on each
	// attempt (e.g. primary on run 1, fallback on run 2) — not merely that
	// a re-run happened. Read via LLMModelsLaunched() under the mock's lock.
	llmModelPerStart []string
}

func NewMockRuntime() *MockRuntime { return &MockRuntime{} }

// LLMModelsLaunched returns a copy of the VORNIK_LLM_MODEL env value seen
// by each StartContainer call, in order. Locked so it is safe to read
// while an executor goroutine is still running.
func (m *MockRuntime) LLMModelsLaunched() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.llmModelPerStart))
	copy(out, m.llmModelPerStart)
	return out
}

// StagedInputsSeen returns a copy of the per-call snapshots of
// <WorkspaceDir>/artifacts/out/ captured at the top of each
// StartContainer call. Locked so it is safe to read while an executor
// goroutine is still running. See the stagedInputsSeen field doc.
func (m *MockRuntime) StagedInputsSeen() []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]string, len(m.stagedInputsSeen))
	for i, seen := range m.stagedInputsSeen {
		cp := make(map[string]string, len(seen))
		for k, v := range seen {
			cp[k] = v
		}
		out[i] = cp
	}
	return out
}

// StartCalls returns the StartContainer count under the mock's lock — use this
// (not the raw field) when reading concurrently with a still-running executor
// goroutine, e.g. inside assert.Eventually, to avoid a data race with the
// mutex-guarded increment in StartContainer.
func (m *MockRuntime) StartCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCalls
}
func (m *MockRuntime) StartContainer(ctx context.Context, c *runtime.ContainerConfig) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startCalls++
	m.lastConfig = c
	m.llmModelPerStart = append(m.llmModelPerStart, c.EnvVars["VORNIK_LLM_MODEL"])
	// e9a5 handoff observation: BEFORE writing any of this step's own
	// deliverables, snapshot whatever the executor already staged into
	// <WorkspaceDir>/artifacts/out/. For a multi-step workflow, step 2's
	// snapshot is the prior step's outputs that stageInputArtifacts
	// copied in (class="output"). Best-effort + read-only: a missing dir
	// records an empty map. Always recorded (mutex-guarded) so the slice
	// indexes 1:1 with StartContainer calls; tests that ignore it simply
	// never read StagedInputsSeen().
	{
		seen := map[string]string{}
		if c.WorkspaceDir != "" {
			stagedOut := filepath.Join(c.WorkspaceDir, "artifacts", "out")
			if entries, err := os.ReadDir(stagedOut); err == nil {
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					if data, rerr := os.ReadFile(filepath.Join(stagedOut, entry.Name())); rerr == nil {
						seen[entry.Name()] = string(data)
					}
				}
			}
		}
		m.stagedInputsSeen = append(m.stagedInputsSeen, seen)
	}
	if m.startErr != nil {
		return "", m.startErr
	}
	if c.OutputDir != "" {
		var payload string
		if len(m.outputJSONSequence) > 0 {
			payload = m.outputJSONSequence[0]
			m.outputJSONSequence = m.outputJSONSequence[1:]
		} else {
			payload = m.outputJSON
		}
		if payload != "" {
			_ = os.MkdirAll(c.OutputDir, 0o755)
			_ = os.WriteFile(filepath.Join(c.OutputDir, "result.json"), []byte(payload), 0o644)
		}
	}
	// Simulate an agent emitting deliverables: write any configured
	// artifact files into the workspace's artifacts/out dir so the real
	// persistArtifacts harvest (artifacts.go) records them. Mirrors a
	// container writing to /app/workspace/artifacts/out at runtime.
	// artifactFilesSequence takes precedence when non-empty (per-step
	// control, popped front-first); once exhausted the mock falls back to
	// the single artifactFiles map (legacy behaviour).
	stepFiles := m.artifactFiles
	if len(m.artifactFilesSequence) > 0 {
		stepFiles = m.artifactFilesSequence[0]
		m.artifactFilesSequence = m.artifactFilesSequence[1:]
	}
	if len(stepFiles) > 0 && c.WorkspaceDir != "" {
		outDir := filepath.Join(c.WorkspaceDir, "artifacts", "out")
		_ = os.MkdirAll(outDir, 0o755)
		for name, content := range stepFiles {
			_ = os.WriteFile(filepath.Join(outDir, name), []byte(content), 0o644)
		}
	}
	return "container-" + c.TaskID, nil
}
func (m *MockRuntime) StopContainer(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	m.stopCalls++
	if m.stopReleasesWaitGate && m.waitGate != nil && !m.stopGateClosed {
		m.stopGateClosed = true
		close(m.waitGate)
	}
	m.mu.Unlock()
	return nil
}
func (m *MockRuntime) InspectContainer(ctx context.Context, id string) (*runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inspectByID != nil {
		if c, ok := m.inspectByID[id]; ok {
			return c, nil
		}
	}
	return nil, nil
}
func (m *MockRuntime) WaitForExit(ctx context.Context, id string, t time.Duration) (int, error) {
	m.mu.Lock()
	gate := m.waitGate
	entered := m.waitEntered
	m.waitEntered = nil // close-once: a multi-step workflow calls WaitForExit per step; only the first signals "entered"
	ignoreCtx := m.ignoreWaitContext
	m.mu.Unlock()
	if gate != nil {
		if entered != nil {
			close(entered)
		}
		if ignoreCtx {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return -1, ctx.Err()
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waitErr != nil {
		return -1, m.waitErr
	}
	return m.waitCode, nil
}
func (m *MockRuntime) GetContainerByTask(ctx context.Context, tid string) (*runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.liveContainers[tid]; ok {
		return c, nil
	}
	return nil, nil
}

// registerLiveContainer makes a subsequent GetContainerByTask return a
// non-nil container for tid. Use this when a test simulates a daemon
// crash where the agent container survived (e.g. SIGTERM raced past
// pauseWithReason). Tests that simulate "container also died" leave
// the map empty.
func (m *MockRuntime) registerLiveContainer(tid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.liveContainers == nil {
		m.liveContainers = map[string]*runtime.Container{}
	}
	m.liveContainers[tid] = &runtime.Container{
		ID:     "container-" + tid,
		Name:   "vornik-test-" + tid,
		Status: runtime.StatusRunning,
	}
}
func (m *MockRuntime) RemoveContainer(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	m.removeCalls++
	m.mu.Unlock()
	return nil
}

func (m *MockRuntime) Logs(ctx context.Context, containerID string, tail int) (string, error) {
	return "", nil
}

type MockExecRepo struct {
	mu              sync.Mutex
	execs           map[string]*persistence.Execution
	err             error
	updateStatusErr error // per-method override for ResumePaused error-path tests
}

func NewMockExecRepo() *MockExecRepo {
	return &MockExecRepo{execs: make(map[string]*persistence.Execution)}
}

// snapshotStatus returns the execution's current status under the
// mock's mutex so tests can assert on it without racing the
// goroutine recoverExecution spawns. Plain map access on
// er.execs[id].Status is a data race when the test runs alongside
// the runExecution goroutine — surfaced by `go test -race` on
// 2026-05-16.
func (m *MockExecRepo) snapshotStatus(id string) persistence.ExecutionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.execs[id]
	if !ok {
		return ""
	}
	return e.Status
}
func (m *MockExecRepo) Create(ctx context.Context, e *persistence.Execution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	cp := *e
	cp.CompletedSteps = append([]string{}, e.CompletedSteps...)
	m.execs[e.ID] = &cp
	return nil
}
func (m *MockExecRepo) Get(ctx context.Context, id string) (*persistence.Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.execs[id]
	if e == nil {
		return nil, nil
	}
	cp := *e
	cp.CompletedSteps = append([]string{}, e.CompletedSteps...)
	return &cp, nil
}
func (m *MockExecRepo) GetByTaskID(ctx context.Context, tid string) (*persistence.Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.execs {
		if e.TaskID == tid {
			cp := *e
			cp.CompletedSteps = append([]string{}, e.CompletedSteps...)
			return &cp, nil
		}
	}
	return nil, errors.New("not found")
}
func (m *MockExecRepo) Update(ctx context.Context, e *persistence.Execution) error {
	m.mu.Lock()
	cp := *e
	cp.CompletedSteps = append([]string{}, e.CompletedSteps...)
	m.execs[e.ID] = &cp
	m.mu.Unlock()
	return nil
}
func (m *MockExecRepo) List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.Execution
	for _, exec := range m.execs {
		if filter.Status != nil && exec.Status != *filter.Status {
			continue
		}
		cp := *exec
		cp.CompletedSteps = append([]string{}, exec.CompletedSteps...)
		out = append(out, &cp)
	}
	return out, nil
}
func (m *MockExecRepo) UpdateStatus(ctx context.Context, id string, s persistence.ExecutionStatus) error {
	m.mu.Lock()
	if m.updateStatusErr != nil {
		err := m.updateStatusErr
		m.mu.Unlock()
		return err
	}
	if e, ok := m.execs[id]; ok {
		e.Status = s
	}
	m.mu.Unlock()
	return nil
}
func (m *MockExecRepo) SaveStateSnapshot(ctx context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error {
	m.mu.Lock()
	if e, ok := m.execs[id]; ok {
		e.StateSnapshot = snapshot
		e.CompletedSteps = append([]string{}, completedSteps...)
		if currentStepID != "" {
			e.CurrentStepID = &currentStepID
		}
	}
	m.mu.Unlock()
	return nil
}
func (m *MockExecRepo) SetWorkflowSnapshot(ctx context.Context, id string, snapshot []byte) error {
	m.mu.Lock()
	if e, ok := m.execs[id]; ok {
		e.WorkflowSnapshot = snapshot
	}
	m.mu.Unlock()
	return nil
}
func (m *MockExecRepo) GetWorkflowSnapshot(ctx context.Context, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.execs[id]; ok {
		return e.WorkflowSnapshot, nil
	}
	return nil, nil
}
func (m *MockExecRepo) RecordCompletion(ctx context.Context, id string, r []byte) error {
	m.mu.Lock()
	if e, ok := m.execs[id]; ok {
		e.Status = persistence.ExecutionStatusCompleted
		// Mirror the postgres repo (UPDATE ... SET status, result=$2): persist
		// the result bytes so tests can assert on the recorded outcome.
		e.Result = append([]byte(nil), r...)
	}
	m.mu.Unlock()
	return nil
}
func (m *MockExecRepo) RecordFailure(ctx context.Context, id string, msg, code string) error {
	m.mu.Lock()
	if e, ok := m.execs[id]; ok {
		e.Status = persistence.ExecutionStatusFailed
		e.ErrorMessage = &msg
		if code != "" {
			c := code
			e.ErrorCode = &c
		}
	}
	m.mu.Unlock()
	return nil
}

// SupersedeNonTerminalForTask mirrors the production cascade: any
// non-terminal execution belonging to taskID is flipped to
// CANCELLED with the standard error_code marker. Returns the
// count for assertion convenience.
func (m *MockExecRepo) SupersedeNonTerminalForTask(_ context.Context, taskID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, e := range m.execs {
		if e.TaskID != taskID {
			continue
		}
		switch e.Status {
		case persistence.ExecutionStatusCompleted,
			persistence.ExecutionStatusFailed,
			persistence.ExecutionStatusCancelled:
			continue
		}
		e.Status = persistence.ExecutionStatusCancelled
		code := "superseded_by_terminal_task"
		e.ErrorCode = &code
		n++
	}
	return n, nil
}

type MockArtifactRepo struct{}

func NewMockArtifactRepo() *MockArtifactRepo                                          { return &MockArtifactRepo{} }
func (m *MockArtifactRepo) Create(ctx context.Context, a *persistence.Artifact) error { return nil }
func (m *MockArtifactRepo) GetByHash(ctx context.Context, h string) (*persistence.Artifact, error) {
	return nil, nil
}
func (m *MockArtifactRepo) List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, nil
}

type MockTaskRepo struct {
	mu    sync.Mutex
	tasks map[string]*persistence.Task
	err   error
}

func NewMockTaskRepo() *MockTaskRepo { return &MockTaskRepo{tasks: make(map[string]*persistence.Task)} }
func (m *MockTaskRepo) Get(ctx context.Context, id string) (*persistence.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	t := m.tasks[id]
	if t == nil {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}
func (m *MockTaskRepo) UpdateStatus(ctx context.Context, id string, s persistence.TaskStatus) error {
	m.mu.Lock()
	if t, ok := m.tasks[id]; ok {
		t.Status = s
	}
	m.mu.Unlock()
	return nil
}
func (m *MockTaskRepo) Create(_ context.Context, task *persistence.Task) error {
	m.mu.Lock()
	m.tasks[task.ID] = task
	m.mu.Unlock()
	return nil
}
func (m *MockTaskRepo) Update(_ context.Context, task *persistence.Task) error {
	m.mu.Lock()
	m.tasks[task.ID] = task
	m.mu.Unlock()
	return nil
}
func (m *MockTaskRepo) GetChildren(_ context.Context, parentID string) ([]*persistence.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.Task
	for _, t := range m.tasks {
		if t.ParentTaskID != nil && *t.ParentTaskID == parentID {
			out = append(out, t)
		}
	}
	return out, nil
}
func (m *MockTaskRepo) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, id)
	return nil
}
func (m *MockTaskRepo) AddTask(t *persistence.Task) { m.mu.Lock(); m.tasks[t.ID] = t; m.mu.Unlock() }

// ReleaseLease updates the task's status + retry-counters in
// place. Mirrors the semantics of postgres TaskRepository's same
// method but in-memory: clears the lease (LeaseID/LeasedBy/
// LeasedAt/LeaseExpiresAt all reset) when the new status is non-
// terminal-leased, captures Attempt/MaxAttempts/Error/ErrorClass
// from opts. Enforces the same "leaseID required" guard the
// real repo has (added 2025.12 to prevent ReleaseLease misuse
// for terminal-to-QUEUED transitions). Tests exercising the
// leaseless path must use TransitionConditional instead — which
// is exactly what the 2026-05-16 self-release fallback does.
func (m *MockTaskRepo) ReleaseLease(_ context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	if leaseID == "" {
		return fmt.Errorf("ReleaseLease: leaseID required (use RequeueTerminalTask for terminal-to-QUEUED transitions)")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskID]
	if !ok {
		return nil
	}
	t.Status = newStatus
	t.LeaseID = nil
	t.LeasedBy = nil
	t.LeasedAt = nil
	t.LeaseExpiresAt = nil
	if opts.Attempt > 0 {
		t.Attempt = opts.Attempt
	}
	if opts.MaxAttempts > 0 {
		t.MaxAttempts = opts.MaxAttempts
	}
	if opts.Error != "" {
		err := opts.Error
		t.LastError = &err
	}
	if opts.ErrorClass != "" {
		ec := string(opts.ErrorClass)
		t.LastErrorClass = &ec
	}
	return nil
}

// TransitionConditional is the leaseless transition path used by
// releaseRecoveredTask when the task has no leaseID (retry-from-step
// executions). Mirrors postgres TaskRepository.TransitionConditional:
// matches the current status against from[], applies the new status
// + optional fields atomically. Returns (true, nil) on hit,
// (false, nil) on status mismatch.
func (m *MockTaskRepo) TransitionConditional(_ context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return false, nil
	}
	matched := false
	for _, s := range from {
		if t.Status == s {
			matched = true
			break
		}
	}
	if !matched {
		return false, nil
	}
	t.Status = to
	if opts.ClearLease {
		t.LeaseID = nil
		t.LeasedBy = nil
		t.LeasedAt = nil
		t.LeaseExpiresAt = nil
	}
	if opts.Attempt > 0 {
		t.Attempt = opts.Attempt
	}
	if opts.MaxAttempts > 0 {
		t.MaxAttempts = opts.MaxAttempts
	}
	if opts.LastError != nil {
		t.LastError = opts.LastError
	}
	if opts.LastErrorClass != nil {
		t.LastErrorClass = opts.LastErrorClass
	}
	if opts.ClosedBy != nil {
		t.ClosedBy = opts.ClosedBy
	}
	if opts.SetClosedAtNow {
		now := time.Now()
		t.ClosedAt = &now
	}
	return true, nil
}

type MockWorkflowResolver struct {
	projects  map[string]*registry.Project
	swarms    map[string]*registry.Swarm
	workflows map[string]*registry.Workflow
}

func (m *MockWorkflowResolver) GetProject(id string) *registry.Project   { return m.projects[id] }
func (m *MockWorkflowResolver) GetSwarm(id string) *registry.Swarm       { return m.swarms[id] }
func (m *MockWorkflowResolver) GetWorkflow(id string) *registry.Workflow { return m.workflows[id] }

func setup() (*Executor, *MockRuntime, *MockExecRepo, *MockArtifactRepo, *MockTaskRepo) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	return NewWithOptions(rt, er, ar, tr, nil), rt, er, ar, tr
}

func TestExecutor_New(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.NotNil(t, e)
	assert.Equal(t, 30*time.Minute, e.config.DefaultTimeout)
}

func TestExecutor_DefaultConfig(t *testing.T) {
	c := DefaultConfig()
	assert.Equal(t, 30*time.Minute, c.DefaultTimeout)
	assert.Equal(t, 3, c.MaxRetries)
}

func TestExecutor_Execute_AlreadyRunning(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	e.mu.Lock()
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1"}
	e.mu.Unlock()
	assert.Error(t, e.Execute("t1"))
}

func TestExecutor_TaskNotFound(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.err = errors.New("not found")
	assert.Error(t, e.Execute("x"))
}

func TestExecutor_CreateExecutionError(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	er.err = errors.New("fail")
	assert.Error(t, e.Execute("t1"))
}

func TestExecutor_CancelNonExistent(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.Error(t, e.Cancel("x"))
}

// TestExecutor_Stop_DrainsRegisteredGoroutines exercises the Phase 1
// lifecycle contract: Stop(ctx) must cancel every in-flight execution
// goroutine AND wait for each one's defer Done() before returning.
// Prior to the WaitGroup being added, Stop returned immediately while
// goroutines raced with the DB close the container does in shutdown.
//
// Test shape: register three fake goroutines against the WG the real
// runExecution path uses; each blocks on its per-handle context and
// signals done on cancel. Stop should unblock and drain all three.
func TestExecutor_Stop_DrainsRegisteredGoroutines(t *testing.T) {
	e, _, _, _, _ := setup()

	const n = 3
	fired := make(chan string, n)

	e.mu.Lock()
	if e.ctx == nil {
		e.ctx, e.cancel = context.WithCancel(context.Background())
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("task-%d", i)
		execCtx, cancel := context.WithCancel(e.ctx)
		e.activeExecutions[id] = &executionHandle{
			taskID:    id,
			projectID: "p",
			startedAt: time.Now(),
			cancel:    cancel,
			ctx:       execCtx,
		}
		e.wg.Add(1)
		go func(id string, ctx context.Context) {
			defer e.wg.Done()
			<-ctx.Done()
			fired <- id
		}(id, execCtx)
	}
	e.mu.Unlock()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, e.Stop(stopCtx))

	// Every fake goroutine must have unblocked and written to fired.
	received := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		select {
		case id := <-fired:
			received[id] = true
		case <-time.After(time.Second):
			t.Fatalf("goroutine %d did not fire before timeout; received so far: %v", i, received)
		}
	}
	assert.Len(t, received, n)
}

// TestExecutor_Stop_RespectsContextDeadline verifies Stop returns the
// context's error when the caller's deadline elapses before the
// in-flight goroutines exit. Real case: a runaway agent step ignores
// cancel; shutdown gets a 30s budget and Stop must surrender rather
// than block forever.
func TestExecutor_Stop_RespectsContextDeadline(t *testing.T) {
	e, _, _, _, _ := setup()

	e.mu.Lock()
	if e.ctx == nil {
		e.ctx, e.cancel = context.WithCancel(context.Background())
	}
	execCtx, cancel := context.WithCancel(e.ctx)
	e.activeExecutions["stuck"] = &executionHandle{
		taskID: "stuck", projectID: "p", startedAt: time.Now(),
		cancel: cancel, ctx: execCtx,
	}
	e.wg.Add(1)
	// Runaway goroutine — never returns until the test ends.
	done := make(chan struct{})
	go func() {
		defer e.wg.Done()
		<-done
	}()
	e.mu.Unlock()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stopCancel()
	err := e.Stop(stopCtx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Release the runaway goroutine so the test suite's goleak-style
	// checks (if any) don't trip.
	close(done)
}

func TestExecutor_ActiveCount(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.Equal(t, 0, e.ActiveCount())
	e.mu.Lock()
	e.activeExecutions["t1"] = &executionHandle{}
	e.mu.Unlock()
	assert.Equal(t, 1, e.ActiveCount())
}

func TestExecutor_IsExecuting(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.False(t, e.IsExecuting("t1"))
	e.mu.Lock()
	e.activeExecutions["t1"] = &executionHandle{}
	e.mu.Unlock()
	assert.True(t, e.IsExecuting("t1"))
}

func TestExecutor_ShouldRetry(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.False(t, e.shouldRetry(nil))
	assert.False(t, e.shouldRetry(context.Canceled))
	assert.False(t, e.shouldRetry(context.DeadlineExceeded))
	assert.False(t, e.shouldRetry(errors.New("x")))
	assert.True(t, e.shouldRetry(markRetryable(errors.New("x"))))
}

func TestGenerateExecutionID(t *testing.T) { assert.Contains(t, generateExecutionID("t1"), "exec_") }
func TestGenerateArtifactID(t *testing.T) {
	assert.Contains(t, generateArtifactID("e1"), "artifact_")
}

func TestExecutor_StartContainer(t *testing.T) {
	e, _, _, _, _ := setup()
	role := &registry.SwarmRole{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}
	id, err := e.startContainer(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p1"}, "e1", "fake-agent:latest", "worker", "/tmp/in", "/tmp/out", "/tmp/work", role, "", e.config.DefaultTimeout, nil)
	assert.NoError(t, err)
	assert.Contains(t, id, "container-")
}

func TestExecutor_StartContainer_Error(t *testing.T) {
	e, rt, _, _, _ := setup()
	rt.startErr = errors.New("fail")
	role := &registry.SwarmRole{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}
	_, err := e.startContainer(context.Background(), &persistence.Task{ID: "t1"}, "e1", "fake-agent:latest", "worker", "/tmp/in", "/tmp/out", "/tmp/work", role, "", e.config.DefaultTimeout, nil)
	assert.Error(t, err)
}

func TestExecutor_StartContainer_ModelOverride(t *testing.T) {
	e, rt, _, _, _ := setup()
	e.config.AgentLLMEnv = map[string]string{
		"VORNIK_LLM_MODEL":    "default-model",
		"VORNIK_LLM_ENDPOINT": "http://localhost:8080",
	}
	role := &registry.SwarmRole{
		Name:  "coder",
		Model: "claude-sonnet-4-20250514",
		Runtime: registry.SwarmRoleRuntime{
			Image: "fake-agent:latest",
		},
	}
	_, err := e.startContainer(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p1"}, "e1", "fake-agent:latest", "coder", "/tmp/in", "/tmp/out", "/tmp/work", role, "", e.config.DefaultTimeout, nil)
	assert.NoError(t, err)
	// Verify the container was started with the overridden model
	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, "claude-sonnet-4-20250514", rt.lastConfig.EnvVars["VORNIK_LLM_MODEL"])
	assert.Equal(t, "http://localhost:8080", rt.lastConfig.EnvVars["VORNIK_LLM_ENDPOINT"])
}

func TestExecutor_WaitForCompletion(t *testing.T) {
	e, rt, _, _, _ := setup()
	rt.waitCode = 0
	code, err := e.waitForCompletion(context.Background(), "c1", time.Minute)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
}

func TestExecutor_HandleSuccess(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1"}
	_ = er.Create(context.Background(), exec)
	e.handleSuccess(context.Background(), tr.tasks["t1"], exec, "c1", []byte(`{"ok":true}`))
	tr.mu.Lock()
	assert.Equal(t, persistence.TaskStatusCompleted, tr.tasks["t1"].Status)
	tr.mu.Unlock()
	// Containers are now cleaned up per-step in executeAgentStep, not in handleSuccess.
}

func TestExecutor_HandleFailure(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1"}
	_ = er.Create(context.Background(), exec)
	e.handleFailure(context.Background(), tr.tasks["t1"], exec, errors.New("fail"))
	tr.mu.Lock()
	assert.Equal(t, persistence.TaskStatusFailed, tr.tasks["t1"].Status)
	tr.mu.Unlock()
}

func TestExecutor_Cancel(t *testing.T) {
	e, rt, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: time.Now()})
	_ = er.Create(context.Background(), &persistence.Execution{
		ID:        "e1",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusRunning,
	})
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1", projectID: "p1", containerID: "c1", cancel: cancel}
	e.mu.Unlock()
	assert.NoError(t, e.Cancel("t1"))
	rt.mu.Lock()
	assert.Equal(t, 1, rt.stopCalls)
	rt.mu.Unlock()
	tr.mu.Lock()
	assert.Equal(t, persistence.TaskStatusCancelled, tr.tasks["t1"].Status)
	tr.mu.Unlock()
	exec, err := er.GetByTaskID(context.Background(), "t1")
	assert.NoError(t, err)
	assert.Equal(t, persistence.ExecutionStatusCancelled, exec.Status)
}

func TestExecutor_HandleCancelled(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	e.handleCancelled(context.Background(), tr.tasks["t1"], exec)

	tr.mu.Lock()
	assert.Equal(t, persistence.TaskStatusCancelled, tr.tasks["t1"].Status)
	tr.mu.Unlock()
	updatedExec, err := er.GetByTaskID(context.Background(), "t1")
	assert.NoError(t, err)
	assert.Equal(t, persistence.ExecutionStatusCancelled, updatedExec.Status)
}

func TestExecutor_CleanupExecution(t *testing.T) {
	e, _, _, _, _ := setup()
	e.mu.Lock()
	e.activeExecutions["t1"] = &executionHandle{}
	e.mu.Unlock()
	assert.Equal(t, 1, e.ActiveCount())
	e.cleanupExecution("t1")
	assert.Equal(t, 0, e.ActiveCount())
}

func TestExecutor_Pause(t *testing.T) {
	e, rt, er, _, tr := setup()

	// Add task and execution
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	// Setup active execution
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1", containerID: "c1", cancel: cancel}
	e.mu.Unlock()

	// Pause
	status, err := e.Pause("t1")
	assert.NoError(t, err)
	assert.Equal(t, "t1", status.TaskID)
	assert.Equal(t, "e1", status.ExecutionID)

	// Verify container was stopped
	rt.mu.Lock()
	assert.Equal(t, 1, rt.stopCalls)
	rt.mu.Unlock()

	// Verify execution status was updated
	updatedExec, _ := er.GetByTaskID(context.Background(), "t1")
	assert.Equal(t, persistence.ExecutionStatusPaused, updatedExec.Status)
}

func TestExecutor_Pause_NoActiveExecution(t *testing.T) {
	e, _, _, _, _ := setup()

	_, err := e.Pause("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no active execution")
}

func TestExecutor_Resume(t *testing.T) {
	e, _, er, _, tr := setup()

	// Add task and paused execution
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusPaused}
	_ = er.Create(context.Background(), exec)

	// Setup executor context with cancelled context to prevent actual execution
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately so runExecution exits quickly
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.mu.Unlock()

	// Resume
	status, err := e.Resume("t1")
	assert.NoError(t, err)
	assert.Equal(t, "t1", status.TaskID)
	assert.Equal(t, "e1", status.ExecutionID)
}

func TestExecutor_Resume_NotPaused(t *testing.T) {
	e, _, er, _, tr := setup()

	// Add task and running execution (not paused)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	_, err := e.Resume("t1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not paused")
}

func TestExecutor_Resume_AlreadyRunning(t *testing.T) {
	e, _, er, _, tr := setup()

	// Add task and paused execution
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusPaused}
	_ = er.Create(context.Background(), exec)

	// Setup executor with active execution
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1", cancel: cancel}
	e.mu.Unlock()

	_, err := e.Resume("t1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already being executed")
}

func TestExecutor_GateWorkflow(t *testing.T) {
	e, rt, er, _, tr := setup()
	rt.outputJSON = `{"review":{"approved":true}}`
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "review",
				Steps: map[string]registry.WorkflowStep{
					"review":   {Type: "agent", Role: "worker", OnSuccess: "decision"},
					"decision": {Type: "gate", Gates: []registry.WorkflowGate{{Condition: "review.approved == true", Target: "done"}}},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"},
				},
			},
		},
	}
	e.SetWorkflowResolver(resolver)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})

	assert.NoError(t, e.Execute("t1"))
	assert.Eventually(t, func() bool {
		exec, _ := er.GetByTaskID(context.Background(), "t1")
		return exec != nil && exec.Status == persistence.ExecutionStatusCompleted
	}, time.Second, 10*time.Millisecond)

	exec, _ := er.GetByTaskID(context.Background(), "t1")
	assert.Equal(t, []string{"review", "decision"}, exec.CompletedSteps)
}

// TestExecutor_TaskTransitionsToRunning guards against the regression where
// a running task was reported as LEASED forever. The scheduler leases the
// task (LEASED) and hands off to the executor; the executor must then flip
// the task to RUNNING so UI / API / autonomy observers can distinguish
// "actively executing" from "held by a lease but not started". The bug
// before this fix: only execution status advanced — task stayed LEASED
// until a terminal state, making live dashboards silently misleading.
func TestExecutor_TaskTransitionsToRunning(t *testing.T) {
	e, rt, er, _, tr := setup()
	rt.outputJSON = `{"status":"COMPLETED"}`

	// Block inside WaitForExit so we can observe the RUNNING window
	// without racing with completion. Gate is closed at the end.
	gate := make(chan struct{})
	rt.mu.Lock()
	rt.waitGate = gate
	rt.ignoreWaitContext = true
	rt.mu.Unlock()

	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "run",
				Steps:      map[string]registry.WorkflowStep{"run": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	}
	e.SetWorkflowResolver(resolver)

	// Start the task in LEASED state — this mirrors the hand-off from the
	// scheduler, which has just leased the task before calling Execute.
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusLeased,
		CreatedAt: time.Now(),
	})

	assert.NoError(t, e.Execute("t1"))

	// While the runtime is gated, the task must move LEASED → RUNNING
	// and the execution must move PENDING → RUNNING. Without the fix,
	// the task would stay LEASED for the entire execution.
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t1")
		return task != nil && task.Status == persistence.TaskStatusRunning
	}, time.Second, 10*time.Millisecond, "task did not transition to RUNNING while executing")

	assert.Eventually(t, func() bool {
		exec, _ := er.GetByTaskID(context.Background(), "t1")
		return exec != nil && exec.Status == persistence.ExecutionStatusRunning
	}, time.Second, 10*time.Millisecond, "execution did not transition to RUNNING")

	// Release the runtime and confirm the task reaches COMPLETED.
	close(gate)
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t1")
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, time.Second, 10*time.Millisecond, "task did not reach COMPLETED after gate released")
}

// TestExecutor_RetryResetsVisitCounterAcrossAttempts pins B-9: a
// workflow with maxStepVisits=1 (companion-rag-ingest's shape) must
// be retryable without the second attempt's first step-visit
// tripping the rework-loop guard. Pre-fix the saveCheckpoint from
// attempt 1 persisted visit_counts={step: 1} into state, then
// attempt 2 incremented to 2, hit the guard, and failed in ~35ms
// before the agent did any work. Reproduced 2026-05-28 on
// task_20260528123225_6187ce355788a3de.
//
// Test shape:
//   - workflow with maxStepVisits=1 and a single step that runs the
//     container (which always fails — startErr set on the mock
//     runtime)
//   - task has MaxAttempts=2, so the retry loop should call
//     executeWorkflowAttempt twice
//   - we assert the runtime's startCalls == 2 (both attempts
//     actually reached the container-start point; the second wasn't
//     short-circuited by the visit guard)
func TestExecutor_RetryResetsVisitCounterAcrossAttempts(t *testing.T) {
	rt := NewMockRuntime()
	rt.startErr = errors.New("podman start failed (forces second attempt)")
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf-tight-loop"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf-tight-loop": {
				ID:            "wf-tight-loop",
				Entrypoint:    "ingest",
				MaxStepVisits: 1, // companion-rag-ingest's shape
				MaxIterations: 5,
				// No on_fail — so an agent failure propagates as a
				// retryable error (markRetryable wraps StartContainer
				// failures), and runExecution's retry loop fires.
				// Pre-fix attempt 2 immediately trips the visit guard
				// because state.VisitCounts[ingest] = 1 from attempt 1.
				Steps:     map[string]registry.WorkflowStep{"ingest": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})
	tr.AddTask(&persistence.Task{
		ID:          "t-tight",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 2,
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute("t-tight"))
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t-tight")
		return task != nil && task.Status == persistence.TaskStatusFailed
	}, time.Second, 10*time.Millisecond)

	rt.mu.Lock()
	startCalls := rt.startCalls
	rt.mu.Unlock()
	// 2 attempts × 1 container start each = 2. Pre-fix, attempt 2
	// would short-circuit on the visit guard BEFORE calling
	// StartContainer → startCalls == 1.
	assert.Equal(t, 2, startCalls,
		"both attempts must reach StartContainer; pre-fix attempt 2 trips the visit guard before the agent runs")
}

func TestExecutor_RetryMetricMatchesActualRetries(t *testing.T) {
	cases := []struct {
		name          string
		attempt       int
		maxAttempts   int
		wantStarts    int
		wantRetryHits float64
	}{
		{name: "fresh task uses every attempt", attempt: 1, maxAttempts: 3, wantStarts: 3, wantRetryHits: 2},
		{name: "manual retry starts at second attempt", attempt: 2, maxAttempts: 3, wantStarts: 2, wantRetryHits: 1},
		{name: "last attempt has no retry metric", attempt: 3, maxAttempts: 3, wantStarts: 1, wantRetryHits: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewMockRuntime()
			rt.startErr = errors.New("podman start failed")
			er := NewMockExecRepo()
			ar := NewMockArtifactRepo()
			tr := NewMockTaskRepo()
			metrics := NewMetrics(prometheus.NewRegistry())
			e := NewWithOptions(rt, er, ar, tr, nil, WithMetrics(metrics))
			e.config.RetryDelay = 0
			e.SetWorkflowResolver(&MockWorkflowResolver{
				projects: map[string]*registry.Project{
					"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
				},
				swarms: map[string]*registry.Swarm{
					"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
				},
				workflows: map[string]*registry.Workflow{
					"wf1": {
						ID:         "wf1",
						Entrypoint: "run",
						Steps:      map[string]registry.WorkflowStep{"run": {Type: "agent", Role: "worker", OnSuccess: "done"}},
						Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
					},
				},
			})
			tr.AddTask(&persistence.Task{
				ID:          "t1",
				ProjectID:   "p1",
				Status:      persistence.TaskStatusLeased,
				Attempt:     tc.attempt,
				MaxAttempts: tc.maxAttempts,
				CreatedAt:   time.Now(),
			})

			assert.NoError(t, e.Execute("t1"))
			assert.Eventually(t, func() bool {
				task, _ := tr.Get(context.Background(), "t1")
				return task != nil && task.Status == persistence.TaskStatusFailed
			}, time.Second, 10*time.Millisecond)

			rt.mu.Lock()
			startCalls := rt.startCalls
			rt.mu.Unlock()
			assert.Equal(t, tc.wantStarts, startCalls)
			assert.Equal(t, tc.wantRetryHits, testutil.ToFloat64(metrics.RetriedTotal.WithLabelValues("p1")))
			assert.Equal(t, float64(startCalls-1), testutil.ToFloat64(metrics.RetriedTotal.WithLabelValues("p1")))
		})
	}
}

func TestExecutor_ApprovalWorkflowPauseAndResume(t *testing.T) {
	e, _, er, _, tr := setup()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "approval",
				Steps: map[string]registry.WorkflowStep{
					"approval": {Type: "approval", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"},
				},
			},
		},
	}
	e.SetWorkflowResolver(resolver)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})

	assert.NoError(t, e.Execute("t1"))
	// Eventually ceilings bound async executor state transitions. Eventually
	// returns the instant the condition holds, so a generous ceiling only
	// bounds the failure path.
	const settle = 10 * time.Second
	assert.Eventually(t, func() bool {
		exec, _ := er.GetByTaskID(context.Background(), "t1")
		return exec != nil && exec.Status == persistence.ExecutionStatusPaused
	}, settle, 10*time.Millisecond)
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t1")
		return task != nil && task.Status == persistence.TaskStatusPending
	}, settle, 10*time.Millisecond)

	_, err := e.Resume("t1")
	assert.NoError(t, err)
	// Assert the STABLE post-resume terminal (COMPLETED), not the transient
	// RUNNING state. After Resume the approval workflow runs straight to its
	// COMPLETED terminal; under low GOMAXPROCS the task passes through RUNNING
	// faster than a poll-based Eventually (whose first tick lands ~10ms after
	// Resume, by which point the background goroutine has already finished)
	// can observe it. Asserting RUNNING here flaked ~50% under GOMAXPROCS=1
	// (CI's 2-core runner) with "Condition never satisfied" — the COMPLETED
	// terminals below are what actually prove Resume drove the workflow forward.
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t1")
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, settle, 10*time.Millisecond)
	assert.Eventually(t, func() bool {
		exec, _ := er.GetByTaskID(context.Background(), "t1")
		return exec != nil && exec.Status == persistence.ExecutionStatusCompleted
	}, settle, 10*time.Millisecond)

	exec, _ := er.GetByTaskID(context.Background(), "t1")
	assert.Equal(t, []string{"approval"}, exec.CompletedSteps)
}

func TestExecutor_ResumeGoroutineIsTrackedByStop(t *testing.T) {
	e, rt, er, _, tr := setup()
	rt.outputJSON = `{"status":"COMPLETED"}`
	gate := make(chan struct{})
	entered := make(chan struct{})
	rt.mu.Lock()
	rt.waitGate = gate
	rt.waitEntered = entered
	rt.ignoreWaitContext = true
	rt.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		_ = e.Stop(context.Background())
	})

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "run",
				Steps:      map[string]registry.WorkflowStep{"run": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})
	tr.AddTask(&persistence.Task{ID: "t-resume", ProjectID: "p1", CreatedAt: time.Now()})
	require.NoError(t, er.Create(context.Background(), &persistence.Execution{
		ID:         "exec-resume",
		TaskID:     "t-resume",
		ProjectID:  "p1",
		WorkflowID: "wf1",
		Status:     persistence.ExecutionStatusPaused,
	}))

	_, err := e.Resume("t-resume")
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return e.ActiveCount() == 1 }, time.Second, 10*time.Millisecond)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("resumed execution did not reach WaitForExit")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = e.Stop(stopCtx)
	require.Error(t, err, "Stop must wait for a resumed run instead of returning while it is still active")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestExecutor_RecoverRunningExecution(t *testing.T) {
	e, rt, er, _, tr := setup()
	rt.outputJSON = `{"status":"COMPLETED"}`
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "step2",
				Steps: map[string]registry.WorkflowStep{
					"step2": {Type: "agent", Role: "worker", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"},
				},
			},
		},
	}
	e.SetWorkflowResolver(resolver)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	current := "step2"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		WorkflowID:     "wf1",
		Status:         persistence.ExecutionStatusRunning,
		CurrentStepID:  &current,
		CompletedSteps: []string{"step1"},
	}
	_ = er.Create(context.Background(), exec)

	// Simulate the realistic recovery case: daemon crashed but the
	// agent container survived. Without this, Recover()'s orphan
	// sweep marks the row FAILED before recoverExecution can resume
	// it. See comment on Recover() in executor.go.
	rt.registerLiveContainer("t1")

	assert.NoError(t, e.Recover(context.Background()))
	assert.Eventually(t, func() bool {
		updated, _ := er.GetByTaskID(context.Background(), "t1")
		return updated != nil && updated.Status == persistence.ExecutionStatusCompleted
	}, time.Second, 10*time.Millisecond)

	updated, _ := er.GetByTaskID(context.Background(), "t1")
	assert.Equal(t, []string{"step1", "step2"}, updated.CompletedSteps)
}

// TestExecutor_RecoverMarksOrphanWhenContainerMissing — the 2026-05-12
// zombie-row bug: a SIGKILL'd daemon (or a graceful-shutdown bailout
// whose pauseWithReason hit a DB error) leaves a RUNNING execution row
// pointing at a dead container. Without the orphan sweep, recoverExecution
// would spawn a goroutine racing the scheduler's retry of the same task,
// producing two RUNNING rows for one task. With the sweep, the row is
// transitioned to FAILED/ORPHANED before recoverExecution touches it.
func TestExecutor_RecoverMarksOrphanWhenContainerMissing(t *testing.T) {
	e, rt, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	current := "step2"
	exec := &persistence.Execution{
		ID:             "exec_orphan",
		TaskID:         "t1",
		ProjectID:      "p1",
		WorkflowID:     "wf1",
		Status:         persistence.ExecutionStatusRunning,
		CurrentStepID:  &current,
		CompletedSteps: []string{"step1"},
	}
	_ = er.Create(context.Background(), exec)

	// Crucially: do NOT registerLiveContainer. GetContainerByTask
	// returns nil → orphan sweep should fire.
	_ = rt

	assert.NoError(t, e.Recover(context.Background()))
	updated, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, persistence.ExecutionStatusFailed, updated.Status,
		"orphan sweep must transition RUNNING execs with no live container to FAILED")
	require.NotNil(t, updated.ErrorCode)
	assert.Equal(t, persistence.TaskFailureClassOrphaned, *updated.ErrorCode,
		"orphan rows must carry the ORPHANED failure class so operators can audit them")
}

func TestBuildAgentInput_PromptCombination(t *testing.T) {
	oldNow := currentDateTimeNow
	currentDateTimeNow = func() time.Time {
		return time.Date(2026, 5, 4, 13, 45, 30, 0, time.UTC)
	}
	t.Cleanup(func() { currentDateTimeNow = oldNow })

	tests := []struct {
		name           string
		payload        string
		stepPrompt     string
		opts           *agentInputOpts
		wantContains   []string
		wantNotContain []string
		wantExact      string // if non-empty, prompt must equal this exactly
	}{
		{
			name:       "step prompt + context.prompt",
			payload:    `{"context":{"prompt":"Write hello world"}}`,
			stepPrompt: "Create a plan",
			wantContains: []string{
				"Create a plan",
				"--- Task ---",
				"Write hello world",
			},
		},
		{
			name:       "step prompt + taskType fallback",
			payload:    `{"taskType":"Write hello world"}`,
			stepPrompt: "Create a plan",
			wantContains: []string{
				"Create a plan",
				"Write hello world",
			},
		},
		{
			name:      "only user prompt",
			payload:   `{"context":{"prompt":"Write hello world"}}`,
			wantExact: "Current date/time context: today is Monday, May 4, 2026; current local time is 13:45:30 UTC (UTC: 2026-05-04T13:45:30Z). Use these values for any \"today\", \"tomorrow\", \"yesterday\", or time-sensitive reasoning.\n\nWrite hello world",
		},
		{
			name:       "only step prompt",
			payload:    "",
			stepPrompt: "Create a plan",
			wantExact:  "Current date/time context: today is Monday, May 4, 2026; current local time is 13:45:30 UTC (UTC: 2026-05-04T13:45:30Z). Use these values for any \"today\", \"tomorrow\", \"yesterday\", or time-sensitive reasoning.\n\nCreate a plan",
		},
		{
			name:       "StepPrompt override",
			payload:    `{"context":{"prompt":"Write hello world"}}`,
			stepPrompt: "Original step prompt",
			opts:       &agentInputOpts{StepPrompt: "Override prompt"},
			wantContains: []string{
				"Override prompt",
				"--- Task ---",
				"Write hello world",
			},
			wantNotContain: []string{
				"Original step prompt",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &persistence.Task{
				ID:        "t1",
				ProjectID: "p1",
				Payload:   json.RawMessage(tt.payload),
			}
			result := buildAgentInput(task, "e1", "wf1", "s1", "step1", "worker", tt.stepPrompt, tt.opts)

			var parsed map[string]any
			err := json.Unmarshal(result, &parsed)
			assert.NoError(t, err, "result should be valid JSON")

			ctx, ok := parsed["context"].(map[string]any)
			assert.True(t, ok, "result should have context object")

			prompt, ok := ctx["prompt"].(string)
			assert.True(t, ok, "context should have prompt string")

			if tt.wantExact != "" {
				assert.Equal(t, tt.wantExact, prompt)
			}
			assert.Contains(t, prompt, "Current date/time context:")
			assert.Contains(t, prompt, "May 4, 2026")
			for _, want := range tt.wantContains {
				assert.Contains(t, prompt, want)
			}
			for _, notWant := range tt.wantNotContain {
				assert.NotContains(t, prompt, notWant)
			}
		})
	}
}

func TestBuildAgentInput_CurrentDateTimeUsesProjectTimezone(t *testing.T) {
	oldNow := currentDateTimeNow
	currentDateTimeNow = func() time.Time {
		return time.Date(2026, 5, 4, 22, 30, 0, 0, time.UTC)
	}
	t.Cleanup(func() { currentDateTimeNow = oldNow })

	task := &persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Payload:   json.RawMessage(`{"context":{"prompt":"Check today's market window"}}`),
	}
	result := buildAgentInput(task, "e1", "wf1", "s1", "step1", "worker", "", &agentInputOpts{
		ProjectTimezone: "Europe/Prague",
	})

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(result, &parsed))
	ctx := parsed["context"].(map[string]any)
	prompt := ctx["prompt"].(string)
	assert.Contains(t, prompt, "Tuesday, May 5, 2026")
	assert.Contains(t, prompt, "00:30:00 Europe/Prague")

	current := ctx["currentDateTime"].(map[string]any)
	assert.Equal(t, "2026-05-05", current["date"])
	assert.Equal(t, "00:30:00", current["time"])
	assert.Equal(t, "Tuesday", current["weekday"])
	assert.Equal(t, "Europe/Prague", current["timezone"])
	assert.Equal(t, "2026-05-04T22:30:00Z", current["utc"])
}

func TestBuildGatePromptSuffix(t *testing.T) {
	gates := []registry.WorkflowGate{
		{Condition: "review.approved == true", Target: "complete"},
		{Condition: "review.approved == false", Target: "implement"},
	}

	result := buildGatePromptSuffix(gates)

	assert.Contains(t, result, "IMPORTANT")
	assert.Contains(t, result, "pure JSON object")
	assert.Contains(t, result, "review.approved == true")
	assert.Contains(t, result, "review.approved == false")
	assert.Contains(t, result, "complete")
	assert.Contains(t, result, "implement")
	assert.Contains(t, result, "review.approved == true (routes to: complete)")
}

func TestEvaluateGateCondition(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		payload   string
		wantMatch bool
		wantErr   string
	}{
		{
			name:      "boolean true match",
			condition: "review.approved == true",
			payload:   `{"review":{"approved":true}}`,
			wantMatch: true,
		},
		{
			name:      "boolean false match",
			condition: "review.approved == false",
			payload:   `{"review":{"approved":false}}`,
			wantMatch: true,
		},
		{
			name:      "boolean mismatch",
			condition: "review.approved == true",
			payload:   `{"review":{"approved":false}}`,
			wantMatch: false,
		},
		{
			name:      "string match",
			condition: `status == "done"`,
			payload:   `{"status":"done"}`,
			wantMatch: true,
		},
		{
			name:      "number match",
			condition: "score == 42",
			payload:   `{"score":42.0}`,
			wantMatch: true,
		},
		{
			name:      "nested path not found",
			condition: "a.b.c == true",
			payload:   `{"a":{}}`,
			wantMatch: false,
		},
		{
			name:      "unsupported operator",
			condition: "x > 5",
			wantErr:   "unsupported",
		},
		{
			name:      "compound AND both true",
			condition: "review.approved == true && review.all_done == true",
			payload:   `{"review":{"approved":true,"all_done":true}}`,
			wantMatch: true,
		},
		{
			name:      "compound AND first true second false",
			condition: "review.approved == true && review.all_done == true",
			payload:   `{"review":{"approved":true,"all_done":false}}`,
			wantMatch: false,
		},
		{
			name:      "compound AND first false",
			condition: "review.approved == true && review.all_done == true",
			payload:   `{"review":{"approved":false,"all_done":true}}`,
			wantMatch: false,
		},
		{
			name:      "compound AND path not found",
			condition: "review.approved == true && review.all_done == true",
			payload:   `{"review":{"approved":true}}`,
			wantMatch: false,
		},
		// Flat-key lenience: many LLMs respond with the dotted
		// condition path as a single literal JSON key rather than
		// nesting objects. Both forms must work for the same gate.
		{
			name:      "flat key matches boolean true",
			condition: "review.approved == true",
			payload:   `{"review.approved":true,"rationale":"looks good"}`,
			wantMatch: true,
		},
		{
			name:      "flat key matches boolean false",
			condition: "review.approved == false",
			payload:   `{"review.approved":false}`,
			wantMatch: true,
		},
		{
			name:      "flat key mismatch",
			condition: "review.approved == true",
			payload:   `{"review.approved":false}`,
			wantMatch: false,
		},
		{
			name:      "nested wins over coincidental flat key",
			condition: "review.approved == true",
			payload:   `{"review.approved":false,"review":{"approved":true}}`,
			// Flat-key is checked first, so {"review.approved":false}
			// wins here. Document the precedence — producers shouldn't
			// mix both shapes, but if they do the explicit flat key
			// is more reliable than stumbling on a nested sibling.
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var payload any
			if tt.payload != "" {
				err := json.Unmarshal([]byte(tt.payload), &payload)
				assert.NoError(t, err)
			}

			match, err := evaluateGateCondition(tt.condition, payload)
			if tt.wantErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantMatch, match)
			}
		})
	}
}

func TestLookupJSONPath(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		path      string
		wantValue any
		wantFound bool
	}{
		{
			name:      "simple nested bool",
			payload:   `{"review":{"approved":true}}`,
			path:      "review.approved",
			wantValue: true,
			wantFound: true,
		},
		{
			name:      "deeply nested string",
			payload:   `{"a":{"b":{"c":"deep"}}}`,
			path:      "a.b.c",
			wantValue: "deep",
			wantFound: true,
		},
		{
			name:      "missing key",
			payload:   `{"x":1}`,
			path:      "missing",
			wantValue: nil,
			wantFound: false,
		},
		{
			name:      "intermediate not a map",
			payload:   `{"a":123}`,
			path:      "a.b",
			wantValue: nil,
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var payload any
			err := json.Unmarshal([]byte(tt.payload), &payload)
			assert.NoError(t, err)

			value, found := lookupJSONPath(payload, tt.path)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.Equal(t, tt.wantValue, value)
			}
		})
	}
}

func TestParseGateValue(t *testing.T) {
	tests := []struct {
		input string
		want  any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{`"hello"`, "hello"},
		{"42", 42.0},
		{"3.14", 3.14},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseGateValue(tt.input)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRecoverExecution_TerminalTaskSkipped(t *testing.T) {
	e, _, er, _, tr := setup()

	// Create a task in terminal state (FAILED)
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusFailed,
		CreatedAt: time.Now(),
	})

	// Create a RUNNING execution that points to the failed task
	exec := &persistence.Execution{
		ID:        "e1",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusRunning,
	}
	_ = er.Create(context.Background(), exec)

	// Recover should mark the orphaned execution as failed, not restart it
	err := e.Recover(context.Background())
	assert.NoError(t, err)

	// Verify execution was marked as FAILED
	updatedExec, getErr := er.Get(context.Background(), "e1")
	assert.NoError(t, getErr)
	assert.Equal(t, persistence.ExecutionStatusFailed, updatedExec.Status)

	// Verify error message references orphaned or the task status
	assert.NotNil(t, updatedExec.ErrorMessage)
	msg := strings.ToLower(*updatedExec.ErrorMessage)
	assert.True(t, strings.Contains(msg, "orphaned") || strings.Contains(msg, string(persistence.TaskStatusFailed)),
		"error message should mention orphaned or task status, got: %s", *updatedExec.ErrorMessage)

	// Verify the execution was NOT added to activeExecutions (not restarted)
	assert.False(t, e.IsExecuting("t1"))
}

func TestWithWorkflowResolver_NilResolver(t *testing.T) {
	e := &Executor{}
	WithWorkflowResolver(nil)(e)
	assert.Nil(t, e.workflows)
}

func TestSetWorkflowResolver_NilResolver(t *testing.T) {
	e := &Executor{}
	e.SetWorkflowResolver(nil)
	assert.Nil(t, e.workflows)
}

func TestSetWorkflowResolver_NilExecutor(t *testing.T) {
	var e *Executor
	e.SetWorkflowResolver(&MockWorkflowResolver{}) // should not panic
}

func TestResolveExecutionPlan_NilWorkflows(t *testing.T) {
	e, _, _, _, _ := setup()
	// e.workflows is nil by default from setup()
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Payload: []byte(`{}`)}
	exec := &persistence.Execution{ID: "e1"}
	plan, err := e.resolveExecutionPlan(context.Background(), task, exec)
	assert.NoError(t, err)
	assert.NotNil(t, plan)
	assert.Equal(t, "default-workflow", plan.workflow.ID)
}

func TestResolveExecutionPlan_WithResolver_ProjectNotFound(t *testing.T) {
	e, _, _, _, _ := setup()
	resolver := &MockWorkflowResolver{
		projects:  map[string]*registry.Project{},
		swarms:    map[string]*registry.Swarm{},
		workflows: map[string]*registry.Workflow{},
	}
	e.SetWorkflowResolver(resolver)
	task := &persistence.Task{ID: "t1", ProjectID: "missing-project"}
	exec := &persistence.Execution{ID: "e1"}
	_, err := e.resolveExecutionPlan(context.Background(), task, exec)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestExecutor_NilMetrics_NoOp(t *testing.T) {
	e, _, er, _, tr := setup()
	// e.metrics is nil by default from setup()
	task := &persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1"}
	_ = er.Create(context.Background(), exec)

	// Should not panic with nil metrics
	e.handleSuccess(context.Background(), task, exec, "c1", []byte(`{"status":"COMPLETED"}`))
	updated, _ := er.Get(context.Background(), "e1")
	assert.Equal(t, persistence.ExecutionStatusCompleted, updated.Status)
}

func TestExecutor_HandleFailure_NilMetrics(t *testing.T) {
	e, _, er, _, tr := setup()
	task := &persistence.Task{ID: "t2", ProjectID: "p1", CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "e2", TaskID: "t2", ProjectID: "p1"}
	_ = er.Create(context.Background(), exec)

	e.handleFailure(context.Background(), task, exec, fmt.Errorf("test error"))
	updated, _ := er.Get(context.Background(), "e2")
	assert.Equal(t, persistence.ExecutionStatusFailed, updated.Status)
}
