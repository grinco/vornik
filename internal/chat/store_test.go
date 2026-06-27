package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveAndLoadNamedConversation(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "sessions.json")

	conv := NewConversation("telegram-42", 50)
	conv.AddMessage(Message{Role: "user", Content: "original"})
	conv.AddMessage(Message{Role: "assistant", Content: "reply"})

	require.NoError(t, SaveNamedConversation(base, 42, "work-thread", conv))

	loaded, err := LoadNamedConversation(base, 42, "work-thread", 50)
	require.NoError(t, err)
	require.Equal(t, 2, loaded.Len())

	names, err := ListNamedSaves(base, 42)
	require.NoError(t, err)
	require.Equal(t, []string{"work-thread"}, names)

	// Isolation: saves for a different chat don't show up.
	names2, err := ListNamedSaves(base, 99)
	require.NoError(t, err)
	require.Empty(t, names2)
}

func TestSaveNamedConversation_TraversalNeutralised(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "sessions.json")
	savesRoot := filepath.Join(tmp, "saves")

	conv := NewConversation("telegram-1", 50)
	conv.AddMessage(Message{Role: "user", Content: "hi"})

	// "../../etc/passwd" survives sanitisation as "etcpasswd" — valid,
	// but must land under the saves tree, never outside it.
	require.NoError(t, SaveNamedConversation(base, 1, "../../etc/passwd", conv))

	path, err := NamedSavePath(base, 1, "../../etc/passwd")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(path, savesRoot), "save path escaped saves dir: %s", path)

	// Pure-separator input has no salvageable characters → explicit error.
	err = SaveNamedConversation(base, 1, "////", conv)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid save name")
}

func TestLoadNamedConversation_Missing(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "sessions.json")
	_, err := LoadNamedConversation(base, 1, "nonexistent", 50)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))
}

func TestNamedSavePath_Sanitisation(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "sessions.json")

	p, err := NamedSavePath(base, 7, "hello-world_2026!!??")
	require.NoError(t, err)
	require.Contains(t, p, "hello-world_2026.json")

	_, err = NamedSavePath(base, 7, "////")
	require.Error(t, err)
}

func TestSaveAndLoadConversations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	convs := map[int64]*Conversation{
		100: newConvWithMessages("telegram-100", 50, []Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
		}),
		200: newConvWithMessages("telegram-200", 50, []Message{
			{Role: "user", Content: "status?"},
		}),
	}

	require.NoError(t, SaveConversations(path, convs))

	loaded, err := LoadConversations(path, 50)
	require.NoError(t, err)
	assert.Len(t, loaded, 2)

	msgs100 := loaded[100].GetMessages()
	assert.Len(t, msgs100, 2)
	assert.Equal(t, "hello", msgs100[0].Content)
	assert.Equal(t, "hi there", msgs100[1].Content)

	msgs200 := loaded[200].GetMessages()
	assert.Len(t, msgs200, 1)
	assert.Equal(t, "status?", msgs200[0].Content)
}

func TestSaveConversations_SkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	convs := map[int64]*Conversation{
		100: NewConversation("telegram-100", 50), // empty
		200: newConvWithMessages("telegram-200", 50, []Message{
			{Role: "user", Content: "hi"},
		}),
	}

	require.NoError(t, SaveConversations(path, convs))

	loaded, err := LoadConversations(path, 50)
	require.NoError(t, err)
	assert.Len(t, loaded, 1)
	assert.NotNil(t, loaded[200])
}

func TestLoadConversations_MissingFile(t *testing.T) {
	loaded, err := LoadConversations("/nonexistent/path/sessions.json", 50)
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

func TestLoadConversations_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0600))

	_, err := LoadConversations(path, 50)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}

func TestSaveConversations_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "sessions.json")

	convs := map[int64]*Conversation{
		100: newConvWithMessages("telegram-100", 50, []Message{
			{Role: "user", Content: "test"},
		}),
	}

	require.NoError(t, SaveConversations(path, convs))

	_, err := os.Stat(path)
	assert.NoError(t, err)
}

func TestSaveConversations_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	// Write initial data
	convs := map[int64]*Conversation{
		100: newConvWithMessages("telegram-100", 50, []Message{
			{Role: "user", Content: "first"},
		}),
	}
	require.NoError(t, SaveConversations(path, convs))

	// Overwrite with new data
	convs[100].AddMessage(Message{Role: "assistant", Content: "second"})
	require.NoError(t, SaveConversations(path, convs))

	// No temp file should remain
	_, err := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err))

	// Verify new content
	loaded, err := LoadConversations(path, 50)
	require.NoError(t, err)
	assert.Len(t, loaded[100].GetMessages(), 2)
}

func TestLoadConversations_RespectsMaxHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	convs := map[int64]*Conversation{
		100: newConvWithMessages("telegram-100", 100, []Message{
			{Role: "user", Content: "msg1"},
			{Role: "assistant", Content: "msg2"},
			{Role: "user", Content: "msg3"},
			{Role: "assistant", Content: "msg4"},
			{Role: "user", Content: "msg5"},
		}),
	}
	require.NoError(t, SaveConversations(path, convs))

	// Load with maxHistory=3 — AddMessage trims during replay
	loaded, err := LoadConversations(path, 3)
	require.NoError(t, err)
	msgs := loaded[100].GetMessages()
	assert.Len(t, msgs, 3)
	assert.Equal(t, "msg3", msgs[0].Content)
}

func newConvWithMessages(id string, maxHistory int, msgs []Message) *Conversation {
	conv := NewConversation(id, maxHistory)
	for _, msg := range msgs {
		conv.AddMessage(msg)
	}
	return conv
}
