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

type capStore struct{ sess *persistence.UISession }

func (s *capStore) Validate(context.Context, string) (*persistence.UISession, error) {
	if s.sess == nil {
		return nil, ErrSessionDead
	}
	return s.sess, nil
}

type capResolver struct {
	calls int
	p     *authz.Principal
}

func (r *capResolver) ResolveUser(context.Context, string) (*authz.Principal, error) {
	r.calls++
	if r.p == nil {
		return &authz.Principal{UserID: "user-1", Role: "user"}, nil
	}
	return r.p, nil
}

type capChecker struct {
	calls int
	err   error
}

func (c *capChecker) CheckSessionCredential(context.Context, string, string) error {
	c.calls++
	return c.err
}

type capRevoker struct{ revoked []string }

func (r *capRevoker) RevokeSession(_ context.Context, id string) error {
	r.revoked = append(r.revoked, id)
	return nil
}

func capSession(originCredential string) *persistence.UISession {
	return &persistence.UISession{
		ID: "sess-1", UserID: "user-1", Provider: "credential",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
		OriginCredentialID: originCredential,
	}
}

func TestCredentialCap_AnOIDCSessionIsNotCapped(t *testing.T) {
	// It is not capped by a credential because there is none — the correct
	// answer rather than a skipped check.
	checker := &capChecker{}
	b := NewSessionBackend(&capStore{sess: capSession("")}, &capResolver{}, time.Minute)
	b.Credentials = checker

	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"}); err != nil {
		t.Fatalf("Authenticate() = %v, want an OIDC session to authenticate", err)
	}
	if checker.calls != 0 {
		t.Fatalf("the checker ran %d times for a session no credential minted", checker.calls)
	}
}

func TestCredentialCap_ALiveCredentialAuthenticates(t *testing.T) {
	checker := &capChecker{}
	b := NewSessionBackend(&capStore{sess: capSession("key-1")}, &capResolver{}, time.Minute)
	b.Credentials = checker

	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"}); err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
	if checker.calls != 1 {
		t.Fatalf("the checker ran %d times, want exactly 1", checker.calls)
	}
}

func TestCredentialCap_ADeadCredentialRefusesAndRevokes(t *testing.T) {
	checker := &capChecker{err: errors.New("revoked")}
	revoker := &capRevoker{}
	b := NewSessionBackend(&capStore{sess: capSession("key-1")}, &capResolver{}, time.Minute)
	b.Credentials, b.Revoker = checker, revoker

	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"})
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want auth.ErrUnauthorized", err)
	}
	// Refusing alone would leave a row failing every request forever, and
	// an operator reading the session list could not tell live from doomed.
	if len(revoker.revoked) != 1 || revoker.revoked[0] != "sess-1" {
		t.Fatalf("revoked = %v, want the dead session ended", revoker.revoked)
	}
}

func TestCredentialCap_RunsBeforeTheResolverAndItsCache(t *testing.T) {
	// THE POINT OF THE WHOLE FILE. The principal cache is keyed by USER
	// ID, so a key revocation cannot invalidate it — a cap applied after
	// the cache would keep authenticating a revoked credential until the
	// entry expired. This asserts the resolver is never even reached.
	checker := &capChecker{err: errors.New("revoked")}
	resolver := &capResolver{}
	b := NewSessionBackend(&capStore{sess: capSession("key-1")}, resolver, time.Minute)
	b.Credentials, b.Revoker = checker, &capRevoker{}

	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"}); err == nil {
		t.Fatal("Authenticate() = nil, want a refusal")
	}
	if resolver.calls != 0 {
		t.Fatalf("the resolver ran %d times for a session with a dead credential", resolver.calls)
	}
}

func TestCredentialCap_IsNotServedFromTheCacheOnLaterRequests(t *testing.T) {
	// The regression this guards: authenticate once successfully (warming
	// the principal cache), then revoke the key. The NEXT request must
	// fail — not the one after the TTL expires.
	checker := &capChecker{}
	resolver := &capResolver{}
	b := NewSessionBackend(&capStore{sess: capSession("key-1")}, resolver, time.Hour)
	b.Credentials, b.Revoker = checker, &capRevoker{}

	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"}); err != nil {
		t.Fatalf("first Authenticate() = %v", err)
	}
	checker.err = errors.New("revoked")

	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"}); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("second Authenticate() = %v, want the revocation to bite on the NEXT request, "+
			"not at the %s cache TTL", err, b.TTL)
	}
	if checker.calls != 2 {
		t.Fatalf("the checker ran %d times across two requests, want 2 — it must be uncached", checker.calls)
	}
}

func TestCredentialCap_FailsClosedWithNoCheckerWired(t *testing.T) {
	// A session declaring an originating credential on a deployment that
	// cannot check it. Waving it through would be a control that cannot
	// distinguish "examined and clean" from "never examined".
	b := NewSessionBackend(&capStore{sess: capSession("key-1")}, &capResolver{}, time.Minute)

	_, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"})
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want auth.ErrUnauthorized", err)
	}
	if !errors.Is(err, ErrCredentialUncheckable) {
		t.Fatalf("err = %v, want ErrCredentialUncheckable so the cause is diagnosable", err)
	}
}

func TestCredentialCap_ARevokeFailureStillRefuses(t *testing.T) {
	// A revoke that fails must not turn a refusal into an acceptance.
	checker := &capChecker{err: errors.New("revoked")}
	b := NewSessionBackend(&capStore{sess: capSession("key-1")}, &capResolver{}, time.Minute)
	b.Credentials, b.Revoker = checker, failingRevoker{}

	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"}); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want the refusal to survive a failed revoke", err)
	}
}

type failingRevoker struct{}

func (failingRevoker) RevokeSession(context.Context, string) error {
	return errors.New("database is gone")
}

func TestCredentialCap_NoRevokerIsSafe(t *testing.T) {
	checker := &capChecker{err: errors.New("revoked")}
	b := NewSessionBackend(&capStore{sess: capSession("key-1")}, &capResolver{}, time.Minute)
	b.Credentials = checker

	if _, err := b.Authenticate(context.Background(), auth.Credential{SessionToken: "t"}); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want a refusal even with no revoker wired", err)
	}
}
