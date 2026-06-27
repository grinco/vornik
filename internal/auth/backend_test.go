package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeBackend is a programmable Backend for chain tests.
type fakeBackend struct {
	name string
	id   *Identity
	err  error
	hits int
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Authenticate(_ context.Context, _ Credential) (*Identity, error) {
	f.hits++
	return f.id, f.err
}

// TestChain_StopsAtFirstMatch — the headline routing contract:
// a successful Authenticate short-circuits the walk; later
// backends in the chain are NOT consulted.
func TestChain_StopsAtFirstMatch(t *testing.T) {
	first := &fakeBackend{name: "first", id: &Identity{Subject: "u1"}}
	second := &fakeBackend{name: "second", id: &Identity{Subject: "u2"}}
	id, err := Chain(context.Background(), []Backend{first, second}, Credential{})
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if id == nil || id.Subject != "u1" {
		t.Errorf("got %+v, want u1", id)
	}
	// Backend.Name() is stamped onto the Identity.
	if id.Backend != "first" {
		t.Errorf("Identity.Backend = %q, want \"first\"", id.Backend)
	}
	if second.hits != 0 {
		t.Errorf("second backend was consulted after first matched (%d hits)", second.hits)
	}
}

// TestChain_FallsThroughOnNoOpinion — ErrNoCredential is the
// signal "this backend has no opinion"; the walk continues to
// the next backend. Critical for letting an OIDC backend share
// the chain with a DB-keys backend without misclaiming each
// other's credentials.
func TestChain_FallsThroughOnNoOpinion(t *testing.T) {
	first := &fakeBackend{name: "first", err: ErrNoCredential}
	second := &fakeBackend{name: "second", id: &Identity{Subject: "u-matched"}}
	id, err := Chain(context.Background(), []Backend{first, second}, Credential{})
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if id.Subject != "u-matched" {
		t.Errorf("Subject = %q, want u-matched", id.Subject)
	}
	if first.hits != 1 || second.hits != 1 {
		t.Errorf("expected both backends consulted; got first=%d second=%d", first.hits, second.hits)
	}
}

// TestChain_ShortCircuitsOnHardReject — ErrUnauthorized is a
// RECOGNISED-but-rejected signal; the chain MUST NOT fall
// through (otherwise a rejected OIDC token could collide-match
// a static key by accident). The chain returns ErrUnauthorized.
func TestChain_ShortCircuitsOnHardReject(t *testing.T) {
	first := &fakeBackend{name: "first", err: ErrUnauthorized}
	second := &fakeBackend{name: "second", id: &Identity{Subject: "would-match"}}
	_, err := Chain(context.Background(), []Backend{first, second}, Credential{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
	if second.hits != 0 {
		t.Errorf("hard reject leaked through; second backend was hit %d times", second.hits)
	}
}

// TestChain_UnexpectedErrorFailsClosed — hardening 2026-06-15
// (AUDIT batch-2 "auth chain should fail closed"): a bare,
// unclassified error from one backend MUST short-circuit the
// walk and be returned to the caller, NOT fall through to a
// later backend. Pre-fix this test's first backend returned a
// raw "DB connection refused" and the chain happily admitted via
// the second (static) backend — exactly the silent-downgrade a
// misconfigured OIDC provider would trigger. The fix makes the
// later backend unreachable on an unclassified error.
func TestChain_UnexpectedErrorFailsClosed(t *testing.T) {
	first := &fakeBackend{name: "first", err: errors.New("misconfigured provider")}
	second := &fakeBackend{name: "second", id: &Identity{Subject: "static-rescue"}}
	id, err := Chain(context.Background(), []Backend{first, second}, Credential{})
	if err == nil {
		t.Fatalf("expected fail-closed error, got identity %+v", id)
	}
	if errors.Is(err, ErrNoCredential) || errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("err = %v, want a fail-closed error (not a fall-through sentinel)", err)
	}
	if second.hits != 0 {
		t.Errorf("fail-closed leaked through; second backend was consulted (%d hits)", second.hits)
	}
}

// TestChain_BackendUnavailableFallsThrough — the EXPLICIT
// graceful-degrade path: a backend that classifies its error as
// transient (ErrBackendUnavailable) still lets the chain fall
// through, so a DB blip on one backend doesn't lock out callers
// the next backend can serve.
func TestChain_BackendUnavailableFallsThrough(t *testing.T) {
	first := &fakeBackend{name: "first", err: errors.Join(ErrBackendUnavailable, errors.New("DB connection refused"))}
	second := &fakeBackend{name: "second", id: &Identity{Subject: "static-rescue"}}
	id, err := Chain(context.Background(), []Backend{first, second}, Credential{})
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if id.Subject != "static-rescue" {
		t.Errorf("Subject = %q, want static-rescue", id.Subject)
	}
	if first.hits != 1 || second.hits != 1 {
		t.Errorf("expected both backends consulted; got first=%d second=%d", first.hits, second.hits)
	}
}

// TestChain_AllNoOpinionReturnsUnauthorized — when every backend
// in the chain has no opinion, the middleware's contract is to
// return ErrUnauthorized so the caller renders 401.
func TestChain_AllNoOpinionReturnsUnauthorized(t *testing.T) {
	first := &fakeBackend{name: "first", err: ErrNoCredential}
	second := &fakeBackend{name: "second", err: ErrNoCredential}
	_, err := Chain(context.Background(), []Backend{first, second}, Credential{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

// TestChain_EmptyChainReturnsUnauthorized — defensive: a
// misconfigured daemon with no backends MUST 401, never let
// requests through unauthenticated.
func TestChain_EmptyChainReturnsUnauthorized(t *testing.T) {
	_, err := Chain(context.Background(), nil, Credential{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("empty chain: err = %v, want ErrUnauthorized", err)
	}
}

// TestStaticKeysBackend_AcceptsValid — happy path. Pin the
// Identity shape so a future refactor of the static-keys
// adapter doesn't silently drop fields the audit / IDOR layers
// rely on.
func TestStaticKeysBackend_AcceptsValid(t *testing.T) {
	b := NewStaticKeysBackend(map[string][]string{
		"secret-key-1234567890": {"proj-a", "proj-b"},
	})
	id, err := b.Authenticate(context.Background(), Credential{
		BearerToken: "secret-key-1234567890",
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(id.Projects) != 2 || id.Projects[0] != "proj-a" {
		t.Errorf("Projects = %v", id.Projects)
	}
	if !strings.HasPrefix(id.Subject, "static:") {
		t.Errorf("Subject = %q, want static:* prefix", id.Subject)
	}
	// Subject MUST NOT contain the raw secret — leaking it
	// into audit rows would defeat the point of the
	// fingerprint helper.
	if strings.Contains(id.Subject, "secret-key-1234567890") {
		t.Errorf("Subject leaked raw secret: %q", id.Subject)
	}
}

// TestStaticKeysBackend_NoOpinionOnUnknown — unknown tokens
// return ErrNoCredential (not ErrUnauthorized). The chain
// behaviour depends on this: a token that doesn't match the
// static map must let the OIDC / DB-keys backend try.
func TestStaticKeysBackend_NoOpinionOnUnknown(t *testing.T) {
	b := NewStaticKeysBackend(map[string][]string{"x": nil})
	_, err := b.Authenticate(context.Background(), Credential{BearerToken: "not-x"})
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
}

// TestStaticKeysBackend_NoOpinionOnEmptyBearer — defensive:
// an empty bearer means "no credential of any kind"; backends
// must not return ErrUnauthorized for that case (the middleware
// builds its own 401 for missing-credential separately).
func TestStaticKeysBackend_NoOpinionOnEmptyBearer(t *testing.T) {
	b := NewStaticKeysBackend(map[string][]string{"x": nil})
	_, err := b.Authenticate(context.Background(), Credential{})
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
}

// TestStaticKeysBackend_NilMapNoOps — a deployment with no
// static keys configured (DB-only) must not crash; the backend
// just always says "no opinion".
func TestStaticKeysBackend_NilMapNoOps(t *testing.T) {
	for _, b := range []*StaticKeysBackend{
		NewStaticKeysBackend(nil),
		NewStaticKeysBackend(map[string][]string{}),
		nil, // nil receiver
	} {
		_, err := b.Authenticate(context.Background(), Credential{BearerToken: "anything"})
		if !errors.Is(err, ErrNoCredential) {
			t.Errorf("empty/nil backend err = %v, want ErrNoCredential", err)
		}
	}
}

// TestStaticKeysBackend_PrefixSuffixNoMatch — guard against a
// future refactor that swaps the constant-time compare for a
// HasPrefix shortcut.
func TestStaticKeysBackend_PrefixSuffixNoMatch(t *testing.T) {
	b := NewStaticKeysBackend(map[string][]string{
		"secret-1234567890": nil,
	})
	for _, candidate := range []string{
		"secret-123",
		"secret-1234567890extra",
		"secret-1234567891", // 1-byte diff at tail
	} {
		if _, err := b.Authenticate(context.Background(), Credential{BearerToken: candidate}); !errors.Is(err, ErrNoCredential) {
			t.Errorf("candidate %q matched unexpectedly", candidate)
		}
	}
}

// TestStaticKeysBackend_HashedCompare pins functional match/no-match
// behaviour across mixed key lengths around the slice-2 change that
// hashes both sides before the constant-time compare (parity with
// internal/api lookupAPIKey). NOTE: the timing property itself is not
// functionally observable — raw and hashed compares return identical
// results — so this test guards the refactor's behaviour preservation,
// not the oracle fix; that is enforced by review and the lockstep
// comments in static.go.
func TestStaticKeysBackend_HashedCompare(t *testing.T) {
	b := NewStaticKeysBackend(map[string][]string{
		"short":                          {"proj-a"},
		"a-much-longer-key-0123456789ab": nil,
	})
	cases := []struct {
		name    string
		token   string
		wantOK  bool
		wantPrj []string
	}{
		{"short key matches", "short", true, []string{"proj-a"}},
		{"long key matches", "a-much-longer-key-0123456789ab", true, nil},
		{"same length as short, no match", "shorx", false, nil},
		{"different length, no match", "sho", false, nil},
		{"empty token", "", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := b.Authenticate(context.Background(), Credential{BearerToken: tc.token})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("expected match, got err=%v", err)
				}
				if len(id.Projects) != len(tc.wantPrj) {
					t.Fatalf("projects = %v, want %v", id.Projects, tc.wantPrj)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected no match, got identity %+v", id)
			}
		})
	}
}

// TestFingerprintForLog_Shapes — pin the audit-row identifier
// format. Long tokens collapse to "head…tail"; short tokens
// land "short:*" so it's obvious in logs that something
// misconfigured.
func TestFingerprintForLog_Shapes(t *testing.T) {
	cases := map[string]string{
		"":                      "empty",
		"abc":                   "short:abc",
		"abcd":                  "short:abcd",
		"abcdefghij":            "short:ab…", // 10 chars → short
		"abcdefghijkl":          "abcdef…ijkl",
		"sk-acme-assistant-XYZ": "sk-acm…0XYZ"[:6] + "…" + "0XYZ"[len("0XYZ")-4:],
	}
	// Last case computed: "sk-acme-assistant-XYZ" — first 6 "sk-swa", last 4 "-XYZ" → "sk-acm…-XYZ"
	got := fingerprintForLog("sk-acme-assistant-XYZ")
	if got != "sk-acm…-XYZ" {
		t.Errorf("fingerprint = %q, want sk-acm…-XYZ", got)
	}
	for in, want := range cases {
		if want == "" || in == "sk-acme-assistant-XYZ" {
			continue
		}
		if got := fingerprintForLog(in); got != want {
			t.Errorf("fingerprintForLog(%q) = %q, want %q", in, got, want)
		}
	}
}
