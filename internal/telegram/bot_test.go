package telegram

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
)

func TestNewBot(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")

	tests := []struct {
		name   string
		config BotConfig
	}{
		{
			name: "basic bot",
			config: BotConfig{
				Token:        "test-token",
				AllowedUsers: map[int64]UserAccess{12345: {Allowed: true, Projects: []string{"*"}}},
				RateLimit:    10,
			},
		},
		{
			name: "no allowed users",
			config: BotConfig{
				Token:     "test-token",
				RateLimit: 10,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot, err := NewBot(tt.config, chatClient)
			assert.NoError(t, err)
			assert.NotNil(t, bot)
			assert.Equal(t, tt.config, bot.config)
		})
	}
}

func TestBot_IsAllowed(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")

	tests := []struct {
		name         string
		allowedUsers map[int64]UserAccess
		userID       int64
		want         bool
	}{
		{
			name:         "user in allowlist",
			allowedUsers: map[int64]UserAccess{12345: {Allowed: true, Projects: []string{"*"}}},
			userID:       12345,
			want:         true,
		},
		{
			name:         "user not in allowlist",
			allowedUsers: map[int64]UserAccess{12345: {Allowed: true, Projects: []string{"*"}}},
			userID:       99999,
			want:         false,
		},
		{
			name:         "empty allowlist allows all",
			allowedUsers: nil,
			userID:       12345,
			want:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot, err := NewBot(BotConfig{
				Token:        "test-token",
				AllowedUsers: tt.allowedUsers,
			}, chatClient)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, bot.IsAllowed(tt.userID))
		})
	}
}

func TestBot_SendMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "sendMessage")
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient, WithHTTPClient(server.Client()))
	assert.NoError(t, err)
	bot.baseURL = server.URL

	err = bot.sendMessage(context.Background(), 12345, "test message")
	assert.NoError(t, err)
}

func TestBot_StartStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient, WithHTTPClient(server.Client()))
	assert.NoError(t, err)
	bot.baseURL = server.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = bot.Start(ctx)
	assert.NoError(t, err)

	err = bot.Stop()
	assert.NoError(t, err)
}

func TestBot_CheckRateLimit(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")

	tests := []struct {
		name      string
		rateLimit int
		userID    int64
		calls     int
		want      []bool
	}{
		{
			name:      "rate limit 2 allows first two calls",
			rateLimit: 2,
			userID:    12345,
			calls:     3,
			want:      []bool{true, true, false},
		},
		{
			name:      "rate limit 0 allows all",
			rateLimit: 0,
			userID:    12345,
			calls:     5,
			want:      []bool{true, true, true, true, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot, err := NewBot(BotConfig{
				Token:     "test-token",
				RateLimit: tt.rateLimit,
			}, chatClient)
			assert.NoError(t, err)

			for i := 0; i < tt.calls; i++ {
				got := bot.CheckRateLimit(tt.userID)
				assert.Equal(t, tt.want[i], got, "call %d", i)
			}
		})
	}
}

func TestBot_GetConversation_UsesConfiguredMaxHistory(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")

	bot, err := NewBot(BotConfig{
		Token:      "test-token",
		MaxHistory: 3,
	}, chatClient)
	assert.NoError(t, err)

	conv := bot.getConversation(12345)
	conv.AddMessage(chat.Message{Role: "user", Content: "1"})
	conv.AddMessage(chat.Message{Role: "assistant", Content: "2"})
	conv.AddMessage(chat.Message{Role: "user", Content: "3"})
	conv.AddMessage(chat.Message{Role: "assistant", Content: "4"})

	// Trimming preserves whole turns so history always starts with a user message.
	// After 4 messages with maxHistory=3: the first turn (user1+ass2) is dropped
	// together to avoid leaving an orphaned assistant message at the start.
	msgs := conv.GetMessages()
	assert.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "3", msgs[0].Content)
}

func TestSanitizeTelegramError_RedactsToken(t *testing.T) {
	err := sanitizeTelegramError(assert.AnError)
	assert.Equal(t, assert.AnError.Error(), err)

	raw := `Get "https://api.telegram.org/bot123456:secret/getUpdates?timeout=30&offset=0": context deadline exceeded`
	sanitized := sanitizeTelegramError(&testError{msg: raw})
	assert.NotContains(t, sanitized, "bot123456:secret")
	assert.Contains(t, sanitized, "bot%3Credacted%3E")
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

func TestHandleNew_ResetsConversation(t *testing.T) {
	bot, err := NewBot(BotConfig{
		Token:        "test",
		AllowedUsers: map[int64]UserAccess{12345: {Allowed: true, Projects: []string{"*"}}},
	}, nil)
	assert.NoError(t, err)

	conv := bot.getConversation(12345)
	conv.AddMessage(chat.Message{Role: "user", Content: "hello"})
	assert.Equal(t, 1, conv.Len())

	result := handleNew(context.Background(), bot, 12345, 0)

	conv = bot.getConversation(12345)
	assert.Equal(t, 0, conv.Len())
	assert.Contains(t, result, "New session")
}

func TestDownloadTelegramFile_RejectsPathTraversalFileName(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient)
	assert.NoError(t, err)

	_, err = bot.DownloadTelegramFile(context.Background(), "file-id", "../escape.txt", t.TempDir())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid telegram file name")
}

func TestDownloadTelegramFile_WritesWithinDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getFile":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"docs/file.txt"}}`))
		case "/file/botbot-token/docs/file.txt":
			_, _ = w.Write([]byte("hello"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "bot-token"}, chatClient, WithHTTPClient(server.Client()))
	assert.NoError(t, err)
	bot.baseURL = server.URL
	bot.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.telegram.org" {
			req = req.Clone(req.Context())
			req.URL.Scheme = "http"
			req.URL.Host = server.Listener.Addr().String()
			if _, _, err := net.SplitHostPort(req.URL.Host); err != nil {
				req.URL.Host = server.URL[len("http://"):]
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	destDir := t.TempDir()
	destPath, err := bot.DownloadTelegramFile(context.Background(), "file-id", "file.txt", destDir)
	assert.NoError(t, err)
	assert.Equal(t, filepath.Join(destDir, "file.txt"), destPath)
	data, readErr := os.ReadFile(destPath)
	assert.NoError(t, readErr)
	assert.Equal(t, "hello", string(data))
}

func TestTelegramToolCallMetricIsExposed(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	metrics.ToolCallsTotal.WithLabelValues("list_tasks").Inc()

	value := testutil.ToFloat64(metrics.ToolCallsTotal.WithLabelValues("list_tasks"))
	assert.Equal(t, 1.0, value)

	gathered, err := reg.Gather()
	require.NoError(t, err)

	found := false
	for _, mf := range gathered {
		if mf.GetName() == "vornik_telegram_tool_calls_total" {
			found = true
			require.Len(t, mf.GetMetric(), 1)
			assert.Equal(t, 1.0, mf.GetMetric()[0].GetCounter().GetValue())
		}
	}
	assert.True(t, found, "expected vornik_telegram_tool_calls_total to be exposed")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestHandleHelp_ReturnsCommands(t *testing.T) {
	bot, err := NewBot(BotConfig{Token: "test"}, nil)
	assert.NoError(t, err)

	result := handleHelp(context.Background(), bot, 12345, 0)
	// Session controls
	for _, want := range []string{
		"/new", "/context", "/summarize", "/save", "/load", "/search", "/undo", "/forget", "/pin",
		"/verbose", "/project", "/autopilot", "/inbox", "/help",
	} {
		assert.Contains(t, result, want,
			"help text must advertise %s — undocumented commands are invisible to operators", want)
	}
}

func TestHandleProject_ShowsActiveProject(t *testing.T) {
	bot, err := NewBot(BotConfig{Token: "test"}, nil)
	assert.NoError(t, err)

	result := handleProject(context.Background(), bot, 12345, 0)
	assert.Contains(t, result, "No active project")

	bot.setActiveProject(12345, "my-project")

	result = handleProject(context.Background(), bot, 12345, 0)
	assert.Contains(t, result, "my-project")
}

func TestCommandsMap_AllRegistered(t *testing.T) {
	assert.NotNil(t, commands["/start"])
	assert.NotNil(t, commands["/help"])
	assert.NotNil(t, commands["/new"])
	assert.NotNil(t, commands["/reset"])
	assert.NotNil(t, commands["/project"])
}

func TestNewBot_NilClient(t *testing.T) {
	bot, err := NewBot(BotConfig{Token: "test"}, nil)
	assert.NoError(t, err)
	assert.NotNil(t, bot)
	// Bot should be created successfully even with nil chat client
	assert.NotNil(t, bot.conversations)
}

func TestHandleMessage_CommandWithNilDispatcher(t *testing.T) {
	// Use a test HTTP server to capture the Telegram sendMessage call
	var sentBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ts.Close()

	bot, err := NewBot(BotConfig{
		Token:        "test",
		AllowedUsers: map[int64]UserAccess{1: {Allowed: true, Projects: []string{"*"}}},
	}, nil)
	assert.NoError(t, err)
	bot.baseURL = ts.URL

	// Command messages work fine without dispatcher
	msg := &Message{ChatID: 1, UserID: 1, Text: "/help"}
	err = bot.HandleMessage(context.Background(), msg)
	assert.NoError(t, err)
	assert.Contains(t, string(sentBody), "/new")
}

func TestBot_NilMetrics_NoOp(t *testing.T) {
	bot, err := NewBot(BotConfig{
		Token:        "test",
		AllowedUsers: map[int64]UserAccess{1: {Allowed: true, Projects: []string{"*"}}},
	}, nil)
	assert.NoError(t, err)

	// metrics is nil by default — these should not panic
	assert.Nil(t, bot.metrics)

	// Command handling with nil metrics should work fine
	result := handleHelp(context.Background(), bot, 1, 0)
	assert.NotEmpty(t, result)

	result = handleNew(context.Background(), bot, 1, 0)
	assert.NotEmpty(t, result)
}

// TestBotMenuCommandsAreDocumented guards against advertising a command that
// has no handler (the dead /streaming entry that fell through to the LLM).
// Every command in the Telegram menu must appear in /help — which only lists
// real, handled commands — so this catches an advertised-but-unhandled command.
func TestBotMenuCommandsAreDocumented(t *testing.T) {
	help := handleHelp(context.Background(), &Bot{}, 0, 0)
	for _, c := range botMenuCommands() {
		cmd := "/" + c["command"]
		if !strings.Contains(help, cmd) {
			t.Errorf("menu advertises %s but /help does not document it — likely advertised-but-unhandled", cmd)
		}
	}
}

func TestBot_HandleNewResets(t *testing.T) {
	bot, err := NewBot(BotConfig{Token: "test"}, nil)
	assert.NoError(t, err)

	// handleNew clears the in-memory conversation and confirms a fresh session.
	result := handleNew(context.Background(), bot, 12345, 0)
	assert.Contains(t, result, "New session")
}

// TestUserAccess_CanAccessProject locks down the three scope shapes a
// user entry can take: wildcard, scoped list, empty list. These are the
// semantics operators rely on when writing allowed_users config.
func TestUserAccess_CanAccessProject(t *testing.T) {
	cases := []struct {
		name   string
		ua     UserAccess
		probes map[string]bool // projectID → expected CanAccessProject
	}{
		{
			name: "wildcard — any project allowed",
			ua:   UserAccess{Allowed: true, Projects: []string{"*"}},
			probes: map[string]bool{
				"snake":     true,
				"headmatch": true,
				"anything":  true,
			},
		},
		{
			name: "scoped — only listed projects",
			ua:   UserAccess{Allowed: true, Projects: []string{"snake", "headmatch"}},
			probes: map[string]bool{
				"snake":     true,
				"headmatch": true,
				"assistant": false,
				"":          false,
			},
		},
		{
			name: "empty list — dispatcher only, no projects accessible",
			ua:   UserAccess{Allowed: true, Projects: []string{}},
			probes: map[string]bool{
				"snake": false,
				"*":     false,
			},
		},
		{
			name: "not allowed — denies even wildcard",
			ua:   UserAccess{Allowed: false, Projects: []string{"*"}},
			probes: map[string]bool{
				"snake": false,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for proj, want := range tc.probes {
				got := tc.ua.CanAccessProject(proj)
				if got != want {
					t.Errorf("CanAccessProject(%q) = %v, want %v", proj, got, want)
				}
			}
		})
	}
}

// TestBot_UserCanAccessProject_NoAllowlist verifies dev-mode semantics:
// an empty AllowedUsers map means "no restrictions" so every user sees
// every project. This is the friction-free default for single-operator
// deployments.
func TestBot_UserCanAccessProject_NoAllowlist(t *testing.T) {
	bot, err := NewBot(BotConfig{Token: "t"}, nil)
	assert.NoError(t, err)
	assert.True(t, bot.UserCanAccessProject(12345, "snake"))
	assert.True(t, bot.UserCanAccessProject(99999, "assistant"))
}

// TestBot_UserCanAccessProject_Scoped is the main per-user scoping
// test. Configured allowlist with four distinct shapes: wildcard,
// scoped, multi-scoped, chat-only. Plus a user absent from the map.
func TestBot_UserCanAccessProject_Scoped(t *testing.T) {
	bot, err := NewBot(BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			111: {Allowed: true, Projects: []string{"*"}},
			222: {Allowed: true, Projects: []string{"snake"}},
			333: {Allowed: true, Projects: []string{"snake", "test"}},
			444: {Allowed: true, Projects: []string{}},
		},
	}, nil)
	assert.NoError(t, err)

	assert.True(t, bot.UserCanAccessProject(111, "snake"))
	assert.True(t, bot.UserCanAccessProject(111, "headmatch"))

	assert.True(t, bot.UserCanAccessProject(222, "snake"))
	assert.False(t, bot.UserCanAccessProject(222, "headmatch"))

	assert.True(t, bot.UserCanAccessProject(333, "snake"))
	assert.True(t, bot.UserCanAccessProject(333, "test"))
	assert.False(t, bot.UserCanAccessProject(333, "assistant"))

	// chat-only: allowed to interact but no project access.
	assert.True(t, bot.IsAllowed(444))
	assert.False(t, bot.UserCanAccessProject(444, "snake"))

	// unknown user: denied everywhere.
	assert.False(t, bot.IsAllowed(555))
	assert.False(t, bot.UserCanAccessProject(555, "snake"))
}
