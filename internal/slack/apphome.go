package slack

import (
	"context"
	"sort"
	"strings"
)

// BACKLOG 2026-07-30: opening the app's Home tab showed Slack's stock "This is still a
// work in progress… visit the About tab". That is what Slack renders when
// home_tab_enabled is true and the app never calls views.publish, and Vornik published
// no home view at all — so the one surface a new user opens first read as broken.
//
// views.publish needs no additional OAuth scope: an app may always publish its own home
// view with the bot token it already has. That is why this landed ahead of the rest of
// the Agents/AI-Apps surface, which needs `assistant:write` and a reinstall.

// homeViewBlock is one Block Kit block. Only the two shapes the home view uses are
// modelled; the rest of Block Kit is deliberately absent.
type homeViewBlock struct {
	Type string        `json:"type"`
	Text *homeViewText `json:"text,omitempty"`
}

type homeViewText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// homeView is the views.publish `view` object.
type homeView struct {
	Type   string          `json:"type"`
	Blocks []homeViewBlock `json:"blocks"`
}

// viewsPublishRequest is the views.publish body. The view rides as a struct rather than
// a hand-assembled string so operator-supplied values (the channel allowlist, the
// command name) are JSON-encoded rather than interpolated — a stray quote in config
// would otherwise produce a payload Slack rejects wholesale.
type viewsPublishRequest struct {
	UserID string   `json:"user_id"`
	View   homeView `json:"view"`
}

func section(markdown string) homeViewBlock {
	return homeViewBlock{Type: "section", Text: &homeViewText{Type: "mrkdwn", Text: markdown}}
}

// handleAppHomeOpened publishes the home view for the user who opened the tab.
//
// Runs on the detached dispatch context: the delivery is acked in milliseconds like
// every other event and the publish happens afterwards.
func (c *Channel) handleAppHomeOpened(ctx context.Context, p eventPayload, inst *installation) {
	ev := p.Event
	if ev == nil || strings.TrimSpace(ev.User) == "" {
		return
	}
	// Slack sends app_home_opened for the Messages tab too. Publishing there would
	// rewrite the home view every time someone opened a DM with the bot.
	if tab := strings.TrimSpace(ev.Tab); tab != "" && tab != "home" {
		return
	}
	// Someone who cannot use the bot should not be shown a page describing what they
	// can ask it, and publishing to them spends an API call for nothing.
	if _, err := c.resolveSpeakerForInstallation(inst, ev.User); err != nil {
		c.logger.Debug().Str("user", ev.User).
			Msg("slack: app_home_opened from a sender not on the allowlist; not publishing")
		return
	}

	var parsed struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	err := c.callSlackAPI(ctx, p.TeamID, "/views.publish", viewsPublishRequest{
		UserID: ev.User,
		View:   c.buildHomeView(inst),
	}, &parsed)
	if err != nil {
		c.logger.Warn().Err(err).Str("user", ev.User).Msg("slack: views.publish failed")
		return
	}
	if !parsed.OK {
		// Most likely cause is the Home tab being switched off in the app config, which
		// is an operator action, so name the Slack error rather than swallowing it.
		c.logger.Warn().
			Str("user", ev.User).
			Str("slack_error", parsed.Error).
			Msg("slack: views.publish refused")
	}
}

// buildHomeView renders what this deployment actually is: which project answers, how to
// reach it, and where it will and will not speak.
//
// Everything here is derived from live configuration rather than written prose, so the
// page cannot drift from what the daemon will really do — which is the failure mode of a
// hand-maintained help text.
func (c *Channel) buildHomeView(inst *installation) homeView {
	command := NormaliseSlashCommand(c.cfg.SlashCommand)

	blocks := []homeViewBlock{
		section("*Vornik*\nAn agent swarm you can talk to in Slack."),
		section("*How to reach me*\n" +
			"• `" + command + " <what you want>` — works in a channel or in a DM\n" +
			"• *@-mention me* in a channel I am in\n" +
			"• *DM me* directly, or *reply in a thread* I am already part of — no mention needed"),
	}

	if project := strings.TrimSpace(inst.projectID); project != "" {
		blocks = append(blocks, section("*Project*\n`"+project+"`"))
	}

	if len(inst.allowedChannels) > 0 {
		names := make([]string, 0, len(inst.allowedChannels))
		for id := range inst.allowedChannels {
			names = append(names, "<#"+id+">")
		}
		sort.Strings(names) // map iteration order would reshuffle the view on every open
		blocks = append(blocks, section(
			"*Where I answer*\nShared channels: "+strings.Join(names, ", ")+
				"\nPlus direct messages, which are never restricted by channel."))
	} else {
		blocks = append(blocks, section(
			"*Where I answer*\nAny channel I have been added to, plus direct messages."))
	}

	blocks = append(blocks, section(
		"*Good to know*\n"+
			"• A turn can take up to a minute; I post _working on it…_ while I think, "+
			"and replace it with the answer.\n"+
			"• Replies are generated by an AI model, not a human.\n"+
			"• I can read images you post, and transcribe voice memos when voice is configured."))

	return homeView{Type: "home", Blocks: blocks}
}
