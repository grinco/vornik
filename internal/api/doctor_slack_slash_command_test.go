package api

import (
	"strings"
	"testing"
	"time"
)

type fakeSlashReporter struct{ reports []SlashCommandReport }

func (f fakeSlashReporter) SlashCommandReports() []SlashCommandReport { return f.reports }

// An unevaluated check must not report OK — a control that passes over a
// surface it never examined is the defect 2026-08-26-doctor-skipped-vs-ok-design
// exists to remove. SKIPPED is the status that says "did not run".
func TestCheckSlackSlashCommand_NotWiredIsSkipped(t *testing.T) {
	h := &DoctorHandlers{}
	c := h.checkSlackSlashCommand()
	if c.Status != "SKIPPED" {
		t.Fatalf("want SKIPPED, got %s", c.Status)
	}
	if !strings.Contains(c.Message, "no Slack channel") {
		t.Fatalf("message must distinguish unwired from clean, got %q", c.Message)
	}
}

// Wired, nothing refused: the clean answer must publish its denominator, the
// same discipline project_config_skew uses.
func TestCheckSlackSlashCommand_CleanNamesWhatItChecked(t *testing.T) {
	h := &DoctorHandlers{slashCommands: fakeSlashReporter{reports: []SlashCommandReport{
		{Projects: []string{"easeit-companion"}, Configured: "/t800"},
	}}}
	c := h.checkSlackSlashCommand()
	if c.Status != "OK" {
		t.Fatalf("want OK, got %s: %s", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "/t800") {
		t.Fatalf("a clean result must name the command it answers, got %q", c.Message)
	}
}

// The incident itself: /t800 arrives, /vornik is answered. The check must name
// both literals, the count, and the config key that fixes it — the operator
// diagnosed this by reading container logs for forty minutes.
func TestCheckSlackSlashCommand_ReportsTheMismatch(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	h := &DoctorHandlers{slashCommands: fakeSlashReporter{reports: []SlashCommandReport{{
		Projects:   []string{"easeit-companion"},
		Configured: "/vornik",
		Unmatched: []SlashCommandMiss{
			{Command: "/t800", Count: 14, FirstSeen: now.Add(-40 * time.Minute), LastSeen: now},
		},
	}}}}

	c := h.checkSlackSlashCommand()
	if c.Status != "WARNING" {
		t.Fatalf("want WARNING, got %s", c.Status)
	}
	for _, want := range []string{"/t800", "14", "/vornik", "slack.slash_command", "easeit-companion"} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("message missing %q: %s", want, c.Message)
		}
	}
}

// A capped tally must say it is capped. A count that has silently stopped being
// complete is worse than a smaller one that says so.
func TestCheckSlackSlashCommand_ReportsDroppedLiterals(t *testing.T) {
	h := &DoctorHandlers{slashCommands: fakeSlashReporter{reports: []SlashCommandReport{{
		Projects:   []string{"p1"},
		Configured: "/vornik",
		Unmatched:  []SlashCommandMiss{{Command: "/x", Count: 1}},
		Dropped:    3,
	}}}}
	c := h.checkSlackSlashCommand()
	if !strings.Contains(c.Message, "3 further") {
		t.Fatalf("dropped literals not reported: %s", c.Message)
	}
}

// A channel serving a workspace whose app matches must not be dragged into a
// warning raised by a different channel; the remediation names one project.
func TestCheckSlackSlashCommand_OnlyTheMismatchedChannelIsNamed(t *testing.T) {
	h := &DoctorHandlers{slashCommands: fakeSlashReporter{reports: []SlashCommandReport{
		{Projects: []string{"healthy"}, Configured: "/ok"},
		{Projects: []string{"broken"}, Configured: "/vornik", Unmatched: []SlashCommandMiss{{Command: "/t800", Count: 2}}},
	}}}
	c := h.checkSlackSlashCommand()
	if c.Status != "WARNING" {
		t.Fatalf("want WARNING, got %s", c.Status)
	}
	if strings.Contains(c.Message, "healthy") {
		t.Fatalf("a healthy channel must not appear in the remediation: %s", c.Message)
	}
}
