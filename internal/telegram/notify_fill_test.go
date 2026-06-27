package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// telegramRecorder is an httptest server that buffers every
// outbound sendMessage call. Tests assert against r.text() to see
// what the bot would have sent.
type telegramRecorder struct {
	mu       sync.Mutex
	messages []sentMessage
	server   *httptest.Server
}

type sentMessage struct {
	ChatID int64
	Text   string
}

func newTelegramRecorder(t *testing.T) *telegramRecorder {
	t.Helper()
	r := &telegramRecorder{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var sm SendMessageRequest
		_ = json.Unmarshal(body, &sm)
		r.mu.Lock()
		r.messages = append(r.messages, sentMessage{ChatID: sm.ChatID, Text: sm.Text})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *telegramRecorder) snapshot() []sentMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sentMessage, len(r.messages))
	copy(out, r.messages)
	return out
}

// fillTestRegistry builds a registry with one project carrying
// notify_fills_chat_id. Used to exercise the project-lookup branch
// in NotifyFill.
func fillTestRegistry(t *testing.T, projectID string, notifyChatID int64) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	yaml := `projectId: ` + projectID + `
displayName: Trading Project
swarmId: swarm-1
defaultWorkflowId: wf-1
trading:
  mode: paper
  notify_fills_chat_id: ` + intToStr(notifyChatID) + `
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p.yaml"), []byte(yaml), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "s.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	out := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		out = string(rune('0'+(n%10))) + out
		n /= 10
	}
	if neg {
		out = "-" + out
	}
	return out
}

func newTestBotWithRecorder(t *testing.T, reg *registry.Registry, rec *telegramRecorder) *Bot {
	t.Helper()
	chatClient := chat.NewClient("https://example.com", "k", "m")
	bot, err := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithHTTPClient(rec.server.Client()),
		WithRegistry(reg),
	)
	require.NoError(t, err)
	bot.baseURL = rec.server.URL
	bot.logger = zerolog.Nop()
	return bot
}

// TestNotifyFill_SingleFillSendsAfterDebounceWindow — happy path:
// one fill arrives, the debouncer waits fillDebounceWindow, then
// flushes a single-fill message to the configured chat.
func TestNotifyFill_SingleFillSendsAfterDebounceWindow(t *testing.T) {
	rec := newTelegramRecorder(t)
	reg := fillTestRegistry(t, "trader-1", 1234567)
	bot := newTestBotWithRecorder(t, reg, rec)

	bot.NotifyFill(context.Background(), &persistence.TradingFill{
		ID: "f1", OrderID: "ord-1", ProjectID: "trader-1", Symbol: "AAPL",
		Qty: 10, Price: 200,
	})

	// Wait long enough for the debounce timer to fire and the
	// async send to complete. fillDebounceWindow is 2s so 3s
	// covers it with margin.
	require.Eventually(t, func() bool {
		return len(rec.snapshot()) == 1
	}, 3*time.Second, 50*time.Millisecond, "expected one message after debounce window")

	got := rec.snapshot()[0]
	assert.Equal(t, int64(1234567), got.ChatID, "must target the project's notify_fills_chat_id")
	assert.Contains(t, got.Text, "AAPL")
	assert.Contains(t, got.Text, "ord-1")
	assert.Contains(t, got.Text, "Qty: 10")
}

// TestNotifyFill_PartialFillsCollapsedIntoSingleMessage —
// the headline contract for the debouncer: 4 fills on the same
// order arriving within the window collapse into ONE message
// with aggregated qty + weighted-avg price.
func TestNotifyFill_PartialFillsCollapsedIntoSingleMessage(t *testing.T) {
	rec := newTelegramRecorder(t)
	reg := fillTestRegistry(t, "trader-1", 1234567)
	bot := newTestBotWithRecorder(t, reg, rec)

	for i, qty := range []float64{2, 3, 1, 4} {
		bot.NotifyFill(context.Background(), &persistence.TradingFill{
			ID:        "f" + intToStr(int64(i)),
			OrderID:   "ord-1",
			ProjectID: "trader-1",
			Symbol:    "AAPL",
			Qty:       qty,
			Price:     100 + float64(i),
		})
		// Small inter-fill gap inside the window — debounce must
		// reset the timer on each call so all four roll up
		// together rather than each firing its own message.
		time.Sleep(50 * time.Millisecond)
	}

	require.Eventually(t, func() bool {
		return len(rec.snapshot()) == 1
	}, 3*time.Second, 50*time.Millisecond)

	got := rec.snapshot()[0]
	assert.Contains(t, got.Text, "4 fills aggregated",
		"the message must report the aggregate count")
	assert.Contains(t, got.Text, "AAPL")
	// Total qty 2+3+1+4 = 10. weighted by price 100,101,102,103 = 200+303+102+412 = 1017.
	assert.Contains(t, got.Text, "Total qty: 10")
}

// TestNotifyFill_NoChatIDSkipsSend — projects without
// notify_fills_chat_id configured silently no-op. This is the
// default for non-trading projects (which still go through the
// fill ingest path under shared brokers).
func TestNotifyFill_NoChatIDSkipsSend(t *testing.T) {
	rec := newTelegramRecorder(t)
	reg := fillTestRegistry(t, "no-notify", 0)
	bot := newTestBotWithRecorder(t, reg, rec)

	bot.NotifyFill(context.Background(), &persistence.TradingFill{
		ID: "f1", OrderID: "ord-1", ProjectID: "no-notify", Symbol: "AAPL",
		Qty: 10, Price: 200,
	})

	// Wait the same amount as the happy-path test; we expect
	// nothing to land in the recorder.
	time.Sleep(2*time.Second + 200*time.Millisecond)
	assert.Empty(t, rec.snapshot(),
		"no notify_fills_chat_id means no fill notifications")
}

// TestNotifyFill_DifferentOrdersFireSeparately — the debouncer is
// per (project, order_id). Fills on two different orders must
// produce two messages, not one aggregate.
func TestNotifyFill_DifferentOrdersFireSeparately(t *testing.T) {
	rec := newTelegramRecorder(t)
	reg := fillTestRegistry(t, "trader-1", 1234567)
	bot := newTestBotWithRecorder(t, reg, rec)

	bot.NotifyFill(context.Background(), &persistence.TradingFill{
		ID: "f1", OrderID: "ord-A", ProjectID: "trader-1", Symbol: "AAPL",
		Qty: 1, Price: 100,
	})
	bot.NotifyFill(context.Background(), &persistence.TradingFill{
		ID: "f2", OrderID: "ord-B", ProjectID: "trader-1", Symbol: "MSFT",
		Qty: 1, Price: 400,
	})

	require.Eventually(t, func() bool {
		return len(rec.snapshot()) == 2
	}, 3*time.Second, 50*time.Millisecond,
		"different orders must produce separate messages")

	texts := []string{rec.snapshot()[0].Text, rec.snapshot()[1].Text}
	hasAAPL, hasMSFT := false, false
	for _, t := range texts {
		if assertContains(t, "AAPL") {
			hasAAPL = true
		}
		if assertContains(t, "MSFT") {
			hasMSFT = true
		}
	}
	assert.True(t, hasAAPL && hasMSFT, "both order symbols must appear in distinct messages")
}

// helper because assert.Contains forces a *testing.T arg and we're
// inside a loop where we don't want to fail on a single absence.
func assertContains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
