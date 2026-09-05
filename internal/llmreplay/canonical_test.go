package llmreplay

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func decode(t *testing.T, body string) chat.ChatRequest {
	t.Helper()
	var req chat.ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

// The same request in three wire spellings — key order, whitespace, and the
// model name the proxy would route elsewhere — is one canonical form.
func TestCanonical_StableAcrossSpellingsAndDropsModel(t *testing.T) {
	a := decode(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}],"tools":[],"temperature":0.2}`)
	b := decode(t, `{ "temperature": 0.2, "tools": [],
	  "messages": [ { "content": "hi", "role": "user" } ], "model": "other-model" }`)
	ca, ha, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, hb, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb || string(ca) != string(cb) {
		t.Fatalf("canonical forms differ:\n%s\n%s", ca, cb)
	}
	var m map[string]any
	_ = json.Unmarshal(ca, &m)
	if _, ok := m["model"]; ok {
		t.Error("model must be dropped from the canonical form")
	}
	if m["temperature"] != 0.2 {
		t.Error("other fields are retained verbatim")
	}
	if Hash(ca) != ha {
		t.Error("Hash must be the digest Canonical returned")
	}
}

func TestCanonical_DifferentMessagesDifferentHash(t *testing.T) {
	a := decode(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	b := decode(t, `{"messages":[{"role":"user","content":"hi!"}]}`)
	_, ha, _ := Canonical(a)
	_, hb, _ := Canonical(b)
	if ha == hb {
		t.Error("distinct conversations must not collide")
	}
}
