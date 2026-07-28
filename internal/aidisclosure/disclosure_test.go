package aidisclosure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestCadenceFor_UnknownChannel_DefaultsToPerMessage pins the fail-safe.
// Named explicitly (design §7) so the protection is legible in CI output
// rather than buried in a table-driven case: a channel added later must
// over-disclose rather than silently disclose nothing. Over-disclosure is a
// UX complaint; under-disclosure is an EU AI Act Art 50(1) non-conformity.
func TestCadenceFor_UnknownChannel_DefaultsToPerMessage(t *testing.T) {
	if got := CadenceFor("some-channel-invented-in-2027"); got != CadencePerMessage {
		t.Fatalf("unknown channel cadence = %v, want CadencePerMessage", got)
	}
	if got := CadenceFor(""); got != CadencePerMessage {
		t.Fatalf("empty channel cadence = %v, want CadencePerMessage", got)
	}
}

// TestCadenceFor_KnownChannels covers all five verified Channel.Name()
// strings (design §2.1). The split: continuous conversational UIs disclose
// once per session; channels whose messages are standalone forwardable
// artifacts carry the notice on every outbound.
func TestCadenceFor_KnownChannels(t *testing.T) {
	for _, tc := range []struct {
		channel string
		want    Cadence
	}{
		{"telegram", CadencePerSession},
		{"slack", CadencePerSession},
		{"web-chat", CadencePerSession},
		{"email", CadencePerMessage},
		{"github-app", CadencePerMessage},
	} {
		if got := CadenceFor(tc.channel); got != tc.want {
			t.Errorf("CadenceFor(%q) = %v, want %v", tc.channel, got, tc.want)
		}
	}
}

func TestNotice_DefaultTextNamesTheAISystemAndCarriesTheURL(t *testing.T) {
	n := New(Config{}, nil).Notice()

	if !strings.Contains(strings.ToLower(n.Text), "ai system") {
		t.Errorf("default text must state the user is interacting with an AI system, got %q", n.Text)
	}
	if !strings.Contains(n.Text, DefaultURL) {
		t.Errorf("default text must carry %q, got %q", DefaultURL, n.Text)
	}
}

func TestNotice_TextAndURLOverrides(t *testing.T) {
	n := New(Config{Text: "Vous interagissez avec une IA. %s", URL: "https://example.test/ai"}, nil).Notice()

	if !strings.Contains(n.Text, "Vous interagissez") {
		t.Errorf("text override not applied: %q", n.Text)
	}
	if !strings.Contains(n.Text, "https://example.test/ai") {
		t.Errorf("URL override not applied: %q", n.Text)
	}
}

// TestNotice_HashIsSHA256OfRenderedText pins the evidentiary contract from
// design §2.4: SHA-256, lower-case hex, over the UTF-8 bytes of the fully
// rendered text. A regulator asking "which wording was this session served"
// is answered from this hash, so the algorithm is part of the contract, not
// an implementation detail.
func TestNotice_HashIsSHA256OfRenderedText(t *testing.T) {
	n := New(Config{}, nil).Notice()

	sum := sha256.Sum256([]byte(n.Text))
	want := hex.EncodeToString(sum[:])
	if n.Hash != want {
		t.Errorf("Hash = %q, want sha256 hex %q", n.Hash, want)
	}
	if n.Hash != strings.ToLower(n.Hash) {
		t.Errorf("Hash must be lower-case hex, got %q", n.Hash)
	}
}

func TestNotice_HashChangesWhenTextChanges(t *testing.T) {
	a := New(Config{}, nil).Notice()
	b := New(Config{Text: "A different disclosure. %s"}, nil).Notice()

	if a.Hash == b.Hash {
		t.Fatal("different disclosure text must produce a different hash")
	}
}

// --- Ensure ---

type stubStore struct {
	served   map[string]bool
	readErr  error
	writeErr error
	writes   []string
}

func newStubStore() *stubStore { return &stubStore{served: map[string]bool{}} }

func (s *stubStore) WasServed(_ context.Context, channel, sessionID string) (bool, error) {
	if s.readErr != nil {
		return false, s.readErr
	}
	return s.served[channel+"|"+sessionID], nil
}

func (s *stubStore) MarkServed(_ context.Context, channel, sessionID, textHash string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.writes = append(s.writes, channel+"|"+sessionID+"|"+textHash)
	s.served[channel+"|"+sessionID] = true
	return nil
}

func TestEnsure_PerSessionChannel_ServesOnceThenStopsAfterRecord(t *testing.T) {
	store := newStubStore()
	svc := New(Config{}, store)
	ctx := context.Background()

	notice, serve, err := svc.Ensure(ctx, "telegram", "chat-1")
	if err != nil || !serve {
		t.Fatalf("first turn: serve=%v err=%v, want serve=true err=nil", serve, err)
	}

	if err := svc.Record(ctx, "telegram", "chat-1", notice); err != nil {
		t.Fatalf("Record: %v", err)
	}

	_, serve, err = svc.Ensure(ctx, "telegram", "chat-1")
	if err != nil {
		t.Fatalf("second turn err: %v", err)
	}
	if serve {
		t.Error("second turn on the same session must not re-serve the disclosure")
	}
}

func TestEnsure_PerSessionChannel_ServesAgainForADifferentSession(t *testing.T) {
	store := newStubStore()
	svc := New(Config{}, store)
	ctx := context.Background()

	n, _, _ := svc.Ensure(ctx, "telegram", "chat-1")
	if err := svc.Record(ctx, "telegram", "chat-1", n); err != nil {
		t.Fatalf("Record: %v", err)
	}

	_, serve, err := svc.Ensure(ctx, "telegram", "chat-2")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !serve {
		t.Error("a different session on the same channel must be disclosed to")
	}
}

func TestEnsure_PerMessageChannel_AlwaysServesAndNeverConsultsTheStore(t *testing.T) {
	store := newStubStore()
	store.readErr = errors.New("store must not be consulted for per-message channels")
	svc := New(Config{}, store)
	ctx := context.Background()

	for i := range 3 {
		_, serve, err := svc.Ensure(ctx, "email", "thread-1")
		if err != nil {
			t.Fatalf("turn %d: unexpected err %v", i, err)
		}
		if !serve {
			t.Fatalf("turn %d: per-message channel must always serve", i)
		}
	}
}

// TestEnsure_StoreReadError_FailsTowardDisclosure pins design §4: a duplicate
// notice is a UX blemish, a skipped one is non-conformity — so a store read
// failure must serve anyway while still surfacing the error for logging.
func TestEnsure_StoreReadError_FailsTowardDisclosure(t *testing.T) {
	store := newStubStore()
	store.readErr = errors.New("db down")
	svc := New(Config{}, store)

	_, serve, err := svc.Ensure(context.Background(), "telegram", "chat-1")

	if !serve {
		t.Error("store read failure must fail TOWARD disclosure (serve=true)")
	}
	if err == nil {
		t.Error("store read failure must still surface the error for logging")
	}
}

// TestEnsure_NilStore_AlwaysServes keeps the service usable before the repo
// is wired (and in tests) without ever silently skipping the obligation.
func TestEnsure_NilStore_AlwaysServes(t *testing.T) {
	svc := New(Config{}, nil)

	_, serve, err := svc.Ensure(context.Background(), "telegram", "chat-1")

	if err != nil {
		t.Fatalf("nil store must not error: %v", err)
	}
	if !serve {
		t.Error("nil store must serve rather than skip")
	}
}

func TestRecord_PerMessageChannel_IsANoop(t *testing.T) {
	store := newStubStore()
	svc := New(Config{}, store)
	ctx := context.Background()

	n := svc.Notice()
	if err := svc.Record(ctx, "email", "thread-1", n); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if len(store.writes) != 0 {
		t.Errorf("per-message channel has no per-session state to keep; writes=%v", store.writes)
	}
}

func TestRecord_PersistsChannelSessionAndHash(t *testing.T) {
	store := newStubStore()
	svc := New(Config{}, store)

	n := svc.Notice()
	if err := svc.Record(context.Background(), "telegram", "chat-9", n); err != nil {
		t.Fatalf("Record: %v", err)
	}

	want := "telegram|chat-9|" + n.Hash
	if len(store.writes) != 1 || store.writes[0] != want {
		t.Errorf("writes = %v, want [%s]", store.writes, want)
	}
}

// --- Config validation ---

// TestConfig_Validate_RejectsWhitespaceOnlyText closes the back door: the
// design refuses an off-switch (§5), so "customise the text" must not become
// a way to disable the disclosure by setting it blank.
func TestConfig_Validate_RejectsWhitespaceOnlyText(t *testing.T) {
	for _, text := range []string{" ", "\t", "\n  \n"} {
		if err := (Config{Text: text}).Validate(); err == nil {
			t.Errorf("Config{Text:%q}.Validate() = nil, want an error", text)
		}
	}
}

func TestConfig_Validate_EmptyIsValidMeaningUseDefaults(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("zero Config must be valid (means 'use compiled defaults'), got %v", err)
	}
}
