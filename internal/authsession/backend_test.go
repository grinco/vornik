package authsession

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// stubValidator records call count and returns a canned session/err.
type stubValidator struct {
	sess  *persistence.UISession
	err   error
	calls int
}

func (s *stubValidator) Validate(_ context.Context, _ string) (*persistence.UISession, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.sess, nil
}

// stubResolver records call count and returns a canned principal/err.
// errSeq, when non-nil, is consumed per-call (lets a test inject a
// failure then a success).
type stubResolver struct {
	p      *authz.Principal
	err    error
	errSeq []error
	calls  int
}

func (s *stubResolver) ResolveUser(_ context.Context, _ string) (*authz.Principal, error) {
	s.calls++
	if len(s.errSeq) > 0 {
		e := s.errSeq[0]
		s.errSeq = s.errSeq[1:]
		if e != nil {
			return nil, e
		}
		return s.p, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.p, nil
}

func liveSession() *persistence.UISession {
	now := time.Now().UTC()
	return &persistence.UISession{
		ID:        "sess_1",
		UserID:    "user_1",
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(time.Hour),
	}
}

func TestSessionBackend_EmptyToken_NoStoreCall(t *testing.T) {
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{p: &authz.Principal{Role: authz.RoleUser}}
	b := NewSessionBackend(store, res, 0)
	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: ""})
	if !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("err = %v, want auth.ErrNoCredential", err)
	}
	if store.calls != 0 {
		t.Errorf("store called %d times for empty token, want 0", store.calls)
	}
}

func TestSessionBackend_DeadSession_NoCredential(t *testing.T) {
	store := &stubValidator{err: ErrSessionDead}
	res := &stubResolver{p: &authz.Principal{}}
	b := NewSessionBackend(store, res, 0)
	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("err = %v, want auth.ErrNoCredential (dead cookie must not block other backends)", err)
	}
	if res.calls != 0 {
		t.Errorf("resolver must not be called for a dead session")
	}
}

func TestSessionBackend_StoreTransportError_Propagates(t *testing.T) {
	boom := errors.New("db down")
	store := &stubValidator{err: boom}
	res := &stubResolver{p: &authz.Principal{}}
	b := NewSessionBackend(store, res, 0)
	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped transport error %v", err, boom)
	}
	if errors.Is(err, auth.ErrNoCredential) || errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("transport error must not masquerade as a sentinel: %v", err)
	}
	// Hardening 2026-06-15: transient store errors must carry the
	// explicit auth.ErrBackendUnavailable degrade sentinel so auth.Chain
	// falls through to the next backend instead of failing closed.
	if !errors.Is(err, auth.ErrBackendUnavailable) {
		t.Errorf("err = %v, want wrapped auth.ErrBackendUnavailable", err)
	}
}

func TestSessionBackend_DisabledUser_Unauthorized(t *testing.T) {
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{err: authz.ErrUserDisabled}
	b := NewSessionBackend(store, res, 0)
	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want auth.ErrUnauthorized (disabled user hard-stop)", err)
	}
}

func TestSessionBackend_VanishedUser_Unauthorized(t *testing.T) {
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{err: authz.ErrUnknownIdentity}
	b := NewSessionBackend(store, res, 0)
	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want auth.ErrUnauthorized (vanished user hard-stop)", err)
	}
}

func TestSessionBackend_ResolverTransportError_Propagates(t *testing.T) {
	boom := errors.New("resolver down")
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{err: boom}
	b := NewSessionBackend(store, res, 0)
	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
	if errors.Is(err, auth.ErrUnauthorized) || errors.Is(err, auth.ErrNoCredential) {
		t.Errorf("resolver transport error must not masquerade as a sentinel: %v", err)
	}
	// Hardening 2026-06-15: same graceful-degrade contract as the
	// store transport error — must carry auth.ErrBackendUnavailable.
	if !errors.Is(err, auth.ErrBackendUnavailable) {
		t.Errorf("err = %v, want wrapped auth.ErrBackendUnavailable", err)
	}
}

func TestSessionBackend_Happy_AllFields(t *testing.T) {
	sess := liveSession()
	store := &stubValidator{sess: sess}
	res := &stubResolver{p: &authz.Principal{
		UserID:      "user_1",
		DisplayName: "Vadim",
		Role:        authz.RoleAdmin,
		Projects:    []string{"*"},
	}}
	b := NewSessionBackend(store, res, 0)
	id, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "user:user_1" {
		t.Errorf("Subject = %q, want user:user_1", id.Subject)
	}
	if id.DisplayName != "Vadim" {
		t.Errorf("DisplayName = %q", id.DisplayName)
	}
	if len(id.Projects) != 1 || id.Projects[0] != "*" {
		t.Errorf("Projects = %v, want [*]", id.Projects)
	}
	if !id.IssuedAt.Equal(sess.CreatedAt) {
		t.Errorf("IssuedAt = %v, want %v", id.IssuedAt, sess.CreatedAt)
	}
	if !id.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", id.ExpiresAt, sess.ExpiresAt)
	}
	if id.Extra[auth.ExtraSessionRole] != authz.RoleAdmin {
		t.Errorf("Extra[role] = %v, want admin", id.Extra[auth.ExtraSessionRole])
	}
	if id.Extra[auth.ExtraSessionID] != "sess_1" {
		t.Errorf("Extra[id] = %v, want sess_1", id.Extra[auth.ExtraSessionID])
	}
	if id.Extra[auth.ExtraSessionUserID] != "user_1" {
		t.Errorf("Extra[user_id] = %v, want user_1", id.Extra[auth.ExtraSessionUserID])
	}
}

func TestSessionBackend_Cache_HitsResolverOnceWithinTTL(t *testing.T) {
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{p: &authz.Principal{UserID: "user_1", Role: authz.RoleUser}}
	b := NewSessionBackend(store, res, time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"}); err != nil {
			t.Fatalf("Authenticate #%d: %v", i, err)
		}
	}
	if res.calls != 1 {
		t.Errorf("resolver calls = %d, want 1 (cached within TTL)", res.calls)
	}
}

func TestSessionBackend_Cache_ExpiresAfterTTL(t *testing.T) {
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{p: &authz.Principal{UserID: "user_1", Role: authz.RoleUser}}
	b := NewSessionBackend(store, res, time.Minute)
	base := time.Now()
	b.now = func() time.Time { return base }
	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"}); err != nil {
		t.Fatalf("Authenticate #1: %v", err)
	}
	// Advance past the TTL.
	b.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"}); err != nil {
		t.Fatalf("Authenticate #2: %v", err)
	}
	if res.calls != 2 {
		t.Errorf("resolver calls = %d, want 2 (cache expired)", res.calls)
	}
}

func TestSessionBackend_Cache_NotSharedAcrossUsers(t *testing.T) {
	store := &stubValidator{}
	res := &stubResolver{p: &authz.Principal{Role: authz.RoleUser}}
	b := NewSessionBackend(store, res, time.Minute)

	store.sess = &persistence.UISession{ID: "s1", UserID: "user_A", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "a"}); err != nil {
		t.Fatalf("Authenticate A: %v", err)
	}
	store.sess = &persistence.UISession{ID: "s2", UserID: "user_B", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "b"}); err != nil {
		t.Fatalf("Authenticate B: %v", err)
	}
	if res.calls != 2 {
		t.Errorf("resolver calls = %d, want 2 (per-user cache must not serve a different user)", res.calls)
	}
}

// TestSessionBackend_AdminNotCached is the A2 regression
// (https://docs.vornik.io): an admin principal must
// NOT be served from the positive cache, so a demoted admin loses the
// admin role on the very next request (within the cache bound) rather
// than riding the ~60s TTL. The resolver is hit every request for an
// admin, and the post-demotion principal is what the caller sees.
func TestSessionBackend_AdminNotCached(t *testing.T) {
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{}
	b := NewSessionBackend(store, res, time.Minute)

	// First request: still admin.
	res.p = &authz.Principal{UserID: "user_1", Role: authz.RoleAdmin, Projects: []string{"*"}}
	id1, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if err != nil {
		t.Fatalf("Authenticate #1: %v", err)
	}
	if id1.Extra[auth.ExtraSessionRole] != authz.RoleAdmin {
		t.Fatalf("first request role = %v, want admin", id1.Extra[auth.ExtraSessionRole])
	}

	// The admin was demoted out of band; the next request must re-resolve
	// and observe the demotion — not serve a cached admin principal.
	res.p = &authz.Principal{UserID: "user_1", Role: authz.RoleUser}
	id2, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if err != nil {
		t.Fatalf("Authenticate #2: %v", err)
	}
	if id2.Extra[auth.ExtraSessionRole] != authz.RoleUser {
		t.Errorf("post-demotion role = %v, want user (admin must not be cached)", id2.Extra[auth.ExtraSessionRole])
	}
	if res.calls != 2 {
		t.Errorf("resolver calls = %d, want 2 (admin principal must never be cached)", res.calls)
	}
}

func TestSessionBackend_Cache_NotPoisonedByError(t *testing.T) {
	// A resolver failure must NOT be cached: the retry must hit the
	// resolver again and succeed.
	store := &stubValidator{sess: liveSession()}
	res := &stubResolver{
		p:      &authz.Principal{UserID: "user_1", Role: authz.RoleUser},
		errSeq: []error{errors.New("transient"), nil},
	}
	b := NewSessionBackend(store, res, time.Minute)
	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"}); err == nil {
		t.Fatal("Authenticate #1 should have failed (transient resolver error)")
	}
	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"}); err != nil {
		t.Fatalf("Authenticate #2 should succeed (error must not poison the cache): %v", err)
	}
	if res.calls != 2 {
		t.Errorf("resolver calls = %d, want 2 (error not cached)", res.calls)
	}
}

func TestNewSessionBackend_DefaultTTL(t *testing.T) {
	b := NewSessionBackend(&stubValidator{}, &stubResolver{}, 0)
	if b.TTL != 60*time.Second {
		t.Errorf("default TTL = %v, want 60s", b.TTL)
	}
	if b.Name() != "session" {
		t.Errorf("Name() = %q, want session", b.Name())
	}
}

func TestSessionBackend_NilReceiver_NoCredential(t *testing.T) {
	var b *SessionBackend
	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "x"})
	if !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("nil backend err = %v, want auth.ErrNoCredential", err)
	}
}
