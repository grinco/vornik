package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// claudeAccountInfo holds the per-install identifiers the CLI embeds
// in every Messages API request's `metadata.user_id` field. The server
// uses this blob to bucket OAuth traffic — without it, requests land
// on an abuse-filter surface that returns terse rate_limit_error 429s
// even when the subscription has plenty of quota left.
//
// The fields come from `~/.claude.json` (not inside the `~/.claude/`
// directory — the CLI persists install-level state at the home-dir
// root). Both values are stable across runs; we cache them after the
// first read.
type claudeAccountInfo struct {
	// UserID is the 64-hex-char install identifier the CLI writes to
	// ~/.claude.json on first launch. Serialised as "device_id" in
	// the metadata blob — the name mismatch is ours-vs-CLI'ss
	// nomenclature; on the wire it's always "device_id".
	UserID string
	// AccountUUID is the Anthropic account UUID from
	// ~/.claude.json.oauthAccount.accountUuid.
	AccountUUID string
}

// claudeAccountResolver is a lazy loader for the install-level account
// data. Zero-value usable; first call to resolve() reads the file and
// caches the result for the life of the process. Concurrency-safe.
type claudeAccountResolver struct {
	path string

	once sync.Once
	info claudeAccountInfo
}

// newClaudeAccountResolver picks the default ~/.claude.json path, with
// CLAUDE_CONFIG_DIR honored the same way the auth manager does — so
// operators who redirect the CLI's state dir get consistent behavior
// here too.
func newClaudeAccountResolver() *claudeAccountResolver {
	path := claudeAccountInfoPath()
	return &claudeAccountResolver{path: path}
}

func claudeAccountInfoPath() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		// When CLAUDE_CONFIG_DIR points at the CLI's overridden
		// state dir, the account file sits at <dir>/.claude.json
		// (sibling of .credentials.json in that directory, NOT a
		// file named ".claude.json" at the user's home). The
		// filename itself is legacy and doesn't match the dir.
		return filepath.Join(dir, ".claude.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude.json")
	}
	return ".claude.json"
}

// resolve returns the cached account info, reading the file on first
// call. Failures are swallowed — we log nothing and return whatever
// fields we managed to parse. An empty AccountUUID is acceptable to
// the server (it's `?? ""` on the CLI side too); an empty UserID means
// the metadata blob will be shorter than the CLI's but still valid
// JSON, which is a better failure mode than refusing to send at all.
func (r *claudeAccountResolver) resolve() claudeAccountInfo {
	r.once.Do(func() {
		raw, err := os.ReadFile(r.path)
		if err != nil {
			return
		}
		var parsed struct {
			UserID       string `json:"userID"`
			OauthAccount struct {
				AccountUUID string `json:"accountUuid"`
			} `json:"oauthAccount"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return
		}
		r.info = claudeAccountInfo{
			UserID:      parsed.UserID,
			AccountUUID: parsed.OauthAccount.AccountUUID,
		}
	})
	return r.info
}
