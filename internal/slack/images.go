package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"vornik.io/vornik/internal/conversation"
)

// imageEngagement decides whether a shared image should start a turn, and the
// thread root the reply should use. It mirrors addressedToUs for the text path,
// which the image path historically skipped — so the bot described any image in
// an allow-listed channel, including one posted in an unrelated thread, and
// answered on the main channel because file_shared carries no thread_ts
// (operator report 2026-08-01).
//
//   - DM: always ours.
//   - channel/group image inside a thread: only if we are engaged in that
//     thread (we started it or were tagged into it → we hold history for it).
//   - channel/group image at top level: only if its own message tagged us.
//
// Returns (threadRoot, allowed): threadRoot is the real thread_ts for a thread,
// or ChannelSessionThreadRoot ("main") at channel level, so the reply lands
// where the human posted.
func (c *Channel) imageEngagement(ctx context.Context, p eventPayload, inst *installation, meta *slackFile) (string, bool) {
	ev := p.Event
	if isDirectMessageChannel(ev.ChannelType, ev.Channel) {
		return ChannelSessionThreadRoot, true
	}
	msgTs, threadTs, ok := meta.shareIn(ev.Channel)
	if !ok {
		// No recorded share for this channel — we cannot establish context, so
		// we drop rather than answer unconditionally (a missed image is
		// recoverable by tagging; an unsolicited one is not).
		return "", false
	}
	if threadTs != "" {
		// A thread is ours if we hold history for it (we were tagged in / have
		// conversed), OR the thread is rooted on our own message (we started
		// it — its first reply may arrive before any thread history exists).
		threadSession := fmt.Sprintf("%s/%s#%s", p.TeamID, ev.Channel, threadTs)
		if c.threadEngaged(ctx, threadSession) {
			return threadTs, true
		}
		if _, rootAuthor, err := c.fetchSlackMessage(ctx, inst, ev.Channel, threadTs); err == nil {
			if bot := p.botUserID(); bot != "" && rootAuthor == bot {
				return threadTs, true
			}
		}
		return threadTs, false
	}
	// Top level: ours only if the file's own message tagged us. file_shared has
	// no text, so fetch the message.
	text, _, err := c.fetchSlackMessage(ctx, inst, ev.Channel, msgTs)
	if err != nil {
		c.logger.Debug().Err(err).Str("file_id", meta.ID).
			Msg("slack: image gate: message fetch failed; dropping")
		return "", false
	}
	return ChannelSessionThreadRoot, textMentionsBot(text, p.botUserID())
}

// textMentionsBot reports whether a message body tags the bot — the encoded
// mention <@BOTID> or the literal "@vornik" that mentionsVornik catches.
func textMentionsBot(text, botID string) bool {
	if mentionsVornik(text) {
		return true
	}
	return botID != "" && strings.Contains(text, "<@"+botID+">")
}

// fetchSlackMessage returns the text + author of a single channel message (by
// ts) via conversations.history. Used to detect a mention on a top-level image
// (whose file_shared delivery carries no text) and the author of a thread root
// (to recognise a thread the bot itself started).
func (c *Channel) fetchSlackMessage(ctx context.Context, inst *installation, channelID, ts string) (text, user string, err error) {
	if strings.TrimSpace(inst.botToken) == "" {
		return "", "", fmt.Errorf("slack channel: no bot token for team %q", inst.teamID)
	}
	u := fmt.Sprintf("%s/conversations.history?channel=%s&latest=%s&oldest=%s&inclusive=true&limit=1",
		c.apiBaseURL, url.QueryEscape(channelID), url.QueryEscape(ts), url.QueryEscape(ts))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+inst.botToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("conversations.history HTTP %d", resp.StatusCode)
	}
	var out struct {
		OK       bool `json:"ok"`
		Messages []struct {
			Text string `json:"text"`
			User string `json:"user"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if !out.OK || len(out.Messages) == 0 {
		return "", "", nil
	}
	return out.Messages[0].Text, out.Messages[0].User, nil
}

// BACKLOG 2026-07-30: a photo posted to Slack was fetched and then thrown away.
// handleFileSharedEvent dropped everything failing isAudioMime with "not audio;
// ignoring (v1 slice scope)", even though the project has a vision role and files:read
// was already granted. Nothing on the Slack side had to change — the limit was in the
// binary, not the manifest.
//
// An image now rides on the turn as a conversation.Attachment, which is the seam the
// media-routing work already built: the dispatcher classifies it, inlines the pixels
// when the model can see them, and hands over to the vision role when it cannot.
// see LLD § https://docs.vornik.io §4.3

// imageMIMEs are the prefixes this channel treats as an image worth carrying. Broader
// than the dispatcher's inline allowlist ON PURPOSE: mediakind decides what a file IS
// and the dispatcher decides what it is willing to hand a provider, so a HEIC should
// arrive and hand over to the vision role rather than be silently dropped here.
var imageMIMEs = []string{"image/"}

// isImageMime reports whether a MIME names an image. Case-insensitive.
func isImageMime(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if m == "" {
		return false
	}
	for _, prefix := range imageMIMEs {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

// channelRefPrefix namespaces the Attachment.ChannelRef this package issues.
//
// The shape is `slack:<team_id>:<url_private_download>`. The team id has to be in there
// because FetchAttachment receives ONLY the attachment — it has no session and no
// installation — and a Slack url_private is not publicly readable, so the call has to
// resolve which workspace's bot token may be spent on it.
//
// It also must not look like a filesystem path: the dispatcher resolves a ChannelRef as
// a host path FIRST and only falls through to the channel's fetcher, so a ref starting
// with "/" would be read off the daemon's own disk.
const channelRefPrefix = "slack:"

// imageChannelRef encodes the fetch coordinates for one Slack file.
func imageChannelRef(teamID, downloadURL string) string {
	return channelRefPrefix + teamID + ":" + downloadURL
}

// parseImageChannelRef splits a ref back into team id and URL.
func parseImageChannelRef(ref string) (teamID, downloadURL string, err error) {
	rest, ok := strings.CutPrefix(ref, channelRefPrefix)
	if !ok {
		return "", "", fmt.Errorf("slack channel: attachment ref %q is not a Slack ref", ref)
	}
	teamID, downloadURL, ok = strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(teamID) == "" || strings.TrimSpace(downloadURL) == "" {
		return "", "", fmt.Errorf("slack channel: attachment ref %q is malformed", ref)
	}
	return teamID, downloadURL, nil
}

// isConfiguredAPIHost reports whether host is the same host as the channel's configured
// Slack API base URL.
//
// This is the plaintext-http seam, and it is deliberately narrow: it admits exactly the
// stub server a test injected via Config.APIBaseURL, because that server also serves the
// file download. In production APIBaseURL is https://slack.com/api and real file URLs
// live on files.slack.com, so nothing plaintext ever qualifies.
func (c *Channel) isConfiguredAPIHost(host string) bool {
	base, err := url.Parse(c.apiBaseURL)
	if err != nil || base.Host == "" {
		return false
	}
	return base.Host == host
}

// dispatchImageFile turns a file_shared image into a ChannelMessage carrying one
// attachment, and hands it to the bound Receiver.
//
// The bytes are NOT fetched here. The dispatcher decides whether it needs them at all —
// a model that cannot see gets a handover instead — and it enforces the per-image and
// per-turn byte caps while fetching. Downloading eagerly would spend bandwidth on every
// image and then discard most of it.
func (c *Channel) dispatchImageFile(ctx context.Context, p eventPayload, inst *installation, meta *slackFile) {
	ev := p.Event

	// Apply the same engagement gate the text path uses. Without it the bot
	// described ANY image in an allow-listed channel — including one posted in
	// an unrelated thread — and answered on the main channel because
	// file_shared carries no thread_ts (operator report 2026-08-01).
	threadRoot, allowed := c.imageEngagement(ctx, p, inst, meta)
	if !allowed {
		c.logger.Debug().
			Str("event_id", p.EventID).
			Str("file_id", meta.ID).
			Str("channel", ev.Channel).
			Msg("slack: shared image not addressed to us (no mention, not an engaged thread); dropping")
		return
	}

	downloadURL := meta.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = meta.URLPrivate
	}
	if strings.TrimSpace(downloadURL) == "" {
		c.logger.Warn().Str("file_id", meta.ID).
			Msg("slack: image file_shared carries no download URL; dropping")
		return
	}

	// threadRoot came from the gate: the real thread_ts in a thread, or "main"
	// at channel level — so the reply threads where the human posted instead of
	// defaulting to the channel.
	sessionID := fmt.Sprintf("%s/%s#%s", p.TeamID, ev.Channel, threadRoot)
	realThreadTs := ""
	if threadRoot != ChannelSessionThreadRoot {
		realThreadTs = threadRoot
	}

	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = meta.ID
	}
	// file_shared does not carry the comment posted alongside the file, so there is no
	// user prompt to pass on. Naming the file is honest and gives the lead something to
	// refer to; the media layer appends its own note about what it did with the pixels.
	text := "(shared an image: " + name + ")"

	msg := conversation.ChannelMessage{
		Source:    channelName,
		ID:        ev.Ts,
		SessionID: sessionID,
		SpeakerID: ev.User,
		Text:      text,
		ThreadID:  realThreadTs,
		Timestamp: c.clock(),
		Attachments: []conversation.Attachment{{
			Name:       name,
			MimeType:   meta.Mimetype,
			SizeBytes:  meta.Size,
			ChannelRef: imageChannelRef(p.TeamID, downloadURL),
		}},
		ChannelSpecific: map[string]string{
			"team_id":      p.TeamID,
			"channel_id":   ev.Channel,
			"channel_type": ev.ChannelType,
			"thread_ts":    realThreadTs,
			"event_id":     p.EventID,
			"event_type":   "file_shared",
			"project_id":   inst.projectID,
			"file_id":      meta.ID,
			"file_mime":    meta.Mimetype,
		},
	}
	c.recordSession(sessionID, channelTitleFromPayload(p), ev.User, msg.Timestamp, inst)
	c.beginProgressSignal(ctx, sessionID)

	c.recvMu.RLock()
	recvAny := c.recv
	c.recvMu.RUnlock()
	recv, ok := recvAny.(conversation.Receiver)
	if !ok || recv == nil {
		c.logger.Warn().Str("event_id", p.EventID).
			Msg("slack: image received but no Receiver bound; dropping")
		return
	}
	if err := recv.Receive(ctx, msg); err != nil {
		c.logger.Warn().Err(err).Str("event_id", p.EventID).
			Msg("slack: image Receiver.Receive returned error")
	}
}

// FetchAttachment implements conversation.AttachmentFetcher so the dispatcher's vision
// path can resolve a Slack image's bytes on demand, authenticated with the owning
// installation's bot token.
//
// Refs are validated rather than trusted: the ref names the workspace whose token pays
// for the call, and the URL must be an https Slack-hosted address. An attachment
// reaches here having ridden in on a ChannelMessage, and a channel-private ref is
// exactly the kind of string that must not become an arbitrary outbound GET — let alone
// a file:// read of the daemon's own disk.
func (c *Channel) FetchAttachment(ctx context.Context, a conversation.Attachment) (io.ReadCloser, error) {
	teamID, downloadURL, err := parseImageChannelRef(a.ChannelRef)
	if err != nil {
		return nil, err
	}
	inst, ok := c.installationsByID[teamID]
	if !ok {
		return nil, fmt.Errorf("%w: team_id %q not configured", ErrUnknownSession, teamID)
	}
	if strings.TrimSpace(inst.botToken) == "" {
		return nil, ErrOutboundNotConfigured
	}
	parsed, err := url.Parse(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("slack channel: attachment URL %q: %w", downloadURL, err)
	}
	if parsed.Host == "" || (parsed.Scheme != "https" && !c.isConfiguredAPIHost(parsed.Host)) {
		return nil, fmt.Errorf("slack channel: attachment URL %q is not an https URL", downloadURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+inst.botToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack channel: attachment fetch: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("slack channel: attachment fetch HTTP %d", resp.StatusCode)
	}
	// The caller caps the read (MaxBytesPerImage) — returning the stream rather than
	// bytes is what lets it do that without this function guessing a limit.
	return resp.Body, nil
}
