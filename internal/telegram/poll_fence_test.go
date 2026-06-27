package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// fenceGate satisfies the telegram LeaderGate (IsLeader) AND the
// leaderelection.EpochVerifier capability (VerifyEpoch). IsLeader
// stays true so the cheap idle gate at the top of pollLoop opens;
// the epoch fence (VerifyEpoch) is what decides whether getUpdates
// may fire. This models review B1: a stale leader's cached leader
// bit is true, but its epoch has been superseded.
type fenceGate struct {
	verifyOK  bool
	verifyErr error
}

func (g fenceGate) IsLeader() bool { return true }

func (g fenceGate) VerifyEpoch(context.Context) (bool, int64, error) {
	return g.verifyOK, 0, g.verifyErr
}

// TestPollLoop_Fence_SkipsGetUpdatesWhenSuperseded pins review B1 on
// the telegram poller: a leader whose epoch was bumped (VerifyEpoch
// ok=false) must NOT call getUpdates, even though IsLeader() is still
// true. Otherwise a resumed stale leader double-consumes updates.
func TestPollLoop_Fence_SkipsGetUpdatesWhenSuperseded(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getUpdates") {
			atomic.AddInt32(&hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	gate := fenceGate{verifyOK: false} // leader bit true, epoch superseded
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(
		BotConfig{Token: "t"},
		chatClient,
		WithHTTPClient(srv.Client()),
		WithLeaderGate(gate),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Several poll cycles; the fence (2s back-off) keeps getUpdates
	// from firing even though IsLeader()=true.
	time.Sleep(500 * time.Millisecond)
	_ = bot.Stop()

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("fenced (superseded) pollLoop hit getUpdates %d time(s); want 0", got)
	}
}

// TestPollLoop_Fence_SkipsGetUpdatesOnVerifyError pins fail-closed: an
// unreadable lock row (VerifyEpoch err != nil) also suppresses getUpdates.
func TestPollLoop_Fence_SkipsGetUpdatesOnVerifyError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getUpdates") {
			atomic.AddInt32(&hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	gate := fenceGate{verifyErr: context.DeadlineExceeded}
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(
		BotConfig{Token: "t"},
		chatClient,
		WithHTTPClient(srv.Client()),
		WithLeaderGate(gate),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = bot.Stop()

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("fenced (read error) pollLoop hit getUpdates %d time(s); want 0", got)
	}
}

// TestPollLoop_Fence_PollsWhenEpochCurrent confirms the fence opens for
// a still-current leader (VerifyEpoch ok=true): getUpdates must fire.
func TestPollLoop_Fence_PollsWhenEpochCurrent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getUpdates") {
			atomic.AddInt32(&hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	gate := fenceGate{verifyOK: true}
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(
		BotConfig{Token: "t"},
		chatClient,
		WithHTTPClient(srv.Client()),
		WithLeaderGate(gate),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = bot.Stop()
	if atomic.LoadInt32(&hits) == 0 {
		t.Errorf("current-epoch leader pollLoop should have called getUpdates; hits=0")
	}
}
