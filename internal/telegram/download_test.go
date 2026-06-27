package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
)

// rewritingTransport sends every outbound request to targetHost, preserving
// the path so the bot's hardcoded https://api.telegram.org URL still works
// in tests without patching production code.
type rewritingTransport struct {
	base       http.RoundTripper
	targetHost string
}

func (t *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(t.targetHost)
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return t.base.RoundTrip(req)
}

// TestDownloadFileEnforcesSizeCap locks in the download-size defence. A
// hostile file path must not be allowed to fill the disk or RAM through the
// bot's file download channel, regardless of what Telegram (or anything
// posing as Telegram) claims the file size is.
func TestDownloadFileEnforcesSizeCap(t *testing.T) {
	// Temporarily lower the cap so we don't have to stream 100 MiB through
	// httptest. The production behavior is identical at any limit value.
	prev := maxTelegramDownloadBytes
	maxTelegramDownloadBytes = 4 * 1024 // 4 KiB
	defer func() { maxTelegramDownloadBytes = prev }()

	overshoot := maxTelegramDownloadBytes + 1024
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", overshoot))
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 512)
		var written int64
		for written < overshoot {
			n, err := w.Write(chunk)
			if err != nil {
				return
			}
			written += int64(n)
		}
	}))
	defer server.Close()

	hc := &http.Client{
		Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: server.URL},
	}

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "unused"}, chatClient, WithHTTPClient(hc))
	require.NoError(t, err)

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "download.bin")

	err = bot.downloadFile(context.Background(), "fake/path.bin", destPath)
	require.Error(t, err, "expected error when payload exceeds the size cap")
	require.Contains(t, err.Error(), "exceeds",
		"expected explicit 'exceeds … limit' message, got: %v", err)

	// The partial file must not linger on disk.
	_, statErr := os.Stat(destPath)
	require.True(t, os.IsNotExist(statErr),
		"expected downloadFile to remove the partial file on overflow; stat err = %v", statErr)
}

// TestSendDocumentStreams verifies sendDocument's io.Pipe wiring: the
// full file content reaches the upstream HTTP server, the request
// carries the multipart Content-Type, and the body is delivered with
// Transfer-Encoding: chunked (i.e. streamed, not pre-buffered with a
// Content-Length). Without the io.Pipe rewrite the entire artifact
// is loaded into a bytes.Buffer before the request is issued —
// hundred-MB artifacts × concurrent watchers OOM the daemon.
func TestSendDocumentStreams(t *testing.T) {
	payload := strings.Repeat("artifact-byte-", 4096) // ~56 KiB

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "artifact.bin")
	require.NoError(t, os.WriteFile(srcPath, []byte(payload), 0o600))

	var receivedBody []byte
	var receivedTransferEncoding []string
	var receivedContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTransferEncoding = r.TransferEncoding
		receivedContentType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		receivedBody = body
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	hc := &http.Client{
		Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: server.URL},
	}
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "unused"}, chatClient, WithHTTPClient(hc))
	require.NoError(t, err)

	require.NoError(t, bot.sendDocument(context.Background(), 12345, srcPath, "test caption"))

	require.Contains(t, receivedContentType, "multipart/form-data")
	require.Contains(t, receivedTransferEncoding, "chunked",
		"sendDocument must stream via chunked encoding; pre-buffered bodies set Content-Length instead")
	require.Contains(t, string(receivedBody), payload, "full file content must reach the server")
	require.Contains(t, string(receivedBody), "test caption")
	require.Contains(t, string(receivedBody), "12345", "chat_id field must be present")
}

// TestDownloadFileAllowsUnderLimit ensures the cap doesn't break legitimate
// small downloads.
func TestDownloadFileAllowsUnderLimit(t *testing.T) {
	payload := strings.Repeat("ok", 1024) // 2 KiB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()

	hc := &http.Client{
		Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: server.URL},
	}

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "unused"}, chatClient, WithHTTPClient(hc))
	require.NoError(t, err)

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "ok.bin")

	require.NoError(t, bot.downloadFile(context.Background(), "fake/ok.bin", destPath))
	body, err := os.ReadFile(destPath)
	require.NoError(t, err)
	require.Equal(t, payload, string(body))
}
