package repotest

// The fifteen repositories the backend-coverage gate seeded as "dual-backend
// debt" on 2026-09-04 (cmd/lint-lld-contracts/repo_backend_allowlist.txt),
// each with a real implementation on BOTH backends and, until 2026-09-05,
// nothing asserting they agree. Two of the fifteen turned out to be SQLite
// stubs on reading (project spawns, healing overrides) and are pinned in
// internal/persistence/sqlite/stub_contract_test.go instead; the thirteen
// below are the shared contracts. Where the two implementations differ in a
// way the suite cannot make one of them wrong, the case says so and the
// difference is filed rather than papered over.
//
// Design: https://docs.vornik.io §8.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SeedChunk inserts one project_memory_chunks row with the given extraction
// flag and creation time. Chunks are cross-table state with backend-specific
// required columns (a vector on Postgres, a TEXT hash on SQLite), so each
// contract test file supplies its own raw insert.
type SeedChunk func(ctx context.Context, id, projectID, content string, needsExtraction bool, createdAt time.Time) error

// sameJSON reports whether two JSON documents are semantically equal. The
// JSON-typed columns in these suites (admin_audit before/after,
// execution_quality_scores.case_evidence, operator_profile.structured) are
// JSONB on Postgres and TEXT on SQLite: Postgres canonicalises key order and
// spacing on the way in, SQLite stores the bytes. Their consumers parse, so
// the contract both backends must meet is semantic equality — unlike
// step_prompts, whose consumers hash the bytes and whose column is TEXT on
// both backends for that reason (2026-07-24 incident).
func sameJSON(a, b string) bool {
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return false
	}
	ca, _ := json.Marshal(va)
	cb, _ := json.Marshal(vb)
	return bytes.Equal(ca, cb)
}

// indexOf returns the position of the first element for which pick returns
// true, or -1.
func indexOf[T any](rows []T, pick func(T) bool) int {
	for i, r := range rows {
		if pick(r) {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------- admin audit

// RunAdminAuditSuite pins persistence.AdminAuditRepository.
func RunAdminAuditSuite(t *testing.T, repo persistence.AdminAuditRepository) {
	t.Helper()
	ctx := context.Background()
	t.Run("Insert_defaults_and_List_requires_page_size", func(t *testing.T) { adminAuditInsertDefaults(ctx, t, repo) })
	t.Run("List_filters_orders_newest_first_and_round_trips_fields", func(t *testing.T) { adminAuditListFilters(ctx, t, repo) })
}

func adminAuditInsertDefaults(ctx context.Context, t *testing.T, repo persistence.AdminAuditRepository) {
	t.Helper()
	principal := uniqueID("principal")
	e := &persistence.AdminAuditEntry{Principal: principal, Source: "ui", Action: "config.reload"}
	if err := repo.Insert(ctx, e); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if e.ID == "" {
		t.Error("Insert must generate an ID when the caller leaves it empty")
	}
	if err := repo.Insert(ctx, nil); err == nil {
		t.Error("Insert(nil) must be refused")
	}
	if _, err := repo.List(ctx, persistence.AdminAuditFilter{Principal: principal}); err == nil {
		t.Error("List with PageSize 0 must be refused — an unbounded scan on a hot operator surface")
	}
}

func adminAuditListFilters(ctx context.Context, t *testing.T, repo persistence.AdminAuditRepository) {
	t.Helper()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	p := uniqueID("actor")
	target := uniqueID("proj") + "_x" // an underscore: the prefix filter must escape it
	entries := []*persistence.AdminAuditEntry{
		{Timestamp: base, Principal: p, Source: "cli", Action: "key.revoke", Target: target, Before: `{"a":1}`, After: `{"a":2}`, IP: "10.0.0.1", UserAgent: "ua-1"},
		{Timestamp: base.Add(time.Minute), Principal: p, Source: "api", Action: "mcp.refresh", Target: target + "-other", UserAgent: "ua-2"},
		{Timestamp: base.Add(2 * time.Minute), Principal: p, Source: "ui", Action: "key.revoke", Target: "elsewhere", UserAgent: "ua-3"},
	}
	for i, e := range entries {
		if err := repo.Insert(ctx, e); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	all, err := repo.List(ctx, persistence.AdminAuditFilter{Principal: p, PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 || all[0].Action != "key.revoke" || all[0].Target != "elsewhere" {
		t.Fatalf("List must return this principal's rows newest first, got %d rows, first %+v", len(all), all[0])
	}
	got := all[2]
	if !sameJSON(got.Before, `{"a":1}`) || !sameJSON(got.After, `{"a":2}`) || got.IP != "10.0.0.1" || got.UserAgent != "ua-1" || !got.Timestamp.Equal(base) {
		t.Errorf("round trip lost a field: %+v", got)
	}
	if got.Source != "cli" || all[1].Before != "" || all[1].IP != "" {
		t.Errorf("empty Before/After/IP must read back as empty strings: %+v", all[1])
	}
	adminAuditFilterCases(ctx, t, repo, p, target, base)
}

func adminAuditFilterCases(ctx context.Context, t *testing.T, repo persistence.AdminAuditRepository, p, target string, base time.Time) {
	t.Helper()
	byAction, _ := repo.List(ctx, persistence.AdminAuditFilter{Principal: p, Action: "mcp.refresh", PageSize: 10})
	if len(byAction) != 1 || byAction[0].Target != target+"-other" {
		t.Errorf("Action filter: got %d rows", len(byAction))
	}
	byPrefix, _ := repo.List(ctx, persistence.AdminAuditFilter{Principal: p, TargetPrefix: target, PageSize: 10})
	if len(byPrefix) != 2 {
		t.Errorf("TargetPrefix must match both targets starting with %q (escaping the underscore), got %d", target, len(byPrefix))
	}
	noMatch, _ := repo.List(ctx, persistence.AdminAuditFilter{Principal: p, TargetPrefix: uniqueID("proj") + "%", PageSize: 10})
	if len(noMatch) != 0 {
		t.Errorf("a literal %% in the prefix must not act as a wildcard, got %d rows", len(noMatch))
	}
	window, _ := repo.List(ctx, persistence.AdminAuditFilter{Principal: p, Since: base.Add(time.Minute), Until: base.Add(2 * time.Minute), PageSize: 10})
	if len(window) != 1 || window[0].Action != "mcp.refresh" {
		t.Errorf("Since is inclusive and Until exclusive: got %d rows", len(window))
	}
	page, _ := repo.List(ctx, persistence.AdminAuditFilter{Principal: p, PageSize: 1, Offset: 1})
	if len(page) != 1 || page[0].Action != "mcp.refresh" {
		t.Errorf("PageSize+Offset must page in ts DESC order, got %+v", page)
	}
}

// ---------------------------------------------------------- capability usage

// RunCapabilityUsageSuite pins persistence.CapabilityUsageRepository. The
// two implementations query differently (one UNION on Postgres, per-signal
// on SQLite so an absent table is "unused" rather than an error); what must
// agree is the row set a caller sees.
func RunCapabilityUsageSuite(t *testing.T, repo persistence.CapabilityUsageRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	if err := tasks.Create(ctx, newQueuedTask(project)); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Run("every_catalogued_capability_appears_even_unused", func(t *testing.T) { capabilityUsageEveryKey(ctx, t, repo) })
	t.Run("a_task_counts_under_tasks_for_its_project", func(t *testing.T) { capabilityUsageCountsTask(ctx, t, repo, project) })
	t.Run("window_excludes_older_signals_but_keeps_the_key", func(t *testing.T) { capabilityUsageWindow(ctx, t, repo, project) })
}

func capabilityUsageEveryKey(ctx context.Context, t *testing.T, repo persistence.CapabilityUsageRepository) {
	t.Helper()
	rows, err := repo.Usage(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Key] = true
	}
	for _, c := range persistence.CapabilitySignals {
		if !seen[c.Key] {
			t.Errorf("capability %q is catalogued but absent from Usage — the unused ones are the enablement list", c.Key)
		}
	}
}

func capabilityUsageCountsTask(ctx context.Context, t *testing.T, repo persistence.CapabilityUsageRepository, project string) {
	t.Helper()
	rows, err := repo.Usage(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	i := indexOf(rows, func(r persistence.CapabilityUsage) bool { return r.Key == "tasks" && r.ProjectID == project })
	if i < 0 || rows[i].Count != 1 || rows[i].LastUsed == nil {
		t.Fatalf("want one (tasks, %s) row with Count 1 and LastUsed set, got index %d", project, i)
	}
}

func capabilityUsageWindow(ctx context.Context, t *testing.T, repo persistence.CapabilityUsageRepository, project string) {
	t.Helper()
	rows, err := repo.Usage(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	keyPresent := false
	for _, r := range rows {
		if r.Key != "tasks" {
			continue
		}
		keyPresent = true
		if r.ProjectID == project && r.Count > 0 {
			t.Errorf("a task created before the window must not count: %+v", r)
		}
	}
	if !keyPresent {
		t.Error("the tasks key must still appear with no usage")
	}
}

// ---------------------------------------------------------------- chat audit

// RunChatAuditSuite pins persistence.ChatAuditRepository.
func RunChatAuditSuite(t *testing.T, repo persistence.ChatAuditRepository) {
	t.Helper()
	ctx := context.Background()
	t.Run("GetByID_miss_obeys_the_contract", func(t *testing.T) {
		AssertMissRepo(t, "ChatAuditRepository.GetByID", repo.GetByID)
	})
	t.Run("Insert_defaults_duplicate_and_GetByID", func(t *testing.T) { chatAuditInsert(ctx, t, repo) })
	t.Run("List_filters_orders_and_round_trips", func(t *testing.T) { chatAuditList(ctx, t, repo) })
	t.Run("SavePrompt_is_idempotent_and_GetPrompt_misses_with_ErrNotFound", func(t *testing.T) { chatAuditPrompts(ctx, t, repo) })
}

func chatAuditInsert(ctx context.Context, t *testing.T, repo persistence.ChatAuditRepository) {
	t.Helper()
	project, chatA := uniqueID("proj"), uniqueID("chat")
	e := &persistence.ChatAuditEntry{ChatID: chatA, UserID: "u1", ProjectID: project, RoleUsed: "dispatcher", Model: "m"}
	if err := repo.Insert(ctx, e); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if e.ID == "" {
		t.Fatal("Insert must generate an ID")
	}
	if err := repo.Insert(ctx, &persistence.ChatAuditEntry{ID: e.ID, ChatID: chatA}); !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Errorf("Insert with an existing ID must return ErrDuplicateKey (the interface says so), got %v", err)
	}
	got, err := repo.GetByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != e.ID || got.ChatID != chatA || got.UserID != "u1" || got.ProjectID != project {
		t.Errorf("GetByID must populate at least ID, ChatID, UserID, ProjectID: %+v", got)
	}
}

func chatAuditList(ctx context.Context, t *testing.T, repo persistence.ChatAuditRepository) {
	t.Helper()
	base := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)
	project, chatB := uniqueID("proj"), uniqueID("chat")
	rows := []*persistence.ChatAuditEntry{
		{Timestamp: base, ChatID: chatB, UserID: "u", ProjectID: project, RoleUsed: "lead", Model: "m1", SystemPromptHash: "h1", UserMessage: "hello", Response: "hi", Iterations: 2, DurationMs: 120, CostUSD: 0.25, HallucinationSignalsJSON: `[{"k":1}]`},
		{Timestamp: base.Add(time.Minute), ChatID: chatB, UserID: "u", ProjectID: project, Model: "m2", ToolCallsJSON: `[{"name":"x"}]`},
		{Timestamp: base.Add(2 * time.Minute), ChatID: chatB, UserID: "u", ProjectID: uniqueID("other"), Model: "m3"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, r); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	if _, err := repo.List(ctx, persistence.ChatAuditFilter{ChatID: chatB}); err == nil {
		t.Error("List with PageSize 0 must be refused")
	}
	all, err := repo.List(ctx, persistence.ChatAuditFilter{ChatID: chatB, PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 || all[0].Model != "m3" || all[2].Model != "m1" {
		t.Fatalf("List must be newest first, got %d rows", len(all))
	}
	chatAuditRoundTrip(t, all[2], all[1], base)
	chatAuditFilterCases(ctx, t, repo, project, chatB, base)
	chatAuditBatch(ctx, t, repo, rows, chatB)
}

func chatAuditRoundTrip(t *testing.T, first, second *persistence.ChatAuditEntry, base time.Time) {
	t.Helper()
	if first.UserMessage != "hello" || first.Response != "hi" || first.Iterations != 2 || first.DurationMs != 120 || first.CostUSD != 0.25 ||
		first.SystemPromptHash != "h1" || first.HallucinationSignalsJSON != `[{"k":1}]` || !first.Timestamp.Equal(base) {
		t.Errorf("round trip lost a field: %+v", first)
	}
	if first.ToolCallsJSON != "[]" {
		t.Errorf("an empty ToolCallsJSON must be stored and read back as the empty array, got %q", first.ToolCallsJSON)
	}
	if second.ToolCallsJSON != `[{"name":"x"}]` || second.HallucinationSignalsJSON != "" {
		t.Errorf("row 2 round trip: %+v", second)
	}
}

func chatAuditFilterCases(ctx context.Context, t *testing.T, repo persistence.ChatAuditRepository, project, chatB string, base time.Time) {
	t.Helper()
	byProject, _ := repo.List(ctx, persistence.ChatAuditFilter{ProjectID: project, ChatID: chatB, PageSize: 10})
	if len(byProject) != 2 {
		t.Errorf("ProjectID filter: got %d rows", len(byProject))
	}
	window, _ := repo.List(ctx, persistence.ChatAuditFilter{ChatID: chatB, Since: base.Add(time.Minute), Until: base.Add(time.Minute), PageSize: 10})
	if len(window) != 1 || window[0].Model != "m2" {
		t.Errorf("Since and Until are both inclusive here: got %d rows", len(window))
	}
	page, _ := repo.List(ctx, persistence.ChatAuditFilter{ChatID: chatB, PageSize: 1, Offset: 2})
	if len(page) != 1 || page[0].Model != "m1" {
		t.Errorf("Offset pages in ts DESC order: got %+v", page)
	}
}

func chatAuditBatch(ctx context.Context, t *testing.T, repo persistence.ChatAuditRepository, rows []*persistence.ChatAuditEntry, chatB string) {
	t.Helper()
	batch, err := repo.GetChatAuditsByTurnIDs(ctx, []string{rows[0].ID, rows[1].ID, uniqueID("ghost")})
	if err != nil {
		t.Fatalf("GetChatAuditsByTurnIDs: %v", err)
	}
	if len(batch) != 2 || batch[rows[0].ID].ChatID != chatB || batch[rows[1].ID].Model != "m2" {
		t.Errorf("batch must key the present rows by ID and omit the absent one: %v", batch)
	}
	empty, err := repo.GetChatAuditsByTurnIDs(ctx, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty input must return an empty, non-nil map: %v %v", empty, err)
	}
}

func chatAuditPrompts(ctx context.Context, t *testing.T, repo persistence.ChatAuditRepository) {
	t.Helper()
	body := "You are the lead.\n" + uniqueID("salt")
	h1, err := repo.SavePrompt(ctx, body)
	if err != nil {
		t.Fatalf("SavePrompt: %v", err)
	}
	if h1 != persistence.HashChatSystemPrompt(body) {
		t.Errorf("SavePrompt must return the digest of the stored bytes")
	}
	if h2, err := repo.SavePrompt(ctx, body); err != nil || h2 != h1 {
		t.Errorf("second SavePrompt of the same body: %q %v", h2, err)
	}
	if got, err := repo.GetPrompt(ctx, h1); err != nil || got != body {
		t.Errorf("GetPrompt round trip: %q %v", got, err)
	}
	if _, err := repo.GetPrompt(ctx, uniqueID("absent")); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("GetPrompt(absent) = %v, want ErrNotFound", err)
	}
	if _, err := repo.SavePrompt(ctx, ""); err == nil {
		t.Error("SavePrompt of an empty body must be refused")
	}
}

// ---------------------------------------------------- chunk graph extraction

type chunkExtractionFx struct {
	repo             persistence.ChunkGraphExtractionRepository
	old, newer, done string
	project          string
	pendingBefore    int
	statsBefore      *persistence.KGStats
}

// RunChunkGraphExtractionSuite pins persistence.ChunkGraphExtractionRepository.
// Counts are asserted as deltas against a baseline because the Postgres
// integration database is shared across suites.
func RunChunkGraphExtractionSuite(t *testing.T, repo persistence.ChunkGraphExtractionRepository, seed SeedChunk) {
	t.Helper()
	ctx := context.Background()
	fx := &chunkExtractionFx{repo: repo, project: uniqueID("proj"), old: uniqueID("chunk"), newer: uniqueID("chunk"), done: uniqueID("chunk")}
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	var err error
	if fx.pendingBefore, err = repo.PendingCount(ctx); err != nil {
		t.Fatalf("PendingCount baseline: %v", err)
	}
	if fx.statsBefore, err = repo.Stats(ctx); err != nil {
		t.Fatalf("Stats baseline: %v", err)
	}
	for _, c := range []struct {
		id      string
		flagged bool
		at      time.Time
	}{{fx.old, true, base}, {fx.newer, true, base.Add(time.Minute)}, {fx.done, false, base.Add(2 * time.Minute)}} {
		if err := seed(ctx, c.id, fx.project, "content of "+c.id, c.flagged, c.at); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
	}
	t.Run("FetchUnextracted_returns_flagged_oldest_first_within_limit", func(t *testing.T) { chunkExtractionFetch(ctx, t, fx) })
	t.Run("PendingCount_and_Stats_move_with_the_flag", func(t *testing.T) { chunkExtractionCounts(ctx, t, fx) })
	t.Run("RecordExtractionFailure_counts_up_and_quarantines_at_max", func(t *testing.T) { chunkExtractionFailure(ctx, t, fx) })
}

func chunkExtractionFetch(ctx context.Context, t *testing.T, fx *chunkExtractionFx) {
	t.Helper()
	rows, err := fx.repo.FetchUnextracted(ctx, 500)
	if err != nil {
		t.Fatalf("FetchUnextracted: %v", err)
	}
	iOld := indexOf(rows, func(r persistence.ChunkForExtraction) bool { return r.ID == fx.old })
	iNew := indexOf(rows, func(r persistence.ChunkForExtraction) bool { return r.ID == fx.newer })
	iDone := indexOf(rows, func(r persistence.ChunkForExtraction) bool { return r.ID == fx.done })
	if iOld < 0 || iNew < 0 || iOld > iNew {
		t.Errorf("both flagged chunks must be returned, oldest first: old=%d newer=%d", iOld, iNew)
	}
	if iOld >= 0 && (rows[iOld].ProjectID != fx.project || rows[iOld].Content != "content of "+fx.old) {
		t.Errorf("row fields: %+v", rows[iOld])
	}
	if iDone >= 0 {
		t.Error("an unflagged chunk must not be fetched")
	}
}

func chunkExtractionCounts(ctx context.Context, t *testing.T, fx *chunkExtractionFx) {
	t.Helper()
	n, _ := fx.repo.PendingCount(ctx)
	if n != fx.pendingBefore+2 {
		t.Errorf("PendingCount = %d, want baseline %d + 2", n, fx.pendingBefore)
	}
	st, err := fx.repo.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.ChunksPending != fx.statsBefore.ChunksPending+2 || st.ChunksDone != fx.statsBefore.ChunksDone+1 {
		t.Errorf("Stats pending/done = %d/%d, want %d/%d", st.ChunksPending, st.ChunksDone, fx.statsBefore.ChunksPending+2, fx.statsBefore.ChunksDone+1)
	}
	if st.EntitiesByType == nil {
		t.Error("EntitiesByType must be a non-nil map")
	}
	if err := fx.repo.MarkExtracted(ctx, fx.old); err != nil {
		t.Fatalf("MarkExtracted: %v", err)
	}
	if n, _ := fx.repo.PendingCount(ctx); n != fx.pendingBefore+1 {
		t.Errorf("after MarkExtracted PendingCount = %d, want %d", n, fx.pendingBefore+1)
	}
	if err := fx.repo.MarkExtracted(ctx, ""); err == nil {
		t.Error("MarkExtracted with an empty id must be refused")
	}
}

func chunkExtractionFailure(ctx context.Context, t *testing.T, fx *chunkExtractionFx) {
	t.Helper()
	attempts, quarantined, err := fx.repo.RecordExtractionFailure(ctx, fx.newer, "boom", 2)
	if err != nil {
		t.Fatalf("RecordExtractionFailure 1: %v", err)
	}
	if attempts != 1 || quarantined {
		t.Errorf("first failure: attempts=%d quarantined=%t, want 1/false", attempts, quarantined)
	}
	if n, _ := fx.repo.PendingCount(ctx); n != fx.pendingBefore+1 {
		t.Errorf("a failure under max keeps the chunk pending: %d", n)
	}
	attempts, quarantined, err = fx.repo.RecordExtractionFailure(ctx, fx.newer, "boom again", 2)
	if err != nil {
		t.Fatalf("RecordExtractionFailure 2: %v", err)
	}
	if attempts != 2 || !quarantined {
		t.Errorf("second failure at max 2: attempts=%d quarantined=%t, want 2/true", attempts, quarantined)
	}
	if n, _ := fx.repo.PendingCount(ctx); n != fx.pendingBefore {
		t.Errorf("a quarantined chunk leaves the pending set: %d, want %d", n, fx.pendingBefore)
	}
	if _, _, err := fx.repo.RecordExtractionFailure(ctx, "", "x", 1); err == nil {
		t.Error("empty chunk id must be refused")
	}
}

// -------------------------------------------------------------- cluster node

// RunClusterNodeSuite pins persistence.ClusterNodeRepository.
func RunClusterNodeSuite(t *testing.T, repo persistence.ClusterNodeRepository) {
	t.Helper()
	ctx := context.Background()
	a, b := uniqueID("node-a"), uniqueID("node-b")
	t.Run("Upsert_inserts_then_updates_and_stamps_last_seen", func(t *testing.T) { clusterNodeUpsert(ctx, t, repo, a) })
	t.Run("List_orders_by_instance_id", func(t *testing.T) { clusterNodeListOrder(ctx, t, repo, a, b) })
	t.Run("DeleteStale_honours_the_protected_list_and_DeleteByInstanceID_removes", func(t *testing.T) { clusterNodeDelete(ctx, t, repo, a, b) })
}

func findClusterNode(ctx context.Context, t *testing.T, repo persistence.ClusterNodeRepository, id string) *persistence.ClusterNode {
	t.Helper()
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if i := indexOf(rows, func(n *persistence.ClusterNode) bool { return n.InstanceID == id }); i >= 0 {
		return rows[i]
	}
	return nil
}

func clusterNodeUpsert(ctx context.Context, t *testing.T, repo persistence.ClusterNodeRepository, a string) {
	t.Helper()
	if err := repo.Upsert(ctx, nil); err == nil {
		t.Error("Upsert(nil) must be refused")
	}
	start := time.Now().UTC().Add(-time.Minute)
	if err := repo.Upsert(ctx, &persistence.ClusterNode{InstanceID: a, Profile: "all", Version: "1", Address: "h:1", Capabilities: map[string]bool{"ui": true, "worker": false}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got := findClusterNode(ctx, t, repo, a)
	if got == nil || got.Profile != "all" || got.Version != "1" || got.Address != "h:1" || !got.Capabilities["ui"] || got.Capabilities["worker"] {
		t.Fatalf("round trip: %+v", got)
	}
	if got.LastSeen.Before(start) || got.LastSeen.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("LastSeen must be stamped by the store at upsert time, got %v", got.LastSeen)
	}
	if err := repo.Upsert(ctx, &persistence.ClusterNode{InstanceID: a, Profile: "worker", Version: "2", Capabilities: map[string]bool{}}); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	if got = findClusterNode(ctx, t, repo, a); got == nil || got.Profile != "worker" || got.Version != "2" || len(got.Capabilities) != 0 {
		t.Errorf("Upsert must replace the row in place: %+v", got)
	}
}

func clusterNodeListOrder(ctx context.Context, t *testing.T, repo persistence.ClusterNodeRepository, a, b string) {
	t.Helper()
	if err := repo.Upsert(ctx, &persistence.ClusterNode{InstanceID: b, Profile: "ui", Capabilities: map[string]bool{}}); err != nil {
		t.Fatalf("Upsert b: %v", err)
	}
	rows, _ := repo.List(ctx)
	ia := indexOf(rows, func(n *persistence.ClusterNode) bool { return n.InstanceID == a })
	ib := indexOf(rows, func(n *persistence.ClusterNode) bool { return n.InstanceID == b })
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("List must be ordered by instance_id: a=%d b=%d", ia, ib)
	}
}

func clusterNodeDelete(ctx context.Context, t *testing.T, repo persistence.ClusterNodeRepository, a, b string) {
	t.Helper()
	n, err := repo.DeleteStale(ctx, 24*time.Hour, nil)
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if findClusterNode(ctx, t, repo, a) == nil || findClusterNode(ctx, t, repo, b) == nil {
		t.Errorf("fresh rows must survive a 24h staleness sweep (deleted %d)", n)
	}
	if _, err := repo.DeleteStale(ctx, 0, []string{a}); err != nil {
		t.Fatalf("DeleteStale(0, protected a): %v", err)
	}
	if findClusterNode(ctx, t, repo, a) == nil {
		t.Error("a protected instance must survive DeleteStale regardless of age")
	}
	if findClusterNode(ctx, t, repo, b) != nil {
		t.Error("an unprotected instance older than the zero threshold must be removed")
	}
	if err := repo.DeleteByInstanceID(ctx, a); err != nil {
		t.Fatalf("DeleteByInstanceID: %v", err)
	}
	if findClusterNode(ctx, t, repo, a) != nil {
		t.Error("DeleteByInstanceID must remove the row")
	}
}

// --------------------------------------------------------------- leader lock

// RunLeaderLockSuite pins the acquisition semantics of
// persistence.DaemonLeaderLockRepository: first acquire wins with epoch 1,
// the holder re-acquires with the same epoch and its original acquired_at,
// a rival is refused while the lease is live and inherits epoch+1 once it
// has expired.
//
// Renew and Release are deliberately NOT asserted: the SQLite implementation
// is a single-process degenerate (Renew always true, Release a no-op — see
// its comments) while Postgres implements both. That difference is filed
// (backlog 2026-09-05), not hidden here.
func RunLeaderLockSuite(t *testing.T, repo persistence.DaemonLeaderLockRepository) {
	t.Helper()
	ctx := context.Background()
	worker := uniqueID("worker")
	now := time.Now().UTC().Truncate(time.Microsecond)
	t.Run("Get_miss_obeys_the_contract", func(t *testing.T) {
		AssertMissRepo(t, "DaemonLeaderLockRepository.Get", repo.Get)
	})
	t.Run("first_acquire_wins_with_epoch_1_and_Get_reads_it_back", func(t *testing.T) { leaderLockFirstAcquire(ctx, t, repo, worker, now) })
	t.Run("holder_reacquires_keeping_epoch_and_acquired_at", func(t *testing.T) { leaderLockReacquire(ctx, t, repo, worker, now) })
	t.Run("rival_refused_while_live_and_wins_epoch_plus_one_after_expiry", func(t *testing.T) { leaderLockRival(ctx, t, repo, worker, now) })
	t.Run("List_orders_by_worker_id", func(t *testing.T) { leaderLockList(ctx, t, repo, worker, now) })
}

func leaderLockFirstAcquire(ctx context.Context, t *testing.T, repo persistence.DaemonLeaderLockRepository, worker string, now time.Time) {
	t.Helper()
	ok, epoch, err := repo.Acquire(ctx, worker, "h1", now, time.Minute)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("Acquire: ok=%t epoch=%d err=%v, want true/1/nil", ok, epoch, err)
	}
	got, err := repo.Get(ctx, worker)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.HolderID != "h1" || got.Epoch != 1 || !got.AcquiredAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(time.Minute)) || !got.IsHeldBy("h1", now) {
		t.Errorf("Get: %+v", got)
	}
}

func leaderLockReacquire(ctx context.Context, t *testing.T, repo persistence.DaemonLeaderLockRepository, worker string, now time.Time) {
	t.Helper()
	later := now.Add(10 * time.Second)
	ok, epoch, err := repo.Acquire(ctx, worker, "h1", later, time.Minute)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("re-Acquire: ok=%t epoch=%d err=%v", ok, epoch, err)
	}
	got, _ := repo.Get(ctx, worker)
	if !got.AcquiredAt.Equal(now) || !got.RenewedAt.Equal(later) || !got.ExpiresAt.Equal(later.Add(time.Minute)) {
		t.Errorf("re-acquire must keep acquired_at and move renewed/expires: %+v", got)
	}
}

func leaderLockRival(ctx context.Context, t *testing.T, repo persistence.DaemonLeaderLockRepository, worker string, now time.Time) {
	t.Helper()
	ok, _, err := repo.Acquire(ctx, worker, "h2", now.Add(20*time.Second), time.Minute)
	if err != nil || ok {
		t.Fatalf("a rival must be refused while the lease is live: ok=%t err=%v", ok, err)
	}
	expired := now.Add(2 * time.Hour)
	ok, epoch, err := repo.Acquire(ctx, worker, "h2", expired, time.Minute)
	if err != nil || !ok || epoch != 2 {
		t.Fatalf("rival after expiry: ok=%t epoch=%d err=%v, want true/2", ok, epoch, err)
	}
	got, _ := repo.Get(ctx, worker)
	if got.HolderID != "h2" || !got.AcquiredAt.Equal(expired) {
		t.Errorf("the new holder's acquired_at is its own: %+v", got)
	}
}

func leaderLockList(ctx context.Context, t *testing.T, repo persistence.DaemonLeaderLockRepository, worker string, now time.Time) {
	t.Helper()
	other := worker + "-z"
	if _, _, err := repo.Acquire(ctx, other, "h9", now, time.Minute); err != nil {
		t.Fatalf("Acquire other: %v", err)
	}
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	iw := indexOf(rows, func(l *persistence.DaemonLeaderLock) bool { return l.WorkerID == worker })
	io := indexOf(rows, func(l *persistence.DaemonLeaderLock) bool { return l.WorkerID == other })
	if iw < 0 || io < 0 || iw > io {
		t.Errorf("List must contain both, ordered by worker_id: %d %d", iw, io)
	}
}

// ------------------------------------------------------------ entity mention

// RunEntityMentionSuite pins persistence.EntityMentionRepository for
// Insert, ListByChunk and DeleteForChunk. ListByEntity's ORDER differs
// between the backends today (Postgres joins the chunk table and orders by
// its created_at; SQLite orders by chunk_id) — filed 2026-09-05 — so only
// membership and the limit are asserted for it.
func RunEntityMentionSuite(t *testing.T, repo persistence.EntityMentionRepository, entities persistence.KnowledgeEntityRepository, seed SeedChunk) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	chunk, other := uniqueID("chunk"), uniqueID("chunk")
	for _, c := range []string{chunk, other} {
		if err := seed(ctx, c, project, "body "+c, false, time.Now().UTC()); err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
	}
	ent := &persistence.KnowledgeEntity{ProjectID: project, Type: "person", CanonicalName: "Ada-" + uniqueID("")}
	if err := entities.Insert(ctx, ent); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	t.Run("Insert_is_idempotent_and_ListByChunk_orders_by_char_start", func(t *testing.T) { entityMentionInsert(ctx, t, repo, chunk, ent.ID) })
	t.Run("ListByEntity_returns_the_mentions_and_honours_the_limit", func(t *testing.T) { entityMentionListByEntity(ctx, t, repo, other, ent.ID) })
	t.Run("DeleteForChunk_removes_only_that_chunks_mentions", func(t *testing.T) { entityMentionDelete(ctx, t, repo, chunk, other) })
}

func entityMentionInsert(ctx context.Context, t *testing.T, repo persistence.EntityMentionRepository, chunk, entity string) {
	t.Helper()
	end := 8
	for _, m := range []*persistence.EntityMention{
		{ChunkID: chunk, EntityID: entity, CharStart: 40, Surface: "Ada L."},
		{ChunkID: chunk, EntityID: entity, CharStart: 5, CharEnd: &end, Surface: "Ada"},
		{ChunkID: chunk, EntityID: entity, CharStart: 5, CharEnd: &end, Surface: "Ada"}, // duplicate
	} {
		if err := repo.Insert(ctx, m); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	rows, err := repo.ListByChunk(ctx, chunk)
	if err != nil {
		t.Fatalf("ListByChunk: %v", err)
	}
	if len(rows) != 2 || rows[0].CharStart != 5 || rows[1].CharStart != 40 {
		t.Fatalf("want two rows by char_start ascending, got %+v", rows)
	}
	if rows[0].CharEnd == nil || *rows[0].CharEnd != 8 || rows[0].Surface != "Ada" || rows[1].CharEnd != nil {
		t.Errorf("CharEnd/Surface round trip: %+v %+v", rows[0], rows[1])
	}
}

func entityMentionListByEntity(ctx context.Context, t *testing.T, repo persistence.EntityMentionRepository, other, entity string) {
	t.Helper()
	if err := repo.Insert(ctx, &persistence.EntityMention{ChunkID: other, EntityID: entity, CharStart: 1}); err != nil {
		t.Fatalf("Insert other: %v", err)
	}
	all, err := repo.ListByEntity(ctx, entity, 10)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("want 3 mentions of the entity, got %d", len(all))
	}
	if limited, _ := repo.ListByEntity(ctx, entity, 1); len(limited) != 1 {
		t.Errorf("limit must cap the result: %d", len(limited))
	}
}

func entityMentionDelete(ctx context.Context, t *testing.T, repo persistence.EntityMentionRepository, chunk, other string) {
	t.Helper()
	if err := repo.DeleteForChunk(ctx, chunk); err != nil {
		t.Fatalf("DeleteForChunk: %v", err)
	}
	if rows, _ := repo.ListByChunk(ctx, chunk); len(rows) != 0 {
		t.Errorf("mentions must be gone: %d", len(rows))
	}
	if rows, _ := repo.ListByChunk(ctx, other); len(rows) != 1 {
		t.Errorf("the other chunk's mention must survive: %d", len(rows))
	}
}

// ------------------------------------------------------------ execution hint

// RunExecutionHintSuite pins persistence.ExecutionHintRepository.
func RunExecutionHintSuite(t *testing.T, repo persistence.ExecutionHintRepository) {
	t.Helper()
	ctx := context.Background()
	task, exec := uniqueID("task"), uniqueID("exec")
	base := time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC)
	t.Run("Insert_validation", func(t *testing.T) { executionHintValidation(ctx, t, repo, task, exec) })
	t.Run("task_scoped_hints_list_pending_and_consume_once", func(t *testing.T) { executionHintTaskScoped(ctx, t, repo, task, base) })
	t.Run("execution_scoped_hints_and_the_union_view", func(t *testing.T) { executionHintExecutionScoped(ctx, t, repo, task, exec, base) })
}

func executionHintValidation(ctx context.Context, t *testing.T, repo persistence.ExecutionHintRepository, task, exec string) {
	t.Helper()
	for name, h := range map[string]*persistence.ExecutionHint{
		"nil":        nil,
		"no id":      {TaskID: task, Content: "x"},
		"no scope":   {ID: uniqueID("hint"), Content: "x"},
		"both":       {ID: uniqueID("hint"), TaskID: task, ExecutionID: exec, Content: "x"},
		"no content": {ID: uniqueID("hint"), TaskID: task},
	} {
		if err := repo.Insert(ctx, h); err == nil {
			t.Errorf("Insert(%s) must be refused", name)
		}
	}
}

func executionHintTaskScoped(ctx context.Context, t *testing.T, repo persistence.ExecutionHintRepository, task string, base time.Time) {
	t.Helper()
	h1 := &persistence.ExecutionHint{ID: uniqueID("hint"), TaskID: task, Content: "first", CreatedAt: base, CreatedBy: "op"}
	h2 := &persistence.ExecutionHint{ID: uniqueID("hint"), TaskID: task, Content: "second", CreatedAt: base.Add(time.Minute), CreatedBy: "op"}
	stepped := &persistence.ExecutionHint{ID: uniqueID("hint"), TaskID: task, StepID: "review", Content: "for review", CreatedAt: base.Add(2 * time.Minute)}
	for _, h := range []*persistence.ExecutionHint{h1, h2, stepped} {
		if err := repo.Insert(ctx, h); err != nil {
			t.Fatalf("Insert %s: %v", h.Content, err)
		}
	}
	pending, err := repo.ListPendingForTask(ctx, task)
	if err != nil {
		t.Fatalf("ListPendingForTask: %v", err)
	}
	if len(pending) != 3 || pending[0].Content != "first" || pending[0].AppliedAt != nil || pending[0].CreatedBy != "op" || !pending[0].CreatedAt.Equal(base) {
		t.Fatalf("pending must be oldest first with fields intact: %+v", pending)
	}
	executionHintConsume(ctx, t, repo, task)
}

func executionHintConsume(ctx context.Context, t *testing.T, repo persistence.ExecutionHintRepository, task string) {
	t.Helper()
	consumed, err := repo.ConsumePending(ctx, task, "", "")
	if err != nil {
		t.Fatalf("ConsumePending: %v", err)
	}
	if len(consumed) != 2 {
		t.Fatalf("a consume with no step takes only step-less hints, got %d", len(consumed))
	}
	for _, c := range consumed {
		if c.AppliedAt == nil || c.StepID != "" {
			t.Errorf("consumed hint must carry applied_at and no step: %+v", c)
		}
	}
	if again, _ := repo.ConsumePending(ctx, task, "", ""); len(again) != 0 {
		t.Errorf("a second consume must find nothing: %d", len(again))
	}
	forStep, err := repo.ConsumePending(ctx, task, "", "review")
	if err != nil || len(forStep) != 1 || forStep[0].Content != "for review" {
		t.Errorf("a consume for the step takes its hint: %+v %v", forStep, err)
	}
	if left, _ := repo.ListPendingForTask(ctx, task); len(left) != 0 {
		t.Errorf("nothing pending after both consumes: %d", len(left))
	}
	if all, _ := repo.ListByTask(ctx, task); len(all) != 3 {
		t.Errorf("ListByTask lists consumed hints too: %d", len(all))
	}
}

func executionHintExecutionScoped(ctx context.Context, t *testing.T, repo persistence.ExecutionHintRepository, task, exec string, base time.Time) {
	t.Helper()
	e1 := &persistence.ExecutionHint{ID: uniqueID("hint"), ExecutionID: exec, Content: "exec-old", CreatedAt: base}
	e2 := &persistence.ExecutionHint{ID: uniqueID("hint"), ExecutionID: exec, Content: "exec-new", CreatedAt: base.Add(time.Hour)}
	for _, h := range []*persistence.ExecutionHint{e1, e2} {
		if err := repo.Insert(ctx, h); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	byExec, err := repo.ListByExecution(ctx, exec)
	if err != nil || len(byExec) != 2 || byExec[0].Content != "exec-new" {
		t.Errorf("ListByExecution newest first: %+v %v", byExec, err)
	}
	union, err := repo.ListForExecution(ctx, exec, task)
	if err != nil || len(union) != 5 {
		t.Errorf("ListForExecution unions the execution's hints with the task's execution-less ones: %d %v", len(union), err)
	}
	if consumed, _ := repo.ConsumePending(ctx, "", exec, ""); len(consumed) != 2 {
		t.Errorf("consume by execution: %d", len(consumed))
	}
	if _, err := repo.ListByExecution(ctx, ""); err == nil {
		t.Error("empty execution id must be refused")
	}
	if _, err := repo.ConsumePending(ctx, "", "", ""); err == nil {
		t.Error("consume with no scope must be refused")
	}
}

// --------------------------------------------------- execution quality score

type qualityScoreFx struct {
	repo            persistence.ExecutionQualityScoreRepository
	project         string
	scored, pending *persistence.Execution
}

// RunExecutionQualityScoreSuite pins persistence.ExecutionQualityScoreRepository.
// Rows join executions by identity on both backends, so a task and an
// execution are seeded through their repositories.
func RunExecutionQualityScoreSuite(t *testing.T, repo persistence.ExecutionQualityScoreRepository, execs persistence.ExecutionRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	fx := &qualityScoreFx{repo: repo, project: uniqueID("proj")}

	t.Run("GetByExecution_miss_obeys_the_contract", func(t *testing.T) {
		AssertMissRepo(t, "ExecutionQualityScoreRepository.GetByExecution", repo.GetByExecution)
	})
	fx.scored = seedTerminalExecution(ctx, t, execs, tasks, fx.project, persistence.ExecutionStatusCompleted)
	fx.pending = seedTerminalExecution(ctx, t, execs, tasks, fx.project, persistence.ExecutionStatusFailed)
	statsBefore, err := repo.PendingTerminalStats(ctx, []string{fx.project})
	if err != nil {
		t.Fatalf("PendingTerminalStats baseline: %v", err)
	}
	if statsBefore.Count != 2 {
		t.Fatalf("two unscored terminal executions in this project, got %d", statsBefore.Count)
	}
	t.Run("Upsert_requires_a_matching_execution_identity", func(t *testing.T) { qualityScoreIdentity(ctx, t, fx) })
	t.Run("Upsert_round_trips_replaces_and_leaves_the_pending_set", func(t *testing.T) { qualityScoreRoundTrip(ctx, t, fx) })
	t.Run("List_filters", func(t *testing.T) { qualityScoreListFilters(ctx, t, fx) })
}

func seedTerminalExecution(ctx context.Context, t *testing.T, execs persistence.ExecutionRepository, tasks persistence.TaskRepository, project string, status persistence.ExecutionStatus) *persistence.Execution {
	t.Helper()
	task := newQueuedTask(project)
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	exec := &persistence.Execution{ID: uniqueID("exec"), TaskID: task.ID, ProjectID: project, WorkflowID: "wf-q", WorkflowRevision: "r1", Status: status, CreatedAt: now, UpdatedAt: now}
	if err := execs.Create(ctx, exec); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	return exec
}

func qualityScoreIdentity(ctx context.Context, t *testing.T, fx *qualityScoreFx) {
	t.Helper()
	v := 0.5
	bad := &persistence.ExecutionQualityScore{ProjectID: fx.project, TaskID: uniqueID("wrong-task"), ExecutionID: fx.scored.ID, WorkflowID: "wf-q", Status: "scored", Score: &v}
	if err := fx.repo.Upsert(ctx, bad); err == nil {
		t.Error("an identity that does not match the execution row must be refused")
	}
	if err := fx.repo.Upsert(ctx, &persistence.ExecutionQualityScore{}); err == nil {
		t.Error("validation must run before the write")
	}
}

func qualityScoreRoundTrip(ctx context.Context, t *testing.T, fx *qualityScoreFx) {
	t.Helper()
	v := 0.75
	at := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
	s := &persistence.ExecutionQualityScore{ProjectID: fx.project, TaskID: fx.scored.TaskID, ExecutionID: fx.scored.ID, WorkflowID: "wf-q", WorkflowRevision: "r1",
		ScorerVersion: "v1", ScoringPolicySHA: "abc", Kind: "contract", Status: "scored", Score: &v, PassedCaseCount: 3, PinnedCaseCount: 4, Diagnostic: "fine",
		CaseEvidence: json.RawMessage(`[{"case":"a","ok":true}]`), RecordedAt: at}
	if err := fx.repo.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := fx.repo.GetByExecution(ctx, fx.scored.ID)
	if err != nil {
		t.Fatalf("GetByExecution: %v", err)
	}
	if got.Score == nil || *got.Score != 0.75 || got.PassedCaseCount != 3 || got.PinnedCaseCount != 4 || got.Diagnostic != "fine" ||
		got.ScorerVersion != "v1" || got.ScoringPolicySHA != "abc" || got.Kind != "contract" || got.WorkflowRevision != "r1" || !got.RecordedAt.Equal(at) {
		t.Errorf("round trip: %+v", got)
	}
	if !sameJSON(string(got.CaseEvidence), `[{"case":"a","ok":true}]`) {
		t.Errorf("CaseEvidence must round-trip as the same JSON document, got %s", got.CaseEvidence)
	}
	s.Status, s.Score, s.Diagnostic = "not_applicable", nil, "n/a"
	if err := fx.repo.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert replace: %v", err)
	}
	if got, _ = fx.repo.GetByExecution(ctx, fx.scored.ID); got.Status != "not_applicable" || got.Score != nil || got.Diagnostic != "n/a" {
		t.Errorf("Upsert must replace in place: %+v", got)
	}
	qualityScorePendingSet(ctx, t, fx)
}

func qualityScorePendingSet(ctx context.Context, t *testing.T, fx *qualityScoreFx) {
	t.Helper()
	stats, _ := fx.repo.PendingTerminalStats(ctx, []string{fx.project})
	if stats.Count != 1 || stats.OldestAt == nil {
		t.Errorf("one unscored terminal execution left, with an oldest timestamp: %+v", stats)
	}
	// ListPendingTerminal is oldest-first across EVERY project, and the
	// Postgres integration database carries other suites' terminal
	// executions, so membership of this suite's row within the limit is
	// not a contract. What is: every returned execution is terminal and
	// has no score row — checked on a bounded sample.
	list, err := fx.repo.ListPendingTerminal(ctx, 500)
	if err != nil {
		t.Fatalf("ListPendingTerminal: %v", err)
	}
	for i, e := range list {
		if i >= 5 {
			break
		}
		if e.Status != persistence.ExecutionStatusCompleted && e.Status != persistence.ExecutionStatusFailed && e.Status != persistence.ExecutionStatusCancelled {
			t.Errorf("ListPendingTerminal returned a non-terminal execution: %+v", e)
		}
		if _, err := fx.repo.GetByExecution(ctx, e.ID); !errors.Is(err, persistence.ErrNotFound) {
			t.Errorf("ListPendingTerminal returned an execution that has a score: %s (%v)", e.ID, err)
		}
		if e.ID == fx.scored.ID {
			t.Error("the scored execution must not be pending")
		}
	}
}

func qualityScoreListFilters(ctx context.Context, t *testing.T, fx *qualityScoreFx) {
	t.Helper()
	v := 0.2
	low := &persistence.ExecutionQualityScore{ProjectID: fx.project, TaskID: fx.pending.TaskID, ExecutionID: fx.pending.ID, WorkflowID: "wf-q", Status: "scored", Score: &v}
	if err := fx.repo.Upsert(ctx, low); err != nil {
		t.Fatalf("Upsert low: %v", err)
	}
	all, err := fx.repo.List(ctx, persistence.ExecutionQualityScoreFilter{ProjectIDs: []string{fx.project}})
	if err != nil || len(all) != 2 {
		t.Fatalf("List by project: %d %v", len(all), err)
	}
	maxScore := 0.5
	lowOnly, _ := fx.repo.List(ctx, persistence.ExecutionQualityScoreFilter{ProjectIDs: []string{fx.project}, MaxScore: &maxScore, Statuses: []string{"scored"}})
	if len(lowOnly) != 1 || lowOnly[0].ExecutionID != fx.pending.ID {
		t.Errorf("MaxScore+Statuses filter: %+v", lowOnly)
	}
	byExec, _ := fx.repo.List(ctx, persistence.ExecutionQualityScoreFilter{ExecutionID: fx.scored.ID})
	if len(byExec) != 1 || byExec[0].Status != "not_applicable" {
		t.Errorf("ExecutionID filter: %+v", byExec)
	}
}

// ------------------------------------------------------- memory search stage

// RunMemorySearchStageSuite pins persistence.MemorySearchStageRepository —
// a write-only interface, so the contract is what RecordStage accepts and
// what it fills in.
func RunMemorySearchStageSuite(t *testing.T, repo persistence.MemorySearchStageRepository) {
	t.Helper()
	ctx := context.Background()
	if err := repo.RecordStage(ctx, nil); err == nil {
		t.Error("RecordStage(nil) must be refused")
	}
	trace := uniqueID("trace")
	stage := &persistence.MemorySearchStage{ProjectID: uniqueID("proj"), TraceID: &trace, Stage: "trust_verdict", Parameters: []byte(`{"k":1}`)}
	if err := repo.RecordStage(ctx, stage); err != nil {
		t.Fatalf("RecordStage: %v", err)
	}
	if stage.ID == "" || stage.CreatedAt.IsZero() {
		t.Errorf("RecordStage must fill ID and CreatedAt: %+v", stage)
	}
	bare := &persistence.MemorySearchStage{ProjectID: uniqueID("proj"), Stage: "trust_verdict"}
	if err := repo.RecordStage(ctx, bare); err != nil {
		t.Errorf("nil Parameters and nil TraceID must be accepted: %v", err)
	}
}

// ---------------------------------------------------------- operator profile

// RunOperatorProfileSuite pins persistence.OperatorProfileRepository.
func RunOperatorProfileSuite(t *testing.T, repo persistence.OperatorProfileRepository) {
	t.Helper()
	ctx := context.Background()
	op := uniqueID("op")
	t.Run("Get_miss_obeys_the_contract_and_empty_id_is_an_error", func(t *testing.T) { operatorProfileMiss(ctx, t, repo) })
	t.Run("Upsert_inserts_then_updates_and_List_orders_by_updated_at", func(t *testing.T) { operatorProfileUpsert(ctx, t, repo, op) })
	t.Run("Delete_then_miss", func(t *testing.T) { operatorProfileDelete(ctx, t, repo, op) })
}

func operatorProfileMiss(ctx context.Context, t *testing.T, repo persistence.OperatorProfileRepository) {
	t.Helper()
	AssertMissRepo(t, "OperatorProfileRepository.Get", repo.Get)
	if _, err := repo.Get(ctx, ""); err == nil || errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("an empty operator id is a caller bug, not a miss: %v", err)
	}
	if err := repo.Upsert(ctx, &persistence.OperatorProfile{}); err == nil {
		t.Error("Upsert without an operator id must be refused")
	}
}

func operatorProfileUpsert(ctx context.Context, t *testing.T, repo persistence.OperatorProfileRepository, op string) {
	t.Helper()
	if err := repo.Upsert(ctx, &persistence.OperatorProfile{OperatorID: op, Notes: "first"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := repo.Get(ctx, op)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Structured) != "{}" || got.Notes != "first" || got.CreatedAt.IsZero() || got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("empty Structured must read back as {}: %+v", got)
	}
	time.Sleep(5 * time.Millisecond)
	if err := repo.Upsert(ctx, &persistence.OperatorProfile{OperatorID: op, Structured: []byte(`{"tone":"terse"}`), Notes: "second"}); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	again, _ := repo.Get(ctx, op)
	if !sameJSON(string(again.Structured), `{"tone":"terse"}`) || again.Notes != "second" || !again.CreatedAt.Equal(got.CreatedAt) || !again.UpdatedAt.After(got.UpdatedAt) {
		t.Errorf("update must keep created_at and advance updated_at: before %+v after %+v", got, again)
	}
	older := uniqueID("op")
	if err := repo.Upsert(ctx, &persistence.OperatorProfile{OperatorID: older}); err != nil {
		t.Fatalf("Upsert older: %v", err)
	}
	list, err := repo.List(ctx, 500)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	iOp := indexOf(list, func(p *persistence.OperatorProfile) bool { return p.OperatorID == op })
	iOlder := indexOf(list, func(p *persistence.OperatorProfile) bool { return p.OperatorID == older })
	if iOp < 0 || iOlder < 0 || iOlder > iOp {
		t.Errorf("List must be updated_at DESC (the newest upsert first): op=%d older=%d", iOp, iOlder)
	}
	if one, _ := repo.List(ctx, 1); len(one) != 1 {
		t.Errorf("limit: %d", len(one))
	}
}

func operatorProfileDelete(ctx context.Context, t *testing.T, repo persistence.OperatorProfileRepository, op string) {
	t.Helper()
	if err := repo.Delete(ctx, op); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, op); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("after Delete, Get = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------- secret redaction audit

// RunSecretRedactionAuditSuite pins persistence.SecretRedactionAuditRepository.
func RunSecretRedactionAuditSuite(t *testing.T, repo persistence.SecretRedactionAuditRepository) {
	t.Helper()
	ctx := context.Background()
	task := uniqueID("task")

	if err := repo.Record(ctx, nil); err != nil {
		t.Errorf("Record of nothing is a no-op: %v", err)
	}
	events := []persistence.SecretRedactionEvent{
		{ProjectID: "p", TaskID: task, Checkpoint: "result_json", FindingType: "openai_key", Count: 2},
		{ProjectID: "p", TaskID: task, ExecutionID: uniqueID("exec"), Checkpoint: "tool_audit", FindingType: "openai_key", Count: 1, Source: "scan"},
		{ProjectID: "p", TaskID: task, Checkpoint: "artifacts", FindingType: "aws_key", Count: 4},
		{ProjectID: "p", Checkpoint: "webhook", FindingType: "aws_key", Count: 9}, // no task: must not count
	}
	if err := repo.Record(ctx, events); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for i, e := range events {
		if e.ID == "" || e.CreatedAt.IsZero() {
			t.Errorf("event %d: Record must fill ID and CreatedAt in place: %+v", i, e)
		}
	}
	if events[0].Source != "live" || events[1].Source != "scan" {
		t.Errorf("Source defaults to live and is kept when set: %q %q", events[0].Source, events[1].Source)
	}
	byType, total, err := repo.CountByTask(ctx, task)
	if err != nil {
		t.Fatalf("CountByTask: %v", err)
	}
	if total != 7 || byType["openai_key"] != 3 || byType["aws_key"] != 4 {
		t.Errorf("CountByTask = %v total %d, want openai_key 3, aws_key 4, total 7", byType, total)
	}
	none, total, err := repo.CountByTask(ctx, uniqueID("ghost"))
	if err != nil || none == nil || len(none) != 0 || total != 0 {
		t.Errorf("a task with no redactions: %v %d %v", none, total, err)
	}
	if m, n, err := repo.CountByTask(ctx, ""); err != nil || m == nil || n != 0 {
		t.Errorf("empty task id: %v %d %v", m, n, err)
	}
}

// ----------------------------------------------------------- task credential

// RunTaskCredentialSuite pins persistence.TaskCredentialRepository. Rows
// reference tasks on Postgres, so the task is seeded through its repository.
func RunTaskCredentialSuite(t *testing.T, repo persistence.TaskCredentialRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	task := newQueuedTask(uniqueID("proj"))
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	base := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	exec1, exec2 := uniqueID("exec"), uniqueID("exec")

	c1 := &persistence.TaskCredential{TaskID: task.ID, ExecutionID: exec1, Tool: "mcp__pagedrop__publish", Label: "viewing password", Value: "v1", ArtifactURL: "https://x/1", CreatedAt: base}
	if err := repo.Upsert(ctx, c1); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if c1.ID == "" {
		t.Error("Upsert must fill the ID")
	}
	// Same (task, execution, tool, artifact) again: the value is replaced in place.
	if err := repo.Upsert(ctx, &persistence.TaskCredential{TaskID: task.ID, ExecutionID: exec1, Tool: "mcp__pagedrop__publish", Label: "viewing password", Value: "v1-rotated", ArtifactURL: "https://x/1", CreatedAt: base.Add(time.Second)}); err != nil {
		t.Fatalf("Upsert conflict: %v", err)
	}
	got, err := repo.ListByTaskLatestExecution(ctx, task.ID)
	if err != nil || len(got) != 1 || got[0].Value != "v1-rotated" || got[0].Label != "viewing password" || got[0].Tool != "mcp__pagedrop__publish" {
		t.Fatalf("conflict must replace the value, not duplicate the row: %+v %v", got, err)
	}
	// A later execution supersedes: only its credentials are surfaced, in created_at order.
	for i, v := range []string{"b", "a"} {
		if err := repo.Upsert(ctx, &persistence.TaskCredential{TaskID: task.ID, ExecutionID: exec2, Tool: "t", Label: "l" + v, Value: v, ArtifactURL: "https://x/" + v, CreatedAt: base.Add(time.Duration(2-i) * time.Minute)}); err != nil {
			t.Fatalf("Upsert exec2 %s: %v", v, err)
		}
	}
	latest, err := repo.ListByTaskLatestExecution(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListByTaskLatestExecution: %v", err)
	}
	if len(latest) != 2 || latest[0].Value != "a" || latest[1].Value != "b" || latest[0].ExecutionID != exec2 {
		t.Errorf("only the latest execution's credentials, ordered by created_at: %+v", latest)
	}
	if none, err := repo.ListByTaskLatestExecution(ctx, uniqueID("ghost")); err != nil || len(none) != 0 {
		t.Errorf("unknown task: %v %v", none, err)
	}
	if none, err := repo.ListByTaskLatestExecution(ctx, ""); err != nil || len(none) != 0 {
		t.Errorf("empty task id: %v %v", none, err)
	}
}
