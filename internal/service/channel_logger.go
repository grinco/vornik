package service

import "github.com/rs/zerolog"

// channelLogger derives the logger a conversation channel (email, Slack,
// GitHub App, …) runs on, tagged with the channel kind and the owning
// project.
//
// It exists because all three channel builders independently forgot to set
// their Config.Logger, and a zero-value zerolog.Logger discards writes
// silently rather than failing — so the whole inbound path (messages
// received, attachments persisted and auto-extracted into project memory,
// allowlist drops, signature failures, IMAP/webhook transport errors) was
// invisible in journald. Routing every channel through one helper keeps the
// field names consistent and gives the next channel one obvious thing to
// call.
//
// projectID may be empty for a channel that serves several projects behind
// one handler (the multi-installation GitHub channel); the field is then
// omitted rather than logged blank.
func channelLogger(base zerolog.Logger, kind, projectID string) zerolog.Logger {
	ctx := base.With().Str("component", kind+"-channel")
	if projectID != "" {
		ctx = ctx.Str("project_id", projectID)
	}
	return ctx.Logger()
}
