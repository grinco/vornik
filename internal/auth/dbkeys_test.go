package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// stubLookup returns a fixed row when the presented hash matches.
type stubLookup struct {
	hash string
	row  *persistence.APIKey
	err  error
}

func (s *stubLookup) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	if h == s.hash {
		return s.row, nil
	}
	return nil, errors.New("not found")
}

// stubToucher records TouchLastUsed calls.
type stubToucher struct {
	mu  sync.Mutex
	ids []string
	ch  chan struct{}
}

func (s *stubToucher) TouchLastUsed(_ context.Context, id string) error {
	s.mu.Lock()
	s.ids = append(s.ids, id)
	s.mu.Unlock()
	if s.ch != nil {
		s.ch <- struct{}{}
	}
	return nil
}

func mustGenerateKey(t *testing.T, projectID string) string {
	t.Helper()
	k, err := apikey.Generate(projectID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return k
}

func TestDBKeysBackend_HappyPath(t *testing.T) {
	key := mustGenerateKey(t, "proj-a")
	created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	row := &persistence.APIKey{
		ID: "01KEY", ProjectID: "proj-a", Name: "ci-bot",
		CreatedAt: created, ClientKind: "claude-code",
	}
	toucher := &stubToucher{ch: make(chan struct{}, 1)}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(key), row: row}, toucher)

	id, err := b.Authenticate(context.Background(), Credential{BearerToken: key})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id.Subject != "01KEY" {
		t.Errorf("Subject = %q, want api_keys.id", id.Subject)
	}
	if id.BoundProjectID != "proj-a" {
		t.Errorf("BoundProjectID = %q", id.BoundProjectID)
	}
	if len(id.Projects) != 1 || id.Projects[0] != "proj-a" {
		t.Errorf("Projects = %v", id.Projects)
	}
	if id.DisplayName != "ci-bot" {
		t.Errorf("DisplayName = %q", id.DisplayName)
	}
	if !id.IssuedAt.Equal(created) {
		t.Errorf("IssuedAt = %v", id.IssuedAt)
	}
	got, ok := id.Extra[ExtraDBKeyRow].(*persistence.APIKey)
	if !ok || got != row {
		t.Errorf("Extra[%q] missing or wrong: %v", ExtraDBKeyRow, id.Extra)
	}
	// Async touch fires.
	select {
	case <-toucher.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("TouchLastUsed never fired")
	}
}

func TestDBKeysBackend_NoOpinionShapes(t *testing.T) {
	b := NewDBKeysBackend(&stubLookup{}, nil)
	for _, token := range []string{"", "some-static-key", "Bearer xyz"} {
		_, err := b.Authenticate(context.Background(), Credential{BearerToken: token})
		if !errors.Is(err, ErrNoCredential) {
			t.Errorf("token %q: err = %v, want ErrNoCredential", token, err)
		}
	}
}

func TestDBKeysBackend_LookupMissFallsThrough(t *testing.T) {
	key := mustGenerateKey(t, "proj-a")
	b := NewDBKeysBackend(&stubLookup{err: errors.New("db down")}, nil)
	_, err := b.Authenticate(context.Background(), Credential{BearerToken: key})
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential (migration fall-through)", err)
	}
}

func TestDBKeysBackend_TamperedPrefixHardRejects(t *testing.T) {
	// Key minted for proj-a but the DB row says proj-b: the
	// prefix-embedded project no longer matches → ErrUnauthorized,
	// NOT fall-through. Pins middleware.go:263-269 behaviour.
	key := mustGenerateKey(t, "proj-a")
	row := &persistence.APIKey{ID: "01KEY", ProjectID: "proj-b", Name: "x"}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(key), row: row}, nil)
	_, err := b.Authenticate(context.Background(), Credential{BearerToken: key})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestDBKeysBackend_NilLookupNoOpinion(t *testing.T) {
	b := NewDBKeysBackend(nil, nil)
	key := mustGenerateKey(t, "proj-a")
	_, err := b.Authenticate(context.Background(), Credential{BearerToken: key})
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
}

func TestDBKeysBackend_ExpiresAtCopied(t *testing.T) {
	key := mustGenerateKey(t, "proj-a")
	exp := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	row := &persistence.APIKey{ID: "01KEY", ProjectID: "proj-a", Name: "x", ExpiresAt: &exp}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(key), row: row}, nil)
	id, err := b.Authenticate(context.Background(), Credential{BearerToken: key})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !id.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want %v", id.ExpiresAt, exp)
	}
}
