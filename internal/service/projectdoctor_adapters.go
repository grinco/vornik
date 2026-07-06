package service

import (
	"context"
	"fmt"
	"sync"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectdoctor"
	"vornik.io/vornik/internal/taskcreate"
)

// chatPinger adapts a daemon chat.Provider's Ping (chat.Pinger) into
// the doctor's ModelPinger. The ping closure is captured at
// construction so the adapter carries no chat-package types.
type chatPinger struct {
	ping func(ctx context.Context) error
}

func newChatPinger(ping func(ctx context.Context) error) projectdoctor.ModelPinger {
	return chatPinger{ping: ping}
}

func (c chatPinger) Ping(ctx context.Context) error {
	if c.ping == nil {
		return fmt.Errorf("no chat model configured")
	}
	return c.ping(ctx)
}

// doctorModelPinger builds the doctor's model probe from the daemon's
// live chat provider. Every provider family (http/router/cli/
// subscription) implements chat.Pinger, and the LoggingProvider/
// QueuedProvider wrappers forward Ping, so a single assertion covers
// all backends — unlike PingCompletion, which only the bare *chat.Client
// exposes (an earlier fix that built a fresh HTTP client from config
// missed the deployed provider: "router", false-blocking every
// project's /setup as "no chat model configured"). Ping is a
// reachability probe (Router.Ping hard-fails when the fallback
// sub-provider is down); that's the right granularity for a "can this
// project reach its model" readiness check. Returns nil when chat is
// disabled (no provider) → checkModel degrades to neutral.
func doctorModelPinger(provider chat.Provider) projectdoctor.ModelPinger {
	if provider == nil {
		return nil
	}
	if p, ok := provider.(chat.Pinger); ok {
		return newChatPinger(p.Ping)
	}
	return nil
}

// smokeRunner enqueues doctor smoke tasks and reports their latest
// state. It keeps the last smoke task id per project in memory
// (shared across the api + ui servers because a single instance is
// injected into both). A daemon restart resets smoke history to
// "not run", which is acceptable — smoke is a manual nicety, not
// durable state.
type smokeRunner struct {
	creator *taskcreate.Creator
	tasks   persistence.TaskRepository
	usage   persistence.TaskLLMUsageRepository

	mu   sync.Mutex
	last map[string]string // projectID -> taskID

	// trigMu serializes Trigger per project (projectID -> *sync.Mutex)
	// so two concurrent triggers for the same project can't both pass
	// the in-flight guard and double-create a task. Companion review
	// finding (2026-07-04).
	trigMu sync.Map
}

func newSmokeRunner(creator *taskcreate.Creator, tasks persistence.TaskRepository, usage persistence.TaskLLMUsageRepository) *smokeRunner {
	return &smokeRunner{creator: creator, tasks: tasks, usage: usage, last: map[string]string{}}
}

// smokeTaskType marks doctor smoke tasks so they're distinguishable
// in task history / cost reporting.
const smokeTaskType = "doctor-smoke"

func (s *smokeRunner) recordLast(projectID, taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last[projectID] = taskID
}

func (s *smokeRunner) lastID(projectID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.last[projectID]
	return id, ok
}

func (s *smokeRunner) Trigger(ctx context.Context, projectID, prompt string) (string, error) {
	if s.creator == nil {
		return "", fmt.Errorf("task creator unavailable")
	}
	// Serialize check-and-create per project so two concurrent triggers
	// can't both pass the in-flight guard and double-bill. Companion
	// review finding (2026-07-04).
	lkAny, _ := s.trigMu.LoadOrStore(projectID, &sync.Mutex{})
	lk := lkAny.(*sync.Mutex)
	lk.Lock()
	defer lk.Unlock()
	// Authoritative in-flight guard (the Doctor-level pre-check is a fast path).
	if st, ok := s.Latest(projectID); ok && st.Running {
		return st.TaskID, nil
	}
	task, err := s.creator.Create(ctx, taskcreate.Params{
		ProjectID:      projectID,
		TaskType:       smokeTaskType,
		Prompt:         prompt,
		CreationSource: persistence.TaskCreationSourceUser,
	})
	if err != nil {
		return "", err
	}
	s.recordLast(projectID, task.ID)
	return task.ID, nil
}

// Latest reports the state of the last-triggered smoke task for a
// project. Once a task id has been recorded, Latest always reports
// ok=true with TaskID populated — the task repository lookup only
// refines the Status/Detail/USD fields. This matters because the
// smoke runner may be constructed without a task repository wired
// (e.g. reduced-dependency test doubles), in which case "triggered
// but status unknown" is still meaningfully different from "never
// triggered".
func (s *smokeRunner) Latest(projectID string) (projectdoctor.SmokeStatus, bool) {
	id, ok := s.lastID(projectID)
	if !ok {
		return projectdoctor.SmokeStatus{}, false
	}
	st := projectdoctor.SmokeStatus{TaskID: id}

	if s.tasks == nil {
		st.Status = projectdoctor.StatusYellow
		st.Detail = "Smoke task status unavailable."
		st.Running = true
		return st, true
	}

	task, err := s.tasks.Get(context.Background(), id)
	if err != nil || task == nil {
		// Transient lookup failure (DB/network hiccup) is not the same
		// as "task is running": reporting Yellow+Running here was
		// indistinguishable from a real in-flight task and permanently
		// disabled the UI smoke button (it disables on Meta.running).
		// Report Unknown + not-running so the button re-enables and the
		// state reads honestly. Companion review finding (2026-07-04).
		st.Status = projectdoctor.StatusUnknown
		st.Detail = "Smoke task status temporarily unavailable — re-run to refresh."
		st.Running = false
		return st, true
	}

	switch task.Status {
	case persistence.TaskStatusCompleted:
		st.Status = projectdoctor.StatusGreen
		st.Detail = "Last smoke task completed."
	case persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
		st.Status = projectdoctor.StatusRed
		st.Detail = "Last smoke task " + string(task.Status) + "."
	default:
		st.Status = projectdoctor.StatusYellow
		st.Detail = "Smoke task " + string(task.Status) + "…"
		st.Running = true
	}
	st.USD = s.sumCost(id)
	return st, true
}

func (s *smokeRunner) sumCost(taskID string) float64 {
	if s.usage == nil {
		return 0
	}
	rows, err := s.usage.List(context.Background(), persistence.TaskLLMUsageFilter{TaskID: &taskID})
	if err != nil {
		return 0
	}
	var total float64
	for _, r := range rows {
		total += r.CostUSD
	}
	return total
}
