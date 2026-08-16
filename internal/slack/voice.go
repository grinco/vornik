package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/voice"
)

// VoiceProviders bundles the STT + TTS implementations the Slack
// channel uses for audio handling (slice 4 of the voice MVP). Either
// or both may be nil to disable a direction:
//
//   - nil STT skips inbound transcription. Slack file_shared events
//     with audio MIME still arrive but the file is ignored — the
//     channel doesn't currently surface generic file attachments
//     anyway (slice-1 Slack scope was "file attachment ingestion is
//     out", so the voice path is additive).
//   - nil TTS keeps outbound replies as chat.postMessage text even
//     when the inbound was an audio clip.
//
// MaxOutboundDuration caps synthesised audio against Slack's 5-minute
// audio-clip cap (the platform plays longer files but the seekable
// UI degrades). Zero falls back to slackAudioMaxDurationMs.
type VoiceProviders struct {
	STT                 voice.STTProvider
	TTS                 voice.TTSProvider
	MaxOutboundDuration int64 // milliseconds
}

const (
	// slackAudioMaxDurationMs is the platform-default audio-clip
	// length cap. Slack accepts longer files but the inline player
	// truncates the seekable region; treating 5 minutes as the
	// MVP-supported ceiling mirrors the design doc's §"Length cap".
	slackAudioMaxDurationMs int64 = 300_000
	// maxOutboundFileBytes matches the dispatcher's cross-channel artifact
	// envelope. It also keeps a malicious/accidental reader from making the
	// daemon buffer an unbounded document before Slack returns an upload URL.
	maxOutboundFileBytes int64 = 50 * 1024 * 1024
)

// audioMIMEs is the set of inbound Content-Type prefixes the channel
// treats as transcribable. Slack's "audio clip" feature surfaces as
// audio/mp4; an audio attachment uploaded from a desktop client can
// arrive as audio/mpeg, audio/webm, or audio/ogg depending on the
// browser. The list is the design-doc-blessed superset.
var audioMIMEs = []string{
	"audio/",
}

// isAudioMime reports whether the given MIME (or Content-Type
// prefix) signals an audio file the channel should route through
// STT. Case-insensitive on the MIME prefix.
func isAudioMime(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if m == "" {
		return false
	}
	for _, prefix := range audioMIMEs {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

// voiceTracker remembers per-(team_id, channel_id, thread_ts) which
// session's most-recent inbound was an audio clip. Channel.Send
// consults it on outbound to decide between chat.postMessage and
// files.upload_v2. Same memory-only / restart-resets posture as the
// Telegram tracker (see internal/telegram/voice.go).
type voiceTracker struct {
	mu  sync.RWMutex
	set map[string]bool
}

func newVoiceTracker() *voiceTracker { return &voiceTracker{set: make(map[string]bool)} }

// MarkVoice records that sessionID's latest inbound was audio.
func (t *voiceTracker) MarkVoice(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.set[sessionID] = true
}

// MarkText clears the audio mark for sessionID — a typed message
// resets the channel back to text-mode replies.
func (t *voiceTracker) MarkText(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.set, sessionID)
}

// IsVoice reports whether sessionID's latest inbound was audio.
// Nil-receiver safe.
func (t *voiceTracker) IsVoice(sessionID string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.set[sessionID]
}

// slackFile is the relevant subset of Slack's file payload (returned
// inline on message events with the `files` field, OR on
// file_shared events via files.info). We lift only the fields the
// voice flow needs.
type slackFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	URLPrivateDownload string `json:"url_private_download"`
	URLPrivate         string `json:"url_private"`
	Filetype           string `json:"filetype"`
	Size               int64  `json:"size"`
	// Shares tells us WHERE the file was posted — the message ts and, when
	// the file was shared as a thread reply, the thread_ts. file_shared events
	// carry neither, so this is how the image path recovers the conversation
	// context it needs to apply the same engagement gate as text messages
	// (2026-08-01: the bot described an image posted in an unrelated thread).
	Shares *slackFileShares `json:"shares,omitempty"`
}

// slackFileShares mirrors files.info's file.shares: channel_id → the shares in
// that channel (public or private).
type slackFileShares struct {
	Public  map[string][]slackFileShare `json:"public"`
	Private map[string][]slackFileShare `json:"private"`
}

type slackFileShare struct {
	Ts       string `json:"ts"`                  // the message ts the file rode on
	ThreadTs string `json:"thread_ts,omitempty"` // set when shared inside a thread
}

// shareIn returns the message ts + thread_ts for the file's share in channelID,
// looking in both the public and private share maps. ok=false when the file has
// no recorded share in that channel (we then cannot gate it and drop
// conservatively rather than answer unconditionally).
func (m *slackFile) shareIn(channelID string) (msgTs, threadTs string, ok bool) {
	if m == nil || m.Shares == nil {
		return "", "", false
	}
	for _, set := range []map[string][]slackFileShare{m.Shares.Public, m.Shares.Private} {
		if shares, found := set[channelID]; found && len(shares) > 0 {
			return shares[0].Ts, shares[0].ThreadTs, true
		}
	}
	return "", "", false
}

// filesInfoResponse is the relevant subset of Slack's files.info Web
// API response. The full envelope carries the user, channels,
// shares, comments — we only need the file.
type filesInfoResponse struct {
	OK    bool       `json:"ok"`
	Error string     `json:"error,omitempty"`
	File  *slackFile `json:"file,omitempty"`
}

// fetchSlackFileBytes downloads the binary payload of a Slack file
// using the installation's bot token as Authorization. Slack file
// downloads require `Authorization: Bearer <bot_token>` even when
// the URL is unguessable, so this isn't a "use the public URL"
// path. Returns the raw bytes.
func (c *Channel) fetchSlackFileBytes(ctx context.Context, inst *installation, url string) ([]byte, error) {
	if strings.TrimSpace(inst.botToken) == "" {
		return nil, fmt.Errorf("slack channel: cannot fetch file — installation %q has no bot token", inst.teamID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+inst.botToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack channel: file fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack channel: file fetch HTTP %d", resp.StatusCode)
	}
	const maxFileBytes = 64 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxFileBytes {
		return nil, fmt.Errorf("slack channel: file exceeds %d byte limit", maxFileBytes)
	}
	return body, nil
}

// fetchSlackFileMeta calls files.info to resolve a file_id into the
// download URL + MIME. Used by the file_shared event handler (which
// only gets the id on the wire). Mirrors the rest-of-package POST-
// to-Web-API shape but uses GET here since files.info is documented
// as GET-friendly.
func (c *Channel) fetchSlackFileMeta(ctx context.Context, inst *installation, fileID string) (*slackFile, error) {
	url := c.apiBaseURL + "/files.info?file=" + fileID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+inst.botToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack channel: files.info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutboundResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack channel: files.info HTTP %d: %s",
			resp.StatusCode, truncateBody(string(respBody)))
	}
	var parsed filesInfoResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("slack channel: files.info parse: %w", err)
	}
	if !parsed.OK || parsed.File == nil {
		return nil, fmt.Errorf("slack channel: files.info error: %s", parsed.Error)
	}
	return parsed.File, nil
}

// handleFileSharedEvent is the slice-4 entry point for inbound audio.
// Triggered by HandleWebhook when the event_callback's inner type is
// "file_shared" and the file's MIME is audio/*. The flow:
//
//  1. Per-installation allowlist gate (channel + sender). Mirrors
//     handleMessageEvent's pattern so audio doesn't sidestep the
//     v1 gate.
//  2. files.info to resolve the file's download URL + MIME.
//  3. fetch the binary bytes (bot-token-authorised GET).
//  4. STTProvider.Transcribe.
//  5. build a ChannelMessage with Text = transcript, voice.* tags
//     on ChannelSpecific, and hand off to the bound Receiver.
//  6. mark the session tracker so the outbound Channel.Send routes
//     through TTS + files.upload_v2.
//
// On any failure short of step 5 the function returns without
// firing the Receiver (no LLM spend on a payload we couldn't
// decode). A user-visible humane error reply rides the existing
// chat.postMessage path — implementation detail of the per-call
// failure-recovery routine; the file_shared handler logs + drops
// rather than retrying, because file_shared events don't carry a
// user-facing ack semantics like message events do.
func (c *Channel) handleFileSharedEvent(ctx context.Context, p eventPayload, inst *installation) {
	ev := p.Event
	if ev == nil || ev.File == nil || strings.TrimSpace(ev.File.ID) == "" {
		c.logger.Debug().Str("event_id", p.EventID).Msg("slack: file_shared event without file payload")
		return
	}
	if !channelAllowed(inst, ev.ChannelType, ev.Channel) {
		c.logger.Warn().
			Str("team_id", p.TeamID).
			Str("channel", ev.Channel).
			Msg("slack: file_shared channel not on allowlist; dropping")
		return
	}
	if _, err := c.resolveSpeakerForInstallation(inst, ev.User); err != nil {
		c.logger.Warn().Str("team_id", p.TeamID).Str("user", ev.User).
			Msg("slack: file_shared sender not on allowlist; dropping")
		return
	}

	meta, err := c.fetchSlackFileMeta(ctx, inst, ev.File.ID)
	if err != nil {
		c.logger.Warn().Err(err).Str("file_id", ev.File.ID).Msg("slack: files.info failed")
		return
	}
	// Route by what the file IS. Audio is transcribed here and rides on as text; an
	// image rides on as an attachment for the vision path to inline or hand over;
	// anything else is still ignored, since no downstream path claims it.
	switch {
	case isImageMime(meta.Mimetype):
		c.dispatchImageFile(ctx, p, inst, meta)
		return
	case !isAudioMime(meta.Mimetype):
		c.logger.Debug().Str("file_id", meta.ID).Str("mime", meta.Mimetype).
			Msg("slack: file_shared MIME is neither audio nor image; ignoring")
		return
	}
	if c.voice.STT == nil {
		c.logger.Debug().Str("team_id", p.TeamID).Str("file_id", meta.ID).
			Msg("slack: audio file_shared but no STT wired; dropping")
		return
	}
	downloadURL := meta.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = meta.URLPrivate
	}
	audioBytes, err := c.fetchSlackFileBytes(ctx, inst, downloadURL)
	if err != nil {
		c.logger.Warn().Err(err).Str("file_id", meta.ID).Msg("slack: file fetch failed")
		return
	}
	tr, err := c.voice.STT.Transcribe(ctx, bytes.NewReader(audioBytes), voice.Hint{MimeType: meta.Mimetype})
	if err != nil {
		c.logger.Warn().Err(err).Str("file_id", meta.ID).Msg("slack: STT failed")
		return
	}

	// Build ChannelMessage. SessionID encoding matches buildMessageChannelMessage:
	// `<team_id>/<channel_id>#<thread_ts>` — file_shared events
	// don't carry a thread_ts so we use event_ts as the thread root.
	threadRoot := ev.Ts
	if threadRoot == "" {
		threadRoot = ev.ThreadTs
	}
	if threadRoot == "" && ev.EventTs != "" {
		threadRoot = ev.EventTs
	}
	if threadRoot == "" {
		// Defensive: synthesise a thread root from the payload's
		// event_time so SessionID has a stable third component.
		threadRoot = strconv.FormatInt(p.EventTime, 10) + ".000000"
	}
	sessionID := fmt.Sprintf("%s/%s#%s", p.TeamID, ev.Channel, threadRoot)
	cs := map[string]string{
		"team_id":       p.TeamID,
		"channel_id":    ev.Channel,
		"channel_type":  ev.ChannelType,
		"thread_ts":     threadRoot,
		"event_id":      p.EventID,
		"event_type":    "file_shared",
		"project_id":    inst.projectID,
		"file_id":       meta.ID,
		"file_mime":     meta.Mimetype,
		"voice.inbound": "true",
	}
	if tr.DurationMs > 0 {
		cs["voice.duration_ms"] = strconv.FormatInt(tr.DurationMs, 10)
	}
	if tr.Confidence > 0 {
		cs["voice.transcript_confidence"] = strconv.FormatFloat(tr.Confidence, 'f', 4, 64)
	}
	if tr.Language != "" {
		cs["voice.language"] = tr.Language
	}

	ts := slackTsToTime(threadRoot, c.clock)
	msg := conversation.ChannelMessage{
		Source:          channelName,
		ID:              ev.Ts,
		SessionID:       sessionID,
		SpeakerID:       ev.User,
		Text:            tr.Text,
		ThreadID:        threadRoot,
		Timestamp:       ts,
		ChannelSpecific: cs,
	}
	c.recordSession(sessionID, channelTitleFromPayload(p), ev.User, msg.Timestamp, inst)

	if c.voiceTracker == nil {
		c.voiceTracker = newVoiceTracker()
	}
	c.voiceTracker.MarkVoice(sessionID)

	c.recvMu.RLock()
	recvAny := c.recv
	c.recvMu.RUnlock()
	if recvAny == nil {
		c.logger.Warn().Str("event_id", p.EventID).Msg("slack: file_shared transcribed but no Receiver bound; dropping")
		return
	}
	recv, ok := recvAny.(conversation.Receiver)
	if !ok {
		c.logger.Error().Str("event_id", p.EventID).Msg("slack: bound Receiver does not implement conversation.Receiver; dropping")
		return
	}
	if err := recv.Receive(ctx, msg); err != nil {
		c.logger.Warn().Err(err).Str("event_id", p.EventID).Msg("slack: file_shared Receiver.Receive returned error")
	}
}

// uploadAudioParams bundles the inputs to a Slack files.upload_v2
// call. The two-step upload (getUploadURLExternal +
// completeUploadExternal) is implemented as one logical operation
// here so the channel's Send path stays simple.
type uploadAudioParams struct {
	Channel  string
	ThreadTs string
	Filename string
	Audio    voice.Audio
}

// uploadFileParams is the channel-generic payload for Slack's external upload
// API. The voice path adapts voice.Audio into this shape; dispatcher artifacts
// use it directly.
type uploadFileParams struct {
	Channel        string
	ThreadTs       string
	Filename       string
	Bytes          []byte
	InitialComment string
}

// getUploadURLResponse mirrors the relevant subset of
// files.getUploadURLExternal's response.
type getUploadURLResponse struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	UploadURL string `json:"upload_url,omitempty"`
}

// completeUploadRequest is the body of files.completeUploadExternal.
// Slack accepts both JSON and form-encoded; we use JSON for symmetry
// with chat.postMessage.
type completeUploadRequest struct {
	Files          []completeUploadFile `json:"files"`
	ChannelID      string               `json:"channel_id,omitempty"`
	ThreadTs       string               `json:"thread_ts,omitempty"`
	InitialComment string               `json:"initial_comment,omitempty"`
}

type completeUploadFile struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

type completeUploadResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Files []struct {
		ID string `json:"id"`
	} `json:"files,omitempty"`
}

// uploadAudioV2 implements the slice-4 outbound audio path via the
// files.upload_v2 family. Returns the new file_id (used as the
// upstream message id for InReplyTo correlation).
func (c *Channel) uploadAudioV2(ctx context.Context, inst *installation, p uploadAudioParams) (string, error) {
	if len(p.Audio.Bytes) == 0 {
		return "", errors.New("slack channel: uploadAudio with empty audio")
	}
	return c.uploadFileV2(ctx, inst, uploadFileParams{
		Channel: p.Channel, ThreadTs: p.ThreadTs, Filename: p.Filename, Bytes: p.Audio.Bytes,
	})
}

// uploadFileV2 implements Slack's files.upload_v2 family for arbitrary
// documents. Returns the new file_id.
func (c *Channel) uploadFileV2(ctx context.Context, inst *installation, p uploadFileParams) (string, error) {
	if strings.TrimSpace(inst.botToken) == "" {
		return "", ErrOutboundNotConfigured
	}
	if len(p.Bytes) == 0 {
		return "", errors.New("slack channel: upload file is empty")
	}
	target, err := c.getExternalUploadURL(ctx, inst, p.Filename, len(p.Bytes))
	if err != nil {
		return "", err
	}
	if err := c.postExternalUpload(ctx, target.UploadURL, p.Filename, p.Bytes); err != nil {
		return "", err
	}
	if err := c.completeExternalUpload(ctx, inst, target.FileID, p); err != nil {
		return "", err
	}
	return target.FileID, nil
}

func (c *Channel) getExternalUploadURL(ctx context.Context, inst *installation, fileName string, size int) (getUploadURLResponse, error) {
	urlGet := fmt.Sprintf("%s/files.getUploadURLExternal?filename=%s&length=%d",
		c.apiBaseURL, url.QueryEscape(fileName), size)
	reqGet, err := http.NewRequestWithContext(ctx, http.MethodGet, urlGet, nil)
	if err != nil {
		return getUploadURLResponse{}, err
	}
	reqGet.Header.Set("Authorization", "Bearer "+inst.botToken)
	respGet, err := c.httpClient.Do(reqGet)
	if err != nil {
		return getUploadURLResponse{}, fmt.Errorf("slack channel: getUploadURLExternal: %w", err)
	}
	bodyGet, _ := io.ReadAll(io.LimitReader(respGet.Body, maxOutboundResponseBytes))
	_ = respGet.Body.Close()
	if respGet.StatusCode != http.StatusOK {
		return getUploadURLResponse{}, fmt.Errorf("slack channel: getUploadURLExternal HTTP %d: %s",
			respGet.StatusCode, truncateBody(string(bodyGet)))
	}
	var resGet getUploadURLResponse
	if err := json.Unmarshal(bodyGet, &resGet); err != nil {
		return getUploadURLResponse{}, fmt.Errorf("slack channel: getUploadURLExternal parse: %w", err)
	}
	if !resGet.OK || resGet.UploadURL == "" || resGet.FileID == "" {
		return getUploadURLResponse{}, fmt.Errorf("slack channel: getUploadURLExternal: ok=%v err=%q url=%q id=%q",
			resGet.OK, resGet.Error, resGet.UploadURL, resGet.FileID)
	}
	return resGet, nil
}

func (c *Channel) postExternalUpload(ctx context.Context, uploadURL, fileName string, data []byte) error {
	// POST the bytes to the upload URL as multipart/form-data
	// with field name "file". Slack's external upload accepts a
	// straight file part — no Content-Type required (it picks up
	// the binary as-is) but we set it for hygiene.
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = pw.Close() }()
		defer func() { _ = writer.Close() }()
		part, err := writer.CreateFormFile("file", fileName)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := part.Write(data); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()
	reqUp, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, pr)
	if err != nil {
		_ = pr.Close()
		return err
	}
	reqUp.Header.Set("Content-Type", writer.FormDataContentType())
	respUp, err := c.httpClient.Do(reqUp)
	if err != nil {
		return fmt.Errorf("slack channel: upload POST: %w", err)
	}
	bodyUp, _ := io.ReadAll(io.LimitReader(respUp.Body, maxOutboundResponseBytes))
	_ = respUp.Body.Close()
	if respUp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack channel: upload POST HTTP %d: %s",
			respUp.StatusCode, truncateBody(string(bodyUp)))
	}
	return nil
}

func (c *Channel) completeExternalUpload(ctx context.Context, inst *installation, fileID string, p uploadFileParams) error {
	// Completing the upload is what surfaces the file in the channel.
	completeReq := completeUploadRequest{
		Files:          []completeUploadFile{{ID: fileID, Title: p.Filename}},
		ChannelID:      p.Channel,
		ThreadTs:       p.ThreadTs,
		InitialComment: p.InitialComment,
	}
	cbody, _ := json.Marshal(completeReq)
	reqDone, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBaseURL+"/files.completeUploadExternal", bytes.NewReader(cbody))
	if err != nil {
		return err
	}
	reqDone.Header.Set("Authorization", "Bearer "+inst.botToken)
	reqDone.Header.Set("Content-Type", "application/json")
	respDone, err := c.httpClient.Do(reqDone)
	if err != nil {
		return fmt.Errorf("slack channel: completeUploadExternal: %w", err)
	}
	defer func() { _ = respDone.Body.Close() }()
	bodyDone, _ := io.ReadAll(io.LimitReader(respDone.Body, maxOutboundResponseBytes))
	if respDone.StatusCode != http.StatusOK {
		return fmt.Errorf("slack channel: completeUploadExternal HTTP %d: %s",
			respDone.StatusCode, truncateBody(string(bodyDone)))
	}
	var resDone completeUploadResponse
	if err := json.Unmarshal(bodyDone, &resDone); err != nil {
		return fmt.Errorf("slack channel: completeUploadExternal parse: %w", err)
	}
	if !resDone.OK {
		return fmt.Errorf("slack channel: completeUploadExternal: %s", resDone.Error)
	}
	return nil
}

// SendFile uploads an arbitrary document into the Slack destination encoded by
// sessionID. It is intentionally streaming at the dispatcher boundary even
// though Slack requires the byte length before issuing an upload URL.
func (c *Channel) SendFile(ctx context.Context, sessionID, fileName string, content io.Reader, caption string) (string, error) {
	if c == nil {
		return "", errors.New("slack channel: nil channel")
	}
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		return "", errors.New("slack channel: file name is required")
	}
	if content == nil {
		return "", errors.New("slack channel: file content is required")
	}
	teamID, channelID, threadRoot, err := parseSlackSessionID(sessionID)
	if err != nil {
		return "", err
	}
	inst, ok := c.installationsByID[teamID]
	if !ok {
		return "", fmt.Errorf("%w: team_id %q not configured", ErrUnknownSession, teamID)
	}
	data, err := io.ReadAll(io.LimitReader(content, maxOutboundFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("slack channel: read outbound file: %w", err)
	}
	if int64(len(data)) > maxOutboundFileBytes {
		return "", fmt.Errorf("slack channel: outbound file exceeds %d byte limit", maxOutboundFileBytes)
	}
	return c.uploadFileV2(ctx, inst, uploadFileParams{
		Channel:        channelID,
		ThreadTs:       resolveThreadTs(threadRoot),
		Filename:       fileName,
		Bytes:          data,
		InitialComment: strings.TrimSpace(caption),
	})
}

// sendVoiceReply is the integration helper Channel.Send calls when
// the session tracker indicates the inbound was audio. Synthesises
// mp4-aac (Slack-native) and uploads via files.upload_v2; returns
// (sentID, true, nil) on success. On TTS failure or oversize audio
// returns ("", false, nil) so the caller falls back to text. On
// upload failure returns ("", false, err) so the caller logs and
// also falls back.
//
// Length-cap decision (same rationale as Telegram slice 3): when the
// synthesised audio exceeds Slack's 5-min UX cap, fall back to text
// rather than truncate or split.
func (c *Channel) sendVoiceReply(ctx context.Context, inst *installation, p uploadAudioParams, text string) (sentID string, used bool, err error) {
	if c.voice.TTS == nil {
		return "", false, nil
	}
	// Rich text is for READING. A synthesiser handed mrkdwn pronounces the asterisks and
	// spells a URL character by character, so the spoken form is derived here — labelled
	// links become their label, bare links are not dictated, emoji and entity refs go.
	// Operator instruction 2026-07-30. The text sent as a FALLBACK below is untouched,
	// because that one is read.
	spoken := voice.ForSpeech(text)
	if strings.TrimSpace(spoken) == "" {
		// Nothing left worth listening to (a reply that was only a link). Fall back to
		// text rather than upload silence.
		return "", false, nil
	}
	audio, synthErr := c.voice.TTS.Synthesize(ctx, spoken, voice.TTSOptions{Format: "mp4-aac"})
	if synthErr != nil {
		c.logger.Info().Err(synthErr).Str("session", p.Channel+"/"+p.ThreadTs).
			Msg("slack: TTS Synthesize failed; falling back to text")
		return "", false, nil
	}
	cap := c.voice.MaxOutboundDuration
	if cap <= 0 {
		cap = slackAudioMaxDurationMs
	}
	if audio.DurationMs > cap {
		c.logger.Info().
			Int64("audio_ms", audio.DurationMs).
			Int64("cap_ms", cap).
			Msg("slack: synthesised audio exceeds platform cap; falling back to text")
		return "", false, nil
	}
	p.Audio = audio
	if p.Filename == "" {
		p.Filename = "reply.m4a"
	}
	fileID, err := c.uploadAudioV2(ctx, inst, p)
	if err != nil {
		return "", false, err
	}
	return fileID, true, nil
}

// shouldReplyAsVoice consults the tracker for the channel's
// voice-vs-text decision. Returns true when the session's most-recent
// inbound was an audio clip AND a TTS provider is wired.
func (c *Channel) shouldReplyAsVoice(sessionID string) bool {
	if c.voiceTracker == nil || c.voice.TTS == nil {
		return false
	}
	return c.voiceTracker.IsVoice(sessionID)
}
