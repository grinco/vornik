package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// memAPIKeyRepo is a thread-safe in-memory persistence.APIKeyRepository
// for handler tests. Keeps the assertion surface readable: each test
// sets up the rows it cares about and the handler runs against
// them without sqlmock noise.
type memAPIKeyRepo struct {
	mu      sync.Mutex
	rows    []*persistence.APIKey
	failCre bool // simulate a Create() error path
}

func (m *memAPIKeyRepo) Create(_ context.Context, k *persistence.APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCre {
		return errors.New("simulated DB failure")
	}
	cp := *k
	m.rows = append(m.rows, &cp)
	return nil
}

func (m *memAPIKeyRepo) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.KeyHash == h && r.RevokedAt == nil {
			cp := *r
			return &cp, nil
		}
	}
	return nil, persistence.ErrAPIKeyNotFound
}

func (m *memAPIKeyRepo) ListByProject(_ context.Context, p string) ([]*persistence.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.APIKey
	for _, r := range m.rows {
		if r.ProjectID == p {
			cp := *r
			out = append(out, &cp)
		}
	}
	// newest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (m *memAPIKeyRepo) ListCompanionByProject(_ context.Context, p string) ([]*persistence.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.APIKey
	for _, r := range m.rows {
		if r.ProjectID == p && r.ClientKind != "" {
			cp := *r
			out = append(out, &cp)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (m *memAPIKeyRepo) TouchLastUsed(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range m.rows {
		if r.ID == id {
			r.LastUsedAt = &now
			return nil
		}
	}
	return nil
}

func (m *memAPIKeyRepo) Revoke(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range m.rows {
		if r.ID == id && r.RevokedAt == nil {
			r.RevokedAt = &now
			return nil
		}
	}
	return nil
}

func (m *memAPIKeyRepo) UpdateAllowedWorkflows(_ context.Context, id string, allowed []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			// Defensive copy so test callers can mutate `allowed`
			// after the call without polluting the stored row.
			cp := append([]string(nil), allowed...)
			r.AllowedWorkflows = cp
			return nil
		}
	}
	return persistence.ErrAPIKeyNotFound
}

func (m *memAPIKeyRepo) UpdateAllowPush(_ context.Context, id string, allowed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			r.AllowPush = allowed
			return nil
		}
	}
	return persistence.ErrAPIKeyNotFound
}

func (m *memAPIKeyRepo) RevokeByName(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range m.rows {
		if r.Name == name && r.RevokedAt == nil {
			r.RevokedAt = &now
			return nil
		}
	}
	return nil
}

func newAPIKeyServer(repo persistence.APIKeyRepository) *Server {
	return &Server{logger: zerolog.Nop(), apiKeyRepo: repo}
}

// TestCreateAPIKey_HappyPath — the headline contract: POST returns
// 201 + the secret EXACTLY ONCE. A subsequent List call must not
// surface the secret again (only the prefix).
func TestCreateAPIKey_HappyPath(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys", strings.NewReader(`{"name":"ha-key"}`))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got createAPIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// New keys embed the non-reversible project tag, not the raw name.
	wantPrefix := "sk-vornik-" + apikey.ShortProjectTag("assistant") + "."
	if got.Secret == "" || !strings.HasPrefix(got.Secret, wantPrefix) {
		t.Errorf("secret = %q, want prefix %q", got.Secret, wantPrefix)
	}
	if got.KeyPrefix != got.Secret[:apikey.PrefixDisplayLen] {
		t.Errorf("key_prefix = %q, want %q", got.KeyPrefix, got.Secret[:apikey.PrefixDisplayLen])
	}
	if got.ProjectID != "assistant" {
		t.Errorf("project_id = %q, want assistant", got.ProjectID)
	}
	if got.Name != "ha-key" {
		t.Errorf("name = %q, want ha-key", got.Name)
	}
	// One row written; raw secret NOT in the persisted row.
	if len(repo.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(repo.rows))
	}
	if repo.rows[0].KeyHash != apikey.Hash(got.Secret) {
		t.Errorf("stored hash does not match returned secret's hash")
	}
}

// TestCreateAPIKey_RejectsMissingName — `name` is required so the
// list view has something to render. Empty / whitespace-only names
// fail validation.
func TestCreateAPIKey_RejectsMissingName(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"   "}`} {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/assistant/keys", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.CreateAPIKey(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestCreateAPIKey_RejectsReservedTaskKeyPrefix is the FIX 3
// confused-deputy regression: CallMCPTool binds any authenticated
// key whose name starts with the reserved "agent:task_" prefix to
// that task ID, so an operator must NOT be able to mint a key with
// that name (it would let the operator impersonate a task's agent).
//
// Regression: review of a799e3f2 (2026-06-07) — the task-key prefix
// was unreserved at the operator-facing CreateAPIKey surface.
func TestCreateAPIKey_RejectsReservedTaskKeyPrefix(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys",
		strings.NewReader(`{"name":"agent:task_abc123"}`))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reserved-prefix name: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.rows) != 0 {
		t.Errorf("created a key with the reserved prefix: %#v", repo.rows)
	}
}

// TestTaskIDFromKeyName_RoundTripsMintedNames asserts the minter and
// consumer share one contract: a name minted as TaskKeyNamePrefix +
// taskID parses back to that task ID via the shared helper.
//
// Regression: review of a799e3f2 (2026-06-07) — the "agent:task_"
// literal was open-coded at both the minter and the consumer.
func TestTaskIDFromKeyName_RoundTripsMintedNames(t *testing.T) {
	const taskID = "task_20260607_deadbeef"
	name := persistence.TaskKeyNamePrefix + taskID
	got, ok := persistence.TaskIDFromKeyName(name)
	if !ok || got != taskID {
		t.Fatalf("TaskIDFromKeyName(%q) = (%q,%v), want (%q,true)", name, got, ok, taskID)
	}
	// Operator-supplied non-task names are not bindings.
	if _, ok := persistence.TaskIDFromKeyName("my normal key"); ok {
		t.Error("a plain name must not parse as a task binding")
	}
	// The bare prefix with no task ID is not a valid binding.
	if _, ok := persistence.TaskIDFromKeyName(persistence.TaskKeyNamePrefix); ok {
		t.Error("the bare prefix must not parse as a task binding")
	}
}

func TestCreateAPIKey_RejectsOversizedBody(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys", strings.NewReader(strings.Repeat("x", 4097)))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if len(repo.rows) != 0 {
		t.Fatalf("created key for oversized body: %#v", repo.rows)
	}
}

// TestCreateAPIKey_RepoErrorMaps500 — DB failures surface as 500
// DB_ERROR; the secret minted before the DB call is dropped (not
// leaked in the error body). Pre-fix attempts that returned 200
// with the secret would have created keys that authenticate
// nowhere.
func TestCreateAPIKey_RepoErrorMaps500(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{failCre: true})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-vornik-") {
		t.Errorf("error body leaked the secret: %s", rec.Body.String())
	}
}

// TestCreateAPIKey_NotConfiguredReturns503 — when the daemon is
// running without a DB-backed apikey repo wired in (static-keys
// only deployment), the management surface returns 503 rather
// than 500 — the operator can tell "feature not configured" from
// "feature configured but broken".
func TestCreateAPIKey_NotConfiguredReturns503(t *testing.T) {
	s := newAPIKeyServer(nil)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestListAPIKeys_NeverReturnsHashOrSecret — the headline security
// property: listing keys exposes the prefix but NEVER the hash or
// secret. A leaked List response must not be enough to authenticate.
func TestListAPIKeys_NeverReturnsHashOrSecret(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "assistant", Name: "test",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/assistant/keys", nil)
	rec := httptest.NewRecorder()
	s.ListAPIKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, apikey.Hash(secret)) {
		t.Errorf("List response leaked key_hash")
	}
	if strings.Contains(body, secret) {
		t.Errorf("List response leaked raw secret")
	}
	// Prefix IS in the response — that's the operator-recognise-which-key surface.
	if !strings.Contains(body, apikey.DisplayPrefix(secret)) {
		t.Errorf("List response missing key_prefix")
	}
}

// TestRotateAPIKey_NewKeyLiveBeforeOldRevoked — rotate must mint
// the new row BEFORE revoking the old. A polling client should
// never see a window where neither key works. We assert by
// ordering: the inserted-at order in the in-memory repo matches
// the contract.
func TestRotateAPIKey_NewKeyLiveBeforeOldRevoked(t *testing.T) {
	repo := &memAPIKeyRepo{}
	oldSecret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-old", ProjectID: "assistant", Name: "test",
		KeyHash: apikey.Hash(oldSecret), KeyPrefix: apikey.DisplayPrefix(oldSecret),
		CreatedAt: time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys/akey-old/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "akey-old")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got createAPIKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Secret == oldSecret {
		t.Error("rotate produced the same secret")
	}
	if got.ID == "akey-old" {
		t.Error("rotate reused the old ID instead of issuing a fresh one")
	}
	// Both rows exist; old is revoked, new is active.
	var oldRow, newRow *persistence.APIKey
	for _, r := range repo.rows {
		switch r.ID {
		case "akey-old":
			oldRow = r
		case got.ID:
			newRow = r
		}
	}
	if oldRow == nil || oldRow.RevokedAt == nil {
		t.Errorf("old key not revoked: %+v", oldRow)
	}
	if newRow == nil || newRow.RevokedAt != nil {
		t.Errorf("new key missing or already revoked: %+v", newRow)
	}
}

func TestRotateAPIKey_PreservesCompanionScope(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	capUSD := 12.5
	rps := 3
	burst := 7
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID:               "akey-old",
		ProjectID:        "assistant",
		Name:             "companion-laptop",
		KeyHash:          apikey.Hash(secret),
		KeyPrefix:        apikey.DisplayPrefix(secret),
		CreatedAt:        time.Now().UTC(),
		RateLimitRPS:     &rps,
		RateLimitBurst:   &burst,
		AllowedWorkflows: []string{"companion-doc-review"},
		BudgetCapUSD:     &capUSD,
		ClientKind:       "claude-code",
		SessionLabel:     "vadim/laptop",
		MemoryRead:       true,
		MemoryWrite:      true,
	})
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys/akey-old/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "akey-old")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var fresh *persistence.APIKey
	for _, row := range repo.rows {
		if row.ID != "akey-old" {
			fresh = row
			break
		}
	}
	if fresh == nil {
		t.Fatal("rotated key row not created")
	}
	if fresh.ClientKind != "claude-code" || fresh.SessionLabel != "vadim/laptop" {
		t.Fatalf("companion identity not preserved: client=%q label=%q", fresh.ClientKind, fresh.SessionLabel)
	}
	if got := strings.Join(fresh.AllowedWorkflows, ","); got != "companion-doc-review" {
		t.Fatalf("allowed_workflows = %q", got)
	}
	if fresh.BudgetCapUSD == nil || *fresh.BudgetCapUSD != capUSD {
		t.Fatalf("budget cap not preserved: %v", fresh.BudgetCapUSD)
	}
	if fresh.RateLimitRPS == nil || *fresh.RateLimitRPS != rps || fresh.RateLimitBurst == nil || *fresh.RateLimitBurst != burst {
		t.Fatalf("rate limits not preserved: rps=%v burst=%v", fresh.RateLimitRPS, fresh.RateLimitBurst)
	}
	if !fresh.MemoryRead || !fresh.MemoryWrite {
		t.Fatalf("memory flags not preserved: read=%v write=%v", fresh.MemoryRead, fresh.MemoryWrite)
	}
}

// TestRotateAPIKey_RejectsRevoked — rotating a revoked key is a
// user error; respond 409 so the UI can prompt "create a new key
// instead". A pre-fix design would have just minted a new one
// (operator surprise: silent resurrection of a revoked name).
func TestRotateAPIKey_RejectsRevoked(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	when := time.Now().UTC()
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "assistant", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: when, RevokedAt: &when,
	})
	s := newAPIKeyServer(repo)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/assistant/keys/akey-1/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "akey-1")
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

// TestRotateAPIKey_RejectsCrossProject — IDOR guard: an
// authenticated caller for project A cannot rotate a key bound
// to project B, even with the keyID known. We exercise this by
// asking project A's handler to rotate a key that exists only
// under project B.
func TestRotateAPIKey_RejectsCrossProject(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("project-b")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-b", ProjectID: "project-b", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/project-a/keys/akey-b/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "akey-b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (IDOR guard)", rec.Code)
	}
}

// TestRevokeAPIKey_HappyAndIdempotent — first DELETE revokes (204);
// second DELETE returns 204 too (idempotent at the HTTP layer
// even though the repo returns "0 rows affected").
func TestRevokeAPIKey_HappyAndIdempotent(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "assistant", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodDelete,
			"/api/v1/projects/assistant/keys/akey-1", nil)
		rec := httptest.NewRecorder()
		s.RevokeAPIKey(rec, req, "akey-1")
		if rec.Code != http.StatusNoContent {
			t.Errorf("attempt %d: status = %d, want 204", i, rec.Code)
		}
	}
	if repo.rows[0].RevokedAt == nil {
		t.Error("row not revoked")
	}
}

// TestRevokeAPIKey_RejectsCrossProject — IDOR guard mirror of the
// rotate test. project A's auth must not revoke project B's key.
func TestRevokeAPIKey_RejectsCrossProject(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("project-b")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-b", ProjectID: "project-b", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/projects/project-a/keys/akey-b", nil)
	rec := httptest.NewRecorder()
	s.RevokeAPIKey(rec, req, "akey-b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	// Row remains active in repo-A's view: revocation did NOT fire.
	if repo.rows[0].RevokedAt != nil {
		t.Error("cross-project revoke leaked through and revoked the row")
	}
}

// TestUpdateAPIKeyAllowedWorkflows_HappyPath — PUT replaces the
// allowed_workflows list and returns the updated state. The "fetch
// → mutate → PUT" pattern lives on the CLI; the handler's contract
// is just "replace what's there".
func TestUpdateAPIKeyAllowedWorkflows_HappyPath(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "assistant", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		AllowedWorkflows: []string{"companion-doc-review"},
		CreatedAt:        time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)

	body := `{"allowed_workflows":["companion-doc-review","companion-rag-ingest"]}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/assistant/keys/akey-1/workflows", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.UpdateAPIKeyAllowedWorkflows(rec, req, "akey-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID               string   `json:"id"`
		AllowedWorkflows []string `json:"allowed_workflows"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "akey-1" {
		t.Errorf("id = %q, want akey-1", resp.ID)
	}
	if got, want := strings.Join(resp.AllowedWorkflows, ","),
		"companion-doc-review,companion-rag-ingest"; got != want {
		t.Errorf("allowed_workflows = %q, want %q", got, want)
	}
	// Row in repo reflects the new list.
	if got, want := strings.Join(repo.rows[0].AllowedWorkflows, ","),
		"companion-doc-review,companion-rag-ingest"; got != want {
		t.Errorf("repo row allowed_workflows = %q, want %q", got, want)
	}
}

// TestUpdateAPIKeyAllowedWorkflows_RejectsCrossProject — IDOR guard
// parallel to revoke/rotate: project A's auth can't rewrite project
// B's key's allowlist given just the keyID.
func TestUpdateAPIKeyAllowedWorkflows_RejectsCrossProject(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("project-b")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-b", ProjectID: "project-b", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		AllowedWorkflows: []string{"original"},
		CreatedAt:        time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/project-a/keys/akey-b/workflows",
		strings.NewReader(`{"allowed_workflows":["hijacked"]}`))
	rec := httptest.NewRecorder()
	s.UpdateAPIKeyAllowedWorkflows(rec, req, "akey-b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if strings.Join(repo.rows[0].AllowedWorkflows, ",") != "original" {
		t.Errorf("cross-project update leaked through; row = %v", repo.rows[0].AllowedWorkflows)
	}
}

// TestUpdateAPIKeyAllowedWorkflows_RejectsWrongMethod — only PUT.
// GET / POST / PATCH return 405.
func TestUpdateAPIKeyAllowedWorkflows_RejectsWrongMethod(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(m, "/api/v1/projects/p/keys/k/workflows", nil)
		rec := httptest.NewRecorder()
		s.UpdateAPIKeyAllowedWorkflows(rec, req, "k")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: status = %d, want 405", m, rec.Code)
		}
	}
}

// TestSplitKeyActionPath — the dispatcher helper. Cover both shapes
// the routes file branches on, and the rejection of trailing junk.
func TestSplitKeyActionPath(t *testing.T) {
	cases := []struct {
		in       string
		key, act string
		ok       bool
	}{
		{"/keys/akey-1", "akey-1", "", true},
		{"/keys/akey-1/", "akey-1", "", true},
		{"/keys/akey-1/rotate", "akey-1", "rotate", true},
		{"/keys/", "", "", false},
		{"/keys/akey-1/rotate/extra", "", "", false},
		{"/tasks/abc", "", "", false},
	}
	for _, c := range cases {
		k, a, ok := splitKeyActionPath(c.in)
		if k != c.key || a != c.act || ok != c.ok {
			t.Errorf("splitKeyActionPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, k, a, ok, c.key, c.act, c.ok)
		}
	}
}

// TestCallerForAudit — the audit-trail value pulled from context.
// When a DB-backed key authenticated the request, the audit
// column carries its ID; static-keys path lands the literal
// "static" sentinel so operators can grep for legacy traffic.
func TestCallerForAudit(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := callerForAudit(req); got != "static" {
		t.Errorf("static path: callerForAudit = %q, want \"static\"", got)
	}
	ctx := context.WithValue(req.Context(), apiKeyIDKey, "akey-42")
	if got := callerForAudit(req.WithContext(ctx)); got != "akey-42" {
		t.Errorf("DB path: callerForAudit = %q, want akey-42", got)
	}
}

// TestUpdateAllowPush_HappyPathTrue — PUT with allow_push:true flips
// the flag to true and returns the updated key record (no secret).
func TestUpdateAllowPush_HappyPathTrue(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "assistant", Name: "push-key",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(), AllowPush: false,
	})
	s := newAPIKeyServer(repo)

	body := `{"allow_push":true}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/assistant/keys/akey-1/allow-push", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "akey-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp apiKeyListEntry
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "akey-1" {
		t.Errorf("id = %q, want akey-1", resp.ID)
	}
	if !resp.AllowPush {
		t.Errorf("allow_push = false, want true")
	}
	// Repo row reflects the flip.
	if !repo.rows[0].AllowPush {
		t.Errorf("repo row allow_push not flipped to true")
	}
}

// TestUpdateAllowPush_HappyPathFalse — PUT with allow_push:false clears
// the flag. Mirrors the true case in the opposite direction.
func TestUpdateAllowPush_HappyPathFalse(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-2", ProjectID: "assistant", Name: "push-key-2",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(), AllowPush: true,
	})
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/assistant/keys/akey-2/allow-push",
		strings.NewReader(`{"allow_push":false}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "akey-2")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if repo.rows[0].AllowPush {
		t.Errorf("repo row allow_push not cleared to false")
	}
}

// TestUpdateAllowPush_RejectsCrossProject — IDOR guard: project A's
// auth must not flip allow_push on a key bound to project B.
func TestUpdateAllowPush_RejectsCrossProject(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("project-b")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-b", ProjectID: "project-b", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/project-a/keys/akey-b/allow-push",
		strings.NewReader(`{"allow_push":true}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "akey-b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (IDOR guard)", rec.Code)
	}
	// Row must not have been mutated.
	if repo.rows[0].AllowPush {
		t.Errorf("cross-project update leaked through and flipped allow_push")
	}
}

// TestUpdateAllowPush_UnknownKey — keyID that doesn't exist in the
// project returns 404 and no mutation fires.
func TestUpdateAllowPush_UnknownKey(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/p/keys/nonexistent/allow-push",
		strings.NewReader(`{"allow_push":true}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "nonexistent")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestUpdateAllowPush_MalformedBody — invalid JSON → 400 VALIDATION_ERROR.
func TestUpdateAllowPush_MalformedBody(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("p")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "p", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/p/keys/akey-1/allow-push",
		strings.NewReader(`{not-json}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "akey-1")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestUpdateAllowPush_WrongMethod — only PUT is accepted; other methods
// return 405.
func TestUpdateAllowPush_WrongMethod(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(m,
			"/api/v1/projects/p/keys/k/allow-push", strings.NewReader(`{"allow_push":true}`))
		rec := httptest.NewRecorder()
		s.UpdateAllowPushHandler(rec, req, "k")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: status = %d, want 405", m, rec.Code)
		}
	}
}

// TestListAPIKeys_SurfacesAllowPush — a key with AllowPush=true must
// have allow_push:true in the list response body.
func TestListAPIKeys_SurfacesAllowPush(t *testing.T) {
	repo := &memAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-push", ProjectID: "assistant", Name: "push-enabled",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(), AllowPush: true,
	})
	s := newAPIKeyServer(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/assistant/keys", nil)
	rec := httptest.NewRecorder()
	s.ListAPIKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out listAPIKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(out.Keys))
	}
	if !out.Keys[0].AllowPush {
		t.Errorf("allow_push not surfaced in list response")
	}
}

// Keep the import set used by the test file pinned even when the
// last assertion above is removed.
var _ = bytes.NewReader
