package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// newSkillTestServer builds a Server backed by an in-memory SQLite
// skill store so the companion skill MCP handlers can be exercised
// against real persistence.
func newSkillTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("sqlite.Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("sqlite.Migrate: %v", err)
	}
	return &Server{skillStore: sqlite.NewSkillRepository(db.DB)}
}

func skillKey(project string, write, admin bool) *persistence.APIKey {
	return &persistence.APIKey{
		ID: "k-" + project, ProjectID: project, ClientKind: "claude-code",
		SkillRead: true, SkillWrite: write, SkillAdmin: admin,
	}
}

func rawArgs(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

func proposeID(t *testing.T, out string) string {
	t.Helper()
	var r struct {
		ID       string `json:"id"`
		Maturity string `json:"maturity"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal propose result: %v", err)
	}
	return r.ID
}

func TestSkillPropose_RequiresSkillWrite(t *testing.T) {
	s := newSkillTestServer(t)
	key := skillKey("p1", false, false) // read only
	_, err := s.companionToolSkillPropose(context.Background(), key,
		rawArgs(t, map[string]any{"name": "x", "description": "d", "body": "b"}))
	if err == nil || !strings.Contains(err.Error(), "skill_write") {
		t.Fatalf("expected skill_write gate error, got %v", err)
	}
}

func TestSkillPropose_LandsDraft(t *testing.T) {
	s := newSkillTestServer(t)
	key := skillKey("p1", true, false)
	out, err := s.companionToolSkillPropose(context.Background(), key,
		rawArgs(t, map[string]any{
			"name": "trace-hang", "description": "when x", "body": "# do it",
			"repo_scope": "github.com/x/a", "roles": []string{"researcher"},
		}))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if !strings.Contains(out, "draft") {
		t.Fatalf("expected draft note, got %s", out)
	}
	got, err := s.skillStore.GetByID(context.Background(), proposeID(t, out))
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Maturity != persistence.SkillMaturityDraft || got.BodySHA256 == "" ||
		got.OriginClient != "claude-code" || got.ProjectID != "p1" {
		t.Fatalf("stored skill wrong: %+v", got)
	}
}

func TestSkillSearch_OnlyReturnsActive(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	writer := skillKey("p1", true, false)
	out, err := s.companionToolSkillPropose(ctx, writer, rawArgs(t, map[string]any{
		"name": "gate", "description": "gated skill", "body": "# b", "repo_scope": "github.com/x/a",
	}))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	id := proposeID(t, out)

	// Draft must NOT surface in search.
	sr, err := s.companionToolSkillSearch(ctx, writer, rawArgs(t, map[string]any{"repo_scope": "github.com/x/a"}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if strings.Contains(sr, "\"count\": 0") == false {
		t.Fatalf("draft should not appear in search, got %s", sr)
	}

	// Approve (needs admin), then it appears.
	admin := skillKey("p1", true, true)
	if _, err := s.companionToolSkillApprove(ctx, admin, rawArgs(t, map[string]any{"id": id})); err != nil {
		t.Fatalf("approve: %v", err)
	}
	sr, err = s.companionToolSkillSearch(ctx, writer, rawArgs(t, map[string]any{"repo_scope": "github.com/x/a"}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(sr, "gate") {
		t.Fatalf("approved skill should appear in search, got %s", sr)
	}
}

func TestSkillApprove_RequiresSkillAdmin(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	writer := skillKey("p1", true, false)
	out, _ := s.companionToolSkillPropose(ctx, writer, rawArgs(t, map[string]any{
		"name": "x", "description": "d", "body": "b",
	}))
	_, err := s.companionToolSkillApprove(ctx, writer, rawArgs(t, map[string]any{"id": proposeID(t, out)}))
	if err == nil || !strings.Contains(err.Error(), "skill_admin") {
		t.Fatalf("expected skill_admin gate error, got %v", err)
	}
}

func TestSkillSearch_ScopeIsolation(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	admin := skillKey("p1", true, true)
	// Approved skill under repo A.
	out, _ := s.companionToolSkillPropose(ctx, admin, rawArgs(t, map[string]any{
		"name": "a-skill", "description": "d", "body": "b", "repo_scope": "github.com/x/a",
	}))
	if _, err := s.companionToolSkillApprove(ctx, admin, rawArgs(t, map[string]any{"id": proposeID(t, out)})); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Search scoped to repo B must not see it.
	sr, err := s.companionToolSkillSearch(ctx, admin, rawArgs(t, map[string]any{
		"repo_scope": "github.com/x/b", "strict_scope": true,
	}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if strings.Contains(sr, "a-skill") {
		t.Fatalf("repo A skill leaked into repo B scoped search: %s", sr)
	}
}

func TestSkillReject_ActiveCreditsCorrected(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	admin := skillKey("p1", true, true)
	out, _ := s.companionToolSkillPropose(ctx, admin, rawArgs(t, map[string]any{
		"name": "flaky", "description": "d", "body": "b", "repo_scope": "github.com/x/a",
	}))
	id := proposeID(t, out)
	if _, err := s.companionToolSkillApprove(ctx, admin, rawArgs(t, map[string]any{"id": id})); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.companionToolSkillReject(ctx, admin, rawArgs(t, map[string]any{"id": id, "reason": "kept misfiring"})); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, _ := s.skillStore.GetByID(ctx, id)
	if got.Maturity != persistence.SkillMaturityRetired {
		t.Errorf("expected retired, got %s", got.Maturity)
	}
	if got.UsageCorrected != 1 {
		t.Errorf("rejecting an active skill must credit corrected once, got %d", got.UsageCorrected)
	}
}

func TestSkillReject_DraftNoCorrected(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	admin := skillKey("p1", true, true)
	out, _ := s.companionToolSkillPropose(ctx, admin, rawArgs(t, map[string]any{
		"name": "raw", "description": "d", "body": "b",
	}))
	id := proposeID(t, out)
	if _, err := s.companionToolSkillReject(ctx, admin, rawArgs(t, map[string]any{"id": id})); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, _ := s.skillStore.GetByID(ctx, id)
	if got.UsageCorrected != 0 {
		t.Errorf("rejecting a draft must NOT credit corrected, got %d", got.UsageCorrected)
	}
}

func TestSkillPropose_GlobalStoresFlag(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	key := skillKey("p1", true, false)
	out, err := s.companionToolSkillPropose(ctx, key, rawArgs(t, map[string]any{
		"name": "g", "description": "d", "body": "# b", "global": true,
	}))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if !strings.Contains(out, "affects ALL projects") {
		t.Fatalf("global propose must label blast radius, got %s", out)
	}
	got, err := s.skillStore.GetByID(ctx, proposeID(t, out))
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.IsGlobal {
		t.Fatalf("propose global:true must store is_global, got %+v", got)
	}
}

func TestSkillSearchAndGet_IncludeGlobalSkills(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	owner := skillKey("p1", true, true)
	out, err := s.companionToolSkillPropose(ctx, owner, rawArgs(t, map[string]any{
		"name": "global-runbook", "description": "shared procedure", "body": "# use it",
		"repo_scope": "github.com/x/a", "global": true,
	}))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	id := proposeID(t, out)
	if _, err := s.companionToolSkillApprove(ctx, owner, rawArgs(t, map[string]any{"id": id})); err != nil {
		t.Fatalf("approve: %v", err)
	}

	reader := skillKey("p2", false, false)
	sr, err := s.companionToolSkillSearch(ctx, reader, rawArgs(t, map[string]any{
		"query": "global", "repo_scope": "github.com/x/a", "strict_scope": true,
	}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(sr, "global-runbook") {
		t.Fatalf("global skill should be discoverable cross-project, got %s", sr)
	}

	gr, err := s.companionToolSkillGet(ctx, reader, rawArgs(t, map[string]any{"id": id}))
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if !strings.Contains(gr, "# use it") {
		t.Fatalf("global skill body should be readable cross-project, got %s", gr)
	}
}

func TestSkillGetByID_GlobalStillRequiresApprovedMaturity(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	owner := skillKey("p1", true, true)
	out, err := s.companionToolSkillPropose(ctx, owner, rawArgs(t, map[string]any{
		"name": "draft-global", "description": "shared draft", "body": "# not approved",
		"global": true,
	}))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	reader := skillKey("p2", false, false)
	_, err = s.companionToolSkillGet(ctx, reader, rawArgs(t, map[string]any{"id": proposeID(t, out)}))
	if err == nil {
		t.Fatal("global draft must not be readable by id from another project")
	}
}

func TestSkillGetByID_RespectsExplicitRepoScope(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	owner := skillKey("p1", true, true)
	out, err := s.companionToolSkillPropose(ctx, owner, rawArgs(t, map[string]any{
		"name": "scoped-global", "description": "shared procedure", "body": "# body",
		"repo_scope": "github.com/x/a", "global": true,
	}))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	id := proposeID(t, out)
	if _, err := s.companionToolSkillApprove(ctx, owner, rawArgs(t, map[string]any{"id": id})); err != nil {
		t.Fatalf("approve: %v", err)
	}

	reader := skillKey("p2", false, false)
	if _, err := s.companionToolSkillGet(ctx, reader, rawArgs(t, map[string]any{
		"id": id, "repo_scope": "github.com/x/other",
	})); err == nil {
		t.Fatal("global skill with explicit mismatched repo_scope must not be readable by id")
	}
	if _, err := s.companionToolSkillGet(ctx, reader, rawArgs(t, map[string]any{
		"id": id, "repo_scope": "github.com/x/a",
	})); err != nil {
		t.Fatalf("matching repo_scope should read global skill by id: %v", err)
	}
}

func TestSkillSetGlobal_RequiresSkillAdmin(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	writer := skillKey("p1", true, false)
	out, _ := s.companionToolSkillPropose(ctx, writer, rawArgs(t, map[string]any{
		"name": "x", "description": "d", "body": "b",
	}))
	_, err := s.companionToolSkillSetGlobal(ctx, writer, rawArgs(t, map[string]any{
		"id": proposeID(t, out), "global": true,
	}))
	if err == nil || !strings.Contains(err.Error(), "skill_admin") {
		t.Fatalf("expected skill_admin gate error, got %v", err)
	}
}

func TestSkillSetGlobal_FlipsWithoutTouchingMaturity(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	admin := skillKey("p1", true, true)
	out, _ := s.companionToolSkillPropose(ctx, admin, rawArgs(t, map[string]any{
		"name": "promote-me", "description": "d", "body": "b", "repo_scope": "github.com/x/a",
	}))
	id := proposeID(t, out)
	if _, err := s.companionToolSkillApprove(ctx, admin, rawArgs(t, map[string]any{"id": id})); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.companionToolSkillSetGlobal(ctx, admin, rawArgs(t, map[string]any{"id": id, "global": true})); err != nil {
		t.Fatalf("set-global: %v", err)
	}
	got, _ := s.skillStore.GetByID(ctx, id)
	if !got.IsGlobal {
		t.Errorf("set-global did not stick: %+v", got)
	}
	if got.Maturity != persistence.SkillMaturityActive {
		t.Errorf("set-global must not touch maturity, got %s", got.Maturity)
	}
}

func TestSkillSetGlobal_RejectsCrossProject(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	p1admin := skillKey("p1", true, true)
	out, _ := s.companionToolSkillPropose(ctx, p1admin, rawArgs(t, map[string]any{
		"name": "mine", "description": "d", "body": "b",
	}))
	id := proposeID(t, out)
	p2admin := skillKey("p2", true, true)
	_, err := s.companionToolSkillSetGlobal(ctx, p2admin, rawArgs(t, map[string]any{"id": id, "global": true}))
	if err == nil || !strings.Contains(err.Error(), "not found in this project") {
		t.Fatalf("expected cross-project rejection, got %v", err)
	}
	// The skill must remain project-only after the rejected attempt.
	got, _ := s.skillStore.GetByID(ctx, id)
	if got.IsGlobal {
		t.Errorf("cross-project set-global must not have flipped the flag")
	}
}

func TestSkillGet_RejectsCrossProject(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	p1admin := skillKey("p1", true, true)
	out, _ := s.companionToolSkillPropose(ctx, p1admin, rawArgs(t, map[string]any{
		"name": "secret", "description": "d", "body": "b", "repo_scope": "github.com/x/a",
	}))
	id := proposeID(t, out)

	p2 := skillKey("p2", true, true)
	_, err := s.companionToolSkillGet(ctx, p2, rawArgs(t, map[string]any{"id": id}))
	if err == nil || !strings.Contains(err.Error(), "not found in this project") {
		t.Fatalf("expected cross-project rejection, got %v", err)
	}
}
