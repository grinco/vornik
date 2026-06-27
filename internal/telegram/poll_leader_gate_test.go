package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// stubLeaderGate is the minimal LeaderGate impl tests use to
// drive the cluster-aware pollLoop behaviour. Flipping leader
// flips the gate atomically.
type stubLeaderGate struct {
	leader atomic.Bool
}

func (s *stubLeaderGate) IsLeader() bool {
	return s.leader.Load()
}

// stubPollerStateRepo records Get + Set calls so tests can
// assert offset persistence + the load-on-acquire behaviour.
type stubPollerStateRepo struct {
	mu       sync.Mutex
	rows     map[string]int64
	getCalls int
	setCalls int
}

func newStubPollerStateRepo() *stubPollerStateRepo {
	return &stubPollerStateRepo{rows: map[string]int64{}}
}

func (s *stubPollerStateRepo) Get(_ context.Context, botID string) (*persistence.TelegramPollerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	off, ok := s.rows[botID]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return &persistence.TelegramPollerState{BotID: botID, Offset: off}, nil
}

func (s *stubPollerStateRepo) Set(_ context.Context, state *persistence.TelegramPollerState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCalls++
	s.rows[state.BotID] = state.Offset
	return nil
}

// TestPollLoop_LeaderGate_SkipsGetUpdatesWhenNotLeader pins
// the cluster contract: a non-leader replica must never hit
// getUpdates. Without this guard, both replicas in a 2-node
// HA deployment double-process every message.
func TestPollLoop_LeaderGate_SkipsGetUpdatesWhenNotLeader(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only count getUpdates hits — registerBotMenu fires
		// setMyCommands on Start, which would otherwise show
		// up as a false positive against the leader-gate
		// contract.
		if strings.Contains(r.URL.Path, "getUpdates") {
			atomic.AddInt32(&hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	gate := &stubLeaderGate{} // leader=false
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

	// Give the pollLoop several chances to wake up. With the
	// gate held closed, even after a few cycles it must not
	// have hit the server.
	time.Sleep(500 * time.Millisecond)
	_ = bot.Stop()

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("non-leader pollLoop hit getUpdates %d time(s); want 0", got)
	}
}

// TestPollLoop_LeaderGate_PollsWhenLeader confirms the gate
// opens cleanly — leader=true must let getUpdates fire.
func TestPollLoop_LeaderGate_PollsWhenLeader(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	gate := &stubLeaderGate{}
	gate.leader.Store(true)
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
	// One getUpdates call is enough to prove the gate opens.
	// The server returns immediately so we don't wait for the
	// 30s long-poll timeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = bot.Stop()
	if atomic.LoadInt32(&hits) == 0 {
		t.Errorf("leader pollLoop should have called getUpdates; hits=0")
	}
}

// TestPollLoop_PersistOffset_LoadsOnStart pins the failover
// contract: when the repo carries a watermark, the pollLoop
// must request getUpdates with offset=<that watermark> on the
// first call — not 0, which would replay queued updates.
func TestPollLoop_PersistOffset_LoadsOnStart(t *testing.T) {
	var capturedOffset atomic.Int64
	capturedOffset.Store(-1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Telegram passes ?offset=N as a query param.
		v := r.URL.Query().Get("offset")
		if v != "" && capturedOffset.Load() == -1 {
			var n int64
			_, _ = parseInt64(v, &n)
			capturedOffset.Store(n)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	repo := newStubPollerStateRepo()
	repo.rows["@vornik_bot"] = 12345 // a deposed leader's last confirm

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(
		BotConfig{Token: "t"},
		chatClient,
		WithHTTPClient(srv.Client()),
		WithPollerStateRepository(repo, "@vornik_bot"),
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
		if capturedOffset.Load() != -1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = bot.Stop()

	if got := capturedOffset.Load(); got != 12345 {
		t.Errorf("initial getUpdates offset = %d, want 12345 (persisted watermark)", got)
	}
}

// parseInt64 is a small helper so the test doesn't pull
// strconv into its import set for one line.
func parseInt64(s string, out *int64) (int, error) {
	var n int64
	consumed := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int64(r-'0')
		consumed++
	}
	*out = n
	return consumed, nil
}
