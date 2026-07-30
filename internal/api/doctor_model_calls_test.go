package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// INCIDENT 2026-07-30, customer deployment. `vornikctl doctor` reported "all 11
// role-pinned model(s) healthy" while six memory-pipeline models were failing 100% of
// their calls — ~500 `context deadline exceeded` per hour — and memory ingestion was
// completely stalled. The operator had no way to see it.
//
// model_health could not have caught it: it enumerates models pinned by swarm ROLES
// (the memory workers are daemon-level config, not roles) and reads
// execution_step_outcomes + task_llm_usage (no execution step exists, and the spend table
// has no error column, so a timeout writes nothing).
//
// This check has no such blind spot by construction: it reads outcomes recorded at the
// provider wrapper every call already passes through, so a call site cannot be missed
// because somebody forgot to enumerate it.
func TestCheckModelCallsLive_FlagsAWorkerModelFailingNow(t *testing.T) {
	stats := chat.NewCallStats()
	// The customer's exact shape: classifier and titler failing, dispatcher fine.
	for i := 0; i < 20; i++ {
		stats.Record("openai.gpt-oss-20b-1:0", "memory.classifier", context.DeadlineExceeded)
		stats.Record("openai.gpt-oss-20b-1:0", "memory.titler", context.DeadlineExceeded)
		stats.Record("zai.glm-5", "dispatcher", nil)
	}

	h := &DoctorHandlers{callStats: stats}
	got := h.checkModelCallsLive()

	if got.Status == "OK" {
		t.Fatalf("status = OK, want a warning — 40 failing calls is the incident this "+
			"check exists for. message=%q", got.Message)
	}
	for _, want := range []string{"memory.classifier", "memory.titler", "gpt-oss-20b"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message does not name %q, so the operator cannot act on it:\n%s",
				want, got.Message)
		}
	}
	// The healthy call site must not be dragged into the finding.
	if strings.Contains(got.Message, "dispatcher") {
		t.Errorf("message names a HEALTHY call site:\n%s", got.Message)
	}
	// It must be honest about its horizon — this is process-lifetime, not history.
	if !strings.Contains(strings.ToLower(got.Message), "since") {
		t.Errorf("message must state the window (since daemon start):\n%s", got.Message)
	}
}

// A model that is merely slow, or occasionally failing, must not alarm — an operator who
// learns to ignore this check is worse off than one who has no check.
func TestCheckModelCallsLive_QuietWhenHealthy(t *testing.T) {
	stats := chat.NewCallStats()
	for i := 0; i < 50; i++ {
		stats.Record("m", "dispatcher", nil)
	}
	stats.Record("m", "dispatcher", context.DeadlineExceeded) // 1 in 51

	h := &DoctorHandlers{callStats: stats}
	if got := h.checkModelCallsLive(); got.Status != "OK" {
		t.Fatalf("status = %s, want OK for a 2%% failure rate: %s", got.Status, got.Message)
	}
}

// Below the sample floor, one or two bad calls must not trip the alarm — the same
// reasoning model_health already applies with modelHealthMinSamples.
func TestCheckModelCallsLive_IgnoresTinySamples(t *testing.T) {
	stats := chat.NewCallStats()
	stats.Record("m", "memory.classifier", context.DeadlineExceeded)
	stats.Record("m", "memory.classifier", context.DeadlineExceeded)

	h := &DoctorHandlers{callStats: stats}
	if got := h.checkModelCallsLive(); got.Status != "OK" {
		t.Fatalf("status = %s, want OK below the sample floor: %s", got.Status, got.Message)
	}
}

// No sink wired (or nothing called yet) is not a problem to report.
func TestCheckModelCallsLive_NoDataIsQuiet(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    *DoctorHandlers
	}{
		{"no sink", &DoctorHandlers{}},
		{"empty sink", &DoctorHandlers{callStats: chat.NewCallStats()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.h.checkModelCallsLive()
			if got.Status != "OK" {
				t.Errorf("status = %s, want OK: %s", got.Status, got.Message)
			}
		})
	}
}

// The finding must carry the error text. "gpt-oss-20b is failing" sends an operator
// hunting; "context deadline exceeded" tells them it is a timeout, which is the
// difference between guessing and knowing.
func TestCheckModelCallsLive_CarriesTheErrorText(t *testing.T) {
	stats := chat.NewCallStats()
	for i := 0; i < 10; i++ {
		stats.Record("m", "memory.classifier", context.DeadlineExceeded)
	}
	h := &DoctorHandlers{callStats: stats}
	got := h.checkModelCallsLive()
	if !strings.Contains(got.Message, "deadline exceeded") {
		t.Errorf("message omits the error text:\n%s", got.Message)
	}
}

// REGRESSION GUARD on the wording that misled the operator. model_health's summary said
// "all 11 role-pinned model(s) healthy" — true as written, but read as "models are fine"
// while six unassessed models were down. It must not claim health it did not verify.
func TestModelHealthSummary_DoesNotOverclaim(t *testing.T) {
	msg := modelHealthHealthySummary(11)
	low := strings.ToLower(msg)
	if !strings.Contains(low, "role-pinned") {
		t.Errorf("summary must keep the role-pinned qualifier: %q", msg)
	}
	if !strings.Contains(low, "model_calls_live") {
		t.Errorf("summary must point at the check that covers non-role models, or an "+
			"operator has no reason to look further: %q", msg)
	}
}

// Sanity: the flag thresholds are the ones documented, so a future edit that loosens them
// has to change this test deliberately.
func TestModelCallsLiveThresholds(t *testing.T) {
	if modelCallsLiveMinSamples < 5 {
		t.Errorf("min samples = %d, too low to be trustworthy", modelCallsLiveMinSamples)
	}
	if modelCallsLiveFailureRate < 0.25 || modelCallsLiveFailureRate > 0.75 {
		t.Errorf("failure rate threshold = %v, outside a defensible range", modelCallsLiveFailureRate)
	}
	_ = time.Second
}
