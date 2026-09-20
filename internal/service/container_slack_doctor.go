package service

import (
	"vornik.io/vornik/internal/api"
)

// slashCommandReporter adapts the container's Slack channels to the doctor
// check's view of them: what each one answers, and what it refused.
//
// It reads c.SlackChannels at CALL time, not at construction, so a channel
// rebuilt by a config reload is reflected without re-wiring the doctor.
type slashCommandReporter struct{ c *Container }

func (r slashCommandReporter) SlashCommandReports() []api.SlashCommandReport {
	if r.c == nil {
		return nil
	}
	out := make([]api.SlashCommandReport, 0, len(r.c.SlackChannels))
	for _, ch := range r.c.SlackChannels {
		if ch == nil {
			continue
		}
		misses := ch.UnmatchedSlashCommands()
		rep := api.SlashCommandReport{
			Projects:   ch.ProjectIDs(),
			Configured: ch.ConfiguredSlashCommand(),
			Dropped:    ch.UnmatchedSlashCommandsDropped(),
		}
		for _, m := range misses {
			rep.Unmatched = append(rep.Unmatched, api.SlashCommandMiss{
				Command:   m.Command,
				Count:     m.Count,
				FirstSeen: m.FirstSeen,
				LastSeen:  m.LastSeen,
			})
		}
		out = append(out, rep)
	}
	return out
}
