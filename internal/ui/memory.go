package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// mathLog wraps math.Log so the log-scale node-radius math is
// trivially testable + a future swap to base-2 / approximation is
// one line.
func mathLog(x float64) float64 { return math.Log(x) }

// Memory is the /ui/memory section's landing page — one row per
// project with summary stats. From there operators drill into a
// per-project view with epochs, quarantine, rollback history.
//
// Phase 2-4 of memory hardening — see
// https://docs.vornik.io
func (s *Server) Memory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	if s.projectReg == nil {
		httpError(w, http.StatusInternalServerError, "project registry unavailable")
		return
	}

	type projectRow struct {
		ID                string
		Name              string
		QueueDepth        int
		QuarantinePending int
		ActiveEpochs      int
		TotalEpochs       int
		LastSnapshotAt    time.Time
	}
	// Page-size cap. Project rows are bounded in practice (single-
	// digit to low-double-digit deployments) but the selector ships
	// on every list view for chrome consistency — see page_size.go
	// for the shared validator. When the project count exceeds
	// Limit, the slice truncates after the rows are populated so
	// the trim is applied to the sorted output rather than to an
	// arbitrary registry iteration order.
	limit := parsePageSize(r.URL.Query().Get("limit"))

	rows := make([]projectRow, 0, len(s.projectReg.ListProjects()))
	for _, p := range s.projectReg.ListProjects() {
		if p == nil || !api.RequestAllowsProject(r, p.ID) {
			continue
		}
		row := projectRow{ID: p.ID, Name: p.DisplayName}
		if row.Name == "" {
			row.Name = p.ID
		}
		if s.ingestQueue != nil {
			if d, err := s.ingestQueue.QueueDepth(ctx, p.ID); err == nil {
				row.QueueDepth = d
			}
		}
		if s.memoryQuarantine != nil {
			// Use the unbounded by-gate map (live, filters
			// released_at IS NULL AND dropped_at IS NULL) and
			// sum it; len(ListPending(200)) caps at the page
			// size and silently under-reports projects whose
			// pending count crosses 200. This is the same
			// lifetime-vs-pending bug pattern that hit the
			// per-project page in 2026-05-27; #7 sweeps the
			// index page to keep the two surfaces in lockstep.
			if counts, err := s.memoryQuarantine.CountByGate(ctx, p.ID); err == nil {
				for _, n := range counts {
					row.QuarantinePending += n
				}
			}
		}
		if s.corpusEpochs != nil {
			eps, _ := s.corpusEpochs.ListEpochs(ctx, p.ID, 200)
			row.TotalEpochs = len(eps)
			for _, e := range eps {
				if e.IsActive {
					row.ActiveEpochs++
				}
				if e.ClosedAt != nil && e.ClosedAt.After(row.LastSnapshotAt) {
					row.LastSnapshotAt = *e.ClosedAt
				}
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	// In-memory LIMIT trim. project_registry doesn't expose a
	// paginated query (the registry is loaded once on daemon start),
	// so the cap applies to the sorted slice rather than to a SQL
	// query. Deployments with more projects than Limit will need to
	// page; today that's a clean follow-on rather than urgent (see
	// the PR description).
	totalProjects := len(rows)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	// Knowledge-graph extraction progress widget. Global pipeline
	// (one worker drains all projects), so this lives on the
	// memory landing page rather than per-project. Nil-safe — when
	// the chunkGraph repo isn't wired the template hides the panel.
	type kgProgress struct {
		Enabled        bool
		ChunksPending  int
		ChunksDone     int
		ChunksTotal    int
		PercentDone    float64
		Entities       int
		Edges          int
		Mentions       int
		EntitiesByType []kgTypeRow
	}
	// The KG pipeline stats are INSTANCE-WIDE (Stats has no project
	// filter — one worker drains all projects). A project-scoped session
	// must not see cross-project entity/edge/chunk totals, so the widget
	// is gated to all-access (admin / auth-off) callers. Scoped users get
	// kg.Enabled=false and the panel is hidden.
	kg := kgProgress{}
	if s.chunkGraph != nil && requestHasAllProjectAccess(r) {
		if stats, err := s.chunkGraph.Stats(ctx); err == nil && stats != nil {
			kg.Enabled = true
			kg.ChunksPending = stats.ChunksPending
			kg.ChunksDone = stats.ChunksDone
			kg.ChunksTotal = stats.ChunksPending + stats.ChunksDone
			if kg.ChunksTotal > 0 {
				kg.PercentDone = 100.0 * float64(stats.ChunksDone) / float64(kg.ChunksTotal)
			}
			kg.Entities = stats.Entities
			kg.Edges = stats.Edges
			kg.Mentions = stats.Mentions
			for t, n := range stats.EntitiesByType {
				kg.EntitiesByType = append(kg.EntitiesByType, kgTypeRow{Type: t, Count: n})
			}
			sort.Slice(kg.EntitiesByType, func(i, j int) bool {
				return kg.EntitiesByType[i].Count > kg.EntitiesByType[j].Count
			})
		}
	}

	data := struct {
		Title         string
		CurrentPage   string
		IsAdmin       bool
		Projects      []projectRow
		Enabled       bool
		KG            kgProgress
		Limit         int
		LimitOptions  []int
		TotalProjects int
	}{
		Title:         "Memory — Vornik",
		CurrentPage:   "memory",
		IsAdmin:       requestHasAllProjectAccess(r),
		Projects:      rows,
		Enabled:       s.memoryQuarantine != nil && s.corpusEpochs != nil,
		KG:            kg,
		Limit:         limit,
		LimitOptions:  PageSizeOptions,
		TotalProjects: totalProjects,
	}
	s.render(w, "memory_index.html", data)
}

type kgTypeRow struct {
	Type  string
	Count int
}

// MemoryProject renders the per-project detail view: epoch list,
// quarantine table, recent rollbacks, queue depth.
func (s *Server) MemoryProject(w http.ResponseWriter, r *http.Request, projectID string) {
	if projectID == "" {
		httpError(w, http.StatusBadRequest, "project id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	type epochRow struct {
		ID                  string
		CreatedAt           time.Time
		ClosedAt            *time.Time
		ChunksAdmitted      int
		ChunksQuarantined   int
		ChunksVerified      int
		ChunksSuperseded    int
		IsActive            bool
		IsTargetForRollback bool // true when older than the most recent active epoch
	}
	type quarantineRow struct {
		ID               string
		SourceArtifactID string
		FailedGate       string
		FailureDetail    string
		ProducerRole     string
		ProposedClass    string
		Preview          string
		QuarantinedAt    time.Time
	}
	type rollbackRow struct {
		ID          string
		FromEpochID string
		ToEpochID   string
		TriggeredBy string
		Reason      string
		AppliedAt   time.Time
		// ChunksRestored — supersession-revert count (migration 89).
		ChunksRestored int
	}

	data := struct {
		Title       string
		CurrentPage string
		ProjectID   string
		ProjectName string
		Enabled     bool

		// Tab is the active panel group: "health" | "search" | "operate".
		// Resolved by resolveMemoryProjectTab after QueueDepth and
		// QuarantinePending are populated so the default is "operate"
		// when something needs attention and "health" otherwise. Drives
		// the per-group {{if eq .Tab "X"}} renders in
		// memory_project.html; the surrounding tab strip stays
		// regardless of which panels render.
		Tab string

		QueueDepth        int
		QuarantineByGate  map[string]int
		QuarantinePending []quarantineRow

		Epochs    []epochRow
		Rollbacks []rollbackRow

		// Per-table page-size limits feed the pageSizeSelector
		// partial at the head of each panel. Each query param is
		// independent so a deep dive into quarantine (?quarantine_limit=100)
		// doesn't drag epochs along. LimitOptions is the shared
		// dropdown set; see internal/ui/page_size.go.
		QuarantineLimit int
		EpochsLimit     int
		RollbacksLimit  int
		EvictionLimit   int
		LimitOptions    []int

		// Visualizations
		Pipeline      pipelineFunnel
		Timeline      []epochBar
		Scatter       scatterData
		FlowGates     []pipelineGateNode
		HasDryRun     bool
		HasSearch     bool
		ProducerRoles []string

		// RepoScopes backs the B-6 scope picker dropdown next to
		// the search box. Empty Scope = uncategorized chunks
		// (repo_scope IS NULL — kept visible during migration).
		// Empty slice = no chunks indexed yet or scope-listing
		// not wired; template renders the dropdown as a no-op.
		RepoScopes []MemoryRepoScope

		// Eviction surface (P0 batch 1 — GDPR Art 17 hook). When
		// EvictEnabled is true the template renders the operator-
		// erase form + the audit-log table. The table itself is
		// loaded lazily here only when the source is wired; without
		// it the panel renders an "evict not enabled" placeholder.
		EvictEnabled   bool
		EvictionAudits []MemoryEvictionAuditRow
	}{
		Title:           "Memory — " + projectID,
		CurrentPage:     "memory",
		ProjectID:       projectID,
		Enabled:         s.memoryQuarantine != nil && s.corpusEpochs != nil,
		QuarantineLimit: parsePageSize(r.URL.Query().Get("quarantine_limit")),
		// parseEpochsLimit (not parsePageSize) accepts 1–500 so operators
		// with >100 snapshots can reach all of them in the rollback picker.
		// parsePageSize's PageSizeOptions max is 100 — the cap this fixes.
		EpochsLimit:    parseEpochsLimit(r.URL.Query().Get("epochs_limit")),
		RollbacksLimit: parsePageSize(r.URL.Query().Get("rollbacks_limit")),
		EvictionLimit:  parsePageSize(r.URL.Query().Get("eviction_limit")),
		LimitOptions:   PageSizeOptions,
	}

	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil {
			data.ProjectName = p.DisplayName
		}
	}
	if data.ProjectName == "" {
		data.ProjectName = projectID
	}

	if s.ingestQueue != nil {
		if d, err := s.ingestQueue.QueueDepth(ctx, projectID); err == nil {
			data.QueueDepth = d
		}
	}

	if s.memoryQuarantine != nil {
		if counts, err := s.memoryQuarantine.CountByGate(ctx, projectID); err == nil {
			data.QuarantineByGate = counts
		}
		if items, err := s.memoryQuarantine.ListPending(ctx, projectID, data.QuarantineLimit); err == nil {
			data.QuarantinePending = make([]quarantineRow, 0, len(items))
			for _, it := range items {
				preview := it.Content
				if len(preview) > 240 {
					preview = preview[:237] + "..."
				}
				row := quarantineRow{
					ID:               it.ID,
					SourceArtifactID: it.SourceArtifactID,
					FailedGate:       it.FailedGate,
					Preview:          preview,
					QuarantinedAt:    it.QuarantinedAt,
				}
				if it.ProducerRole != nil {
					row.ProducerRole = *it.ProducerRole
				}
				if it.ProposedClass != nil {
					row.ProposedClass = *it.ProposedClass
				}
				if it.FailureDetail != nil {
					row.FailureDetail = *it.FailureDetail
				}
				data.QuarantinePending = append(data.QuarantinePending, row)
			}
		}
	}

	if s.memoryEvictor != nil {
		data.EvictEnabled = true
		// Best-effort fetch of recent tombstones. Failures fall
		// back to an empty table rather than blocking the whole
		// page render — the operator can still POST a fresh
		// eviction.
		if audits, err := s.memoryEvictor.ListEvictionAudits(ctx, projectID, data.EvictionLimit); err == nil {
			data.EvictionAudits = audits
		}
	}

	if s.corpusEpochs != nil {
		eps, _ := s.corpusEpochs.ListEpochs(ctx, projectID, data.EpochsLimit)
		// The "most recent active epoch" is the rollback baseline:
		// targeting it is a no-op, targeting older epochs deactivates
		// newer ones. UI flags rollback-eligible rows so the action
		// button only appears where it'd do something.
		var mostRecentActive time.Time
		for _, e := range eps {
			if e.IsActive && e.CreatedAt.After(mostRecentActive) {
				mostRecentActive = e.CreatedAt
			}
		}
		data.Epochs = make([]epochRow, 0, len(eps))
		for _, e := range eps {
			data.Epochs = append(data.Epochs, epochRow{
				ID:                  e.ID,
				CreatedAt:           e.CreatedAt,
				ClosedAt:            e.ClosedAt,
				ChunksAdmitted:      e.ChunksAdmitted,
				ChunksQuarantined:   e.ChunksQuarantined,
				ChunksVerified:      e.ChunksVerified,
				ChunksSuperseded:    e.ChunksSuperseded,
				IsActive:            e.IsActive,
				IsTargetForRollback: !mostRecentActive.IsZero() && e.CreatedAt.Before(mostRecentActive),
			})
		}
		rbs, _ := s.corpusEpochs.ListRollbacks(ctx, projectID, data.RollbacksLimit)
		data.Rollbacks = make([]rollbackRow, 0, len(rbs))
		for _, rb := range rbs {
			row := rollbackRow{
				ID:             rb.ID,
				TriggeredBy:    rb.TriggeredBy,
				AppliedAt:      rb.AppliedAt,
				ChunksRestored: rb.ChunksRestored,
			}
			if rb.FromEpochID != nil {
				row.FromEpochID = *rb.FromEpochID
			}
			if rb.ToEpochID != nil {
				row.ToEpochID = *rb.ToEpochID
			}
			if rb.Reason != nil {
				row.Reason = *rb.Reason
			}
			data.Rollbacks = append(data.Rollbacks, row)
		}
	}

	// === Pipeline flow diagram ===
	// Static node list (canonical pipeline) annotated with
	// per-gate trip counts from the quarantine table. The template
	// renders an SVG with nodes + edges; the inspector form
	// posts to /inspect and the result-trail JS lights up matching
	// nodes by gate name.
	data.FlowGates = canonicalPipelineGates(data.QuarantineByGate)
	// Producer roles offered in the inspector form. The set
	// matches the deterministic role→class table (see
	// memory/class.go); unknown roles fall through to
	// "unclassified" so they're still useful as a worst-case
	// preview.
	data.ProducerRoles = []string{
		"researcher", "scout", "analyst", "writer",
		"coder", "reviewer", "architect", "tester",
		"unknown",
	}
	data.HasDryRun = s.pipelineDryRun != nil
	data.HasSearch = s.memorySearcher != nil

	// B-6: scope inventory for the search-box dropdown. Best-effort
	// — a search panel without scope filtering still renders fine
	// (the dropdown just shows "All scopes" and nothing else).
	if s.memorySearcher != nil {
		if scopes, err := s.memorySearcher.ListRepoScopes(ctx, projectID); err == nil {
			data.RepoScopes = scopes
		} else {
			s.logger.Warn().Err(err).Str("project_id", projectID).Msg("ui: scope inventory list failed; dropdown will render empty")
		}
	}

	// === Pipeline funnel ===
	// Derived from epochs (admit/quarantine/verify/supersede sums)
	// + queue depth + a coarse "total enqueued" proxy. The funnel
	// is a relative view, not exact accounting — the bars convey
	// "where in the pipeline does volume drop off".
	for _, e := range data.Epochs {
		data.Pipeline.Admitted += e.ChunksAdmitted
		data.Pipeline.Quarantined += e.ChunksQuarantined
		data.Pipeline.Verified += e.ChunksVerified
		data.Pipeline.Superseded += e.ChunksSuperseded
	}
	data.Pipeline.Pending = data.QueueDepth
	// QuarantinePending is the LIVE pending-row count summed from
	// CountByGate (which filters released_at IS NULL AND
	// dropped_at IS NULL). Pipeline.Quarantined is a frozen epoch
	// snapshot that never decrements; reading the live count here
	// keeps any alert/header widget in lockstep with the
	// quarantine list — drops/releases/rollbacks reflect
	// immediately. Using the by-gate map (not len(QuarantinePending))
	// because the latter is page-size-bounded by QuarantineLimit and
	// would under-report when more than a page is pending.
	for _, n := range data.QuarantineByGate {
		data.Pipeline.QuarantinePending += n
	}
	data.Pipeline.Enqueued = data.Pipeline.Admitted + data.Pipeline.Quarantined + data.Pipeline.Pending

	// === Epoch timeline ===
	// Stacked bars, one per epoch, oldest left → newest right.
	// Heights in SVG units (px); the bar SVG has a fixed 60px
	// content area so heights are pct of max.
	{
		const barH = 60
		maxAdmit := 1
		for _, e := range data.Epochs {
			t := e.ChunksAdmitted + e.ChunksQuarantined + e.ChunksSuperseded
			if t > maxAdmit {
				maxAdmit = t
			}
		}
		// Epochs come out newest-first; reverse for oldest→newest.
		ordered := make([]epochRow, len(data.Epochs))
		for i, e := range data.Epochs {
			ordered[len(data.Epochs)-1-i] = e
		}
		const barW = 14
		const gap = 4
		data.Timeline = make([]epochBar, len(ordered))
		for i, e := range ordered {
			bar := epochBar{
				ID:        e.ID,
				CreatedAt: e.CreatedAt,
				IsActive:  e.IsActive,
				Total:     e.ChunksAdmitted + e.ChunksQuarantined + e.ChunksSuperseded,
				XOffset:   i * (barW + gap),
			}
			scale := func(n int) int {
				if maxAdmit == 0 {
					return 0
				}
				h := (n * barH) / maxAdmit
				if h < 0 {
					h = 0
				}
				return h
			}
			bar.AdmittedHeight = scale(e.ChunksAdmitted)
			bar.QuarantinedHeight = scale(e.ChunksQuarantined)
			bar.VerifiedHeight = scale(e.ChunksVerified)
			bar.SupersededHeight = scale(e.ChunksSuperseded)
			data.Timeline[i] = bar
		}
	}

	// === Vector scatter ===
	// PCA-projected sample of up to 500 chunks with embeddings.
	if s.vectorViz != nil && s.corpusEpochs != nil {
		activeEpochs, _ := s.corpusEpochs.ListActive(ctx, projectID)
		points, vErr := s.vectorViz.SampleProjection(ctx, projectID, activeEpochs, 500)
		if vErr == nil && len(points) >= 2 {
			data.Scatter.HasEmbeddings = true
			data.Scatter.ClassLegend = make(map[string]int)
			data.Scatter.Points = make([]scatterPoint, len(points))

			// Find min/max content size so the per-node radius
			// log-scale spans the full corpus range. Single
			// linear pass; cheaper than computing on the JS side
			// where it'd need to walk all 500 elements per
			// rotation frame.
			minSize, maxSize := 0, 0
			for i, p := range points {
				if i == 0 || p.ContentSize < minSize {
					minSize = p.ContentSize
				}
				if p.ContentSize > maxSize {
					maxSize = p.ContentSize
				}
			}
			if minSize < 1 {
				minSize = 1
			}
			if maxSize <= minSize {
				maxSize = minSize + 1
			}
			logMin := mathLog(float64(minSize))
			logMax := mathLog(float64(maxSize))

			for i, p := range points {
				preview := p.Preview
				if len(preview) > 120 {
					preview = preview[:117] + "..."
				}
				tip := fmt.Sprintf("%s · %s · %s · %d bytes\n%s",
					p.SourceName, p.ContentClass, p.ValidationStatus, p.ContentSize, preview)
				type edge struct {
					ID  string  `json:"id"`
					Sim float32 `json:"sim"`
				}
				edges := make([]edge, 0, len(p.Neighbors))
				for _, n := range p.Neighbors {
					edges = append(edges, edge{ID: n.ChunkID, Sim: n.Similarity})
				}
				edgesBytes, _ := json.Marshal(edges)

				// Log-scale content size → radius range
				// [minR, maxR] = [2.0, 7.0] px.
				size := p.ContentSize
				if size < 1 {
					size = 1
				}
				const minR, maxR = 2.0, 7.0
				t := (mathLog(float64(size)) - logMin) / (logMax - logMin)
				if t < 0 {
					t = 0
				}
				if t > 1 {
					t = 1
				}
				radius := float32(minR + t*(maxR-minR))

				data.Scatter.Points[i] = scatterPoint{
					X:             p.X,
					Y:             p.Y,
					Z:             p.Z,
					R:             radius,
					ChunkID:       p.ChunkID,
					SourceName:    p.SourceName,
					ContentClass:  p.ContentClass,
					Status:        p.ValidationStatus,
					ProducerRole:  p.ProducerRole,
					Preview:       preview,
					Tooltip:       tip,
					NeighborsJSON: string(edgesBytes),
				}
				data.Scatter.ClassLegend[p.ContentClass]++
			}
		}
	}

	// Use the unbounded live count (data.Pipeline.QuarantinePending)
	// not len(data.QuarantinePending). The slice is page-size-bounded
	// by QuarantineLimit so for a project with > QuarantineLimit
	// pending rows the slice-length under-reports and the default
	// tab logic would mis-route a busy project to "health" instead
	// of "operate". Same lifetime-vs-pending fix family as the
	// alert widget below; #7 keeps both surfaces consistent.
	data.Tab = resolveMemoryProjectTab(r.URL.Query().Get("tab"), data.QueueDepth, data.Pipeline.QuarantinePending)

	s.render(w, "memory_project.html", data)
}

// resolveMemoryProjectTab picks the active tab for /ui/memory/<id>.
// Explicit ?tab= wins when it names a known panel group; otherwise
// the default is "operate" when there's something queued or
// quarantined that needs attention, falling back to "health" on a
// quiet project. Lifted out as a free function so the rule is
// unit-testable without re-running the whole handler.
func resolveMemoryProjectTab(requested string, queueDepth, quarantinePending int) string {
	switch requested {
	case "health", "search", "operate":
		return requested
	}
	if quarantinePending > 0 || queueDepth > 0 {
		return "operate"
	}
	return "health"
}

// MemoryRollbackAction handles the rollback POST from the UI form.
// Body shape: ?project=<id>&to=<epoch>&reason=<text>. Apply is
// implicit in the POST — the UI never offers a dry-run since the
// preview is already on the page.
func (s *Server) MemoryRollbackAction(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.corpusEpochs == nil {
		httpError(w, http.StatusServiceUnavailable, "memory hardening not enabled")
		return
	}
	if err := r.ParseForm(); err != nil {
		httpError(w, http.StatusBadRequest, "bad form")
		return
	}
	target := r.FormValue("to")
	reason := r.FormValue("reason")
	if target == "" {
		httpError(w, http.StatusBadRequest, "rollback target required")
		return
	}
	by := "ui"
	if u := r.Header.Get("X-User"); u != "" {
		by = "ui:" + u
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, _, _, err := s.corpusEpochs.RollbackTo(ctx, projectID, target, by, reason); err != nil {
		httpError(w, http.StatusInternalServerError, "rollback failed: "+err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/memory/%s", projectID), http.StatusSeeOther)
}

// MemoryQuarantineAction handles drop POSTs from the UI.
// Form: id=<quarantine_id>&action=drop|release.
func (s *Server) MemoryQuarantineAction(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.memoryQuarantine == nil {
		httpError(w, http.StatusServiceUnavailable, "memory hardening not enabled")
		return
	}
	if err := r.ParseForm(); err != nil {
		httpError(w, http.StatusBadRequest, "bad form")
		return
	}
	id := r.FormValue("id")
	action := r.FormValue("action")
	if id == "" || action == "" {
		httpError(w, http.StatusBadRequest, "id and action required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Defense-in-depth project check.
	item, err := s.memoryQuarantine.Get(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, "quarantine row not found")
		return
	}
	if item.ProjectID != projectID {
		httpError(w, http.StatusForbidden, "quarantine row belongs to another project")
		return
	}
	switch strings.ToLower(action) {
	case "drop":
		if err := s.memoryQuarantine.MarkDropped(ctx, id); err != nil {
			httpError(w, http.StatusInternalServerError, "drop failed: "+err.Error())
			return
		}
	case "release":
		// Release marks the quarantine row as reviewed-and-cleared
		// without re-promoting the chunk. The MarkReleased
		// signature accepts a released_chunk_id which we leave
		// empty — re-insertion is a separate operator action via
		// the corrector's InsertCorrection path (the quarantine
		// row itself doesn't carry the content, only a reference
		// to the source artifact, so a UI-triggered re-insert
		// would have to re-fetch + re-chunk). What this action
		// DOES do: dismisses the item from the pending review
		// list so the operator's queue stays clean.
		if err := s.memoryQuarantine.MarkReleased(ctx, id, ""); err != nil {
			httpError(w, http.StatusInternalServerError, "release failed: "+err.Error())
			return
		}
	default:
		httpError(w, http.StatusBadRequest, "action must be drop or release")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/memory/%s", projectID), http.StatusSeeOther)
}

// MemoryEvictAction handles operator-triggered hard eviction.
// Form fields:
//   - chunks: comma- or newline-separated chunk IDs
//   - reason: free-text justification recorded on every audit row
//   - confirm: must equal "yes" to proceed (refuses otherwise)
//
// The handler does not search — operators paste IDs they've
// already identified via the search panel or via vornikctl. That
// keeps the surface deliberately small: it's a destructive,
// audited action, not a fuzzy "find and delete."
//
// On success the page redirects back to /ui/memory/<project>
// where the audit-log table refreshes with the new entries.
func (s *Server) MemoryEvictAction(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.memoryEvictor == nil {
		httpError(w, http.StatusServiceUnavailable, "hard-eviction not enabled (no MemoryEvictor wired)")
		return
	}
	if err := r.ParseForm(); err != nil {
		httpError(w, http.StatusBadRequest, "bad form")
		return
	}
	if r.FormValue("confirm") != "yes" {
		httpError(w, http.StatusBadRequest, "eviction is destructive — tick the confirm box and re-submit")
		return
	}
	chunkIDs := parseEvictChunkIDs(r.FormValue("chunks"))
	if len(chunkIDs) == 0 {
		httpError(w, http.StatusBadRequest, "at least one chunk ID required")
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// operator identity stamped into the audit row. The UI today
	// authenticates via static API key or Telegram user ID; the
	// session middleware doesn't expose either as a "user name"
	// yet. evictedBy="ui-operator" is a placeholder hook —
	// upgrade once the auth middleware adds an operator-name
	// surface (regulatory roadmap §GDPR Art 30 ROPA item).
	_, err := s.memoryEvictor.HardEvict(ctx, projectID, chunkIDs, reason, "ui-operator")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "evict failed: "+err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/memory/%s", projectID), http.StatusSeeOther)
}

// parseEvictChunkIDs tokenises the chunks textarea. Operators paste
// IDs from clipboards / search results — the parser accepts comma
// AND newline separators and trims surrounding whitespace from each
// entry. Empty / pure-whitespace lines drop silently.
func parseEvictChunkIDs(raw string) []string {
	// Normalise newlines into commas before splitting so a single
	// loop handles both.
	normalised := strings.ReplaceAll(raw, "\n", ",")
	parts := strings.Split(normalised, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// memoryRouter dispatches /ui/memory/* requests:
//
//	GET  /ui/memory/                         → Memory (index)
//	GET  /ui/memory/<project>                → MemoryProject
//	POST /ui/memory/<project>/rollback       → MemoryRollbackAction
//	POST /ui/memory/<project>/quarantine     → MemoryQuarantineAction (action=drop|release)
//	POST /ui/memory/<project>/inspect        → MemoryInspectAction (dry-run)
//	POST /ui/memory/<project>/evict          → MemoryEvictAction (GDPR hard delete)
//	GET  /ui/memory/<project>/search?q=...   → MemorySearchAction
func (s *Server) memoryRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/memory/")
	if rest == "" || rest == "/" {
		s.Memory(w, r)
		return
	}
	// Operator-profile surface — /memory/operators[/<id>].
	// Matched before the per-project fall-through so the literal
	// project name "operators" can't squat the route (the project
	// registry rejects "operators" as a project id).
	if rest == "operators" || rest == "operators/" {
		s.MemoryOperators(w, r)
		return
	}
	if opID := strings.TrimPrefix(rest, "operators/"); opID != rest && opID != "" {
		s.MemoryOperator(w, r, opID)
		return
	}
	parts := strings.SplitN(strings.Trim(rest, "/"), "/", 3)
	projectID := parts[0]
	// Project-scope choke point. Every per-project memory sub-route
	// (detail, search, inspect, evict, rollback, quarantine, and the
	// knowledge-graph entities/subgraph routes) dispatches from here, so
	// gating once means current and future sub-routes all inherit the
	// guard. Without it a scoped browser user could read another
	// tenant's memory by guessing the project id — the LIST page
	// (memory.go, ~line 59) was filtered but the per-project surfaces
	// were not. NotFound (not 403) so wrong-scope is indistinguishable
	// from not-found, matching the sibling per-project detail surfaces
	// (ProjectDocuments, ProjectArtifacts). No-op when auth is off or
	// the key is unscoped (RequestAllowsProject returns true).
	if !api.RequestAllowsProject(r, projectID) {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		s.MemoryProject(w, r, projectID)
		return
	}
	switch parts[1] {
	case "rollback":
		s.MemoryRollbackAction(w, r, projectID)
	case "quarantine":
		s.MemoryQuarantineAction(w, r, projectID)
	case "inspect":
		s.MemoryInspectAction(w, r, projectID)
	case "evict":
		s.MemoryEvictAction(w, r, projectID)
	case "search":
		s.MemorySearchAction(w, r, projectID)
	case "entities":
		// /entities (browser) or /entities/<entityID> (detail).
		// see https://docs.vornik.io §7.1
		if len(parts) == 3 && parts[2] != "" {
			s.MemoryEntityDetail(w, r, projectID, parts[2])
			return
		}
		s.MemoryEntities(w, r, projectID)
	case "subgraph":
		// /subgraph/<entityID> — server-rendered SVG neighbourhood.
		// see https://docs.vornik.io §7.3
		if len(parts) == 3 && parts[2] != "" {
			s.MemorySubgraph(w, r, projectID, parts[2])
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// MemorySearchAction handles GET /ui/memory/<project>/search?q=<query>&limit=<n>.
// Returns the top hybrid-search hits as JSON for the search panel.
// The UI uses chunk_id to cross-reference results with the scatter
// plot, so an operator can click a hit and see it highlighted along
// with its nearest-neighbor edges.
func (s *Server) MemorySearchAction(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.memorySearcher == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
			"error": "memory searcher not configured",
		})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "query parameter 'q' is required"})
		return
	}
	if len(q) > 512 {
		q = q[:512]
	}
	limit := 20
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}
	// B-6: optional repo_scope filter. Empty = project-wide search
	// (legacy behaviour). The cap mirrors the q-length cap so a
	// pathological scope value can't blow the query.
	scope := strings.TrimSpace(r.URL.Query().Get("repo_scope"))
	if len(scope) > 512 {
		scope = scope[:512]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// B-15: stamp retrieval context so the audit row carries who did
	// the search. Without this every /ui/memory recall produced an
	// audit row with NULL actor_kind / actor_id — the new scope picker
	// (B-6) would otherwise muddy the dashboards that split companion
	// vs operator-direct retrievals. ActorID empty for static-keys-only
	// is fine — actor_kind="ui" still distinguishes the surface.
	ctx = memory.WithRetrievalContext(ctx, &memory.RetrievalContext{
		ActorKind: "ui",
		ActorID:   api.APIKeyIDFromContext(r.Context()),
	})

	results, err := s.memorySearcher.SearchWithScope(ctx, projectID, q, limit, scope)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
			"error": "search failed: " + err.Error(),
		})
		return
	}
	// Cap content snippet so the JSON stays under a few hundred KB
	// even when the searcher returns long chunks. The panel only
	// needs enough to recognise the hit.
	const snippetMax = 800
	out := make([]MemorySearchResult, len(results))
	for i, r := range results {
		content := r.Content
		if len(content) > snippetMax {
			content = content[:snippetMax] + "…"
		}
		out[i] = MemorySearchResult{
			ChunkID:    r.ChunkID,
			ProjectID:  r.ProjectID,
			TaskID:     r.TaskID,
			SourceName: r.SourceName,
			Content:    content,
			Score:      r.Score,
			RepoScope:  r.RepoScope,
		}
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{
		"query":   q,
		"limit":   limit,
		"results": out,
		"count":   len(out),
	})
}

// MemoryInspectAction handles POST /ui/memory/<project>/inspect.
// Form fields:
//
//	content        — the candidate body to dry-run
//	producer_role  — role-of-record context for class assignment
//	source_name    — optional artifact name (defaults to "inspect.md")
//
// Returns JSON. The inline JS on the project page parses this and
// renders the gate trail + final-action block.
func (s *Server) MemoryInspectAction(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.pipelineDryRun == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
			"error": "pipeline dry-runner not configured",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad form: " + err.Error()})
		return
	}
	content := r.FormValue("content")
	role := strings.TrimSpace(r.FormValue("producer_role"))
	sourceName := strings.TrimSpace(r.FormValue("source_name"))
	executionID := strings.TrimSpace(r.FormValue("execution_id"))
	if sourceName == "" {
		sourceName = "inspect.md"
	}
	// Cap inspect content at 64KB so the form can't be turned into
	// a memory bomb. Real chunks are typically far smaller; this is
	// a comfortable upper bound for "paste a research note".
	const maxBytes = 64 * 1024
	if len(content) > maxBytes {
		content = content[:maxBytes]
	}
	var res DryRunResult
	if executionID != "" {
		res = s.pipelineDryRun.DryRunWithExecution(projectID, sourceName, role, executionID, content)
	} else {
		res = s.pipelineDryRun.DryRun(projectID, sourceName, role, content)
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{
		"input": map[string]any{
			"sourceName":   sourceName,
			"producerRole": role,
			"executionId":  executionID,
			"contentBytes": len(content),
		},
		"final":                res.Final,
		"trail":                res.Trail,
		"class":                res.Class,
		"ttlDays":              res.TTLDays,
		"defaultConfidence":    res.DefaultConfidence,
		"roleOfRecordEligible": res.RoleOfRecordEligible,
		"postRedactContent":    res.PostRedactContent,
		"claims":               res.Claims,
	})
}

// pipelineGateNode is one node in the pipeline flow diagram.
// Pre-positioned by the handler so the template just plots
// rectangles and connecting lines.
type pipelineGateNode struct {
	Name        string // gate name (matches GateName in memory pkg)
	Stage       string // logical stage: receive | chunk | classify | publish | snapshot
	X, Y        int    // SVG coords of node centre
	Trips       int    // operator-visible: how many times this gate has fired non-allow
	Description string // one-liner shown in tooltip
}

// canonicalPipelineGates returns the static list of gate nodes for
// the flow diagram, with positions pre-computed for a 900×220 SVG
// viewBox. Trip counts are filled in from the quarantine table.
func canonicalPipelineGates(quarantineByGate map[string]int) []pipelineGateNode {
	const yReceive, yChunk, yClassify, yPublish = 60, 60, 60, 60
	nodes := []pipelineGateNode{
		// Receive stage
		{Name: "schema_match", Stage: "receive", X: 60, Y: yReceive,
			Description: "Required fields present (project_id, source_artifact_id, producer_role, content)"},
		{Name: "provenance_complete", Stage: "receive", X: 160, Y: yReceive,
			Description: "Source artifact id + producer role present (refuse not quarantine)"},
		// Chunk stage
		{Name: "secret_scan", Stage: "chunk", X: 280, Y: yChunk,
			Description: "Secret detector — redact (default) or quarantine on policy=block"},
		{Name: "policy_match", Stage: "chunk", X: 380, Y: yChunk,
			Description: "Project deny patterns — substring match → quarantine"},
		{Name: "min_content", Stage: "chunk", X: 480, Y: yChunk,
			Description: "<64 chars rejects, <10 words quarantines"},
		// Classify stage
		{Name: "class_known", Stage: "classify", X: 580, Y: yClassify,
			Description: "Producer role → content_class lookup; unknown classes downgrade to unclassified with warn-log"},
		// Publish stage
		{Name: "dedup_hash", Stage: "publish", X: 700, Y: yPublish,
			Description: "(project_id, content_hash) unique — exact dups silently rejected"},
		{Name: "near_dup_supersede", Stage: "publish", X: 800, Y: yPublish,
			Description: "Same source_name + content_class → older chunk marked superseded"},
	}
	for i := range nodes {
		nodes[i].Trips = quarantineByGate[nodes[i].Name]
	}
	return nodes
}

// pipelineFunnel summarises the funnel from queue → published over
// recent epochs. The bars in the SVG are scaled to the largest
// stage so the funnel reads at a glance regardless of absolute
// volume.
type pipelineFunnel struct {
	Enqueued    int // total ingest_queue rows ever (queued + processing + done + failed)
	Pending     int // currently queued or processing
	Admitted    int // sum across recent epochs
	Quarantined int // sum across recent epochs (LIFETIME — for funnel only; doesn't decrement on drop/release/rollback)
	Verified    int // sum across recent epochs
	Superseded  int // sum across recent epochs
	// QuarantinePending is the LIVE count of rows in
	// project_memory_quarantine where released_at IS NULL AND
	// dropped_at IS NULL. Use this — not Quarantined — for any
	// "needs action / triage" affordance (alert colour, header
	// counter). Quarantined is a cumulative epoch snapshot that
	// never decrements, so it diverges from the list view after the
	// first drop/release/rollback.
	QuarantinePending int
}

// AdmittedPct / QuarantinedPct etc help the template do bar-width
// math without inline arithmetic.
func (f pipelineFunnel) BarMax() int {
	m := f.Enqueued
	if f.Admitted > m {
		m = f.Admitted
	}
	if f.Quarantined > m {
		m = f.Quarantined
	}
	if m == 0 {
		return 1
	}
	return m
}

// pctOf is a template helper — Pct(n, max) → 0..100 int.
func pctOf(n, max int) int {
	if max <= 0 {
		return 0
	}
	v := (n * 100) / max
	if v > 100 {
		return 100
	}
	if v < 0 {
		return 0
	}
	return v
}

// epochBar is one bar in the timeline. Heights are pre-computed in
// SVG units so the template doesn't do arithmetic.
type epochBar struct {
	ID                string
	CreatedAt         time.Time
	IsActive          bool
	Total             int
	AdmittedHeight    int
	QuarantinedHeight int
	VerifiedHeight    int
	SupersededHeight  int
	// X position in the timeline SVG (pixels from left edge).
	XOffset int
}

// scatterData holds the PCA-projected points for the SVG viz.
type scatterData struct {
	Points        []scatterPoint
	ClassLegend   map[string]int // class → count for the legend
	HasEmbeddings bool
}

// scatterPoint mirrors VizPoint as raw 3D coords + log-scaled
// radius. Inline JS computes screen positions on each
// rotation/zoom frame; the server doesn't pre-bake cx/cy because
// any pixel position would be invalidated by the first user
// gesture.
type scatterPoint struct {
	// Raw projection coords in [-1, 1] per axis, ready to feed
	// into the rotation matrix on the client.
	X, Y, Z float32
	// R is the per-node radius in SVG units, log-scaled from
	// content_size so a 200-byte note vs a 5000-byte dossier
	// reads at a glance. Range: ~2..7 px.
	R            float32
	ChunkID      string
	SourceName   string
	ContentClass string
	Status       string
	ProducerRole string
	Preview      string
	Tooltip      string
	// NeighborsJSON is a pre-encoded JSON array of
	// [{id, sim}] used by the inline JS to draw edges +
	// populate the related-chunks list. Pre-encoding here
	// avoids escaping headaches in the template.
	NeighborsJSON string
}

// classColour maps each content class to a stable hex colour so the
// legend ↔ scatter pairing is consistent across page loads. Built-
// in classes get curated colours; unknown classes fall through to
// gray.
func classColour(class string) string {
	switch class {
	case "research":
		return "#60a5fa" // blue-400
	case "spec":
		return "#34d399" // emerald-400
	case "decision":
		return "#a78bfa" // violet-400
	case "commit_msg":
		return "#fbbf24" // amber-400
	case "diagnostic":
		return "#f87171" // red-400
	case "external_fetch":
		return "#659157" // accent-500 (Coolors forest green)
	case "summary":
		return "#fb923c" // orange-400
	case "unclassified":
		return "#94a3b8" // slate-400
	}
	return "#6b7280" // gray-500
}

// statusStrokeColour styles the scatter point's outer ring by
// validation status: verified=green, refuted=red (won't appear in
// scatter — filter excludes), superseded=amber, legacy=gray,
// unverified=transparent (no stroke).
func statusStrokeColour(status string) string {
	switch status {
	case "verified":
		return "#10b981" // emerald-500
	case "superseded":
		return "#f59e0b" // amber-500
	case "refuted":
		return "#ef4444" // red-500
	case "legacy":
		return "#64748b" // slate-500
	}
	return "transparent"
}

// silence unused-import warnings until we extend further.
var _ = persistence.MemoryQuarantineItem{}

// httpError writes a plain-text error response — same shape as the
// http.Error helper but with a leading "vornik ui:" prefix so an
// operator hitting the page directly sees the source of the error.
func httpError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, "vornik ui: "+msg, code)
}

// writeJSONStatus serialises body as JSON and writes the response
// with status. Distinct name from the export package's writeJSON
// (which is download-shaped with Content-Disposition).
func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
