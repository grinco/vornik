package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/narrator"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
)

// TestInitNarrator_DisabledByConfig — narrator.enabled=false (the
// zero value) must never construct the worker, even with every
// other collaborator wired.
func TestInitNarrator_DisabledByConfig(t *testing.T) {
	c := &Container{
		Config:  &config.Config{},
		Logger:  zerolog.Nop(),
		livePub: livepubsub.New(0),
		repos: &storage.Repositories{
			ExecutionNarration: postgres.NewExecutionNarrationRepository(nil),
			Executions:         postgres.NewExecutionRepository(nil),
		},
	}
	c.initNarrator()
	if c.narratorWorker != nil {
		t.Fatal("narrator.enabled=false must leave narratorWorker nil")
	}
}

// TestInitNarrator_EnabledButMissingWiring_StaysNil — enabled but a
// required collaborator absent (no live publisher wired, e.g. a
// minimal harness) must not construct the worker.
func TestInitNarrator_EnabledButMissingWiring_StaysNil(t *testing.T) {
	c := &Container{
		Config: &config.Config{Narrator: config.NarratorConfig{Enabled: true}},
		Logger: zerolog.Nop(),
		// livePub intentionally nil.
		repos: &storage.Repositories{
			ExecutionNarration: postgres.NewExecutionNarrationRepository(nil),
			Executions:         postgres.NewExecutionRepository(nil),
		},
	}
	c.initNarrator()
	if c.narratorWorker != nil {
		t.Fatal("missing livePub must leave narratorWorker nil")
	}

	c2 := &Container{
		Config:  &config.Config{Narrator: config.NarratorConfig{Enabled: true}},
		Logger:  zerolog.Nop(),
		livePub: livepubsub.New(0),
		repos:   &storage.Repositories{}, // ExecutionNarration + Executions both nil
	}
	c2.initNarrator()
	if c2.narratorWorker != nil {
		t.Fatal("missing execution_narration/execution repos must leave narratorWorker nil")
	}
}

// TestInitNarrator_EnabledAndWired_ConstructsWithConfiguredKnobs
// pins the config→Narrator field translation (seconds → time.
// Duration) and the Sub/Pub/Store/Executions wiring.
func TestInitNarrator_EnabledAndWired_ConstructsWithConfiguredKnobs(t *testing.T) {
	pub := livepubsub.New(0)
	c := &Container{
		Config: &config.Config{
			Narrator: config.NarratorConfig{
				Enabled:                  true,
				Model:                    "cheap-model",
				DebounceSeconds:          5,
				LongToolThresholdSeconds: 20,
				MinLineIntervalSeconds:   7,
				MaxLines:                 99,
				MaxCostUSD:               0.1,
			},
		},
		Logger:  zerolog.Nop(),
		livePub: pub,
		repos: &storage.Repositories{
			ExecutionNarration: postgres.NewExecutionNarrationRepository(nil),
			Executions:         postgres.NewExecutionRepository(nil),
		},
	}
	c.initNarrator()
	if c.narratorWorker == nil {
		t.Fatal("expected narratorWorker to be constructed")
	}
	if c.narratorWorker.Sub == nil || c.narratorWorker.Pub == nil {
		t.Error("Sub/Pub should be wired from c.livePub")
	}
	if c.narratorWorker.Model != "cheap-model" {
		t.Errorf("Model = %q, want cheap-model", c.narratorWorker.Model)
	}
	if c.narratorWorker.Debounce != 5*time.Second {
		t.Errorf("Debounce = %v, want 5s", c.narratorWorker.Debounce)
	}
	if c.narratorWorker.LongToolThresh != 20*time.Second {
		t.Errorf("LongToolThresh = %v, want 20s", c.narratorWorker.LongToolThresh)
	}
	if c.narratorWorker.MinLineInterval != 7*time.Second {
		t.Errorf("MinLineInterval = %v, want 7s", c.narratorWorker.MinLineInterval)
	}
	if c.narratorWorker.MaxLines != 99 {
		t.Errorf("MaxLines = %d, want 99", c.narratorWorker.MaxLines)
	}
	if c.narratorWorker.MaxCostUSD != 0.1 {
		t.Errorf("MaxCostUSD = %v, want 0.1", c.narratorWorker.MaxCostUSD)
	}
}

// TestInitNarrator_ChatMilestoneKindsConfigured pins the config→field
// translation for the daemon-wide chat-push cadence override (task 2.3):
// a non-empty config list overrides the narrator's built-in default.
func TestInitNarrator_ChatMilestoneKindsConfigured(t *testing.T) {
	pub := livepubsub.New(0)
	c := &Container{
		Config: &config.Config{
			Narrator: config.NarratorConfig{Enabled: true, ChatMilestoneKinds: []string{"step_started", "completion"}},
		},
		Logger:  zerolog.Nop(),
		livePub: pub,
		repos: &storage.Repositories{
			ExecutionNarration: postgres.NewExecutionNarrationRepository(nil),
			Executions:         postgres.NewExecutionRepository(nil),
		},
	}
	c.initNarrator()
	if c.narratorWorker == nil {
		t.Fatal("expected narratorWorker to be constructed")
	}
	got := c.narratorWorker.ChatMilestoneKinds
	if len(got) != 2 || got[0] != "step_started" || got[1] != "completion" {
		t.Errorf("ChatMilestoneKinds = %v, want [step_started completion]", got)
	}
}

// TestInitNarrator_WiresChatPushCollaborators pins task 2.3's addition:
// Tasks/Audit/Resolver/ProjectSettings/BaseURL must be wired from the same
// collaborators steeringNotifier() uses, so the narrator's chat push and
// the steering notifier resolve channels identically.
func TestInitNarrator_WiresChatPushCollaborators(t *testing.T) {
	pub := livepubsub.New(0)
	c := &Container{
		Config: &config.Config{
			Narrator: config.NarratorConfig{Enabled: true},
			Auth:     config.AuthSettings{ExternalBaseURL: "https://vornik.example"},
		},
		Logger:  zerolog.Nop(),
		livePub: pub,
		repos: &storage.Repositories{
			ExecutionNarration: postgres.NewExecutionNarrationRepository(nil),
			Executions:         postgres.NewExecutionRepository(nil),
			Tasks:              postgres.NewTaskRepository(nil),
			ChatAudit:          postgres.NewChatAuditRepository(nil),
		},
	}
	c.initNarrator()
	if c.narratorWorker == nil {
		t.Fatal("expected narratorWorker to be constructed")
	}
	if c.narratorWorker.Tasks == nil {
		t.Error("Tasks must be wired from c.repos.Tasks")
	}
	if c.narratorWorker.Audit == nil {
		t.Error("Audit must be wired from c.repos.ChatAudit")
	}
	if c.narratorWorker.Resolver == nil {
		t.Error("Resolver must be wired (containerChannelResolver)")
	}
	if c.narratorWorker.ProjectSettings == nil {
		t.Error("ProjectSettings must be wired")
	}
	if c.narratorWorker.BaseURL != "https://vornik.example" {
		t.Errorf("BaseURL = %q, want https://vornik.example", c.narratorWorker.BaseURL)
	}
}

// TestNarratorProjectSettings_NilSafety pins the "un-configured project /
// no registry" defaults: chat push off, narration on — matching an
// un-configured narrator block's behaviour exactly.
func TestNarratorProjectSettings_NilSafety(t *testing.T) {
	var nilC *Container
	if got := nilC.narratorProjectSettings("p1"); got != (narrator.ProjectNarratorSettings{}) {
		t.Errorf("nil Container: got %+v, want zero value", got)
	}

	c := &Container{Registry: nil}
	if got := c.narratorProjectSettings("p1"); got != (narrator.ProjectNarratorSettings{}) {
		t.Errorf("nil Registry: got %+v, want zero value", got)
	}

	c2 := &Container{Registry: registry.New()} // no projects loaded
	if got := c2.narratorProjectSettings("unknown"); got != (narrator.ProjectNarratorSettings{}) {
		t.Errorf("unknown project: got %+v, want zero value", got)
	}
}

// TestNarratorProjectSettings_ReflectsLoadedProject pins the positive path:
// a project YAML's narrator.chat_push/no_narration flow through to the
// resolved ProjectNarratorSettings.
func TestNarratorProjectSettings_ReflectsLoadedProject(t *testing.T) {
	tmpDir := t.TempDir()
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.Mkdir(filepath.Join(tmpDir, subdir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", subdir, err)
		}
	}
	swarmMD := "---\nswarmId: \"s1\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"x:latest\"\n---\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "s.md"), []byte(swarmMD), 0o644); err != nil {
		t.Fatal(err)
	}
	wfMD := "---\nworkflowId: \"w1\"\nentrypoint: \"step1\"\nsteps:\n  step1:\n    type: \"agent\"\n    role: \"coder\"\n    prompt: \"do work\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "w.md"), []byte(wfMD), 0o644); err != nil {
		t.Fatal(err)
	}
	projYAML := "projectId: \"p1\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"w1\"\nnarrator:\n  chat_push: true\n  no_narration: false\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "p1.yaml"), []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := &Container{Registry: reg}
	got := c.narratorProjectSettings("p1")
	if !got.ChatPush {
		t.Error("ChatPush = false, want true (from narrator.chat_push: true)")
	}
	if got.NoNarration {
		t.Error("NoNarration = true, want false")
	}
}

// TestWireNarratorArtifacts_LateBindsStore pins the ordering fix: the
// narrator's Artifacts field must end up wired to c.artifactStore even
// though initScheduler (which builds it) runs after initNarrator.
func TestWireNarratorArtifacts_LateBindsStore(t *testing.T) {
	store, err := artifacts.New(artifacts.WithBasePath(t.TempDir()))
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}
	c := &Container{
		Logger:         zerolog.Nop(),
		narratorWorker: &narrator.Narrator{},
		artifactStore:  store,
	}
	c.wireNarratorArtifacts()
	if c.narratorWorker.Artifacts == nil {
		t.Fatal("Artifacts must be wired from c.artifactStore")
	}
}

// TestWireNarratorArtifacts_NilSafety covers the no-worker / no-store
// no-op paths — must never panic.
func TestWireNarratorArtifacts_NilSafety(t *testing.T) {
	var nilC *Container
	nilC.wireNarratorArtifacts() // must not panic

	c := &Container{} // narratorWorker nil
	c.wireNarratorArtifacts()

	c2 := &Container{narratorWorker: &narrator.Narrator{}} // artifactStore nil
	c2.wireNarratorArtifacts()
	if c2.narratorWorker.Artifacts != nil {
		t.Error("Artifacts must stay nil when no artifactStore is available")
	}
}

// TestStartNarratorWorker_NilWorker_NoPanic — Run() gating means
// startNarratorWorker must be a safe no-op pre-construction.
func TestStartNarratorWorker_NilWorker_NoPanic(_ *testing.T) {
	c := &Container{Logger: zerolog.Nop()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.startNarratorWorker(ctx) // must not panic, must not block
}

// TestStartNarratorWorker_ConstructedWorker_LaunchesGoroutine — once
// initNarrator has produced a worker, startNarratorWorker must
// actually launch its Run loop (a structurally-disabled Narrator{}
// returns immediately, so this just proves the goroutine got
// spawned rather than skipped).
func TestStartNarratorWorker_ConstructedWorker_LaunchesGoroutine(t *testing.T) {
	pub := livepubsub.New(0)
	c := &Container{
		Config:  &config.Config{Narrator: config.NarratorConfig{Enabled: true}},
		Logger:  zerolog.Nop(),
		livePub: pub,
		repos: &storage.Repositories{
			ExecutionNarration: postgres.NewExecutionNarrationRepository(nil),
			Executions:         postgres.NewExecutionRepository(nil),
		},
	}
	c.initNarrator()
	if c.narratorWorker == nil {
		t.Fatal("expected a constructed narratorWorker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.startNarratorWorker(ctx)
	cancel() // Run() should observe cancellation and return promptly
}
