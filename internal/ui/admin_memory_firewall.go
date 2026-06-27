package ui

// /ui/admin/memory/firewall — Policy-Aware Memory Firewall
// admin landing page. Single view in Phase C v1:
//
//   - GET /ui/admin/memory/firewall  → recent evaluations +
//                                      mode chip + recent
//                                      block-by-class summary
//
// Per-chunk policy editing lands alongside the Phase D
// follow-on (per-project YAML). Same admin gate as
// /ui/admin/* — the adminRouter wrapper enforces it.

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// FirewallLandingData backs the firewall page template.
type FirewallLandingData struct {
	adminCommonData
	Available         bool
	Mode              string
	ModeDescription   string
	RecentEvaluations []FirewallEvaluationRow
	BlocksByDecision  []FirewallBlocksByDecisionRow
	ProjectFilter     string
	NoProjectSelected bool
	Error             string
}

// FirewallEvaluationRow is the pre-formatted shape the template
// renders for each evaluation. Plain strings (no time.Time) so
// the template doesn't have to format inline.
type FirewallEvaluationRow struct {
	EvaluatedAt    string
	Decision       string
	IsBlock        bool
	ChunkID        string
	RequestRole    string
	RequestPurpose string
	ReasonDetail   string
}

// FirewallBlocksByDecisionRow is one row of the
// "blocks-by-class summary" panel. The template renders it as
// a bar chart with operator-readable labels.
type FirewallBlocksByDecisionRow struct {
	Decision string
	Count    int
}

// AdminMemoryFirewall renders /ui/admin/memory/firewall.
func (s *Server) AdminMemoryFirewall(w http.ResponseWriter, r *http.Request) {
	data := FirewallLandingData{
		adminCommonData: adminCommonData{
			Title:       "Memory Firewall",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.memoryPolicyEvaluations != nil,
		Mode:      s.memoryFirewallMode,
	}
	if data.Mode == "" {
		data.Mode = "unknown"
	}
	data.ModeDescription = firewallModeDescription(data.Mode)

	if !data.Available {
		s.render(w, "admin_memory_firewall.html", data)
		return
	}

	// Project filter — required for the evaluations query.
	// When omitted, render the landing page with a "pick a
	// project" empty-state.
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		data.NoProjectSelected = true
		s.render(w, "admin_memory_firewall.html", data)
		return
	}
	data.ProjectFilter = projectID

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := s.memoryPolicyEvaluations.ListRecent(ctx, projectID, "", time.Now().Add(-7*24*time.Hour), 100)
	if err != nil {
		data.Error = err.Error()
		s.render(w, "admin_memory_firewall.html", data)
		return
	}

	// Pre-format rows + roll up decision counts in one pass
	// so the template stays presentation-only.
	byDecision := map[string]int{}
	for _, row := range rows {
		isBlock := string(row.Decision) != string(memoryfirewall.DecisionAllow)
		data.RecentEvaluations = append(data.RecentEvaluations, FirewallEvaluationRow{
			EvaluatedAt:    row.EvaluatedAt.Local().Format("2006-01-02 15:04 MST"),
			Decision:       string(row.Decision),
			IsBlock:        isBlock,
			ChunkID:        row.ChunkID,
			RequestRole:    row.RequestRole,
			RequestPurpose: row.RequestPurpose,
			ReasonDetail:   row.ReasonDetail,
		})
		byDecision[string(row.Decision)]++
	}
	for d, c := range byDecision {
		data.BlocksByDecision = append(data.BlocksByDecision, FirewallBlocksByDecisionRow{
			Decision: d, Count: c,
		})
	}
	s.render(w, "admin_memory_firewall.html", data)
}

// FirewallChunkData backs /ui/admin/memory/firewall/chunks/{id}.
// The page shows the chunk's current policy + last N recent
// evaluations for that specific chunk, with an edit form that
// POSTs to the existing API endpoint.
type FirewallChunkData struct {
	adminCommonData
	Available    bool
	ChunkID      string
	NotFound     bool
	Error        string
	PolicyDigest string

	// Current policy values (rendered into the form's `value=`
	// attributes so operators see what they're editing).
	TenantID           string
	SensitivityTier    string
	ProvenanceSource   string
	ProvenanceProducer string
	ProvenanceTrust    int
	ProvenanceURL      string
	FirewallExpiresAt  string // RFC3339 string for the form
	PermittedRolesCSV  string
	AllowedPurposesCSV string
	ContentClass       string
	ValidationStatus   string

	// RecentEvaluations for this specific chunk (last 50).
	// Shown beneath the form so operators can see how the
	// current policy is playing out.
	RecentEvaluations []FirewallEvaluationRow
}

// AdminMemoryFirewallChunk renders the per-chunk detail page.
// Routed via the admin dispatcher
// (/ui/admin/memory/firewall/chunks/{chunk_id}).
//
// Read-only in v1: the form posts to the existing
// /api/v1/admin/memory/policy/chunks/{id} endpoint via
// vanilla HTML form. No JS framework dependency.
func (s *Server) AdminMemoryFirewallChunk(w http.ResponseWriter, r *http.Request, chunkID string) {
	data := FirewallChunkData{
		adminCommonData: adminCommonData{
			Title:       "Chunk Policy · " + chunkID,
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.firewallEditor != nil,
		ChunkID:   chunkID,
	}
	if !data.Available {
		s.render(w, "admin_memory_firewall_chunk.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	policies, err := s.firewallEditor.LoadChunkPolicies(ctx, []string{chunkID})
	if err != nil {
		data.Error = err.Error()
		s.render(w, "admin_memory_firewall_chunk.html", data)
		return
	}
	row, ok := policies[chunkID]
	if !ok {
		data.NotFound = true
		s.render(w, "admin_memory_firewall_chunk.html", data)
		return
	}
	data.PolicyDigest = row.PolicyDigest
	data.TenantID = row.TenantID
	data.SensitivityTier = row.SensitivityTier
	data.ProvenanceSource = row.ProvenanceSource
	data.ProvenanceProducer = row.ProvenanceProducer
	data.ProvenanceTrust = row.ProvenanceTrust
	data.ProvenanceURL = row.ProvenanceURL
	if row.FirewallExpiresAt != nil && !row.FirewallExpiresAt.IsZero() {
		data.FirewallExpiresAt = row.FirewallExpiresAt.Format(time.RFC3339)
	}
	data.PermittedRolesCSV = joinCSV(row.PermittedRoles)
	data.AllowedPurposesCSV = joinCSV(row.AllowedPurposes)
	data.ContentClass = row.ContentClass
	data.ValidationStatus = row.ValidationStatus

	// Recent evaluations for THIS chunk specifically — an index-backed
	// per-chunk query (ListByChunk), not a cross-project scan filtered in Go.
	if s.memoryPolicyEvaluations != nil {
		rows, err := s.memoryPolicyEvaluations.ListByChunk(ctx, chunkID, 50)
		if err != nil {
			s.logger.Warn().Err(err).Str("chunk_id", chunkID).Msg("admin firewall chunk: list evaluations failed")
		}
		for _, row := range rows {
			data.RecentEvaluations = append(data.RecentEvaluations, FirewallEvaluationRow{
				EvaluatedAt:    row.EvaluatedAt.Local().Format("2006-01-02 15:04 MST"),
				Decision:       string(row.Decision),
				IsBlock:        string(row.Decision) != "allow",
				ChunkID:        row.ChunkID,
				RequestRole:    row.RequestRole,
				RequestPurpose: row.RequestPurpose,
				ReasonDetail:   row.ReasonDetail,
			})
		}
	}
	s.render(w, "admin_memory_firewall_chunk.html", data)
}

// joinCSV is the inverse of splitCSV: builds "a, b, c" from
// []string{"a", "b", "c"}; empty slice → empty string.
func joinCSV(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	out := s[0]
	for _, x := range s[1:] {
		out += ", " + x
	}
	return out
}

// WithMemoryFirewallEditor wires the per-chunk policy editor
// behind /ui/admin/memory/firewall/chunks/{id}. Same shape +
// name as the api package's option but lives separately so the
// ui package doesn't depend on internal/memory.
func WithMemoryFirewallEditor(ed FirewallEditor) ServerOption {
	return func(s *Server) {
		s.firewallEditor = ed
	}
}

// FirewallEditor is the narrow surface the chunk-detail page
// reads from. Field-compatible with api.MemoryFirewallEditor;
// kept separate so the ui package doesn't import internal/api.
type FirewallEditor interface {
	LoadChunkPolicies(ctx context.Context, chunkIDs []string) (map[string]FirewallChunkRow, error)
}

// FirewallChunkRow mirrors memory.ChunkPolicyRow / api.ChunkPolicyRow
// at the ui boundary. Adapters in the service package
// translate between the three.
type FirewallChunkRow struct {
	ChunkID            string
	TenantID           string
	SensitivityTier    string
	ProvenanceSource   string
	ProvenanceProducer string
	ProvenanceTrust    int
	ProvenanceURL      string
	FirewallExpiresAt  *time.Time
	PermittedRoles     []string
	AllowedPurposes    []string
	PolicyDigest       string
	ContentClass       string
	ValidationStatus   string
}

// firewallModeDescription mirrors the API's description map
// so the UI doesn't need a separate fetch.
func firewallModeDescription(mode string) string {
	switch mode {
	case "off":
		return "Firewall evaluates + audits, but blocked chunks still surface in recall results."
	case "advisory":
		return "Firewall evaluates + audits, blocked chunks surface with a PolicyWarning."
	case "enforce":
		return "Firewall evaluates + audits, blocked chunks do NOT surface in recall results."
	default:
		return "Mode unknown — daemon's enforcement state not reported."
	}
}

// WithMemoryPolicyEvaluations wires the audit repo behind the
// /ui/admin/memory/firewall endpoint. Nil keeps the endpoint at
// "not configured" state.
func WithMemoryPolicyEvaluations(repo persistence.MemoryPolicyEvaluationRepository) ServerOption {
	return func(s *Server) {
		s.memoryPolicyEvaluations = repo
	}
}

// WithMemoryFirewallMode supplies the current enforcement mode.
// Mirror of the API-side option.
func WithMemoryFirewallMode(mode memoryfirewall.EnforcementMode) ServerOption {
	return func(s *Server) {
		s.memoryFirewallMode = string(mode)
	}
}
