package api

// Unit tests for POST /api/v1/admin/memory/policy/chunks/{id}.
// Pins the merge-then-recompute-digest contract + the 4 error
// shapes (no editor → 503; bad path → 400; missing chunk → 404;
// happy path → 200 + new digest).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memoryfirewall"
)

// stubFirewallEditor captures the calls so each test can assert
// the API handler invoked LoadChunkPolicies + UpdateChunkPolicy
// with the expected merged shape.
type stubFirewallEditor struct {
	existing  map[string]ChunkPolicyRow
	updates   []ChunkPolicyRow
	updateErr error
	loadErr   error
}

func (s *stubFirewallEditor) LoadChunkPolicies(_ context.Context, ids []string) (map[string]ChunkPolicyRow, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := make(map[string]ChunkPolicyRow, len(ids))
	for _, id := range ids {
		if row, ok := s.existing[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

func (s *stubFirewallEditor) UpdateChunkPolicy(_ context.Context, row ChunkPolicyRow) (int64, error) {
	if s.updateErr != nil {
		return 0, s.updateErr
	}
	// Verify the chunk exists in the stub's store before
	// "writing" — mirrors the postgres RowsAffected=0 semantic
	// for missing chunks.
	if _, ok := s.existing[row.ChunkID]; !ok {
		return 0, nil
	}
	s.updates = append(s.updates, row)
	return 1, nil
}

func newFirewallServer(editor MemoryFirewallEditor) *Server {
	return NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithMemoryFirewallEditor(editor),
	)
}

func TestAdminMemoryFirewallChunkPolicy_NoEditor_503(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/c1",
			bytes.NewBufferString(`{}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "FIREWALL_DISABLED")
}

func TestAdminMemoryFirewallChunkPolicy_BadPath_400(t *testing.T) {
	s := newFirewallServer(&stubFirewallEditor{})
	// Missing chunk_id (trailing /).
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/",
			bytes.NewBufferString(`{}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "chunk_id required")
}

func TestAdminMemoryFirewallChunkPolicy_MissingChunk_404(t *testing.T) {
	s := newFirewallServer(&stubFirewallEditor{
		existing: map[string]ChunkPolicyRow{}, // empty — load returns nothing
	})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/nope",
			bytes.NewBufferString(`{}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "CHUNK_NOT_FOUND")
}

func TestAdminMemoryFirewallChunkPolicy_MergeAndRecomputeDigest(t *testing.T) {
	// Existing chunk has sensitivity=internal + permitted_roles
	// {coder, analyst} + tenant=. Admin posts a partial update
	// flipping sensitivity → restricted; everything else
	// untouched.
	existingExpiry := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	stub := &stubFirewallEditor{
		existing: map[string]ChunkPolicyRow{
			"c1": {
				ChunkID:           "c1",
				SensitivityTier:   "internal",
				PermittedRoles:    []string{"coder", "analyst"},
				FirewallExpiresAt: &existingExpiry,
				PolicyDigest:      "old-digest",
			},
		},
	}
	s := newFirewallServer(stub)

	body, _ := json.Marshal(chunkPolicyUpdateRequest{
		SensitivityTier: ptr("restricted"),
	})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/c1",
			bytes.NewBuffer(body)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp chunkPolicyUpdateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "c1", resp.ChunkID)
	assert.NotEmpty(t, resp.PolicyDigest)
	assert.NotEqual(t, "old-digest", resp.PolicyDigest, "digest must recompute on edit")
	assert.Equal(t, "restricted", resp.Policy.SensitivityTier)
	// Untouched fields preserved.
	assert.Equal(t, []string{"coder", "analyst"}, resp.Policy.PermittedRoles)
	assert.Equal(t, existingExpiry, *resp.Policy.FirewallExpiresAt)

	// Stub captured the UpdateChunkPolicy call with the new
	// digest + merged fields.
	require.Len(t, stub.updates, 1)
	assert.Equal(t, "restricted", stub.updates[0].SensitivityTier)
	assert.Equal(t, resp.PolicyDigest, stub.updates[0].PolicyDigest)
}

func TestAdminMemoryFirewallChunkPolicy_EmptySliceClearsField(t *testing.T) {
	// Pin the "empty slice = clear" semantic: callers can
	// send {"permitted_roles": []} to wipe the field
	// (deny-all under strict-enforce).
	stub := &stubFirewallEditor{
		existing: map[string]ChunkPolicyRow{
			"c1": {ChunkID: "c1", PermittedRoles: []string{"old1", "old2"}},
		},
	}
	s := newFirewallServer(stub)
	empty := []string{}
	body, _ := json.Marshal(chunkPolicyUpdateRequest{PermittedRoles: &empty})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/c1",
			bytes.NewBuffer(body)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Len(t, stub.updates, 1)
	assert.Empty(t, stub.updates[0].PermittedRoles, "empty slice must clear the field")
}

// ptr is a small generic for setting partial-update pointer fields
// in test bodies. Mirrors lo.ToPtr without the dep.
func ptr[T any](v T) *T { return &v }

// stubEvaluationsRepo serves the CSV endpoint test below. The
// stub returns canned rows so the test can assert on the CSV
// wire shape (RFC 4180 + stable column order).
type stubEvaluationsRepo struct {
	rows []memoryfirewall.EvaluationRow
	// byDigest records the last digest ListByDigest was called with so
	// the digest-endpoint test can assert the handler threaded the
	// path arg through.
	byDigest string
	// byChunk records the last chunk id ListByChunk was called with.
	byChunk string
}

func (s *stubEvaluationsRepo) BatchInsert(_ context.Context, _ []memoryfirewall.EvaluationRow) error {
	return nil
}

func (s *stubEvaluationsRepo) ListRecent(_ context.Context, _, _ string, _ time.Time, _ int) ([]memoryfirewall.EvaluationRow, error) {
	return s.rows, nil
}

func (s *stubEvaluationsRepo) ListByDigest(_ context.Context, digest string, _ int) ([]memoryfirewall.EvaluationRow, error) {
	s.byDigest = digest
	return s.rows, nil
}

func (s *stubEvaluationsRepo) ListByChunk(_ context.Context, chunkID string, _ int) ([]memoryfirewall.EvaluationRow, error) {
	s.byChunk = chunkID
	return s.rows, nil
}

// TestAdminMemoryFirewallEvaluationsByDigest pins the §8.3
// proof-verifier route (was 404 — documented in the firewall LLD but
// never registered). The handler must extract the digest from the
// path tail, thread it to ListByDigest, and echo it back.
func TestAdminMemoryFirewallEvaluationsByDigest(t *testing.T) {
	repo := &stubEvaluationsRepo{
		rows: []memoryfirewall.EvaluationRow{
			{ID: "ev1", ProjectID: "p1", ChunkID: "c1", Decision: memoryfirewall.DecisionAllow, PolicyDigest: "abc123"},
		},
	}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithMemoryPolicyEvaluations(repo),
	)

	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/memory/policy/evaluations/digest/abc123?limit=10", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluationsByDigest(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "abc123", repo.byDigest, "handler must thread the path digest to the repo")
	var out struct {
		Count        int    `json:"count"`
		PolicyDigest string `json:"policy_digest"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 1, out.Count)
	assert.Equal(t, "abc123", out.PolicyDigest)

	// Empty digest in path → 400.
	req2 := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/memory/policy/evaluations/digest/", nil),
		"sk-admin")
	rec2 := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluationsByDigest(rec2, req2)
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
}

// TestAdminMemoryFirewallEvaluationsCSV_StableShape pins the
// CSV column order + the Content-Disposition filename pattern
// so downstream spreadsheet templates don't break on a
// reordering. The 11-column header lands first; each row
// follows.
func TestAdminMemoryFirewallEvaluationsCSV_StableShape(t *testing.T) {
	repo := &stubEvaluationsRepo{
		rows: []memoryfirewall.EvaluationRow{
			{
				ID: "ev1", ProjectID: "p1", TenantID: "tenant-a",
				ChunkID: "c1", Decision: memoryfirewall.DecisionAllow,
				PolicyDigest: "digest1", RequestRole: "rest_api",
				RequestPurpose: "operational", RequestOperator: "akey_x",
				TraceID: "tr1", ReasonDetail: "",
				EvaluatedAt: time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC),
			},
			{
				ID: "ev2", ProjectID: "p1",
				ChunkID:      "c2",
				Decision:     memoryfirewall.DecisionBlockExpired,
				ReasonDetail: "chunk expired at 2026-01-01T00:00:00Z",
				EvaluatedAt:  time.Date(2026, 5, 29, 12, 1, 0, 0, time.UTC),
			},
		},
	}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithMemoryPolicyEvaluations(repo),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/memory/policy/evaluations.csv?project_id=p1",
			nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluationsCSV(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/csv")
	assert.Contains(t, rec.Header().Get("Content-Disposition"), `filename="memory_policy_evaluations_p1_`)

	// Parse the CSV back + assert on the rows.
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 3, "header + 2 rows")
	// Header row — pin column order.
	assert.Equal(t,
		"evaluated_at,project_id,tenant_id,chunk_id,decision,policy_digest,request_role,request_purpose,request_operator,trace_id,reason_detail",
		strings.TrimRight(lines[0], "\r"))
	assert.Contains(t, lines[1], "c1")
	assert.Contains(t, lines[1], "allow")
	assert.Contains(t, lines[1], "tenant-a")
	assert.Contains(t, lines[2], "c2")
	assert.Contains(t, lines[2], "block_expired")
	// RFC 4180 quoting: the second row's reason_detail has a
	// timestamp with `:` (not a comma but still inside the
	// CSV body) — encoding/csv won't add quotes for that, so
	// the row stays unquoted.
}
