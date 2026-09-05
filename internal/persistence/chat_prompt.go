package persistence

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashChatSystemPrompt is the identity of a row in chat_system_prompts: the
// sha256 hex digest of the body as STORED.
//
// It lives here, beside the interface, rather than at each writer, because
// two writers used to compute it independently (the dispatcher's turn audit
// and the chat proxy's) — and a hash computed by the caller is a hash taken
// before the repository's redaction seam can act, which makes the stored hash
// name bytes the store does not hold. SavePrompt hashes what it is about to
// write; nobody else should be hashing at all.
//
// Mirrors HashStepPrompt for the step-prompt store.
func HashChatSystemPrompt(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
