package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// MockTaskRepository implements TaskRepository for testing.
type MockTaskRepository struct {
	mu sync.Mutex

	// Tasks by ID
	tasks map[string]*persistence.Task

	// Lease tracking
	leasedTasks []*persistence.Task

	// Expired leases for recovery
	expiredLeases []*persistence.Task

	// Call counters
	leaseTaskCalls     int
	renewLeaseCalls    int
	releaseLeaseCalls  int
	findExpiredCalls   int
	countByStatusCalls int

	// Error injection
	leaseTaskErr    error
	releaseLeaseErr error
	findExpiredErr  error
}

func NewMockTaskRepository() *MockTaskRepository {
	return &MockTaskRepository{
		tasks: make(map[string]*persistence.Task),
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func (m *MockTaskRepository) Get(ctx context.Context, id string) (*persistence.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return cloneTask(task), nil
}

func (m *MockTaskRepository) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leaseTaskCalls++

	if m.leaseTaskErr != nil {
		return nil, m.leaseTaskErr
	}

	// Find highest priority queued task. Lower numeric priority wins.
	var bestTask *persistence.Task
	for _, task := range m.tasks {
		if task.Status != persistence.TaskStatusQueued {
			continue
		}
		if task.LeaseID != nil {
			continue
		}
		if limit := opts.ProjectConcurrencyLimits[task.ProjectID]; limit > 0 {
			active := 0
			for _, other := range m.tasks {
				if other.ProjectID != task.ProjectID {
					continue
				}
				switch other.Status {
				case persistence.TaskStatusLeased, persistence.TaskStatusRunning, persistence.TaskStatusWaitingForChildren:
					active++
				case persistence.TaskStatusQueued:
					if other.LeaseID != nil {
						active++
					}
				}
			}
			if active >= limit {
				continue
			}
		}
		if opts.PriorityFloor > 0 && task.Priority < opts.PriorityFloor {
			continue
		}
		if opts.ProjectID != "" && task.ProjectID != opts.ProjectID {
			continue
		}
		if bestTask == nil || task.Priority < bestTask.Priority ||
			(task.Priority == bestTask.Priority && taskQueueTime(task).Before(taskQueueTime(bestTask))) {
			bestTask = task
		}
	}

	if bestTask == nil {
		return nil, persistence.ErrNoTasksAvailable
	}

	// Mark as leased
	now := time.Now()
	leaseID := "lease-" + bestTask.ID
	bestTask.Status = persistence.TaskStatusLeased
	bestTask.LeaseID = &leaseID
	bestTask.LeasedAt = &now
	bestTask.LeasedBy = &opts.LeaseHolder
	expiry := now.Add(time.Duration(opts.LeaseDurationSeconds) * time.Second)
	bestTask.LeaseExpiresAt = &expiry

	m.leasedTasks = append(m.leasedTasks, cloneTask(bestTask))
	return cloneTask(bestTask), nil
}

// taskQueueTime returns the field used as the lease tiebreaker
// when two queued tasks share a priority. Mirrors the postgres
// LeaseTask query, which orders by (priority ASC, created_at ASC)
// — using updated_at would let a recently-retried task jump ahead
// of an older queued sibling, breaking the FIFO contract operators
// rely on when chaining tasks with input-artifact dependencies.
func taskQueueTime(task *persistence.Task) time.Time {
	if task == nil {
		return time.Time{}
	}
	return task.CreatedAt
}

func (m *MockTaskRepository) RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renewLeaseCalls++

	task, ok := m.tasks[taskID]
	if !ok {
		return persistence.ErrNotFound
	}
	if task.LeaseID == nil || *task.LeaseID != leaseID {
		return persistence.ErrLeaseNotFound
	}
	expiry := time.Now().Add(time.Duration(extendBySeconds) * time.Second)
	task.LeaseExpiresAt = &expiry
	return nil
}

func (m *MockTaskRepository) UpdateStatus(ctx context.Context, taskID string, status persistence.TaskStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return persistence.ErrNotFound
	}
	task.Status = status
	return nil
}

func (m *MockTaskRepository) ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseLeaseCalls++

	if m.releaseLeaseErr != nil {
		return m.releaseLeaseErr
	}

	task, ok := m.tasks[taskID]
	if !ok {
		return persistence.ErrNotFound
	}

	task.Status = newStatus
	task.LeaseID = nil
	task.LeasedAt = nil
	task.LeasedBy = nil
	task.LeaseExpiresAt = nil
	if opts.Attempt > 0 {
		task.Attempt = opts.Attempt
	}
	if opts.Error != "" {
		task.LastError = &opts.Error
	}

	return nil
}

func (m *MockTaskRepository) FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.findExpiredCalls++

	if m.findExpiredErr != nil {
		return nil, m.findExpiredErr
	}

	result := make([]*persistence.Task, 0, len(m.expiredLeases))
	for i := 0; i < limit && i < len(m.expiredLeases); i++ {
		result = append(result, m.expiredLeases[i])
	}
	m.expiredLeases = m.expiredLeases[len(result):]
	return result, nil
}

func (m *MockTaskRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.countByStatusCalls++

	counts := make(map[persistence.TaskStatus]int64)
	for _, task := range m.tasks {
		if projectID == "" || task.ProjectID == projectID {
			counts[task.Status]++
		}
	}
	return counts, nil
}

func (m *MockTaskRepository) CountRecentFailures(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error) {
	return 0, nil
}

// Helper to add a task
func (m *MockTaskRepository) AddTask(task *persistence.Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[task.ID] = task
}

// Helper to add an expired lease for recovery testing
func (m *MockTaskRepository) AddExpiredLease(task *persistence.Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task.Status = persistence.TaskStatusLeased
	m.expiredLeases = append(m.expiredLeases, task)
	m.tasks[task.ID] = task
}

func cloneTask(task *persistence.Task) *persistence.Task {
	if task == nil {
		return nil
	}
	cloned := *task
	if task.Payload != nil {
		cloned.Payload = append([]byte(nil), task.Payload...)
	}
	if task.Dependencies != nil {
		cloned.Dependencies = append([]string(nil), task.Dependencies...)
	}
	if task.LeaseID != nil {
		v := *task.LeaseID
		cloned.LeaseID = &v
	}
	if task.LeasedAt != nil {
		v := *task.LeasedAt
		cloned.LeasedAt = &v
	}
	if task.LeasedBy != nil {
		v := *task.LeasedBy
		cloned.LeasedBy = &v
	}
	if task.LeaseExpiresAt != nil {
		v := *task.LeaseExpiresAt
		cloned.LeaseExpiresAt = &v
	}
	if task.LastError != nil {
		v := *task.LastError
		cloned.LastError = &v
	}
	if task.LastErrorClass != nil {
		v := *task.LastErrorClass
		cloned.LastErrorClass = &v
	}
	if task.WorkflowID != nil {
		v := *task.WorkflowID
		cloned.WorkflowID = &v
	}
	if task.IdempotencyKey != nil {
		v := *task.IdempotencyKey
		cloned.IdempotencyKey = &v
	}
	if task.ParentTaskID != nil {
		v := *task.ParentTaskID
		cloned.ParentTaskID = &v
	}
	if task.DelegationMode != nil {
		v := *task.DelegationMode
		cloned.DelegationMode = &v
	}
	return &cloned
}

// Test Priority Ordering: lower numeric priority tasks should be leased first.
func TestScheduler_PriorityOrdering(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	// Add tasks with different priorities
	now := time.Now()
	taskLow := &persistence.Task{
		ID:        "task-priority-1",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  1,
		CreatedAt: now,
	}
	taskHigh := &persistence.Task{
		ID:        "task-priority-10",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  10,
		CreatedAt: now,
	}
	taskMedium := &persistence.Task{
		ID:        "task-priority-5",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  5,
		CreatedAt: now,
	}

	mockRepo.AddTask(taskLow)
	mockRepo.AddTask(taskHigh)
	mockRepo.AddTask(taskMedium)

	config := &Config{
		MaxConcurrency:       1,
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour, // Don't interfere with this test
	}
	scheduler := New(mockRepo, config)

	// Directly call leaseTask (which schedule uses internally)
	task, err := scheduler.leaseTask()
	require.NoError(t, err)
	assert.Equal(t, "task-priority-1", task.ID, "lowest numeric priority task should be leased first")
}

// TestScheduler_LeaseObservesQueueWait — queue residency (creation→lease)
// is now owned by the scheduler. Regression guard for the metric-ownership
// fix: the old queue.Lease() that used to emit vornik_queue_latency_seconds
// was dead (never called), so queue residency went unobserved. leaseTask()
// must now record it on vornik_scheduler_queue_wait_seconds.
func TestScheduler_LeaseObservesQueueWait(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	mockRepo.AddTask(&persistence.Task{
		ID:        "task-qw-1",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  5,
		CreatedAt: time.Now().Add(-3 * time.Second),
	})

	m := NewMetrics(prometheus.NewRegistry())
	config := &Config{
		MaxConcurrency:       1,
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
	}
	scheduler := NewWithOptions(mockRepo, config, WithMetrics(m))

	task, err := scheduler.leaseTask()
	require.NoError(t, err)
	require.Equal(t, "task-qw-1", task.ID)

	require.Equal(t, 1, testutil.CollectAndCount(m.QueueWaitSeconds),
		"leaseTask must observe queue residency on vornik_scheduler_queue_wait_seconds")
}

// TestScheduler_FIFOWithinSamePriority — operators chaining tasks
// that depend on prior tasks' output artifacts rely on submission
// order being preserved within a project. With per-project
// concurrency=1 (the standard config) and the lease query's
// (priority ASC, created_at ASC) ordering, two tasks at the same
// priority are leased in creation-time order regardless of their
// updated_at values.
//
// Regression guard: the prior implementation tiebroke on
// updated_at, which gets bumped by retries and status changes —
// causing a recently-retried task to jump ahead of an older
// queued sibling and break the dependency chain.
func TestScheduler_FIFOWithinSamePriority(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	first := &persistence.Task{
		ID:        "task-A-first",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  50,
		CreatedAt: now,
		// Older created_at, but RECENT updated_at — simulates
		// a task that was retried after originally being queued.
		// Under the old ordering this task would lose to a fresh
		// sibling created later; under FIFO it correctly wins.
		UpdatedAt: now.Add(5 * time.Minute),
	}
	second := &persistence.Task{
		ID:        "task-B-second",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  50,
		CreatedAt: now.Add(time.Minute),
		UpdatedAt: now.Add(time.Minute),
	}
	mockRepo.AddTask(second) // insert in REVERSE order to verify
	mockRepo.AddTask(first)  // ordering is by created_at, not insert order

	config := &Config{
		MaxConcurrency:       1,
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
	}
	s := New(mockRepo, config)

	leased, err := s.leaseTask()
	require.NoError(t, err)
	assert.Equal(t, "task-A-first", leased.ID,
		"earlier created_at must lease first within the same priority — FIFO is the dependency-chain contract")
}

// Test Concurrency Limit Enforcement - test schedule() directly
func TestScheduler_ConcurrencyLimit(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	// Add multiple tasks
	now := time.Now()
	for i := 0; i < 5; i++ {
		task := &persistence.Task{
			ID:        string(rune('a' + i)),
			ProjectID: "project-1",
			Status:    persistence.TaskStatusQueued,
			Priority:  i,
			CreatedAt: now,
		}
		mockRepo.AddTask(task)
	}

	config := &Config{
		MaxConcurrency:       2, // Only 2 concurrent tasks
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
	}
	// Wire a BLOCKING executor so the dispatched task stays running for the
	// duration of the schedule() pass. Without it (nil executor), dispatchTask
	// completes the task instantly, freeing the slot mid-pass — so schedule()'s
	// capacity-fill loop leases a second task and the cumulative leasedTasks
	// count flakes to 2 even though concurrency never exceeds MaxConcurrency
	// (CI -race caught this intermittently). Blocking keeps runningCount pinned
	// at MaxConcurrency, so the pass leases exactly the one free slot.
	exec := newBlockingExecutor(mockRepo)
	scheduler := NewWithOptions(mockRepo, config, WithExecutor(exec))

	// Set runningCount to 1 to simulate one already-running task
	scheduler.mu.Lock()
	scheduler.runningCount = 1
	scheduler.mu.Unlock()

	// Call schedule() directly - should only lease 1 more (capacity = 2 - 1 = 1)
	scheduler.schedule()

	mockRepo.mu.Lock()
	leasedCount := len(mockRepo.leasedTasks)
	mockRepo.mu.Unlock()

	// Should have leased exactly 1 task (not more) due to concurrency limit
	assert.LessOrEqual(t, leasedCount, 1, "should not exceed remaining capacity")

	// Unblock the dispatched task and let the dispatch goroutine drain so the
	// test doesn't leak it or race on mockRepo after return.
	close(exec.done)
	scheduler.dispatchWg.Wait()
}

type blockingExecutor struct {
	// done is the test's signal to stop blocking the executor.
	done chan struct{}
	// completed is closed AFTER the status update is persisted —
	// IsExecuting keys off this rather than `done` so the
	// scheduler's post-execution status read is guaranteed to see
	// the COMPLETED status. With a single channel, the scheduler
	// could race between IsExecuting=false (done closed) and the
	// status-update goroutine actually writing — passing
	// intermittently and producing dispatchFailed when it lost.
	completed chan struct{}
	repo      *MockTaskRepository
}

func newBlockingExecutor(repo *MockTaskRepository) *blockingExecutor {
	return &blockingExecutor{
		done:      make(chan struct{}),
		completed: make(chan struct{}),
		repo:      repo,
	}
}

func (b *blockingExecutor) ExecuteWithContext(ctx context.Context, taskID string) error {
	go func() {
		<-b.done
		if b.repo != nil {
			_ = b.repo.UpdateStatus(ctx, taskID, persistence.TaskStatusCompleted)
		}
		// Signal completion AFTER the status update so the
		// scheduler's IsExecuting==false → repo.Get sequence
		// observes the COMPLETED status, mirroring the production
		// contract where a real executor finalises the task before
		// becoming idle.
		close(b.completed)
	}()
	return nil
}

func (b *blockingExecutor) IsExecuting(taskID string) bool {
	select {
	case <-b.completed:
		return false
	default:
		return true
	}
}

func (b *blockingExecutor) Cancel(taskID string) error {
	// Tests that need cancel-tracking can override; default no-op so
	// the renewal-failure escalation path can call this safely.
	return nil
}

type mockExecutionRepository struct {
	mu         sync.Mutex
	executions map[string]*persistence.Execution
}

func (m *mockExecutionRepository) Create(ctx context.Context, execution *persistence.Execution) error {
	return nil
}

func (m *mockExecutionRepository) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.executions == nil {
		return nil, persistence.ErrNotFound
	}
	exec, ok := m.executions[taskID]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return exec, nil
}

func (m *mockExecutionRepository) UpdateStatus(ctx context.Context, id string, status persistence.ExecutionStatus) error {
	return nil
}

func (m *mockExecutionRepository) RecordCompletion(ctx context.Context, id string, result []byte) error {
	return nil
}

func (m *mockExecutionRepository) RecordFailure(ctx context.Context, id string, errorMessage, errorCode string) error {
	return nil
}

func (m *mockExecutionRepository) SetWorkflowSnapshot(ctx context.Context, id string, snapshot []byte) error {
	return nil
}

func (m *mockExecutionRepository) GetWorkflowSnapshot(ctx context.Context, id string) ([]byte, error) {
	return nil, nil
}

func TestScheduler_DispatchViaExecutor_RenewsLease(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	now := time.Now()
	leaseID := "lease-task-1"
	expires := now.Add(200 * time.Millisecond)
	task := &persistence.Task{
		ID:             "task-1",
		ProjectID:      "project-1",
		Status:         persistence.TaskStatusLeased,
		Priority:       10,
		LeaseID:        &leaseID,
		LeaseExpiresAt: &expires,
		CreatedAt:      now,
	}
	mockRepo.AddTask(task)

	s := New(mockRepo, &Config{
		LeaseDurationSeconds: 1,
	})
	exec := newBlockingExecutor(mockRepo)
	s.executor = exec

	resultCh := make(chan dispatchOutcome, 1)
	go func() {
		outcome, _ := s.dispatchViaExecutor(context.Background(), task)
		resultCh <- outcome
	}()

	// Wait for the first lease renewal rather than racing a fixed sleep.
	// With LeaseDurationSeconds=1 the first jittered renewal lands in
	// [375ms, 625ms] and is only observed on a 100ms ticker, so a fixed
	// wait sits near the worst case — and under `-race` + coverage the
	// dispatch goroutine can be starved well past it (a wall-clock flake,
	// not a bug). The executor stays blocked on exec.done, so renewals
	// keep firing until we observe one.
	renewed := func() int {
		mockRepo.mu.Lock()
		defer mockRepo.mu.Unlock()
		return mockRepo.renewLeaseCalls
	}
	waitFor(t, 5*time.Second, func() bool { return renewed() > 0 })
	close(exec.done)

	outcome := <-resultCh
	assert.Equal(t, dispatchSucceeded, outcome)

	assert.Greater(t, renewed(), 0, "expected lease to be renewed while executor was still running")
}

// Test Empty Queue Handling
func TestScheduler_EmptyQueue(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	config := &Config{
		MaxConcurrency:       10,
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
	}
	scheduler := New(mockRepo, config)

	// Should return ErrNoTasksAvailable when queue is empty
	task, err := scheduler.leaseTask()
	assert.Nil(t, task)
	assert.ErrorIs(t, err, persistence.ErrNoTasksAvailable)

	// Starting with empty queue should not panic
	err = scheduler.Start()
	require.NoError(t, err)

	// Let it run for a few cycles
	time.Sleep(50 * time.Millisecond)

	assert.NoError(t, scheduler.Stop())
}

// Test Lease Expiry Recovery
func TestScheduler_LeaseExpiryRecovery(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	// Add a task with an expired lease
	now := time.Now()
	task := &persistence.Task{
		ID:        "expired-task",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusLeased,
		Priority:  5,
		Attempt:   1,
		CreatedAt: now,
	}
	mockRepo.AddExpiredLease(task)

	config := &Config{
		MaxConcurrency:       10,
		LeaseDurationSeconds: 60,
		PollInterval:         100 * time.Millisecond,
		RecoveryInterval:     50 * time.Millisecond, // Fast for testing
		RecoveryBatchSize:    10,
	}
	scheduler := New(mockRepo, config)

	err := scheduler.Start()
	require.NoError(t, err)

	// Wait for the recovery cycle rather than racing a fixed sleep. The
	// 50ms RecoveryInterval normally fires well within ~150ms, but under
	// `-race` + coverage instrumentation (full-suite CI run) the recovery
	// goroutine can be starved past any fixed wait, leaving the counters
	// at 0 — a wall-clock flake, not a bug.
	counts := func() (int, int) {
		mockRepo.mu.Lock()
		defer mockRepo.mu.Unlock()
		return mockRepo.findExpiredCalls, mockRepo.releaseLeaseCalls
	}
	waitFor(t, 5*time.Second, func() bool { f, r := counts(); return f > 0 && r > 0 })

	err = scheduler.Stop()
	assert.NoError(t, err)

	// Verify recovery was called.
	findCalls, releaseCalls := counts()
	assert.Greater(t, findCalls, 0, "FindExpiredLeases should have been called")
	assert.Greater(t, releaseCalls, 0, "ReleaseLease should have been called for recovery")
}

func TestScheduler_LeaseExpiryRecovery_UsesObservedLeaseAndFailsPastMaxAttempts(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	leaseID := "lease-expired-task"
	now := time.Now()
	task := &persistence.Task{
		ID:          "expired-task",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusLeased,
		Priority:    5,
		Attempt:     3,
		MaxAttempts: 3,
		LeaseID:     &leaseID,
		CreatedAt:   now,
	}
	mockRepo.AddExpiredLease(task)

	scheduler := New(mockRepo, &Config{RecoveryBatchSize: 10})
	scheduler.recoverExpiredLeases()

	mockRepo.mu.Lock()
	defer mockRepo.mu.Unlock()
	require.Equal(t, 1, mockRepo.releaseLeaseCalls)
	assert.Equal(t, persistence.TaskStatusFailed, mockRepo.tasks["expired-task"].Status)
	assert.Equal(t, 3, mockRepo.tasks["expired-task"].Attempt)
	require.NotNil(t, mockRepo.tasks["expired-task"].LastError)
	assert.Contains(t, *mockRepo.tasks["expired-task"].LastError, "lease expired")
}

func TestScheduler_LeaseExpiryRecovery_RenewsActiveExecutorLease(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	leaseID := "lease-active-task"
	now := time.Now()
	task := &persistence.Task{
		ID:             "active-task",
		ProjectID:      "project-1",
		Status:         persistence.TaskStatusRunning,
		Priority:       5,
		Attempt:        1,
		MaxAttempts:    3,
		LeaseID:        &leaseID,
		LeaseExpiresAt: ptrTime(now.Add(-time.Minute)),
		CreatedAt:      now,
	}
	mockRepo.AddExpiredLease(task)

	exec := newBlockingExecutor(mockRepo)
	scheduler := NewWithOptions(mockRepo, &Config{RecoveryBatchSize: 10, LeaseDurationSeconds: 60}, WithExecutor(exec))
	scheduler.recoverExpiredLeases()

	mockRepo.mu.Lock()
	defer mockRepo.mu.Unlock()
	assert.Equal(t, 1, mockRepo.renewLeaseCalls)
	assert.Equal(t, 0, mockRepo.releaseLeaseCalls)
	assert.Equal(t, persistence.TaskStatusLeased, mockRepo.tasks["active-task"].Status)
}

func TestScheduler_ReleaseExecutorLease_InterruptedRequeuesWithoutAttemptBurn(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	leaseID := "lease-task-1"
	task := &persistence.Task{
		ID:          "task-1",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusRunning,
		LeaseID:     &leaseID,
		Attempt:     2,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	mockRepo.AddTask(task)

	scheduler := New(mockRepo, DefaultConfig())
	scheduler.runningCount = 1
	err := scheduler.releaseExecutorLease(task, dispatchInterrupted, context.Canceled.Error())
	require.NoError(t, err)

	mockRepo.mu.Lock()
	defer mockRepo.mu.Unlock()
	assert.Equal(t, 0, scheduler.RunningCount())
	assert.Equal(t, persistence.TaskStatusQueued, mockRepo.tasks["task-1"].Status)
	assert.Equal(t, 2, mockRepo.tasks["task-1"].Attempt)
	require.NotNil(t, mockRepo.tasks["task-1"].LastError)
	assert.Contains(t, *mockRepo.tasks["task-1"].LastError, "context canceled")
}

func TestScheduler_ReleaseExecutorLease_InterruptedSkipsPausedExecution(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	leaseID := "lease-task-1"
	task := &persistence.Task{
		ID:          "task-1",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusRunning,
		LeaseID:     &leaseID,
		Attempt:     2,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	mockRepo.AddTask(task)

	execRepo := &mockExecutionRepository{
		executions: map[string]*persistence.Execution{
			"task-1": {
				ID:     "exec-1",
				TaskID: "task-1",
				Status: persistence.ExecutionStatusPaused,
			},
		},
	}

	scheduler := NewWithOptions(mockRepo, DefaultConfig(), WithExecutionRepository(execRepo))
	scheduler.runningCount = 1
	err := scheduler.releaseExecutorLease(task, dispatchInterrupted, context.Canceled.Error())
	require.NoError(t, err)

	mockRepo.mu.Lock()
	defer mockRepo.mu.Unlock()
	assert.Equal(t, 0, scheduler.RunningCount())
	assert.Equal(t, persistence.TaskStatusRunning, mockRepo.tasks["task-1"].Status)
	assert.Equal(t, 0, mockRepo.releaseLeaseCalls)
}

func TestScheduler_ReleaseExecutorLease_PausedPreservesTaskStatus(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	leaseID := "lease-task-1"
	task := &persistence.Task{
		ID:          "task-1",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusWaitingForChildren,
		LeaseID:     &leaseID,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	mockRepo.AddTask(task)

	scheduler := New(mockRepo, DefaultConfig())
	scheduler.runningCount = 1

	err := scheduler.releaseExecutorLease(task, dispatchPaused, "")
	require.NoError(t, err)

	mockRepo.mu.Lock()
	defer mockRepo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusWaitingForChildren, mockRepo.tasks["task-1"].Status)
	assert.Nil(t, mockRepo.tasks["task-1"].LeaseID)
	assert.Equal(t, 0, scheduler.RunningCount())
}

// Test Start/Stop lifecycle
func TestScheduler_StartStop(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	config := DefaultConfig()
	scheduler := New(mockRepo, config)

	// Initially not started
	assert.False(t, scheduler.IsStarted())

	// Start
	err := scheduler.Start()
	require.NoError(t, err)
	assert.True(t, scheduler.IsStarted())

	// Double start should error
	err = scheduler.Start()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already started")

	// Stop
	err = scheduler.Stop()
	assert.NoError(t, err)
	assert.False(t, scheduler.IsStarted())

	// Double stop is a no-op (no error)
	err = scheduler.Stop()
	assert.NoError(t, err)
}

// Test TaskCompleted updates running count
func TestScheduler_TaskCompleted(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusLeased,
		Priority:  5,
		CreatedAt: now,
	}
	mockRepo.AddTask(task)

	config := DefaultConfig()
	scheduler := New(mockRepo, config)

	// Manually increment running count (simulating a running task)
	scheduler.mu.Lock()
	scheduler.runningCount = 1
	scheduler.mu.Unlock()

	assert.Equal(t, 1, scheduler.RunningCount())

	// Complete the task
	err := scheduler.TaskCompleted("task-1", "lease-1", true, "")
	require.NoError(t, err)

	assert.Equal(t, 0, scheduler.RunningCount())

	// Check task status was updated
	mockRepo.mu.Lock()
	updatedTask := mockRepo.tasks["task-1"]
	mockRepo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusCompleted, updatedTask.Status)
}

// Test TaskCompleted with failure
func TestScheduler_TaskCompletedFailure(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusLeased,
		Priority:  5,
		CreatedAt: now,
	}
	mockRepo.AddTask(task)

	config := DefaultConfig()
	scheduler := New(mockRepo, config)

	scheduler.mu.Lock()
	scheduler.runningCount = 1
	scheduler.mu.Unlock()

	// Complete with failure
	err := scheduler.TaskCompleted("task-1", "lease-1", false, "something went wrong")
	require.NoError(t, err)

	mockRepo.mu.Lock()
	updatedTask := mockRepo.tasks["task-1"]
	mockRepo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusFailed, updatedTask.Status)
	assert.NotNil(t, updatedTask.LastError)
	assert.Equal(t, "something went wrong", *updatedTask.LastError)
}

// Test Priority Floor
func TestScheduler_PriorityFloor(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	taskLow := &persistence.Task{
		ID:        "task-low",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  1,
		CreatedAt: now,
	}
	taskHigh := &persistence.Task{
		ID:        "task-high",
		ProjectID: "project-1",
		Status:    persistence.TaskStatusQueued,
		Priority:  10,
		CreatedAt: now,
	}

	mockRepo.AddTask(taskLow)
	mockRepo.AddTask(taskHigh)

	config := &Config{
		MaxConcurrency:       10,
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
		PriorityFloor:        5, // Only tasks with priority >= 5
	}
	scheduler := New(mockRepo, config)

	task, err := scheduler.leaseTask()
	require.NoError(t, err)
	assert.Equal(t, "task-high", task.ID, "should only lease tasks above priority floor")
}

// Test Project Filtering via LeaseOptions
func TestScheduler_ProjectFiltering(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	taskA := &persistence.Task{
		ID:        "task-a",
		ProjectID: "project-a",
		Status:    persistence.TaskStatusQueued,
		Priority:  5,
		CreatedAt: now,
	}
	taskB := &persistence.Task{
		ID:        "task-b",
		ProjectID: "project-b",
		Status:    persistence.TaskStatusQueued,
		Priority:  10, // Higher priority but wrong project
		CreatedAt: now,
	}

	mockRepo.AddTask(taskA)
	mockRepo.AddTask(taskB)

	// Lease with project filter - test the mock directly
	leaseOpts := persistence.LeaseOptions{
		ProjectID:            "project-a",
		LeaseHolder:          "test",
		LeaseDurationSeconds: 60,
	}

	task, err := mockRepo.LeaseTask(context.Background(), leaseOpts)
	require.NoError(t, err)
	assert.Equal(t, "task-a", task.ID, "should only lease tasks from specified project")
}

type staticProjectLimits struct {
	limits map[string]int
}

func (s staticProjectLimits) ProjectConcurrencyLimits() map[string]int {
	return s.limits
}

// ProjectPriorities returns nil — the existing tests don't exercise
// the priority sort key. Adding it here keeps the test fixture
// compatible with the expanded ProjectRegistry interface.
func (s staticProjectLimits) ProjectPriorities() map[string]int {
	return nil
}

// ArchivedProjectIDs returns nil — these fixtures don't exercise
// the archived-project hard-guard either. Real wiring lives in
// the registry layer.
func (s staticProjectLimits) ArchivedProjectIDs() []string {
	return nil
}

func TestScheduler_ProjectConcurrencyCountsWaitingForChildren(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	now := time.Now()
	mockRepo.AddTask(&persistence.Task{
		ID:        "parent",
		ProjectID: "janka",
		Status:    persistence.TaskStatusWaitingForChildren,
		Priority:  100,
		CreatedAt: now,
	})
	mockRepo.AddTask(&persistence.Task{
		ID:        "queued-janka",
		ProjectID: "janka",
		Status:    persistence.TaskStatusQueued,
		Priority:  100,
		CreatedAt: now,
	})
	mockRepo.AddTask(&persistence.Task{
		ID:        "queued-other",
		ProjectID: "other",
		Status:    persistence.TaskStatusQueued,
		Priority:  10,
		CreatedAt: now,
	})

	scheduler := NewWithOptions(mockRepo, DefaultConfig(), WithProjectRegistry(staticProjectLimits{
		limits: map[string]int{"janka": 1},
	}))

	task, err := scheduler.leaseTask()
	require.NoError(t, err)
	assert.Equal(t, "queued-other", task.ID, "janka should be blocked by its active waiting parent")
}

func TestScheduler_ProjectConcurrencySkipsSecondHigherPriorityTaskInSameProject(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	now := time.Now()
	mockRepo.AddTask(&persistence.Task{
		ID:        "janka-first",
		ProjectID: "janka",
		Status:    persistence.TaskStatusQueued,
		Priority:  1,
		CreatedAt: now,
	})
	mockRepo.AddTask(&persistence.Task{
		ID:        "janka-second",
		ProjectID: "janka",
		Status:    persistence.TaskStatusQueued,
		Priority:  1,
		CreatedAt: now.Add(time.Second),
	})
	mockRepo.AddTask(&persistence.Task{
		ID:        "assistant-work",
		ProjectID: "assistant",
		Status:    persistence.TaskStatusQueued,
		Priority:  15,
		CreatedAt: now,
	})

	scheduler := NewWithOptions(mockRepo, DefaultConfig(), WithProjectRegistry(staticProjectLimits{
		limits: map[string]int{"janka": 1, "assistant": 1},
	}))

	first, err := scheduler.leaseTask()
	require.NoError(t, err)
	assert.Equal(t, "janka-first", first.ID)

	second, err := scheduler.leaseTask()
	require.NoError(t, err)
	assert.Equal(t, "assistant-work", second.ID, "same-project priority-1 task should wait behind project concurrency limit")
}

// Test UnlimitedConcurrency - schedule() with MaxConcurrency=0
func TestScheduler_UnlimitedConcurrency(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	for i := 0; i < 3; i++ {
		task := &persistence.Task{
			ID:        string(rune('a' + i)),
			ProjectID: "project-1",
			Status:    persistence.TaskStatusQueued,
			Priority:  i,
			CreatedAt: now,
		}
		mockRepo.AddTask(task)
	}

	config := &Config{
		MaxConcurrency:       0, // Unlimited
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
	}
	scheduler := New(mockRepo, config)

	// With unlimited concurrency, schedule() should lease all available tasks
	scheduler.schedule()

	mockRepo.mu.Lock()
	leasedCount := len(mockRepo.leasedTasks)
	mockRepo.mu.Unlock()

	// Should have leased at least 1 task (all of them, since concurrency is unlimited)
	assert.GreaterOrEqual(t, leasedCount, 1, "should lease at least one task with unlimited concurrency")
}

// Test Error Handling - LeaseTask error
func TestScheduler_LeaseTaskError(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	mockRepo.leaseTaskErr = errors.New("database connection error")

	config := DefaultConfig()
	scheduler := New(mockRepo, config)

	task, err := scheduler.leaseTask()
	assert.Nil(t, task)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "database connection error")
}

// Test Error Handling - FindExpiredLeases error
func TestScheduler_FindExpiredLeasesError(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	mockRepo.findExpiredErr = errors.New("query error")

	config := &Config{
		MaxConcurrency:       10,
		LeaseDurationSeconds: 60,
		PollInterval:         100 * time.Millisecond,
		RecoveryInterval:     50 * time.Millisecond,
	}
	scheduler := New(mockRepo, config)

	// Should not panic on error
	err := scheduler.Start()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	// Stop should succeed even with errors
	err = scheduler.Stop()
	assert.NoError(t, err)
}

// Test Default Config
func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	assert.Equal(t, 10, config.MaxConcurrency)
	assert.Equal(t, 300, config.LeaseDurationSeconds)
	assert.Equal(t, 1*time.Second, config.PollInterval)
	assert.Equal(t, 30*time.Second, config.RecoveryInterval)
	assert.Equal(t, 100, config.RecoveryBatchSize)
	assert.Equal(t, 0, config.PriorityFloor)
}

// Test New with nil config uses defaults
func TestNew_NilConfig(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	scheduler := New(mockRepo, nil)

	assert.NotNil(t, scheduler)
	assert.NotNil(t, scheduler.config)
	assert.Equal(t, 10, scheduler.config.MaxConcurrency)
}

func TestNew_NormalizesInvalidIntervals(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	cfg := &Config{}
	scheduler := New(mockRepo, cfg)

	assert.Equal(t, 300, scheduler.config.LeaseDurationSeconds)
	assert.Equal(t, time.Second, scheduler.config.PollInterval)
	assert.Equal(t, 30*time.Second, scheduler.config.RecoveryInterval)
	assert.Equal(t, 100, scheduler.config.RecoveryBatchSize)
	assert.Equal(t, 30*time.Minute, scheduler.config.ExecutionTimeout)
}

func TestScheduler_TaskCompletedDoesNotUnderflowRunningCount(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	mockRepo.AddTask(&persistence.Task{
		ID:          "task-underflow",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
	})
	scheduler := New(mockRepo, DefaultConfig())

	require.NoError(t, scheduler.TaskCompleted("task-underflow", "lease-1", false, "failed"))
	assert.Equal(t, 0, scheduler.RunningCount())
}

// Test TaskCompleted re-queues a failed task when retries remain
func TestScheduler_TaskCompletedWithRetry(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	task := &persistence.Task{
		ID:          "task-retry",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   now,
	}
	mockRepo.AddTask(task)

	config := DefaultConfig()
	scheduler := New(mockRepo, config)

	scheduler.mu.Lock()
	scheduler.runningCount = 1
	scheduler.mu.Unlock()

	err := scheduler.TaskCompleted("task-retry", "lease-1", false, "transient error")
	require.NoError(t, err)

	mockRepo.mu.Lock()
	updatedTask := mockRepo.tasks["task-retry"]
	mockRepo.mu.Unlock()

	assert.Equal(t, persistence.TaskStatusQueued, updatedTask.Status)
	assert.Equal(t, 2, updatedTask.Attempt)
	require.NotNil(t, updatedTask.LastError)
	assert.Equal(t, "transient error", *updatedTask.LastError)
}

// Test TaskCompleted fails the task when all retries are exhausted
func TestScheduler_TaskCompletedRetriesExhausted(t *testing.T) {
	mockRepo := NewMockTaskRepository()

	now := time.Now()
	task := &persistence.Task{
		ID:          "task-exhausted",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     3,
		MaxAttempts: 3,
		CreatedAt:   now,
	}
	mockRepo.AddTask(task)

	config := DefaultConfig()
	scheduler := New(mockRepo, config)

	scheduler.mu.Lock()
	scheduler.runningCount = 1
	scheduler.mu.Unlock()

	err := scheduler.TaskCompleted("task-exhausted", "lease-1", false, "final error")
	require.NoError(t, err)

	mockRepo.mu.Lock()
	updatedTask := mockRepo.tasks["task-exhausted"]
	mockRepo.mu.Unlock()

	assert.Equal(t, persistence.TaskStatusFailed, updatedTask.Status)
	require.NotNil(t, updatedTask.LastError)
	assert.Equal(t, "final error", *updatedTask.LastError)
}

func TestScheduler_TaskCompleted_NilMetrics(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	task := &persistence.Task{
		ID:          "task-nm",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	mockRepo.AddTask(task)

	config := DefaultConfig()
	s := New(mockRepo, config)
	// s.metrics is nil by default
	s.mu.Lock()
	s.runningCount = 1
	s.mu.Unlock()

	err := s.TaskCompleted("task-nm", "", true, "")
	require.NoError(t, err)
}

func TestScheduler_New_ValidConfig(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	config := DefaultConfig()
	s := New(mockRepo, config)
	assert.NotNil(t, s)
	assert.Equal(t, 0, s.runningCount)
}
