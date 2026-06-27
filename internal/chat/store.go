package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type conversationRecord struct {
	ChatID   int64     `json:"chat_id"`
	Messages []Message `json:"messages"`
}

// SaveConversations writes conversations to a JSON file atomically.
// The parent directory is created if it does not exist.
func SaveConversations(path string, conversations map[int64]*Conversation) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}

	records := make([]conversationRecord, 0, len(conversations))
	for chatID, conv := range conversations {
		msgs := conv.GetMessages()
		if len(msgs) == 0 {
			continue
		}
		records = append(records, conversationRecord{
			ChatID:   chatID,
			Messages: msgs,
		})
	}

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal conversations: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// NamedSavePath returns the on-disk location for a per-chat named
// conversation save. basePath is the session persistence path (e.g.
// /var/lib/vornik/telegram-sessions.json) — saves live under a sibling
// `saves/<chatID>/<name>.json` tree so the primary session file isn't
// polluted. name is sanitised to reject path-traversal and control chars.
func NamedSavePath(basePath string, chatID int64, name string) (string, error) {
	clean := sanitiseSaveName(name)
	if clean == "" {
		return "", fmt.Errorf("invalid save name %q — use letters, digits, '-' or '_'", name)
	}
	dir := filepath.Join(filepath.Dir(basePath), "saves", fmt.Sprintf("%d", chatID))
	return filepath.Join(dir, clean+".json"), nil
}

// SaveNamedConversation persists a single conversation under a user-chosen
// name. Atomic: writes to a temp file and renames into place.
func SaveNamedConversation(basePath string, chatID int64, name string, conv *Conversation) error {
	path, err := NamedSavePath(basePath, chatID, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create saves directory: %w", err)
	}
	record := conversationRecord{
		ChatID:   chatID,
		Messages: conv.GetMessages(),
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal saved conversation: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// LoadNamedConversation reads a per-chat named save and returns a fresh
// Conversation populated with those messages. Missing file → os.ErrNotExist.
func LoadNamedConversation(basePath string, chatID int64, name string, maxHistory int) (*Conversation, error) {
	path, err := NamedSavePath(basePath, chatID, name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record conversationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("unmarshal saved conversation: %w", err)
	}
	conv := NewConversation(fmt.Sprintf("telegram-%d", chatID), maxHistory)
	for _, msg := range record.Messages {
		conv.AddMessage(msg)
	}
	return conv, nil
}

// ListNamedSaves returns the names of all saves for a chat. Missing dir
// or empty dir return a nil slice; filesystem errors propagate.
func ListNamedSaves(basePath string, chatID int64) ([]string, error) {
	dir := filepath.Join(filepath.Dir(basePath), "saves", fmt.Sprintf("%d", chatID))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		const suffix = ".json"
		if len(n) > len(suffix) && n[len(n)-len(suffix):] == suffix {
			names = append(names, n[:len(n)-len(suffix)])
		}
	}
	return names, nil
}

// sanitiseSaveName restricts a save name to [A-Za-z0-9_-] and caps length.
// Returns "" for any name that, after filtering, is empty.
func sanitiseSaveName(name string) string {
	const maxLen = 64
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name) && len(out) < maxLen; i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c == '-' || c == '_':
			out = append(out, c)
		}
	}
	return string(out)
}

// LoadConversations reads conversations from a JSON file.
// Returns an empty map if the file does not exist.
func LoadConversations(path string, maxHistory int) (map[int64]*Conversation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[int64]*Conversation), nil
		}
		return nil, fmt.Errorf("read conversations file: %w", err)
	}

	var records []conversationRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("unmarshal conversations: %w", err)
	}

	conversations := make(map[int64]*Conversation, len(records))
	for _, rec := range records {
		conv := NewConversation(fmt.Sprintf("telegram-%d", rec.ChatID), maxHistory)
		for _, msg := range rec.Messages {
			conv.AddMessage(msg)
		}
		conversations[rec.ChatID] = conv
	}
	return conversations, nil
}
