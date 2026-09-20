package authsession_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
)

// stubRepo is an in-memory UISessionRepository.
// map key is the token hash (sha256 hex).
// touchCall records the args of one TouchSession invocation so tests can
// assert the IP refresh (2026-06-23) as well as the session id.
type touchCall struct {
	id string
	ip string
}

type stubRepo struct {
	mu       sync.Mutex
	byHash   map[string]*persistence.UISession
	byID     map[string]*persistence.UISession
	touchCh  chan touchCall // receives the args of each TouchSession call
	touchErr error
	getErr   error // returned by GetActiveByTokenHash (overrides normal lookup)
}

func newStubRepo() *stubRepo {
	return &stubRepo{
		byHash:  make(map[string]*persistence.UISession),
		byID:    make(map[string]*persistence.UISession),
		touchCh: make(chan touchCall, 4),
	}
}

func (r *stubRepo) CreateSession(_ context.Context, s *persistence.UISession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := *s
	r.byHash[s.TokenHash] = &clone
	r.byID[s.ID] = &clone
	return nil
}

func (r *stubRepo) GetActiveByTokenHash(_ context.Context, hash string) (*persistence.UISession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	s, ok := r.byHash[hash]
	if !ok {
		return nil, persistence.ErrSessionNotFound
	}
	clone := *s
	return &clone, nil
}

func (r *stubRepo) TouchSession(_ context.Context, id, ip string) error {
	if r.touchErr != nil {
		return r.touchErr
	}
	r.touchCh <- touchCall{id: id, ip: ip}
	return nil
}

func (r *stubRepo) RevokeSession(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	if !ok {
		return persistence.ErrSessionNotFound
	}
	// Already revoked: RevokeSession must return ErrSessionNotFound
	// (zero affected rows is an error per the contract).
	if s.RevokedAt != nil {
		return persistence.ErrSessionNotFound
	}
	now := time.Now().UTC()
	s.RevokedAt = &now
	// remove from hash map so GetActiveByTokenHash returns not-found
	delete(r.byHash, s.TokenHash)
	return nil
}

func (r *stubRepo) DeleteExpiredSessions(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (r *stubRepo) CountByStatus(_ context.Context) (persistence.UISessionStatusCounts, error) {
	return persistence.UISessionStatusCounts{}, nil
}

func (r *stubRepo) ListActiveByUser(_ context.Context, _ string) ([]*persistence.UISession, error) {
	return nil, nil
}

func (r *stubRepo) RevokeSessionForUser(_ context.Context, _, _ string) error {
	return nil
}

// waitTouch blocks until TouchSession fires for any session ID or timeout.
func (r *stubRepo) waitTouch(t *testing.T, timeout time.Duration) touchCall {
	t.Helper()
	select {
	case tc := <-r.touchCh:
		return tc
	case <-time.After(timeout):
		t.Fatal("TouchSession never fired")
		return touchCall{}
	}
}

// expectNoTouch asserts TouchSession does NOT fire within the window.
func (r *stubRepo) expectNoTouch(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case tc := <-r.touchCh:
		t.Fatalf("TouchSession fired unexpectedly for %q", tc.id)
	case <-time.After(window):
	}
}

// --- helpers ---

func newStore(repo *stubRepo, lifetime, idle time.Duration, now func() time.Time) *authsession.Store {
	return authsession.New(repo, lifetime, idle, authsession.WithNow(now))
}

var (
	fixedNow     = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	oneDay       = 24 * time.Hour
	oneHour      = time.Hour
	testUserID   = "user_abc"
	testProvider = "github"
	testIP       = "1.2.3.4"
	testUA       = "Mozilla/5.0"
)

// --- tests ---

// Case 1: Create returns a 43-char base64url token; repo receives a session
// with all fields set correctly.
func TestCreate_Happy(t *testing.T) {
	repo := newStubRepo()
	st := newStore(repo, oneDay, oneHour, func() time.Time { return fixedNow })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 32 random bytes base64url-raw-encoded = ceil(32*8/6)=43 chars.
	if len(raw) != 43 {
		t.Errorf("raw token len = %d, want 43", len(raw))
	}
	// Must be base64url (no padding, no +/)
	for _, c := range raw {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", c) {
			t.Errorf("raw token contains non-base64url char %q", c)
		}
	}

	repo.mu.Lock()
	if len(repo.byHash) != 1 {
		repo.mu.Unlock()
		t.Fatalf("repo has %d sessions, want 1", len(repo.byHash))
	}
	var stored *persistence.UISession
	for _, s := range repo.byHash {
		stored = s
	}
	repo.mu.Unlock()

	// TokenHash != raw token.
	if stored.TokenHash == raw {
		t.Error("TokenHash must not equal raw token")
	}
	// TokenHash is sha256 hex: 64 chars.
	if len(stored.TokenHash) != 64 {
		t.Errorf("TokenHash len = %d, want 64 (hex sha256)", len(stored.TokenHash))
	}
	// ID prefixed "sess_".
	if !strings.HasPrefix(stored.ID, "sess_") {
		t.Errorf("ID = %q, want sess_ prefix", stored.ID)
	}
	// Timestamps.
	if !stored.ExpiresAt.Equal(fixedNow.Add(oneDay)) {
		t.Errorf("ExpiresAt = %v, want %v", stored.ExpiresAt, fixedNow.Add(oneDay))
	}
	if !stored.LastSeenAt.Equal(fixedNow) {
		t.Errorf("LastSeenAt = %v, want %v", stored.LastSeenAt, fixedNow)
	}
	if !stored.CreatedAt.Equal(fixedNow) {
		t.Errorf("CreatedAt = %v, want %v", stored.CreatedAt, fixedNow)
	}
	// Fields passed through.
	if stored.UserID != testUserID {
		t.Errorf("UserID = %q, want %q", stored.UserID, testUserID)
	}
	if stored.Provider != testProvider {
		t.Errorf("Provider = %q, want %q", stored.Provider, testProvider)
	}
	if stored.IP != testIP {
		t.Errorf("IP = %q, want %q", stored.IP, testIP)
	}
	if stored.UserAgent != testUA {
		t.Errorf("UserAgent = %q, want %q", stored.UserAgent, testUA)
	}
}

// Case 2: Validate happy path — returns session and fires async Touch
// once the last_seen row is older than the coalescing interval.
func TestValidate_Happy(t *testing.T) {
	repo := newStubRepo()
	clock := fixedNow
	st := newStore(repo, oneDay, oneHour, func() time.Time { return clock })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance past the touch interval (idle=1h → interval=min(5m,30m)=5m)
	// so the coalesced last_seen touch fires.
	clock = fixedNow.Add(6 * time.Minute)
	got, err := st.Validate(context.Background(), raw)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got == nil {
		t.Fatal("Validate returned nil session")
	}
	if got.UserID != testUserID {
		t.Errorf("UserID = %q", got.UserID)
	}

	// Async touch must fire with the session's ID.
	tc := repo.waitTouch(t, 2*time.Second)
	if tc.id != got.ID {
		t.Errorf("touched ID = %q, want %q", tc.id, got.ID)
	}
}

// TestValidate_RefreshesIP is the stale-IP regression (2026-06-23): the
// admin viewer showed a session's login-time IP forever because the touch
// path never refreshed it. Validate must read the spoof-safe client IP off
// the request ctx (realip) and pass it to TouchSession so the stored IP
// tracks where the session is currently active. Pre-fix TouchSession took
// no IP and the value was frozen at CreateSession.
func TestValidate_RefreshesIP(t *testing.T) {
	repo := newStubRepo()
	clock := fixedNow
	st := newStore(repo, oneDay, oneHour, func() time.Time { return clock })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance past the touch interval so the coalesced touch fires, and
	// validate from a DIFFERENT, current IP carried on the request ctx.
	clock = fixedNow.Add(6 * time.Minute)
	const currentIP = "203.0.113.9"
	ctx := realip.WithClientIP(context.Background(), currentIP)
	if _, err := st.Validate(ctx, raw); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	tc := repo.waitTouch(t, 2*time.Second)
	if tc.ip != currentIP {
		t.Errorf("touched IP = %q, want %q (current request IP, not the %q login IP)", tc.ip, currentIP, testIP)
	}
}

// TestValidate_TouchOmitsIPWhenAbsent guards the empty-IP contract: a
// validate with no resolved client IP on the ctx (non-middleware path)
// must pass "" so the repo leaves the stored IP untouched rather than
// blanking a real value.
func TestValidate_TouchOmitsIPWhenAbsent(t *testing.T) {
	repo := newStubRepo()
	clock := fixedNow
	st := newStore(repo, oneDay, oneHour, func() time.Time { return clock })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	clock = fixedNow.Add(6 * time.Minute)
	if _, err := st.Validate(context.Background(), raw); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	tc := repo.waitTouch(t, 2*time.Second)
	if tc.ip != "" {
		t.Errorf("touched IP = %q, want empty (no IP on ctx)", tc.ip)
	}
}

// TestValidate_CoalescesTouch is the hardening regression (2026-06-15,
// auth LLD review batch 2 — last_seen_at hot spot): a Validate within the
// touch interval of the last write must NOT fire a TouchSession, so the
// ui_sessions write rate is capped independent of request rate. Pre-fix
// every validated request spawned a TouchSession goroutine.
func TestValidate_CoalescesTouch(t *testing.T) {
	repo := newStubRepo()
	clock := fixedNow
	st := newStore(repo, oneDay, oneHour, func() time.Time { return clock })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Validate 1 minute later — well inside the 5m interval. No touch.
	clock = fixedNow.Add(time.Minute)
	if _, err := st.Validate(context.Background(), raw); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	repo.expectNoTouch(t, 200*time.Millisecond)
}

// Case 3: Validate("") returns ErrNoSession without touching the repo.
func TestValidate_EmptyToken(t *testing.T) {
	repo := newStubRepo()
	st := newStore(repo, oneDay, oneHour, func() time.Time { return fixedNow })

	_, err := st.Validate(context.Background(), "")
	if !errors.Is(err, authsession.ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
	// Repo must not have been called.
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.byHash) != 0 {
		t.Error("repo was touched for empty token")
	}
}

// Case 4: Unknown token → ErrNoSession.
func TestValidate_UnknownToken(t *testing.T) {
	repo := newStubRepo()
	st := newStore(repo, oneDay, oneHour, func() time.Time { return fixedNow })

	_, err := st.Validate(context.Background(), "unknownTOKENthatdoesnotexistXXXXXXXXXX123")
	if !errors.Is(err, authsession.ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

// Case 5: Expired session → ErrNoSession.
func TestValidate_Expired(t *testing.T) {
	repo := newStubRepo()
	now := fixedNow
	st := newStore(repo, oneDay, oneHour, func() time.Time { return now })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance clock past ExpiresAt.
	now = fixedNow.Add(oneDay + time.Second)

	_, err = st.Validate(context.Background(), raw)
	if !errors.Is(err, authsession.ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession (expired)", err)
	}
}

// Case 6: Idle-timed-out session → ErrNoSession.
// idleTimeout=1h, but LastSeenAt is 2h ago (not yet globally expired).
func TestValidate_IdleTimeout(t *testing.T) {
	repo := newStubRepo()
	now := fixedNow
	st := newStore(repo, 7*24*time.Hour, oneHour, func() time.Time { return now })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance 2h — past idle but well before expiry.
	now = fixedNow.Add(2 * time.Hour)

	_, err = st.Validate(context.Background(), raw)
	if !errors.Is(err, authsession.ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession (idle)", err)
	}
}

// Case 7: idleTimeout=0 disables the idle check — same setup as case 6 but allowed.
func TestValidate_IdleDisabled(t *testing.T) {
	repo := newStubRepo()
	now := fixedNow
	st := newStore(repo, 7*24*time.Hour, 0, func() time.Time { return now })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance 2h — would be idle if check were enabled.
	now = fixedNow.Add(2 * time.Hour)

	got, err := st.Validate(context.Background(), raw)
	if err != nil {
		t.Errorf("err = %v, want nil (idle disabled)", err)
	}
	if got == nil {
		t.Error("expected session, got nil")
	}
}

// Case 8: Repo transport error (not ErrSessionNotFound) → wrapped error, NOT ErrNoSession.
func TestValidate_RepoTransportError(t *testing.T) {
	repo := newStubRepo()
	transportErr := errors.New("db: connection refused")
	repo.getErr = transportErr
	st := newStore(repo, oneDay, oneHour, func() time.Time { return fixedNow })

	_, err := st.Validate(context.Background(), "somevalidlookingtoken1234567890123456789")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, authsession.ErrNoSession) {
		t.Error("transport error must not be mapped to ErrNoSession")
	}
	if !errors.Is(err, transportErr) {
		t.Errorf("err = %v, want it to wrap %v", err, transportErr)
	}
}

// Case 9: Revoke delegates to repo; ErrSessionNotFound → ErrNoSession; nil → nil.
func TestRevoke(t *testing.T) {
	repo := newStubRepo()
	st := newStore(repo, oneDay, oneHour, func() time.Time { return fixedNow })

	raw, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Retrieve the session ID.
	var sessID string
	repo.mu.Lock()
	for _, s := range repo.byHash {
		sessID = s.ID
	}
	repo.mu.Unlock()

	// Valid revoke → nil.
	if err := st.Revoke(context.Background(), sessID); err != nil {
		t.Errorf("Revoke: %v", err)
	}

	// After revoke, Validate should return ErrNoSession.
	_, err = st.Validate(context.Background(), raw)
	if !errors.Is(err, authsession.ErrNoSession) {
		t.Errorf("post-revoke Validate err = %v, want ErrNoSession", err)
	}

	// Second revoke on same ID → ErrNoSession (session already revoked/absent).
	err = st.Revoke(context.Background(), sessID)
	if !errors.Is(err, authsession.ErrNoSession) {
		t.Errorf("double revoke err = %v, want ErrNoSession", err)
	}

	// Revoke unknown ID → ErrNoSession.
	err = st.Revoke(context.Background(), "sess_unknown")
	if !errors.Is(err, authsession.ErrNoSession) {
		t.Errorf("unknown revoke err = %v, want ErrNoSession", err)
	}
}

// Case 10: Two Creates produce different tokens (entropy sanity).
func TestCreate_UniqueTokens(t *testing.T) {
	repo := newStubRepo()
	st := newStore(repo, oneDay, oneHour, func() time.Time { return fixedNow })

	raw1, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	raw2, err := st.Create(context.Background(), testUserID, testProvider, testIP, testUA, "")
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if raw1 == raw2 {
		t.Error("two consecutive tokens are identical — entropy failure")
	}
}
