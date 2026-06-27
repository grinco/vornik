package ui

// /ui/admin/memory-audit — B-16. Two-tab operator surface listing
// recent rows from memory_retrieval_audit and memory_ingest_audit
// with filter axes for project / actor_kind / repo_scope / since.
// Pairs with B-15's retrieval-context stamping: every row now
// carries actor_kind, so dashboards can split companion / agent /
// rest_api / ui retrievals cleanly.

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminMemoryAuditData backs admin_memory_audit.html. Two slices —
// only the active tab's slice is populated to keep the page render
// bounded.
type AdminMemoryAuditData struct {
	adminCommonData
	// Tab picks the active panel ("retrieval" | "ingest"). Defaults
	// to retrieval when the operator lands on the page without a
	// ?tab= param.
	Tab string
	// AvailableRetrieval / AvailableIngest expose per-repo wiring
	// state so the template can render a per-tab "not wired" hint
	// without dimming the whole page.
	AvailableRetrieval bool
	AvailableIngest    bool
	// Filters echoed back to the form. Free-form strings so the
	// template can rehydrate the input values verbatim.
	FilterProject   string
	FilterActorKind string
	FilterRepoScope string
	FilterDecision  string // ingest tab only
	FilterSince     string
	Limit           int
	LimitOptions    []int
	// One slice populated per tab.
	Retrieval []*persistence.MemoryRetrievalAudit
	Ingest    []*persistence.MemoryIngestAudit
	// Error surfaces a per-query failure (e.g. PageSize=0 from a
	// malformed URL the operator pasted). The page still renders;
	// the table area shows the message inline.
	Error string
}

// AdminMemoryAudit renders /ui/admin/memory-audit. The page is
// strictly read-only — no POST surface, no mutating actions. The
// admin gate is enforced at the adminRouter dispatch step (same
// pattern every other admin page uses).
func (s *Server) AdminMemoryAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tab := q.Get("tab")
	if tab != "ingest" {
		tab = "retrieval"
	}
	limit := adminClampLimit(q.Get("limit"))
	data := AdminMemoryAuditData{
		adminCommonData: adminCommonData{
			Title:       "Memory Audit",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Tab:                tab,
		AvailableRetrieval: s.memoryRetrievalAudit != nil,
		AvailableIngest:    s.memoryIngestAudit != nil,
		FilterProject:      q.Get("project"),
		FilterActorKind:    q.Get("actor_kind"),
		FilterRepoScope:    q.Get("repo_scope"),
		FilterDecision:     q.Get("decision"),
		FilterSince:        q.Get("since"),
		Limit:              limit,
		LimitOptions:       adminLimitOptions,
	}

	// Parse `since` once and share between tabs. Accept both
	// RFC3339 (operator pastes from a log line) and YYYY-MM-DD
	// (operator picks a calendar day) since the chat-audit page
	// uses the same dual-format convention.
	var since time.Time
	if data.FilterSince != "" {
		if t, err := time.Parse(time.RFC3339, data.FilterSince); err == nil {
			since = t
		} else if t, err := time.Parse("2006-01-02", data.FilterSince); err == nil {
			since = t
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	switch tab {
	case "ingest":
		if s.memoryIngestAudit == nil {
			break
		}
		filter := persistence.MemoryIngestAuditFilter{
			ProjectID: data.FilterProject,
			ActorKind: data.FilterActorKind,
			RepoScope: data.FilterRepoScope,
			Decision:  data.FilterDecision,
			Since:     since,
			PageSize:  limit,
		}
		rows, err := s.memoryIngestAudit.List(ctx, filter)
		if err != nil {
			s.logger.Warn().Err(err).Msg("admin memory-audit ingest list failed")
			data.Error = err.Error()
		} else {
			data.Ingest = rows
		}
	default:
		if s.memoryRetrievalAudit == nil {
			break
		}
		filter := persistence.MemoryRetrievalAuditFilter{
			ProjectID: data.FilterProject,
			ActorKind: data.FilterActorKind,
			RepoScope: data.FilterRepoScope,
			Since:     since,
			PageSize:  limit,
		}
		rows, err := s.memoryRetrievalAudit.List(ctx, filter)
		if err != nil {
			s.logger.Warn().Err(err).Msg("admin memory-audit retrieval list failed")
			data.Error = err.Error()
		} else {
			data.Retrieval = rows
		}
	}

	s.render(w, "admin_memory_audit.html", data)
}
