package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// keysStubRepo is an APIKeyRepository slice sufficient for the claim path.
type keysStubRepo struct {
	persistence.APIKeyRepository
	rows map[string]*persistence.APIKey
}

func (k *keysStubRepo) GetByID(_ context.Context, id string) (*persistence.APIKey, error) {
	row, ok := k.rows[id]
	if !ok {
		return nil, persistence.ErrAPIKeyNotFound
	}
	return row, nil
}

func newKeysServer(t *testing.T) (*Server, *accountsStubRepo, *accountsStubAudit, *keysStubRepo) {
	t.Helper()
	repo, audit := newAccountsStubRepo(), &accountsStubAudit{}
	keys := &keysStubRepo{rows: map[string]*persistence.APIKey{}}
	cfg := config.DefaultConfig()
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}
	acc := authz.NewAccounts(repo, audit).WithAPIKeys(keys)
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg), WithAccountsService(acc))
	return s, repo, audit, keys
}

func claimPost(t *testing.T, s *Server, userID, keyID, secret string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"keyId": keyID, "keySecret": secret})
	req := accountsReq(http.MethodPost, "/api/v1/operator/accounts/"+userID+"/claim-key", string(body))
	rec := httptest.NewRecorder()
	s.OperatorAccountItem(rec, req)
	return rec
}

// TestClaimKeyRoute_RefusalsAreByteIdentical is §5.4's oracle rule at the HTTP
// boundary. A caller holding a valid secret must not learn from the response
// whether the key exists or is already claimed — and the probe input here is a
// real credential, which is why this matters more than the link-code oracle.
func TestClaimKeyRoute_RefusalsAreByteIdentical(t *testing.T) {
	s, repo, _, keys := newKeysServer(t)
	repo.users["user_a"] = &persistence.User{ID: "user_a", DisplayName: "A"}
	repo.users["user_b"] = &persistence.User{ID: "user_b", DisplayName: "B"}
	keys.rows["akey_live"] = &persistence.APIKey{ID: "akey_live", ProjectID: "p", KeyHash: apikey.Hash("sk-right"), CreatedAt: time.Now().UTC()}
	keys.rows["akey_taken"] = &persistence.APIKey{ID: "akey_taken", ProjectID: "p", KeyHash: apikey.Hash("sk-taken"), CreatedAt: time.Now().UTC()}

	if rec := claimPost(t, s, "user_b", "akey_taken", "sk-taken"); rec.Code != http.StatusOK {
		t.Fatalf("seed claim: status %d body %s", rec.Code, rec.Body.String())
	}

	cases := []struct{ name, keyID, secret string }{
		{"wrong secret", "akey_live", "sk-WRONG"},
		{"key id does not exist", "akey_ghost", "sk-right"},
		{"already claimed by someone else", "akey_taken", "sk-taken"},
	}
	var status []int
	var bodies []string
	for _, tc := range cases {
		rec := claimPost(t, s, "user_a", tc.keyID, tc.secret)
		status = append(status, rec.Code)
		bodies = append(bodies, rec.Body.String())
	}
	for i := 1; i < len(cases); i++ {
		if status[i] != status[0] {
			t.Errorf("%s returned %d, %s returned %d — the status distinguishes the cause",
				cases[i].name, status[i], cases[0].name, status[0])
		}
		if bodies[i] != bodies[0] {
			t.Errorf("%s body %q differs from %s body %q — that is the oracle §5.4 forbids",
				cases[i].name, bodies[i], cases[0].name, bodies[0])
		}
	}
}

// TestClaimKeyRoute_RateLimitIsUniform is round-3 finding F4: if the limiter
// only ever fired on resolved keys, a 429 would tell a caller "that secret
// matched" even after the refusals were equalised. Every attempt counts, so
// the 429 arrives on whichever cause happens to be attempted.
func TestClaimKeyRoute_RateLimitIsUniform(t *testing.T) {
	s, repo, _, _ := newKeysServer(t)
	repo.users["user_a"] = &persistence.User{ID: "user_a", DisplayName: "A"}

	// Every attempt names a key id that does NOT exist — the cause that could
	// never increment a bucket keyed on a resolved key.
	var last *httptest.ResponseRecorder
	for i := 0; i < authz.ClaimAttemptLimit+1; i++ {
		last = claimPost(t, s, "user_a", "akey_ghost", "sk-guess")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d returned %d, want 429 — a bucket that only counts resolved keys cannot bound guessing",
			authz.ClaimAttemptLimit+1, last.Code)
	}
	if !strings.Contains(last.Body.String(), claimRefusalMessage) {
		t.Errorf("the 429 body %q should carry the same message as a refusal, so it adds no information", last.Body.String())
	}
}

// TestClaimKeyRoute_SecretNeverEscapes is the §5.4 hygiene bullet: the raw key
// secret must not appear in the response body or in any audit row. §8 covers
// secrets at rest; this is the in-flight property, on the second surface that
// receives a raw credential.
func TestClaimKeyRoute_SecretNeverEscapes(t *testing.T) {
	s, repo, audit, keys := newKeysServer(t)
	repo.users["user_a"] = &persistence.User{ID: "user_a", DisplayName: "A"}
	const secret = "sk-supersecret-value"
	keys.rows["akey_h"] = &persistence.APIKey{ID: "akey_h", ProjectID: "p", KeyHash: apikey.Hash(secret), CreatedAt: time.Now().UTC()}

	for _, tc := range []struct{ name, use string }{
		{"correct secret", secret},
		{"wrong secret", "sk-wrong-but-still-a-secret"},
	} {
		rec := claimPost(t, s, "user_a", "akey_h", tc.use)
		if strings.Contains(rec.Body.String(), tc.use) {
			t.Errorf("%s: the presented secret appeared in the response body: %s", tc.name, rec.Body.String())
		}
	}
	for _, row := range audit.rows {
		if strings.Contains(row.After+row.Before+row.Target, secret) {
			t.Fatalf("the raw secret reached an audit row: %+v", row)
		}
	}
}

// TestLinkCodeRoute_ReturnsTheCodeOnceAndNeverAudits it.
func TestLinkCodeRoute_ReturnsTheCodeOnceAndNeverAudits(t *testing.T) {
	repo, audit := newAccountsStubRepo(), &accountsStubAudit{}
	repo.users["user_a"] = &persistence.User{ID: "user_a", DisplayName: "A"}
	codes := newStubLinkCodes()
	cfg := config.DefaultConfig()
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}
	acc := authz.NewAccounts(repo, audit).WithLinkCodes(codes)
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg), WithAccountsService(acc))

	req := accountsReq(http.MethodPost, "/api/v1/operator/accounts/user_a/link-code", "{}")
	rec := httptest.NewRecorder()
	s.OperatorAccountItem(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code == "" {
		t.Fatal("the response must carry the raw code — it is shown once and never stored")
	}
	for _, row := range audit.rows {
		if strings.Contains(row.After+row.Target, body.Code) {
			t.Fatalf("the raw code reached an audit row, making the audit log a redemption oracle: %+v", row)
		}
	}
}

// TestStubLinkCodes_SatisfiesMissContract holds this file's double to the same
// miss contract the real repositories obey. A double looser or stricter than
// production is the bug class the miss-contract design exists to stop.
func TestStubLinkCodes_SatisfiesMissContract(t *testing.T) {
	c := newStubLinkCodes()
	repotest.AssertMissRepo(t, "LinkCodeRepository.GetLinkCode", c.GetLinkCode)
	repotest.AssertMiss(t, "LinkCodeRepository.ConsumeLinkCode", func() (*persistence.LinkCode, error) {
		return c.ConsumeLinkCode(context.Background(), "absent", "telegram", "1")
	})
}

// stubLinkCodes is a LinkCodeRepository slice for the issue path.
type stubLinkCodes struct {
	codes map[string]*persistence.LinkCode
}

func newStubLinkCodes() *stubLinkCodes {
	return &stubLinkCodes{codes: map[string]*persistence.LinkCode{}}
}

func (s *stubLinkCodes) CreateLinkCode(_ context.Context, lc *persistence.LinkCode) error {
	cp := *lc
	s.codes[lc.CodeHash] = &cp
	return nil
}

func (s *stubLinkCodes) GetLinkCode(_ context.Context, hash string) (*persistence.LinkCode, error) {
	lc, ok := s.codes[hash]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return lc, nil
}

func (s *stubLinkCodes) ConsumeLinkCode(_ context.Context, hash, channel, ext string) (*persistence.LinkCode, error) {
	lc, ok := s.codes[hash]
	if !ok || lc.UsedAt != nil || !lc.ExpiresAt.After(time.Now().UTC()) {
		return nil, persistence.ErrNotFound
	}
	now := time.Now().UTC()
	lc.UsedAt, lc.UsedByChannel, lc.UsedByExternalID = &now, channel, ext
	return lc, nil
}

func (s *stubLinkCodes) OutstandingLinkCodes(_ context.Context, userID string) ([]*persistence.LinkCode, error) {
	var out []*persistence.LinkCode
	for _, lc := range s.codes {
		if lc.UserID == userID && lc.UsedAt == nil {
			out = append(out, lc)
		}
	}
	return out, nil
}
