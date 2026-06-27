package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/scheduler"
	"vornik.io/vornik/internal/secrets"
)

// Phase 29 — Telegram Forum Topics. Each task gets its own topic in
// a designated supergroup so lifecycle events fan out to a
// per-task surface and operator replies route back via the
// message_thread_id field. See
// https://docs.vornik.io

// validForumIconColors is the Telegram-accepted palette for forum
// topic icons. Any value outside this set causes ensureTaskThread
// to omit the field and let Telegram pick a default colour, which
// is a strictly safer fallback than getting createForumTopic
// rejected with "TOPIC_ICON_COLOR_INVALID".
var validForumIconColors = map[int]struct{}{
	7322096:  {}, // blue
	16766590: {}, // yellow
	13338331: {}, // purple
	9367192:  {}, // green
	16749490: {}, // red
	16478047: {}, // orange
}

// maxTopicNameLen mirrors Telegram's createForumTopic constraint
// (1–128 characters). Anything longer must be truncated by the
// caller before the API call.
const maxTopicNameLen = 128

// forumEnabled reports whether forum features are fully wired —
// both a chat to post into and a repo to persist the mapping.
// Either missing → fall back to the existing flat-chat behaviour.
func (b *Bot) forumEnabled() bool {
	return b != nil && b.forumChatID != 0 && b.threadRepo != nil
}

// createForumTopic calls Telegram's createForumTopic and returns
// the assigned message_thread_id. Does NOT store the mapping —
// the caller does that immediately after.
func (b *Bot) createForumTopic(ctx context.Context, name string) (int64, error) {
	if !b.forumEnabled() {
		return 0, errors.New("forum: not enabled")
	}
	if name == "" {
		return 0, errors.New("forum: topic name required")
	}
	if len(name) > maxTopicNameLen {
		name = name[:maxTopicNameLen]
	}

	payload := map[string]any{
		"chat_id": b.forumChatID,
		"name":    name,
	}
	if _, ok := validForumIconColors[b.forumIconColor]; ok {
		payload["icon_color"] = b.forumIconColor
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("forum: marshal createForumTopic: %w", err)
	}

	url := fmt.Sprintf("%s/createForumTopic", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("forum: createForumTopic request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("forum: createForumTopic do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0, fmt.Errorf("forum: createForumTopic read: %w", err)
	}

	var parsed struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageThreadID int64  `json:"message_thread_id"`
			Name            string `json:"name"`
		} `json:"result"`
		Description string `json:"description,omitempty"`
		ErrorCode   int    `json:"error_code,omitempty"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("forum: createForumTopic parse: %w (body=%s)", err, truncateTelegramLogString(string(respBody), 256))
	}
	if !parsed.OK || parsed.Result.MessageThreadID == 0 {
		return 0, fmt.Errorf("forum: createForumTopic failed: code=%d desc=%s", parsed.ErrorCode, parsed.Description)
	}
	return parsed.Result.MessageThreadID, nil
}

// sendForumMessage posts text to a specific forum topic thread.
// Mirrors sendMessage but adds message_thread_id and skips the
// per-message secrets redaction path (which already runs in
// sendMessage and would re-run on the same content otherwise).
// Returns the Telegram message_id.
func (b *Bot) sendForumMessage(ctx context.Context, threadID int64, text string) (int64, error) {
	if text == "" {
		return 0, ErrEmptyMessage
	}
	if threadID == 0 {
		return 0, errors.New("forum: thread_id required")
	}
	if b.forumChatID == 0 {
		return 0, errors.New("forum: chat not configured")
	}

	// Apply the same redaction backstop sendMessage does so a
	// poisoned task summary can't leak via the forum surface
	// either.
	if b.secretsDetector != nil {
		if findings := b.secretsDetector.Scan([]byte(text)); len(findings) > 0 {
			text = string(secrets.Redact([]byte(text), findings))
			b.logger.Warn().
				Int64("thread_id", threadID).
				Int("findings", len(findings)).
				Msg("forum: redacted secret(s) before transmit")
		}
	}

	payload := map[string]any{
		"chat_id":           b.forumChatID,
		"message_thread_id": threadID,
		"text":              text,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/sendMessage", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("forum: sendMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("forum: sendMessage do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0, fmt.Errorf("forum: sendMessage read: %w", err)
	}
	var parsed SendMessageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("forum: sendMessage parse: %w", err)
	}
	if !parsed.OK {
		return 0, fmt.Errorf("forum: sendMessage failed: %s", parsed.Description)
	}
	return parsed.Result.MessageID, nil
}

// sendDocumentToForum mirrors sendDocument but targets a specific
// forum thread via message_thread_id. Streams the file through
// io.Pipe so large artifacts don't buffer the whole payload in
// memory — mirrors the OOM-avoiding approach in sendDocument.
//
// 429 handling: a fan-out of N artifacts to one thread routinely
// trips Telegram's per-chat rate limit (observed 2026-05-18 on
// janka's 4-artifact CV delivery: all four sends got 429 +
// retry_after=19s and were silently dropped). The method now
// parses parameters.retry_after from a 429 body, sleeps that long,
// and retries up to telegramSendRetries times before giving up.
// Each retry rebuilds the multipart pipe from scratch — io.Pipe
// can't be rewound, and `file` is re-opened so a sticky 429 (queue
// + restart cycle) still produces a usable upload.
func (b *Bot) sendDocumentToForum(ctx context.Context, threadID int64, filePath, caption string) error {
	if !b.forumEnabled() {
		return errors.New("forum: not enabled")
	}
	if threadID == 0 {
		return errors.New("forum: thread_id required")
	}
	// Read once. Each retry rewinds the bytes.Reader instead of
	// re-opening the file — io.Pipe in sendDocumentToForumOnce
	// isn't seekable, but a fresh bytes.NewReader per attempt is.
	body, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("forum: open file: %w", err)
	}
	return b.sendDocumentBytesToForum(ctx, threadID, body, filepath.Base(filePath), caption)
}

// sendDocumentBytesToForum is the inner retry loop on a buffered
// body. Callers that already have the bytes (read via a backend-
// aware Store) skip the on-disk read entirely.
func (b *Bot) sendDocumentBytesToForum(ctx context.Context, threadID int64, body []byte, fileName, caption string) error {
	if !b.forumEnabled() {
		return errors.New("forum: not enabled")
	}
	if threadID == 0 {
		return errors.New("forum: thread_id required")
	}
	var lastErr error
	for attempt := 0; attempt < telegramSendRetries; attempt++ {
		retryAfter, err := b.sendDocumentToForumOnce(ctx, threadID, body, fileName, caption)
		if err == nil {
			return nil
		}
		lastErr = err
		if retryAfter <= 0 {
			// Non-429 failure — no point retrying; the next call
			// would just rebuild the multipart and fail the same
			// way. Surface immediately.
			return err
		}
		// Telegram-supplied backoff. Cap at a sane ceiling so a
		// rogue retry_after (or a bug) can't hang the fan-out for
		// hours; the cap is generous enough that real Telegram
		// rate-limits (sub-minute) flow through.
		if retryAfter > telegramRetryAfterCap {
			retryAfter = telegramRetryAfterCap
		}
		b.logger.Warn().
			Err(err).
			Dur("retry_after", retryAfter).
			Int("attempt", attempt+1).
			Int("max_attempts", telegramSendRetries).
			Str("file", fileName).
			Msg("forum: sendDocument rate-limited — sleeping then retrying")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryAfter):
		}
	}
	return lastErr
}

// telegramSendRetries caps the per-document retry budget for 429s.
// Three attempts at retry_after=19s ≈ 38s of sleeping max; that's a
// reasonable wall-clock cost for the deliverable surface and
// well under the per-chat 60s/30 messages bot limit, so the cap
// itself can't compound a backlog.
const telegramSendRetries = 3

// telegramRetryAfterCap bounds a single retry_after to 60s — the
// longest reasonable Telegram-bot rate-limit window. Anything
// beyond that points at a misconfigured retry_after upstream
// (or a daemon bug) and warrants surfacing the failure rather
// than blocking the fan-out indefinitely.
const telegramRetryAfterCap = 60 * time.Second

// sendDocumentToForumOnce makes a single HTTP attempt. Returns
// (retryAfter > 0, error) for 429 responses; (0, error) for any
// other failure; (0, nil) on success. Pulled out so the retry
// wrapper can build a fresh multipart pipe per attempt — io.Pipe
// isn't seekable so we can't reuse the body across retries.
func (b *Bot) sendDocumentToForumOnce(ctx context.Context, threadID int64, body []byte, fileName, caption string) (time.Duration, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = pw.Close() }()
		defer func() { _ = writer.Close() }()

		if err := writer.WriteField("chat_id", strconv.FormatInt(b.forumChatID, 10)); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart chat_id: %w", err))
			return
		}
		if err := writer.WriteField("message_thread_id", strconv.FormatInt(threadID, 10)); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart message_thread_id: %w", err))
			return
		}
		if caption != "" {
			if err := writer.WriteField("caption", caption); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("multipart caption: %w", err))
				return
			}
		}
		part, err := writer.CreateFormFile("document", fileName)
		if err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart form file: %w", err))
			return
		}
		if _, err := io.Copy(part, bytes.NewReader(body)); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart copy: %w", err))
			return
		}
	}()

	url := fmt.Sprintf("%s/sendDocument", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		_ = pr.Close()
		return 0, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("forum: sendDocument do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return 0, nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	wrapped := fmt.Errorf("forum: sendDocument returned %d: %s", resp.StatusCode, string(respBody))
	if resp.StatusCode == http.StatusTooManyRequests {
		return parseTelegramRetryAfter(respBody), wrapped
	}
	return 0, wrapped
}

// parseTelegramRetryAfter extracts parameters.retry_after (seconds)
// from a Telegram 429 response body. Returns 1s on parse failure
// so we don't lose the retry signal entirely when Telegram
// occasionally omits parameters but still 429s.
func parseTelegramRetryAfter(body []byte) time.Duration {
	var parsed struct {
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr == nil && parsed.Parameters.RetryAfter > 0 {
		return time.Duration(parsed.Parameters.RetryAfter) * time.Second
	}
	return time.Second
}

// artifactFanoutCtx returns a detached background context with a
// generous deadline for the whole artifact fan-out. The parent ctx
// (from NotifyTaskCompleted) is bounded at 30s — fine for one
// document, but a fan-out of N artifacts where Telegram drops a
// 429 retry_after=35s on the first one will trip the parent
// deadline mid-retry and silently drop every subsequent document.
//
// 5 minutes covers: up to 20 artifacts × (5s upload + 60s
// retry_after cap × 3 retries) worst case ≈ 4 min 20 s, plus
// slack. The deadline is a ceiling, not a target — typical
// fan-outs finish in seconds.
func artifactFanoutCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

// sendArtifactsToForum ships a completed task's OUTPUT artifacts
// into its forum thread. Mirrors sendArtifactsToWatchers's filter
// (output class, no *-response.md raw dumps) so the operator sees
// the same curated set whether they read the thread or a DM. Each
// document is captioned with the emitting subtask's ID so a
// consolidated tree's artifact stream is still attributable.
//
// 2026-05-16 fix: a task that hits AWAITING_INPUT and later
// COMPLETED triggers NotifyTaskCompleted TWICE (lead_handoff.go
// and workflow.go are independent fire sites). Before this fix
// each call shipped every output artifact, so artifacts produced
// before the handoff were uploaded twice. Now we dedup per
// (thread_id, artifact_id) for the lifetime of the daemon
// process via forumSentArtifacts.
//
// In-memory only — a daemon restart "forgets" past sends and
// would re-ship on the next notify. That's an acceptable
// trade-off: the common case (same task, second notify in a
// minute) is fixed, and the rare case (restart between
// notifies) is no worse than today.
func (b *Bot) sendArtifactsToForum(ctx context.Context, taskID string, threadID int64) {
	if b.artifactRepo == nil {
		return
	}
	filter := persistence.ArtifactFilter{TaskID: &taskID, PageSize: 20}
	artifacts, err := b.artifactRepo.List(ctx, filter)
	if err != nil || len(artifacts) == 0 {
		return
	}
	// The artifact list query uses the caller's ctx (cheap DB read).
	// The actual uploads run under a detached context so Telegram's
	// 429 retry_after (up to 60s) doesn't trip the parent's 30s
	// deadline mid-fan-out — pre-fix the first 429 wiped every
	// subsequent document on the same task. Observed live
	// 2026-05-18 11:48 on janka CV delivery (retry_after=35s vs
	// 30s parent ctx → 16 of 17 artifacts dropped).
	uploadCtx, uploadCancel := artifactFanoutCtx()
	defer uploadCancel()
	suffix := taskID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	sent := 0
	for _, a := range artifacts {
		if a == nil {
			continue
		}
		if a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		if strings.HasSuffix(a.Name, "-response.md") {
			continue
		}
		if b.forumArtifactAlreadySent(threadID, a.ID) {
			b.logger.Debug().Str("artifact_id", a.ID).Str("name", a.Name).Int64("thread_id", threadID).
				Msg("forum: artifact already sent to this thread — skipping duplicate")
			continue
		}
		caption := "from [" + suffix + "]"
		// Phase-4 storage abstraction: route blob reads through
		// Bot.artifactStore.Retrieve when wired (Retrieve was added
		// to the InputArtifactStore interface alongside the S3
		// backend cutover). Filesystem fallback keeps the path-based
		// sender alive for tests + boot paths without a Store.
		var sendErr error
		if b.artifactStore != nil {
			body, rerr := b.artifactStore.Retrieve(uploadCtx, a.ID)
			if rerr != nil {
				b.logger.Warn().Err(rerr).Str("artifact", a.Name).Int64("thread_id", threadID).Msg("forum: failed to read artifact via backend")
				continue
			}
			sendErr = b.sendDocumentBytesToForum(uploadCtx, threadID, body, a.Name, caption)
		} else {
			sendErr = b.sendDocumentToForum(uploadCtx, threadID, a.StoragePath, caption)
		}
		if sendErr != nil {
			b.logger.Warn().Err(sendErr).Str("artifact", a.Name).Int64("thread_id", threadID).Msg("forum: failed to send artifact")
			continue
		}
		b.markForumArtifactSent(threadID, a.ID)
		sent++
	}
	if sent > 0 {
		b.logger.Info().Str("task_id", taskID).Int("artifacts", sent).Int64("thread_id", threadID).Msg("forum: sent output artifacts to thread")
	}
}

// forumArtifactAlreadySent / markForumArtifactSent guard the
// per-(thread, artifact) idempotency set. Threadsafe via
// forumMu. The set is in-memory; daemon restart loses it,
// which we accept as a tradeoff (see sendArtifactsToForum
// comment).
func (b *Bot) forumArtifactAlreadySent(threadID int64, artifactID string) bool {
	if b == nil || artifactID == "" {
		return false
	}
	b.forumMu.Lock()
	defer b.forumMu.Unlock()
	if b.forumSentArtifacts == nil {
		return false
	}
	set, ok := b.forumSentArtifacts[threadID]
	if !ok {
		return false
	}
	_, found := set[artifactID]
	return found
}

func (b *Bot) markForumArtifactSent(threadID int64, artifactID string) {
	if b == nil || artifactID == "" {
		return
	}
	b.forumMu.Lock()
	defer b.forumMu.Unlock()
	if b.forumSentArtifacts == nil {
		b.forumSentArtifacts = make(map[int64]map[string]struct{})
	}
	set, ok := b.forumSentArtifacts[threadID]
	if !ok {
		set = make(map[string]struct{})
		b.forumSentArtifacts[threadID] = set
	}
	set[artifactID] = struct{}{}
}

// closeForumTopic locks a topic so members can't post further
// non-admin messages. Called when a task reaches a terminal
// status. Best-effort — failures log a warning, they don't fail
// the surrounding notification.
func (b *Bot) closeForumTopic(ctx context.Context, threadID int64) error {
	if !b.forumEnabled() || threadID == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id":           b.forumChatID,
		"message_thread_id": threadID,
	})
	url := fmt.Sprintf("%s/closeForumTopic", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("forum: closeForumTopic request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("forum: closeForumTopic do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("forum: closeForumTopic parse: %w", err)
	}
	if !parsed.OK {
		return fmt.Errorf("forum: closeForumTopic failed: %s", parsed.Description)
	}
	return nil
}

// resolveRootTask walks ParentTaskID up to the topmost ancestor and
// returns it. Capped at 10 levels to bound the query count on
// malformed data; a cycle is detected via the visited set. Returns
// the input task unchanged when it has no parent, when the parent
// lookup fails, or when taskRepo is unwired (grandfather path).
func (b *Bot) resolveRootTask(ctx context.Context, task *persistence.Task) *persistence.Task {
	if task == nil || b.taskRepo == nil {
		return task
	}
	const maxDepth = 10
	cur := task
	seen := map[string]bool{cur.ID: true}
	for i := 0; i < maxDepth; i++ {
		if cur.ParentTaskID == nil || *cur.ParentTaskID == "" {
			return cur
		}
		pid := *cur.ParentTaskID
		if seen[pid] {
			return cur
		}
		seen[pid] = true
		parent, err := b.taskRepo.Get(ctx, pid)
		if err != nil || parent == nil {
			// Parent lookup failed (deleted, missing, repo error).
			// Treat the current task as root rather than stranding
			// the topic creation — the operator still gets a topic,
			// just not the cross-tree consolidation.
			return cur
		}
		cur = parent
	}
	return cur
}

// ensureTaskThread gets or creates the forum topic for a task and
// returns its thread_id. Idempotent across concurrent callers via
// a per-task lock; the DB UNIQUE(chat_id, thread_id) is the
// durable guard for cross-process races (multiple bot instances,
// process restart between create and insert).
//
// Subtask consolidation (Phase 2 / T1): a task with a ParentTaskID
// resolves to its ROOT ancestor's topic, so a whole task tree
// shares one Telegram thread. Existing rows keyed by the subtask's
// own ID are honoured (grandfather migration) — only NEW
// hierarchies resolve to root, so an in-flight tree's open topic
// isn't orphaned mid-conversation. Returns (threadID, rootTask)
// so callers can prefix messages with the subtask label and decide
// when to close the topic (only on root terminal status).
func (b *Bot) ensureTaskThread(ctx context.Context, task *persistence.Task) (int64, *persistence.Task, error) {
	if !b.forumEnabled() {
		return 0, task, errors.New("forum: not enabled")
	}
	if task == nil || task.ID == "" {
		return 0, task, errors.New("forum: nil task")
	}

	// Grandfather: if this exact task already owns a topic, use it.
	// Covers two cases: (1) the root path on subsequent events for
	// the root task itself, (2) legacy subtasks that got their own
	// topic before the consolidation rolled out.
	if existing, err := b.threadRepo.GetByTask(ctx, task.ID); err == nil && existing != nil {
		return existing.ThreadID, task, nil
	} else if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return 0, task, fmt.Errorf("forum: GetByTask: %w", err)
	}

	// Resolve the root task — subtasks consolidate into the root's
	// topic. For a root task this is a no-op.
	root := b.resolveRootTask(ctx, task)
	if root.ID != task.ID {
		// Subtask path: check whether the root already owns a topic
		// before paying for the create-lock dance.
		if existing, err := b.threadRepo.GetByTask(ctx, root.ID); err == nil && existing != nil {
			return existing.ThreadID, root, nil
		} else if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return 0, root, fmt.Errorf("forum: GetByTask(root): %w", err)
		}
	}

	// Lock on the root's ID so concurrent first-events for any node
	// in the same tree all serialise on the same key.
	done := b.acquireForumCreateLock(root.ID)
	defer b.releaseForumCreateLock(root.ID, done)

	// Re-check under the lock — a concurrent goroutine may have
	// inserted while we were waiting.
	if existing, err := b.threadRepo.GetByTask(ctx, root.ID); err == nil && existing != nil {
		return existing.ThreadID, root, nil
	}

	name := buildForumTopicName(root)
	threadID, err := b.createForumTopic(ctx, name)
	if err != nil {
		return 0, root, fmt.Errorf("forum: create: %w", err)
	}

	row := &persistence.TelegramTaskThread{
		TaskID:    root.ID,
		ChatID:    b.forumChatID,
		ThreadID:  threadID,
		TopicName: name,
	}
	if err := b.threadRepo.Insert(ctx, row); err != nil {
		if errors.Is(err, persistence.ErrDuplicateKey) {
			// Cross-process race: another instance created and
			// inserted the row between our GetByTask and our
			// Insert. Look up the winner and use that thread.
			if winner, gerr := b.threadRepo.GetByTask(ctx, root.ID); gerr == nil && winner != nil {
				b.logger.Warn().
					Str("task_id", root.ID).
					Int64("orphan_thread", threadID).
					Int64("winner_thread", winner.ThreadID).
					Msg("forum: createForumTopic race lost; orphan topic on Telegram, using winner")
				return winner.ThreadID, root, nil
			}
		}
		return 0, root, fmt.Errorf("forum: insert thread row: %w", err)
	}

	// Post the root task's brief as the first message in the
	// freshly-created topic. Subtasks reuse this topic without a
	// fresh brief — their lifecycle events arrive prefixed with the
	// subtask label so the operator can tell them apart.
	if brief := formatTaskBrief(root); brief != "" {
		if _, err := b.sendForumMessage(ctx, threadID, brief); err != nil {
			b.logger.Warn().
				Err(err).
				Str("task_id", root.ID).
				Int64("thread_id", threadID).
				Msg("forum: failed to post task brief; topic is intact, lifecycle events will still post")
		}
	}

	b.logger.Info().
		Str("task_id", root.ID).
		Int64("thread_id", threadID).
		Str("name", name).
		Msg("forum: created topic for task tree")
	return threadID, root, nil
}

// acquireForumCreateLock returns a channel the caller closes when
// done. Concurrent callers for the same task_id block on the
// existing channel and re-check the DB once the channel closes.
func (b *Bot) acquireForumCreateLock(taskID string) chan struct{} {
	for {
		b.forumCreateMu.Lock()
		ch, busy := b.forumCreateInFly[taskID]
		if !busy {
			done := make(chan struct{})
			b.forumCreateInFly[taskID] = done
			b.forumCreateMu.Unlock()
			return done
		}
		b.forumCreateMu.Unlock()
		<-ch
	}
}

func (b *Bot) releaseForumCreateLock(taskID string, done chan struct{}) {
	b.forumCreateMu.Lock()
	delete(b.forumCreateInFly, taskID)
	b.forumCreateMu.Unlock()
	close(done)
}

// buildForumTopicName composes a 1–128 char topic name from the
// task: "<project-id> • <short-task-suffix>". The chip is for
// scanning the supergroup's topic list — short, project-grouped,
// with the ID suffix as cross-reference to logs / UI. The actual
// task prompt is posted as the first message in the thread by
// postTaskBriefToThread, so the chip stays short.
func buildForumTopicName(task *persistence.Task) string {
	suffix := task.ID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	project := strings.TrimSpace(task.ProjectID)
	if project == "" {
		// No project — use just the suffix (or full ID if short
		// enough). Keep within Telegram's 1–128 char limit.
		if len(task.ID) > maxTopicNameLen {
			return task.ID[:maxTopicNameLen]
		}
		return task.ID
	}
	name := project + " • " + suffix
	if len(name) > maxTopicNameLen {
		// Project IDs shouldn't approach 100+ chars in practice,
		// but trim defensively rather than let Telegram 400 the
		// createForumTopic request.
		name = name[:maxTopicNameLen]
	}
	return name
}

// formatTaskBrief builds the first message posted into a new forum
// topic: a one-line metadata header (task id, project, type,
// priority) followed by the full payload.context.prompt verbatim.
// Anyone arriving cold to the topic sees the task's intent as
// message #1; subsequent lifecycle events (AWAITING_INPUT,
// COMPLETED, FAILED) accumulate below.
//
// Falls back to a minimal "Task <id> created" line when the payload
// carries no prompt — better than posting an empty brief.
func formatTaskBrief(task *persistence.Task) string {
	var sb strings.Builder
	sb.WriteString("📝 Task ")
	sb.WriteString(task.ID)
	if strings.TrimSpace(task.ProjectID) != "" {
		sb.WriteString(" · project: ")
		sb.WriteString(task.ProjectID)
	}
	taskType := taskTypeFromPayload(task.Payload)
	if taskType != "" {
		sb.WriteString(" · type: ")
		sb.WriteString(taskType)
	}
	fmt.Fprintf(&sb, " · priority: %d", task.Priority)

	// Pull the full prompt without the topic-name truncation that
	// taskTitleFromPayload applies — the thread body has no 128-char
	// ceiling so the operator sees the whole brief. Telegram caps
	// sendMessage at 4096 chars; trim with an ellipsis when we get
	// close so the brief never tanks the create-and-post flow.
	prompt := promptFromPayload(task.Payload)
	if prompt != "" {
		sb.WriteString("\n\n")
		const maxPromptInBrief = 3800
		if len(prompt) > maxPromptInBrief {
			prompt = prompt[:maxPromptInBrief] + "\n\n…(prompt truncated; see UI for full text)"
		}
		sb.WriteString(prompt)
	}
	return sb.String()
}

// promptFromPayload returns payload.context.prompt verbatim
// (trimmed) without the topic-name length cap. Empty string when
// the payload has no prompt or fails to parse.
func promptFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var pl struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if err := json.Unmarshal(payload, &pl); err != nil {
		return ""
	}
	return strings.TrimSpace(pl.Context.Prompt)
}

// taskTypeFromPayload returns payload.taskType. Empty when absent.
func taskTypeFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var pl struct {
		TaskType string `json:"taskType"`
	}
	if err := json.Unmarshal(payload, &pl); err != nil {
		return ""
	}
	return strings.TrimSpace(pl.TaskType)
}

// formatTaskEvent builds the human-readable message body posted to
// a task's forum topic. Three shapes:
//
//   - AWAITING_INPUT (lead emitted a checkpoint): includes the
//     checkpoint question and option list when one is open.
//   - terminal success (COMPLETED): completion summary.
//   - terminal failure (FAILED / CANCELLED): error + last_error
//     class when present.
//
// When isSubtask is true the event line is prefixed with the
// short-form task ID so operators can tell which subtask emitted
// it within the root task's consolidated topic. ctx is used to
// load the open checkpoint message; on lookup failure the function
// falls back to the bare summary so the notification still goes
// out.
func (b *Bot) formatTaskEvent(ctx context.Context, task *persistence.Task, success bool, message string, isSubtask bool) string {
	humanized := strings.TrimSpace(humanizeTaskMessage(message))

	// Subtask events ride into the root task's topic — prefix the
	// header so the operator can tell who emitted what without
	// opening the task in the UI.
	subPrefix := ""
	if isSubtask {
		suffix := task.ID
		if len(suffix) > 8 {
			suffix = suffix[len(suffix)-8:]
		}
		subPrefix = "↳ [" + suffix + "] "
	}

	var sb strings.Builder
	switch {
	case task.Status == persistence.TaskStatusAwaitingInput:
		sb.WriteString(subPrefix)
		sb.WriteString("⏸ Task awaiting input")
		if humanized != "" {
			sb.WriteString("\n\n")
			sb.WriteString(humanized)
		}
		b.appendCheckpointDetail(ctx, &sb, task)
		sb.WriteString("\n\nReply in this thread to answer.")

	case success:
		sb.WriteString(subPrefix)
		sb.WriteString("✅ Task completed")
		if humanized != "" {
			if len(humanized) > 800 {
				humanized = humanized[:800] + "…"
			}
			sb.WriteString("\n\n")
			sb.WriteString(humanized)
		}

	default:
		sb.WriteString(subPrefix)
		sb.WriteString("❌ Task failed")
		if humanized != "" {
			if len(humanized) > 500 {
				humanized = humanized[:500] + "…"
			}
			sb.WriteString("\n\n")
			sb.WriteString(humanized)
		}
		if task.LastError != nil && *task.LastError != "" {
			errText := *task.LastError
			if len(errText) > 300 {
				errText = errText[:300] + "…"
			}
			fmt.Fprintf(&sb, "\n\nError: %s", errText)
		}
		if task.LastErrorClass != nil && *task.LastErrorClass != "" {
			fmt.Fprintf(&sb, "\nFailure class: %s", *task.LastErrorClass)
		}
	}
	return sb.String()
}

// appendCheckpointDetail loads the open checkpoint (when one
// exists) and appends its question + options to sb. Best-effort —
// missing repo, missing pointer, or lookup error each falls back
// to "no detail available" silence so the parent event still
// posts.
func (b *Bot) appendCheckpointDetail(ctx context.Context, sb *strings.Builder, task *persistence.Task) {
	if b.taskMessageRepo == nil || task.OpenCheckpointID == nil || *task.OpenCheckpointID == "" {
		return
	}
	cp, err := b.taskMessageRepo.GetOpenCheckpoint(ctx, task.ID)
	if err != nil || cp == nil {
		return
	}
	question := strings.TrimSpace(cp.Content)
	if question == "" {
		return
	}
	sb.WriteString("\n\n📋 Checkpoint:\n")
	if len(question) > 800 {
		question = question[:800] + "…"
	}
	sb.WriteString(question)

	if len(cp.Metadata) == 0 {
		return
	}
	var meta struct {
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal(cp.Metadata, &meta); err != nil || len(meta.Options) == 0 {
		return
	}
	sb.WriteString("\n\nOptions (reply with the ID or label):\n")
	for _, o := range meta.Options {
		fmt.Fprintf(sb, "  • %s — %s\n", o.ID, o.Label)
	}
}

// notifyForumThread is the public entry point called from
// NotifyTaskCompleted. It ensures the task (or its root, for
// subtasks) has a topic, posts the formatted lifecycle event to
// it, ships any OUTPUT artifacts into the same thread, and closes
// the topic on terminal task state — but only when the terminating
// task IS the topic owner, so a subtask completing doesn't strand
// a still-running root.
//
// Returns true when delivery succeeded — callers (NotifyTaskCompleted)
// use this to suppress the DM watcher artifact fanout so the
// operator gets one canonical copy in the thread instead of a
// half-here / half-DM split.
//
// All failures are logged WARN and swallowed beyond the bool —
// forum notifications are best-effort, the watcher-based push
// path remains available as a fallback when forum delivery fails.
func (b *Bot) notifyForumThread(ctx context.Context, task *persistence.Task, success bool, message string) bool {
	if !b.forumEnabled() || task == nil {
		return false
	}
	threadID, root, err := b.ensureTaskThread(ctx, task)
	if err != nil {
		b.logger.Warn().Err(err).Str("task_id", task.ID).Msg("forum: ensureTaskThread failed; skipping notification")
		return false
	}
	isSubtask := root.ID != task.ID
	text := b.formatTaskEvent(ctx, task, success, message, isSubtask)
	if _, err := b.sendForumMessage(ctx, threadID, text); err != nil {
		b.logger.Warn().Err(err).Str("task_id", task.ID).Int64("thread_id", threadID).Msg("forum: sendForumMessage failed")
		return false
	}

	// Ship artifacts into the same thread on success. Mirrors the
	// DM-side gate (sendArtifactsToWatchers fires on success, not
	// on terminal status) — IsTerminalTaskStatus deliberately
	// excludes COMPLETED in this codebase (conversational
	// lifecycle: tasks can be reopened), so gating artifact upload
	// on terminal would silently drop the common COMPLETED case.
	if success && b.artifactRepo != nil {
		b.sendArtifactsToForum(ctx, task.ID, threadID)
	}

	// Only close the topic when the terminating task is the topic
	// owner. For subtasks, the root task's eventual terminal status
	// is what closes the thread; closing on the first subtask
	// completion would shut the operator out of the still-running
	// root + sibling subtasks.
	if scheduler.IsTerminalTaskStatus(task.Status) && !isSubtask {
		if err := b.closeForumTopic(ctx, threadID); err != nil {
			b.logger.Warn().Err(err).Str("task_id", task.ID).Int64("thread_id", threadID).Msg("forum: closeForumTopic failed")
			return true
		}
		if err := b.threadRepo.MarkClosed(ctx, task.ID); err != nil {
			b.logger.Warn().Err(err).Str("task_id", task.ID).Msg("forum: MarkClosed failed (topic closed on telegram, DB out of sync)")
		}
	}
	return true
}
