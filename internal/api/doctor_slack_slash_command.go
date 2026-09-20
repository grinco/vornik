package api

import (
	"fmt"
	"strings"
	"time"
)

// SlashCommandMiss is one slash-command literal a Slack channel was sent and
// does not answer, with the window it was seen over.
type SlashCommandMiss struct {
	Command   string    `json:"command"`
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// SlashCommandReport is one Slack channel's side of the comparison: the command
// it answers, and the commands it refused.
type SlashCommandReport struct {
	Projects   []string           `json:"projects"`
	Configured string             `json:"configured"`
	Unmatched  []SlashCommandMiss `json:"unmatched,omitempty"`
	Dropped    int                `json:"dropped,omitempty"`
}

// SlashCommandReporter supplies the reports. An interface rather than a
// *slack.Channel slice so the check is testable without constructing a channel,
// and so `api` does not depend on the Slack package's construction rules.
type SlashCommandReporter interface {
	SlashCommandReports() []SlashCommandReport
}

// checkSlackSlashCommand reconciles the slash command a Slack app publishes
// against the one this daemon answers.
//
// THE INCIDENT (2026-09-15). The easeit-companion project had no
// slash_command set, so the daemon answered the default /vornik. The T-800
// Slack app publishes /t800. Every `/t800 link <code>` reached the webhook,
// passed signature verification, and was refused at the command comparison.
// Five link codes were minted and expired unused over forty minutes.
//
// Diagnosis was slow for a reason worth keeping separate from the defect: the
// HTTP access logger records only non-2xx, and a refused command is answered
// 200, so "zero /api/v1/slack/webhook entries" read as "Slack never reached
// this daemon" when it meant the opposite. That logging asymmetry is defensible
// on volume grounds and is NOT fixed here — but any runbook that reasons from
// log absence on this daemon is unsound, which is why this check reads a tally
// the daemon keeps rather than a log it does not write.
//
// WARNING rather than ERROR: a refused command is not a broken deployment. It
// is one app talking past one project, and the daemon serves everything else
// correctly while it happens.
func (h *DoctorHandlers) checkSlackSlashCommand() DoctorCheck {
	c := DoctorCheck{Name: "slack_slash_command", Status: "OK"}

	// SKIPPED, not OK: there is no Slack channel to reconcile anything against,
	// so this check did not run. An OK here would be a control reporting a pass
	// over a surface it never examined (2026-08-26-doctor-skipped-vs-ok-design).
	if h.slashCommands == nil {
		c.Status = "SKIPPED"
		c.Message = "no Slack channel is wired on this daemon; nothing to reconcile"
		return c
	}
	reports := h.slashCommands.SlashCommandReports()
	if len(reports) == 0 {
		c.Status = "SKIPPED"
		c.Message = "no Slack channel is wired on this daemon; nothing to reconcile"
		return c
	}

	var problems []string
	answered := make([]string, 0, len(reports))
	for _, r := range reports {
		if r.Configured != "" {
			answered = append(answered, r.Configured)
		}
		if len(r.Unmatched) == 0 {
			continue
		}
		var parts []string
		for _, m := range r.Unmatched {
			parts = append(parts, fmt.Sprintf("%s %dx%s", m.Command, m.Count, window(m)))
		}
		line := fmt.Sprintf("received %s; this daemon answers %s", strings.Join(parts, ", "), r.Configured)
		if len(r.Projects) > 0 {
			// The remedy is per project, because slash_command is a
			// per-project key. Naming the project is the difference between a
			// finding and an instruction.
			line += fmt.Sprintf("\n  -> set slack.slash_command: %q for project %s",
				r.Unmatched[0].Command, strings.Join(r.Projects, ", "))
		}
		if r.Dropped > 0 {
			// Say that the tally is capped. A count that has silently stopped
			// being complete is worse than a smaller one that admits it.
			line += fmt.Sprintf("\n  (%d further distinct command(s) were seen and not tracked; the per-channel cap is %d)",
				r.Dropped, slashCommandTallyCap)
		}
		problems = append(problems, line)
	}

	if len(problems) == 0 {
		// Publish the denominator: "clean" must say what it examined, or it
		// cannot be told from "examined nothing".
		c.Message = fmt.Sprintf("%d Slack channel(s) wired, answering %s; no refused slash commands since daemon start",
			len(reports), strings.Join(dedupe(answered), ", "))
		return c
	}

	c.Status = "WARNING"
	c.Message = "a Slack app is sending a command this daemon does not answer:\n  " +
		strings.Join(problems, "\n  ")
	return c
}

// slashCommandTallyCap mirrors the slack package's bound. Kept as a printed
// number only — the enforcement lives in one place, and this is the message
// telling an operator what the number they are reading is bounded by.
const slashCommandTallyCap = 8

func window(m SlashCommandMiss) string {
	if m.FirstSeen.IsZero() || m.LastSeen.IsZero() || !m.LastSeen.After(m.FirstSeen) {
		return ""
	}
	return fmt.Sprintf(" over %s", m.LastSeen.Sub(m.FirstSeen).Round(time.Minute))
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
