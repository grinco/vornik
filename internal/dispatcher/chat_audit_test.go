package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
)

// stubChatAuditRepo is an in-memory persistence.ChatAuditRepository
// for dispatcher chat-audit tests. Capture every Insert + SavePrompt
// so tests can assert on the persisted shape without touching a DB.
type stubChatAuditRepo struct {
	mu         sync.Mutex
	entries    []*persistence.ChatAuditEntry
	prompts    map[string]string
	insertErr  error
	saveErr    error
	getPromptH map[string]string
}

func newStubChatAuditRepo() *stubChatAuditRepo {
	return &stubChatAuditRepo{prompts: map[string]string{}, getPromptH: map[string]string{}}
}

func (s *stubChatAuditRepo) Insert(_ context.Context, e *persistence.ChatAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	s.entries = append(s.entries, e)
	return nil
}

func (s *stubChatAuditRepo) List(_ context.Context, _ persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	return nil, nil
}

func (s *stubChatAuditRepo) SavePrompt(_ context.Context, hash, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.prompts[hash] = body
	return nil
}

func (s *stubChatAuditRepo) GetPrompt(_ context.Context, hash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.prompts[hash]; ok {
		return v, nil
	}
	return "", persistence.ErrNotFound
}

func TestChatAuditTurn_NilSafetyAllOps(t *testing.T) {
	// All chatAuditTurn methods are nil-safe; newChatAuditTurn returns
	// nil when the agent has no chatAuditRepo wired.
	var t0 *chatAuditTurn
	t0.captureRequest("sys", "msg", "role")
	t0.recordIteration(&chat.ChatResponse{}, "m", 0.5)
	t0.recordToolCall("create_task", "{}", "ok", "")
	t0.finish(context.Background(), Request{}, Result{})

	// Agent without repo → newChatAuditTurn yields nil.
	a := &Agent{logger: zerolog.Nop()}
	assert.Nil(t, newChatAuditTurn(a))
	assert.Nil(t, newChatAuditTurn(nil))
}

func TestChatAuditTurn_HappyPath_PersistsRowAndPrompt(t *testing.T) {
	repo := newStubChatAuditRepo()
	a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}

	turn := newChatAuditTurn(a)
	require.NotNil(t, turn)

	sysPrompt := "system-prompt-body"
	turn.captureRequest(sysPrompt, "hello", "lead")

	resp := &chat.ChatResponse{Model: "test-model"}
	resp.Usage.PromptTokens = 10
	resp.Usage.CompletionTokens = 5
	turn.recordIteration(resp, "test-model", 0.123)
	turn.recordIteration(resp, "", 0.001) // empty model is ignored

	turn.recordToolCall("create_task", "{\"x\":1}", "ok", "")
	turn.recordToolCall("list_tasks", "{}", "", "boom")

	turn.finish(context.Background(), Request{ChatID: 42, Project: "janka"}, Result{Text: "final answer"})

	require.Len(t, repo.entries, 1)
	e := repo.entries[0]
	assert.Equal(t, "42", e.ChatID)
	assert.Equal(t, "janka", e.ProjectID)
	assert.Equal(t, "lead", e.RoleUsed)
	assert.Equal(t, "test-model", e.Model)
	assert.Equal(t, "hello", e.UserMessage)
	assert.Equal(t, 2, e.Iterations)
	// Tokens accumulate across recordIteration calls (10*2 = 20 each side).
	assert.Equal(t, 20, turn.tokensIn)
	assert.Equal(t, 10, turn.tokensOut)

	// Tool calls were serialised as JSON in ToolCallsJSON.
	var calls []chatAuditToolCall
	require.NoError(t, json.Unmarshal([]byte(e.ToolCallsJSON), &calls))
	require.Len(t, calls, 2)
	assert.Equal(t, "create_task", calls[0].Name)
	assert.Equal(t, "boom", calls[1].Error)

	// SystemPromptHash matches sha256 of body and is stored.
	sum := sha256.Sum256([]byte(sysPrompt))
	expected := hex.EncodeToString(sum[:])
	assert.Equal(t, expected, e.SystemPromptHash)
	assert.Equal(t, sysPrompt, repo.prompts[expected])

	// Response truncated copy carries final answer.
	assert.Equal(t, "final answer", e.Response)

	// Cost accumulated across recordIteration calls.
	assert.InDelta(t, 0.124, e.CostUSD, 0.0001)
}

func TestChatAuditTurn_FinishWithErrorAndEmptyText(t *testing.T) {
	repo := newStubChatAuditRepo()
	a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}
	turn := newChatAuditTurn(a)
	require.NotNil(t, turn)

	turn.captureRequest("", "", "")
	turn.finish(context.Background(), Request{}, Result{Err: errors.New("boom")})

	require.Len(t, repo.entries, 1)
	assert.Equal(t, "error: boom", repo.entries[0].Response)
	// No system prompt → no SavePrompt call → repo.prompts empty.
	assert.Empty(t, repo.entries[0].SystemPromptHash)
	assert.Empty(t, repo.prompts)
}

func TestChatAuditTurn_FinishSwallowsInsertError(t *testing.T) {
	repo := newStubChatAuditRepo()
	repo.insertErr = errors.New("db down")
	a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}
	turn := newChatAuditTurn(a)
	require.NotNil(t, turn)
	turn.captureRequest("sys", "msg", "role")
	// Insert error must not panic and must be best-effort silent.
	assert.NotPanics(t, func() {
		turn.finish(context.Background(), Request{ChatID: 1}, Result{Text: "ok"})
	})
}

func TestChatAuditTurn_RecordIterationIgnoresNilResponse(t *testing.T) {
	repo := newStubChatAuditRepo()
	a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}
	turn := newChatAuditTurn(a)
	require.NotNil(t, turn)

	turn.recordIteration(nil, "model-x", 9.99)
	assert.Equal(t, 0, turn.iterations)
	assert.Equal(t, 0.0, turn.costUSD)
}

// TestChatAuditTurn_IDPreallocated — the turn id must be generated at
// turn start (so create_task can stamp it on tasks BEFORE the audit
// row exists) and persisted unchanged on the resulting entry. This is
// the linchpin of the tasks.chat_turn_id story: if the id moved
// between turn start and Insert, the soft link would point at the
// wrong row.
func TestChatAuditTurn_IDPreallocated(t *testing.T) {
	repo := newStubChatAuditRepo()
	a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}

	turn := newChatAuditTurn(a)
	require.NotNil(t, turn)
	require.NotEmpty(t, turn.id, "turn id must be allocated at start")
	require.True(t, strings.HasPrefix(turn.id, "chat_"), "id prefix: %s", turn.id)

	turn.captureRequest("sys", "hi", "lead")
	turn.finish(context.Background(), Request{ChatID: 7, Project: "p"}, Result{Text: "ok"})

	require.Len(t, repo.entries, 1)
	assert.Equal(t, turn.id, repo.entries[0].ID,
		"persisted entry.ID must match the pre-allocated turn id")
}

// TestChatTurnIDContext — the WithChatTurnID / ChatTurnIDFromContext
// helpers round-trip a turn id through context. Empty id is a no-op
// (no context value is set) so non-chat call paths can pass through
// the helper without bookkeeping.
func TestChatTurnIDContext(t *testing.T) {
	ctx := context.Background()
	assert.Empty(t, ChatTurnIDFromContext(ctx), "bare ctx has no turn id")

	ctx2 := WithChatTurnID(ctx, "chat_xyz")
	assert.Equal(t, "chat_xyz", ChatTurnIDFromContext(ctx2))
	// Original ctx unchanged.
	assert.Empty(t, ChatTurnIDFromContext(ctx))

	// Empty id is a no-op — the helper must not bury an empty value.
	ctx3 := WithChatTurnID(ctx, "")
	assert.Empty(t, ChatTurnIDFromContext(ctx3))

	// Nil ctx is tolerated; helper returns empty.
	//nolint:staticcheck
	assert.Empty(t, ChatTurnIDFromContext(nil))
}

func TestTruncateForAudit(t *testing.T) {
	t.Run("short stays intact", func(t *testing.T) {
		assert.Equal(t, "abc", truncateForAudit("abc", 100))
	})
	t.Run("zero limit returns input", func(t *testing.T) {
		assert.Equal(t, "abc", truncateForAudit("abc", 0))
	})
	t.Run("negative limit returns input", func(t *testing.T) {
		assert.Equal(t, "abc", truncateForAudit("abc", -1))
	})
	t.Run("limit shorter than suffix → hard chop", func(t *testing.T) {
		out := truncateForAudit(strings.Repeat("a", 50), 5)
		assert.Equal(t, "aaaaa", out)
		assert.Len(t, out, 5)
	})
	t.Run("normal truncation appends suffix", func(t *testing.T) {
		out := truncateForAudit(strings.Repeat("a", 200), 50)
		assert.Len(t, out, 50)
		assert.True(t, strings.HasSuffix(out, "…(truncated)"))
	})

	// Reproduces the 2026-05-21 T-0918 follow-up audit-insert
	// failure: a 4-byte emoji landed on the byte-limit boundary and
	// Postgres rejected the row with "invalid byte sequence for
	// encoding UTF8: 0xf0 0x9f 0x93 0xe2". Post-fix the cut must
	// always land on a rune boundary, so the output is valid UTF-8
	// regardless of where the truncate falls.
	t.Run("emoji on cut boundary stays valid UTF-8", func(t *testing.T) {
		// 📊 is U+1F4CA → bytes f0 9f 93 8a. Put one at byte 498 of
		// a 500-byte limit so the naive cut would split it across
		// the suffix join.
		body := strings.Repeat("a", 498) + "📊" + strings.Repeat("b", 100)
		// Cap below the body so truncation fires.
		out := truncateForAudit(body, 500)
		require.True(t, utf8.ValidString(out), "output must be valid UTF-8: %q", out)
		assert.True(t, strings.HasSuffix(out, "…(truncated)"))
	})
	t.Run("string entirely of multi-byte runes truncates cleanly", func(t *testing.T) {
		// Each "あ" is 3 bytes. 200 of them = 600 bytes. Limit 100
		// must NOT split the last "あ" in the prefix.
		body := strings.Repeat("あ", 200)
		out := truncateForAudit(body, 100)
		assert.True(t, utf8.ValidString(out), "output must be valid UTF-8: %q", out)
		assert.True(t, strings.HasSuffix(out, "…(truncated)"))
	})
	t.Run("short limit with multi-byte input stays valid", func(t *testing.T) {
		// Limit smaller than the suffix forces the hard-chop branch.
		// Even there, the prefix must respect rune boundaries.
		body := strings.Repeat("あ", 50)
		out := truncateForAudit(body, 5)
		assert.True(t, utf8.ValidString(out), "output must be valid UTF-8: %q", out)
		// 5 bytes can fit exactly one 3-byte rune, not two.
		assert.LessOrEqual(t, len(out), 5)
	})
}

// TestSafeUTF8Prefix — direct exercise of the helper because
// truncateForAudit exposes only the combined truncate+suffix
// behaviour and we want the rune-walk fast paths pinned.
func TestSafeUTF8Prefix(t *testing.T) {
	assert.Equal(t, "", safeUTF8Prefix("", 5))
	assert.Equal(t, "", safeUTF8Prefix("abc", 0))
	assert.Equal(t, "", safeUTF8Prefix("abc", -1))
	assert.Equal(t, "abc", safeUTF8Prefix("abc", 99))
	assert.Equal(t, "ab", safeUTF8Prefix("abc", 2))
	// 3-byte rune; fits only if n ≥ 3.
	assert.Equal(t, "", safeUTF8Prefix("あ", 2))
	assert.Equal(t, "あ", safeUTF8Prefix("あい", 3))
	assert.Equal(t, "あ", safeUTF8Prefix("あい", 5))
	assert.Equal(t, "あい", safeUTF8Prefix("あい", 6))
}

func TestModelFromResponse(t *testing.T) {
	assert.Equal(t, "", modelFromResponse(nil))
	assert.Equal(t, "m-1", modelFromResponse(&chat.ChatResponse{Model: "m-1"}))
}

func TestFormatChatID(t *testing.T) {
	cases := map[string]struct {
		id   int64
		want string
	}{
		"zero":         {0, ""},
		"single digit": {7, "7"},
		"multi digit":  {123456, "123456"},
		"negative":     {-42, "-42"},
		"max int":      {9223372036854775807, "9223372036854775807"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatChatID(tc.id))
		})
	}
}

// TestResolveChatID — three-bucket priority pinned. Telegram-style
// int64 wins (back-compat for existing UI filters); else
// channel:session form when both are set (the email/webchat fix);
// else empty for synthesised turns.
func TestResolveChatID(t *testing.T) {
	cases := map[string]struct {
		req  Request
		want string
	}{
		"telegram int64 wins": {
			Request{ChatID: 123, OriginatingChannel: "email", OriginatingSessionID: "x"},
			"123",
		},
		"negative telegram int64 wins": {
			Request{ChatID: -1001234567890, OriginatingChannel: "email", OriginatingSessionID: "x"},
			"-1001234567890",
		},
		"email channel:session": {
			Request{OriginatingChannel: "email", OriginatingSessionID: "<thread@x.com>"},
			"email:<thread@x.com>",
		},
		"webchat channel:session": {
			Request{OriginatingChannel: "web-chat", OriginatingSessionID: "abc-def"},
			"web-chat:abc-def",
		},
		"missing channel":   {Request{OriginatingSessionID: "x"}, ""},
		"missing sessionID": {Request{OriginatingChannel: "email"}, ""},
		"all empty (synth)": {Request{}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveChatID(tc.req))
		})
	}
}

// TestChatAuditTurn_RecordHallucinationSignals covers the wiring
// that surfaces detector output onto the audit row. Empty + nil
// inputs are no-ops; multiple calls accumulate; finish marshals
// the running list to HallucinationSignalsJSON.
func TestChatAuditTurn_RecordHallucinationSignals(t *testing.T) {
	t.Run("nil receiver is a no-op", func(t *testing.T) {
		var nilTurn *chatAuditTurn
		assert.NotPanics(t, func() {
			nilTurn.recordHallucinationSignals([]hallucination.Signal{{Detector: "x"}})
		})
	})

	t.Run("empty input does not allocate", func(t *testing.T) {
		repo := newStubChatAuditRepo()
		a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}
		turn := newChatAuditTurn(a)
		require.NotNil(t, turn)
		turn.recordHallucinationSignals(nil)
		turn.recordHallucinationSignals([]hallucination.Signal{})
		assert.Nil(t, turn.hallucinationSignals)
	})

	t.Run("multiple calls accumulate and finish marshals to JSON", func(t *testing.T) {
		repo := newStubChatAuditRepo()
		a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}
		turn := newChatAuditTurn(a)
		require.NotNil(t, turn)

		turn.captureRequest("sys", "msg", "lead")
		turn.recordHallucinationSignals([]hallucination.Signal{
			{Detector: "url_not_fetched", Severity: hallucination.SeverityHigh, ClaimValue: "https://x"},
		})
		turn.recordHallucinationSignals([]hallucination.Signal{
			{Detector: "fact_unverified", Severity: hallucination.SeverityInfo, ClaimValue: "y"},
		})
		require.Len(t, turn.hallucinationSignals, 2)

		turn.finish(context.Background(), Request{ChatID: 1, Project: "p"}, Result{Text: "ok"})
		require.Len(t, repo.entries, 1)
		raw := repo.entries[0].HallucinationSignalsJSON
		require.NotEmpty(t, raw)

		var got []hallucination.Signal
		require.NoError(t, json.Unmarshal([]byte(raw), &got))
		require.Len(t, got, 2)
		assert.Equal(t, "url_not_fetched", got[0].Detector)
		assert.Equal(t, "fact_unverified", got[1].Detector)
	})

	t.Run("no signals → empty JSON column (no false-positive badge)", func(t *testing.T) {
		repo := newStubChatAuditRepo()
		a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}
		turn := newChatAuditTurn(a)
		require.NotNil(t, turn)
		turn.captureRequest("sys", "msg", "lead")
		turn.finish(context.Background(), Request{ChatID: 1}, Result{Text: "ok"})
		require.Len(t, repo.entries, 1)
		assert.Equal(t, "", repo.entries[0].HallucinationSignalsJSON,
			"empty signal list should render as '' not '[]' so the UI badge gate stays false")
	})
}

func TestWithChatAuditRepo_AssignsField(t *testing.T) {
	repo := newStubChatAuditRepo()
	a := &Agent{}
	WithChatAuditRepo(repo)(a)
	assert.Same(t, repo, a.chatAuditRepo)
}

func (s *stubChatAuditRepo) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return nil, persistence.ErrNotFound
}
