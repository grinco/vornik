package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

// TestInitScheduler_NonWorkerSkipsRuntimeManager is a regression test for the
// 2026-06-12 incident: a thin webhook node crash-looped on
//
//	failed to initialize runtime manager: podman not available
//
// because initScheduler() built the podman runtime manager with no RunWorkers
// gate. A non-worker node (ui or webhook) must still get the artifact store —
// a ui node serves artifact downloads — but must NOT build the runtime
// manager, executor, or task scheduler, none of which it can or should run.
// TestInstinctAutoApplyMinCleanSupport pins the nil-safe getter for the
// clean-support knob (instinct-auto-apply supply, 2026-06-23): nil config
// reads 0 (gate off), a set value is returned verbatim.
func TestInstinctAutoApplyMinCleanSupport(t *testing.T) {
	if got := instinctAutoApplyMinCleanSupport(nil); got != 0 {
		t.Errorf("nil config = %d, want 0 (gate off)", got)
	}
	cfg := &config.Config{}
	cfg.Instinct.Consumers.AutoApply.MinCleanSupport = 10
	if got := instinctAutoApplyMinCleanSupport(cfg); got != 10 {
		t.Errorf("min_clean_support = %d, want 10", got)
	}
}

func TestInitScheduler_NonWorkerSkipsRuntimeManager(t *testing.T) {
	for _, profile := range []string{"webhook", "ui"} {
		t.Run(profile, func(t *testing.T) {
			cfg := &config.Config{
				Node:    config.NodeConfig{Profile: profile}, // RunWorkers=false
				Storage: config.StorageConfig{Backend: "filesystem", ArtifactsPath: t.TempDir()},
			}
			c := &Container{Config: cfg, Logger: zerolog.Nop(), repos: &storage.Repositories{}}

			if err := c.initScheduler(); err != nil {
				t.Fatalf("initScheduler on a %q node must not error (no podman required): %v", profile, err)
			}
			if c.artifactStore == nil {
				t.Error("artifact store must still be built on a non-worker node")
			}
			if c.runtimeManager != nil {
				t.Error("runtime manager must NOT be built on a non-worker node (needs podman)")
			}
			if c.Scheduler != nil {
				t.Error("scheduler must NOT be built on a non-worker node")
			}
		})
	}
}

// TestSkipNonWorker covers the centralized worker-only gate that the
// scheduler, watchdog, effective-cost monitor, and reminders init steps share
// (centralize-on-recurrence: each had been open-coding or missing this check).
func TestSkipNonWorker(t *testing.T) {
	cases := []struct {
		profile  string
		wantSkip bool
	}{
		{"all", false},
		{"worker", false},
		{"ui", true},
		{"webhook", true},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			c := &Container{Config: &config.Config{Node: config.NodeConfig{Profile: tc.profile}}, Logger: zerolog.Nop()}
			if got := c.skipNonWorker("test"); got != tc.wantSkip {
				t.Fatalf("skipNonWorker(profile=%q) = %v, want %v", tc.profile, got, tc.wantSkip)
			}
		})
	}
}

// TestInitWatchdog_SkippedOnNonWorker: the stuck-execution watchdog scans
// executions, which only a worker node produces. A ui/webhook node must not
// build it.
func TestInitWatchdog_SkippedOnNonWorker(t *testing.T) {
	c := &Container{Config: &config.Config{Node: config.NodeConfig{Profile: "webhook"}}, Logger: zerolog.Nop()}
	if err := c.initWatchdog(); err != nil {
		t.Fatalf("initWatchdog on a webhook node must not error: %v", err)
	}
	if c.Watchdog != nil {
		t.Error("watchdog must NOT be built on a non-worker node")
	}
}

// TestWarnAgentLLMDirectRoute is a regression test for the fresh-install
// "Invalid API key" incident (2026-07-25, design F2b): initScheduler used to
// log the empty-agent_llm.endpoint fallback at Info, which fresh-install
// operators never see before every task starts failing. The extracted
// warnAgentLLMDirectRoute helper (see container_scheduler.go) must WARN and
// name the breakage explicitly so it's greppable in the daemon's own logs —
// this is the same failure mode internal/api's agent_llm_topology doctor
// check guards, surfaced here at the point it actually gets logged.
func TestWarnAgentLLMDirectRoute(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	warnAgentLLMDirectRoute(logger, "https://api.z.ai/v1")

	rec := findLog(logRecords(t, &buf), "Invalid API key")
	if rec == nil {
		t.Fatalf("expected a warning naming the 'Invalid API key' breakage; got %q", buf.String())
	}
	if lvl, _ := rec["level"].(string); lvl != "warn" {
		t.Errorf("expected warn level, got %q (full record: %v)", lvl, rec)
	}
	if ep, _ := rec["resolved_endpoint"].(string); ep != "https://api.z.ai/v1" {
		t.Errorf("expected resolved_endpoint field, got %q", ep)
	}
	if msg, _ := rec["message"].(string); !strings.Contains(msg, "vornikctl doctor") {
		t.Errorf("expected message to name the correct CLI command 'vornikctl doctor', got %q", msg)
	}
}

// TestInitEffectiveCostMonitor_SkippedOnNonWorker: the cost-drift monitor
// queries LLM-usage rows produced by workers; skip it on non-worker nodes.
func TestInitEffectiveCostMonitor_SkippedOnNonWorker(t *testing.T) {
	c := &Container{Config: &config.Config{Node: config.NodeConfig{Profile: "ui"}}, Logger: zerolog.Nop()}
	if err := c.initEffectiveCostMonitor(); err != nil {
		t.Fatalf("initEffectiveCostMonitor on a ui node must not error: %v", err)
	}
	if c.EffectiveCostMon != nil {
		t.Error("effective-cost monitor must NOT be built on a non-worker node")
	}
}
