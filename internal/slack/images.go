package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/conversation"
)

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
	downloadURL := meta.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = meta.URLPrivate
	}
	if strings.TrimSpace(downloadURL) == "" {
		c.logger.Warn().Str("file_id", meta.ID).
			Msg("slack: image file_shared carries no download URL; dropping")
		return
	}

	// Same session encoding as the audio path: file_shared carries no thread_ts, so the
	// event's own ts becomes the thread root.
	threadRoot := ev.Ts
	if threadRoot == "" {
		threadRoot = ev.ThreadTs
	}
	if threadRoot == "" && ev.EventTs != "" {
		threadRoot = ev.EventTs
	}
	if threadRoot == "" {
		threadRoot = strconv.FormatInt(p.EventTime, 10) + ".000000"
	}
	sessionID := fmt.Sprintf("%s/%s#%s", p.TeamID, ev.Channel, threadRoot)

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
		ThreadID:  ev.ThreadTs,
		Timestamp: slackTsToTime(threadRoot, c.clock),
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
			"thread_ts":    ev.ThreadTs,
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
