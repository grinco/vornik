package service

import (
	"context"

	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/ui"
)

// emailChannelInventory bridges the boot-time email-channel slices
// on the service Container into the ui.EmailChannelInventory the
// admin UI consumes. One row per channel, paired by index with the
// project the channel is pinned to. The bridge keeps the UI package
// free of the internal/email + internal/registry imports it would
// otherwise need to introspect channels directly.
type emailChannelInventory struct {
	channels []*email.Channel
	projects []*registry.Project
}

// newEmailChannelInventory returns a ui.EmailChannelInventory backed
// by the supplied channel+project pairs (Container.EmailChannels and
// Container.EmailProjects). Returns nil when no channels are wired
// so the UI page renders its empty-state — operators can still see
// the route works even on deployments without email enabled.
func newEmailChannelInventory(channels []*email.Channel, projects []*registry.Project) ui.EmailChannelInventory {
	if len(channels) == 0 {
		return nil
	}
	return &emailChannelInventory{channels: channels, projects: projects}
}

// EmailChannels implements ui.EmailChannelInventory. ListSessions
// is called per-render so the page reflects fresh inbound activity
// without an external cache layer. ListSessions today never errors
// but the conversation.Channel contract permits it; we stash the
// message on the row so the template surfaces it instead of dropping
// the row silently.
func (e *emailChannelInventory) EmailChannels(ctx context.Context) []ui.AdminEmailChannelRow {
	if e == nil {
		return nil
	}
	out := make([]ui.AdminEmailChannelRow, 0, len(e.channels))
	for i, ch := range e.channels {
		if ch == nil || i >= len(e.projects) || e.projects[i] == nil {
			continue
		}
		p := e.projects[i]
		row := ui.AdminEmailChannelRow{
			ProjectID:          p.ID,
			IMAPHost:           p.Email.IMAPHost,
			IMAPPort:           p.Email.IMAPPort,
			IMAPMailbox:        p.Email.IMAPMailbox,
			OutboundConfigured: p.Email.SMTPHost != "",
			SMTPHost:           p.Email.SMTPHost,
			FromAddress:        p.Email.FromAddress,
			AllowlistSize:      len(p.Email.SenderAllowlist),
			AttachmentCapBytes: p.Email.AttachmentSizeCapBytes,
			VerifyInboundAuth:  p.Email.VerifyInboundAuth,
			AuthPolicy:         p.Email.AuthPolicy,
		}
		sessions, err := ch.ListSessions(ctx)
		if err != nil {
			row.SessionsError = err.Error()
		} else {
			row.Sessions = make([]ui.AdminEmailSessionRow, 0, len(sessions))
			for _, s := range sessions {
				row.Sessions = append(row.Sessions, ui.AdminEmailSessionRow{
					ID:               s.ID,
					Title:            s.Title,
					LastActivity:     s.LastActivity,
					ParticipantCount: s.ParticipantCount,
				})
			}
		}
		out = append(out, row)
	}
	return out
}
