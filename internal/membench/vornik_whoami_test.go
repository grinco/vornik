package membench

import (
	"encoding/json"
	"testing"
)

// The daemon's whoami emits "project_id", not "project".
//
// The struct tag said "project", which encoding/json resolves to nothing and
// leaves empty WITHOUT erroring. The failure surfaced only at the far end, as
// the per-run clear refusing every run with "the daemon reported an EMPTY
// project, which names no store to clear" — a guard doing its job over a bug
// two layers away.
//
// This pins the wire contract against a payload captured from a live daemon on
// 2026-08-21 rather than against the struct, which is what was wrong.
func TestWhoamiReply_ParsesTheDaemonsActualPayload(t *testing.T) {
	const live = `{
	  "client_kind": "claude-code",
	  "database": "vornik_bench",
	  "default_repo_scope": "",
	  "effective_repo_scope": "",
	  "embedding_readiness": 1,
	  "memory_chunks_embedded": 1897,
	  "memory_chunks_total": 1897,
	  "memory_embed_queue_depth": 0,
	  "memory_read": true,
	  "memory_write": true,
	  "project_id": "bench",
	  "session_label": "membench-postfix"
	}`

	var got whoamiReply
	if err := json.Unmarshal([]byte(live), &got); err != nil {
		t.Fatalf("unmarshal live whoami: %v", err)
	}

	if got.Project != "bench" {
		t.Errorf("Project = %q, want \"bench\" — the tag must match the daemon's "+
			"project_id key, and a mismatch is silent", got.Project)
	}
	if got.Database != "vornik_bench" {
		t.Errorf("Database = %q, want \"vornik_bench\"", got.Database)
	}
	if got.EmbeddingReadiness == nil || *got.EmbeddingReadiness != 1 {
		t.Errorf("EmbeddingReadiness = %v, want 1", got.EmbeddingReadiness)
	}
	if got.ChunksTotal != 1897 || got.ChunksEmbedded != 1897 {
		t.Errorf("chunk counts = %d/%d, want 1897/1897", got.ChunksTotal, got.ChunksEmbedded)
	}
}
