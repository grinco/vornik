package telegram

// Supervised web-write → operator notification (Telegram side). When a
// web_submit preview lands a pending approval, the daemon calls
// NotifyWebWritePending to alert every operator with a preview screenshot, a
// short summary (project / target host / submission_id) and a DEEP LINK to the
// /inbox approval card.
//
// CRITICAL — NOTIFY-ONLY (LLD §Components.4, invariant I7): this message
// carries NO inline approve/reject callback button or action. The binding
// approve/reject happens exclusively in the authenticated, CSRF-protected
// /inbox POST that mints the one-time token; Telegram never mints it. So this
// deliberately uses the plain sendMessage / sendPhotoBytes paths (no
// InlineKeyboardMarkup), mirroring NotifyScraperBlock's notify-only shape.
// Design: https://docs.vornik.io

import (
	"context"
	"errors"
	"strings"
)

// NotifyWebWritePending delivers a pending-approval alert to every operator
// (config.AllowedUsers). Satisfies a structural notifier seam (primitive params
// → the caller need not import telegram). When screenshotJPEG is non-empty it
// sends a photo with the summary as caption, falling back to a text message if
// the photo upload fails; otherwise text only. Best-effort per recipient — one
// failure doesn't abort the others; the joined error (if any) is returned so
// the caller can record a metric.
//
// It is notify-only: no inline approve/reject callback is attached. The deep
// link in the caption points the operator to the /inbox card where the binding
// decision (and token mint) happens under an authenticated CSRF POST.
func (b *Bot) NotifyWebWritePending(ctx context.Context, project, targetHost, submissionID, inboxURL string, screenshotJPEG []byte) error {
	caption := buildWebWriteCaption(project, targetHost, submissionID, inboxURL)
	var errs []error
	for chatID := range b.config.AllowedUsers {
		var err error
		if len(screenshotJPEG) > 0 {
			if err = b.sendPhotoBytes(ctx, chatID, screenshotJPEG, caption); err != nil {
				// Photo upload failed — still get the actionable text out.
				err = b.sendMessage(ctx, chatID, caption)
			}
		} else {
			err = b.sendMessage(ctx, chatID, caption)
		}
		if err != nil {
			b.logger.Warn().Err(err).Int64("chat_id", chatID).Str("host", targetHost).
				Str("submission_id", submissionID).Msg("web-write notify: send failed")
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildWebWriteCaption composes the plain-text alert. Kept < Telegram's
// 1024-char caption limit; no markup (all fields are system-controlled, so no
// HTML parse mode and no injection surface) and — critically — no inline
// callback: the operator must open the deep link and approve in the
// authenticated /inbox, never from the notification itself.
func buildWebWriteCaption(project, targetHost, submissionID, inboxURL string) string {
	var sb strings.Builder
	sb.WriteString("📝 Web write awaiting approval\n")
	sb.WriteString("Project:    ")
	sb.WriteString(project)
	sb.WriteString("\nTarget:     ")
	sb.WriteString(targetHost)
	sb.WriteString("\nSubmission: ")
	sb.WriteString(submissionID)
	sb.WriteString("\n\nOpen the inbox to review and decide (the binding decision happens only there):\n")
	sb.WriteString(inboxURL)
	return sb.String()
}
