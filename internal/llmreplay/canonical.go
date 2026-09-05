// Package llmreplay records nothing itself: it owns the ONE canonical form of
// a chat request that both the recorder (the chat proxy) and the replay
// provider hash, so a recording made at the proxy matches what a replayed
// container sends (llm-exchange record/replay design §5). The replay server
// lives beside it.
package llmreplay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"vornik.io/vornik/internal/chat"
)

// Canonical renders a decoded request in its canonical form and returns the
// bytes and their sha256: `model` dropped (a replay may run under another
// model name; the messages are what identify a turn), `messages` and `tools`
// retained, every other field retained verbatim, keys sorted, no
// insignificant whitespace. Both the recorder and the replayer decode the
// wire bytes into chat.ChatRequest and call this, so JSON formatting
// differences between the container and the store cannot split a hash.
func Canonical(req chat.ChatRequest) ([]byte, string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("llmreplay: marshal request: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", fmt.Errorf("llmreplay: reshape request: %w", err)
	}
	delete(m, "model")
	out, err := json.Marshal(m) // encoding/json sorts map keys and emits no whitespace
	if err != nil {
		return nil, "", fmt.Errorf("llmreplay: canonical request: %w", err)
	}
	return out, Hash(out), nil
}

// Hash is the digest a recording row carries in request_hash: sha256 hex of
// the stored canonical bytes.
func Hash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
