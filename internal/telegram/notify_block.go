package telegram

// Scraper block → operator notification (Telegram side). The daemon's
// mcp.BlockNotifier calls NotifyScraperBlock when the scraper hits a solvable
// gate on a curated portal; this delivers a photo of the challenge (or a text
// fallback) plus the exact `vornikctl scraper login` command to every operator.
// Design: https://docs.vornik.io

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// NotifyScraperBlock delivers a scraper-block alert to every operator
// (config.AllowedUsers). Satisfies mcp.OperatorNotifier structurally (primitive
// params → the mcp package need not import telegram). When screenshotJPEG is
// non-empty it sends a photo with the alert as caption, falling back to a text
// message if the photo upload fails; otherwise text only. Best-effort per
// recipient — one failure doesn't abort the others; the joined error (if any)
// is returned so the caller can record a metric.
func (b *Bot) NotifyScraperBlock(ctx context.Context, project, host, reason, detail, loginCmd string, screenshotJPEG []byte) error {
	caption := buildBlockCaption(project, host, reason, detail, loginCmd)
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
			b.logger.Warn().Err(err).Int64("chat_id", chatID).Str("host", host).
				Msg("scraper block-notify: send failed")
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildBlockCaption composes the plain-text alert. Kept < Telegram's 1024-char
// caption limit; no markup (all fields are system/operator-controlled, so no
// HTML parse mode and no injection surface).
func buildBlockCaption(project, host, reason, detail, loginCmd string) string {
	var sb strings.Builder
	sb.WriteString("🔐 Scraper blocked — action needed\n")
	sb.WriteString("Project: ")
	sb.WriteString(project)
	sb.WriteString("\nPortal:  ")
	sb.WriteString(host)
	sb.WriteString("\nReason:  ")
	sb.WriteString(reason)
	if detail != "" {
		sb.WriteString(" (")
		sb.WriteString(detail)
		sb.WriteString(")")
	}
	sb.WriteString("\n\nSolve it, then the next scan gets through:\n")
	sb.WriteString(loginCmd)
	return sb.String()
}

// sendPhotoBytes uploads a JPEG to a chat as a photo with a caption, via a
// streamed multipart POST to /sendPhoto — mirrors sendDocumentToForumOnce.
// Single attempt (the caller is best-effort and the worker swallows errors).
func (b *Bot) sendPhotoBytes(ctx context.Context, chatID int64, jpeg []byte, caption string) error {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = pw.Close() }()
		defer func() { _ = writer.Close() }()
		if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart chat_id: %w", err))
			return
		}
		if caption != "" {
			if err := writer.WriteField("caption", caption); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("multipart caption: %w", err))
				return
			}
		}
		part, err := writer.CreateFormFile("photo", "challenge.jpg")
		if err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart form file: %w", err))
			return
		}
		if _, err := io.Copy(part, bytes.NewReader(jpeg)); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart copy: %w", err))
			return
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/sendPhoto", b.baseURL), pr)
	if err != nil {
		_ = pr.Close()
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sendPhoto do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return fmt.Errorf("sendPhoto returned %d: %s", resp.StatusCode, string(body))
}
