package ui

// Operator-profile UI surface, mounted under /ui/memory/operators.
// Pairs with the dispatcher's per-turn <operator_profile> block
// (internal/dispatcher/operator_profile.go) — same data; this
// page lets operators inspect what the assistant is reading on
// their behalf.
//
// Read-only this slice. Edit / delete flows ship in a follow-up
// alongside the agent's update_operator_profile tool. The
// confirmation here is observability-only: "what does the
// assistant believe about me?" so operators can spot drift
// before the write tool starts populating things autonomously.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/persistence"
)

// OperatorProfileSource is the narrow contract the UI consumes.
// Lifted from persistence.OperatorProfileRepository so the UI
// can read + edit + delete via the same repo the dispatcher
// tool writes to.
type OperatorProfileSource interface {
	Get(ctx context.Context, operatorID string) (*persistence.OperatorProfile, error)
	List(ctx context.Context, limit int) ([]*persistence.OperatorProfile, error)
	Upsert(ctx context.Context, profile *persistence.OperatorProfile) error
	Delete(ctx context.Context, operatorID string) error
}

// OperatorProfileAuditSource is the narrow audit-log reader
// the detail page's "Recent changes" panel consumes. Decouples
// the UI from AdminAuditRepository's wider surface.
type OperatorProfileAuditSource interface {
	List(ctx context.Context, filter persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error)
}

// MemoryOperatorsData backs /ui/memory/operators.
type MemoryOperatorsData struct {
	adminCommonData

	// Available reports whether the source is wired. False
	// renders the "not wired" hint instead of empty tables.
	Available bool

	// Error carries a List() failure for operator visibility.
	Error string

	// Rows is the list of profile cards in display order
	// (recently-updated first).
	Rows []OperatorProfileCard

	// GeneratedAt is the wall-clock when the snapshot was taken.
	GeneratedAt time.Time
}

// OperatorProfileCard is the per-row display shape — flattened
// from persistence.OperatorProfile with the structured JSONB
// pre-parsed into key/value lines so the template doesn't have
// to.
type OperatorProfileCard struct {
	OperatorID     string
	Channel        string // derived: prefix before ":"
	StructuredKeys []OperatorProfileKV
	Notes          string
	UpdatedAt      time.Time
	UpdatedAgo     string
}

// OperatorProfileKV is one rendered key/value pair from the
// structured JSONB blob. Only allow-listed keys (the dispatcher's
// operatorProfileKnownKeys) appear here.
type OperatorProfileKV struct {
	Key   string
	Value string
}

// MemoryOperatorDetailData backs /ui/memory/operators/<id>.
type MemoryOperatorDetailData struct {
	adminCommonData

	Card  OperatorProfileCard
	Error string

	// Notice is a transient success/info banner — set after a
	// successful POST (key updated, key removed, profile
	// forgotten) so the operator sees a confirmation.
	Notice string

	// AllowedKeys is the explicit allow-list rendered into the
	// edit-form's <select> so the page never has to "trust" the
	// template to know what's safe to write.
	AllowedKeys []string

	// RawStructuredJSON is the formatted JSON of the structured
	// column — surfaced so power-operators can see the source of
	// truth alongside the parsed rendering.
	RawStructuredJSON string

	// AuditEntries lists every "operator_profile.updated" admin-
	// audit row for this operator, newest first. Each carries
	// the (key, value, rationale) the dispatcher tool stamped.
	AuditEntries []OperatorProfileAuditRow

	CreatedAt time.Time
	UpdatedAt time.Time
}

// OperatorProfileAuditRow is the per-row shape for the "Recent
// changes" panel.
type OperatorProfileAuditRow struct {
	Timestamp time.Time
	Source    string // "dispatcher" (chat tool) / "ui" (admin form) / "cli"
	Key       string
	Value     string
	Rationale string
}

// memoryOperatorsKnownKeys mirrors the dispatcher's
// operatorProfileKnownKeys list. Duplicated here rather than
// imported because the dispatcher package symbol is unexported
// and the UI shouldn't depend on dispatcher internals.
var memoryOperatorsKnownKeys = []string{
	"tone",
	"verbosity",
	"time_zone",
	"communication_style",
	"preferred_channel",
}

// MemoryOperators handles GET /ui/memory/operators — the list
// page. Nil-safe; renders the "not wired" hint when the source
// isn't configured (SQLite + pre-migration deployments).
func (s *Server) MemoryOperators(w http.ResponseWriter, r *http.Request) {
	if !admin.IsAdminFromContext(r.Context()) {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	data := MemoryOperatorsData{
		adminCommonData: adminCommonData{
			Title:       "Memory — Operators",
			CurrentPage: "memory",
		},
		Available:   s.operatorProfiles != nil,
		GeneratedAt: time.Now().UTC(),
	}
	if s.operatorProfiles == nil {
		s.render(w, "memory_operators.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.operatorProfiles.List(ctx, 200)
	if err != nil {
		data.Error = err.Error()
		s.logger.Warn().Err(err).Msg("memory operators: list failed")
		s.render(w, "memory_operators.html", data)
		return
	}
	now := time.Now().UTC()
	for _, r := range rows {
		if r == nil {
			continue
		}
		data.Rows = append(data.Rows, buildOperatorCard(r, now))
	}
	s.render(w, "memory_operators.html", data)
}

// MemoryOperator handles GET /ui/memory/operators/<id>. 404 on
// unknown id so the URL surface is safe to share without the
// page leaking "operator X exists". POST routes through
// MemoryOperatorAction (edit/forget forms).
func (s *Server) MemoryOperator(w http.ResponseWriter, r *http.Request, operatorID string) {
	if !admin.IsAdminFromContext(r.Context()) {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost {
		s.MemoryOperatorAction(w, r, operatorID)
		return
	}
	s.renderOperatorDetail(w, r, operatorID, "")
}

// renderOperatorDetail centralises the detail-page assembly so
// the POST action handler can re-render with a Notice banner
// after a successful edit without an extra redirect hop.
func (s *Server) renderOperatorDetail(w http.ResponseWriter, r *http.Request, operatorID, notice string) {
	if operatorID == "" {
		http.NotFound(w, r)
		return
	}
	if s.operatorProfiles == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := s.operatorProfiles.Get(ctx, operatorID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Warn().Err(err).Str("operator_id", operatorID).Msg("memory operator: get failed")
		// Render a 200 with an error banner — same shape the rest
		// of the memory pages use so operators have a consistent
		// failure surface.
		data := MemoryOperatorDetailData{
			adminCommonData: adminCommonData{Title: "Memory — Operator", CurrentPage: "memory"},
			Error:           err.Error(),
		}
		s.render(w, "memory_operator_detail.html", data)
		return
	}
	now := time.Now().UTC()
	card := buildOperatorCard(row, now)
	data := MemoryOperatorDetailData{
		adminCommonData:   adminCommonData{Title: "Memory — Operator", CurrentPage: "memory"},
		Card:              card,
		Notice:            notice,
		AllowedKeys:       append([]string(nil), memoryOperatorsKnownKeys...),
		RawStructuredJSON: prettyJSON(row.Structured),
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
	// "notes" is a write-key but not part of the read-time
	// known-keys list (it's rendered separately). Append so
	// the edit form's <select> offers it too.
	data.AllowedKeys = append(data.AllowedKeys, "notes")
	// Audit panel — best-effort. A failure here doesn't blank
	// the rest of the page; operators just see "(audit
	// unavailable)" in the panel header.
	if s.operatorProfileAudit != nil {
		entries, aerr := s.operatorProfileAudit.List(ctx, persistence.AdminAuditFilter{
			Action:    "operator_profile.updated",
			Principal: operatorID,
			PageSize:  50,
		})
		if aerr == nil {
			for _, e := range entries {
				if e == nil {
					continue
				}
				row := OperatorProfileAuditRow{
					Timestamp: e.Timestamp,
					Source:    e.Source,
				}
				var after struct {
					Key       string `json:"key"`
					Value     string `json:"value"`
					Rationale string `json:"rationale"`
				}
				if e.After != "" {
					_ = json.Unmarshal([]byte(e.After), &after)
				}
				row.Key = after.Key
				row.Value = after.Value
				row.Rationale = after.Rationale
				data.AuditEntries = append(data.AuditEntries, row)
			}
		} else {
			s.logger.Warn().Err(aerr).Str("operator_id", operatorID).Msg("memory operator: audit list failed")
		}
	}
	s.render(w, "memory_operator_detail.html", data)
}

// MemoryOperatorAction handles POST /ui/memory/operators/<id>.
// Form values:
//   - action=set: requires key + value (empty value removes
//     the key from structured, or clears notes).
//   - action=forget: deletes the row + a hard-stop audit
//     entry so the privacy revocation is auditable.
//
// Successful actions redirect-after-POST back to the detail
// page with a Notice banner.
func (s *Server) MemoryOperatorAction(w http.ResponseWriter, r *http.Request, operatorID string) {
	if !admin.IsAdminFromContext(r.Context()) {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if operatorID == "" {
		http.NotFound(w, r)
		return
	}
	if s.operatorProfiles == nil {
		http.Error(w, "operator profile source not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	action := r.FormValue("action")
	rationale := strings.TrimSpace(r.FormValue("rationale"))
	if rationale == "" {
		rationale = "manual edit via admin UI"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	switch action {
	case "set":
		key := strings.TrimSpace(r.FormValue("key"))
		value := strings.TrimSpace(r.FormValue("value"))
		if key == "" {
			s.renderOperatorDetail(w, r, operatorID, "Set rejected: key is required.")
			return
		}
		allowed := false
		for _, k := range memoryOperatorsKnownKeys {
			if k == key {
				allowed = true
				break
			}
		}
		if !allowed && key != "notes" {
			s.renderOperatorDetail(w, r, operatorID, "Set rejected: unknown key.")
			return
		}
		current, err := s.operatorProfiles.Get(ctx, operatorID)
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			http.Error(w, "load failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if current == nil {
			current = &persistence.OperatorProfile{OperatorID: operatorID}
		}
		if key == "notes" {
			current.Notes = value
		} else {
			structured := map[string]any{}
			if len(current.Structured) > 0 {
				_ = json.Unmarshal(current.Structured, &structured)
			}
			if value == "" {
				delete(structured, key)
			} else {
				structured[key] = value
			}
			raw, _ := json.Marshal(structured)
			current.Structured = raw
		}
		if err := s.operatorProfiles.Upsert(ctx, current); err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditOperatorProfileEdit(ctx, operatorID, key, value, rationale)
		notice := fmt.Sprintf("Updated %s.", key)
		if value == "" && key != "notes" {
			notice = fmt.Sprintf("Removed %s.", key)
		}
		s.renderOperatorDetail(w, r, operatorID, notice)

	case "forget":
		if err := s.operatorProfiles.Delete(ctx, operatorID); err != nil {
			http.Error(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditOperatorProfileForget(ctx, operatorID, rationale)
		// Forget → redirect to the list page; the detail URL
		// would 404 immediately and confuse the operator.
		http.Redirect(w, r, "/ui/memory/operators?notice=forgotten", http.StatusSeeOther)

	default:
		http.Error(w, "unknown action: "+action, http.StatusBadRequest)
	}
}

// auditOperatorProfileEdit logs a UI-initiated set call into
// admin_audit so the detail page's "Recent changes" panel
// shows both dispatcher-tool and admin-UI edits in one stream.
func (s *Server) auditOperatorProfileEdit(ctx context.Context, operatorID, key, value, rationale string) {
	if s.adminAuditRepo == nil {
		return
	}
	afterJSON, _ := json.Marshal(map[string]string{
		"key":       key,
		"value":     value,
		"rationale": rationale,
	})
	_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: operatorID,
		Source:    "ui",
		Action:    "operator_profile.updated",
		Target:    operatorID,
		After:     string(afterJSON),
	})
}

// auditOperatorProfileForget logs a UI-initiated delete with a
// distinct action="operator_profile.forgotten" so the audit
// list can distinguish "preference updated" from "operator
// purged".
func (s *Server) auditOperatorProfileForget(ctx context.Context, operatorID, rationale string) {
	if s.adminAuditRepo == nil {
		return
	}
	afterJSON, _ := json.Marshal(map[string]string{"rationale": rationale})
	_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: operatorID,
		Source:    "ui",
		Action:    "operator_profile.forgotten",
		Target:    operatorID,
		After:     string(afterJSON),
	})
}

// buildOperatorCard turns a persistence row into the flattened
// card shape the template consumes. Same key allow-list the
// dispatcher uses — power-operators see ALL structured fields
// on the detail page (via RawStructuredJSON), but the card
// only renders the documented set.
func buildOperatorCard(row *persistence.OperatorProfile, now time.Time) OperatorProfileCard {
	card := OperatorProfileCard{
		OperatorID: row.OperatorID,
		Notes:      row.Notes,
		UpdatedAt:  row.UpdatedAt,
	}
	if i := strings.IndexByte(row.OperatorID, ':'); i > 0 {
		card.Channel = row.OperatorID[:i]
	}
	if !row.UpdatedAt.IsZero() {
		card.UpdatedAgo = humanClusterDuration(now.Sub(row.UpdatedAt)) + " ago"
	}
	if len(row.Structured) > 0 {
		var parsed map[string]any
		if err := json.Unmarshal(row.Structured, &parsed); err == nil && len(parsed) > 0 {
			for _, key := range memoryOperatorsKnownKeys {
				v, ok := parsed[key]
				if !ok {
					continue
				}
				s := operatorScalarToString(v)
				if s == "" {
					continue
				}
				card.StructuredKeys = append(card.StructuredKeys, OperatorProfileKV{Key: key, Value: s})
			}
			sort.Slice(card.StructuredKeys, func(i, j int) bool { return card.StructuredKeys[i].Key < card.StructuredKeys[j].Key })
		}
	}
	return card
}

// operatorScalarToString mirrors the dispatcher's scalarToString
// — strings / numbers / bools only, everything else returns
// empty so a malformed nested shape doesn't poison the UI row.
func operatorScalarToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		// Numeric JSON values land as float64 — strconv-friendly,
		// no trailing ".000000".
		if float64(int(t)) == t {
			return formatInt64(int64(t))
		}
		return formatFloat(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// prettyJSON re-formats the raw structured bytes for the detail
// page. Garbage in → empty out (the detail page just hides the
// raw block).
func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func formatInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 16)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func formatFloat(f float64) string {
	const decimals = 2
	neg := f < 0
	if neg {
		f = -f
	}
	rounded := f
	for i := 0; i < decimals; i++ {
		rounded *= 10
	}
	rounded += 0.5
	intval := int64(rounded)
	scale := int64(1)
	for i := 0; i < decimals; i++ {
		scale *= 10
	}
	whole := intval / scale
	frac := intval % scale
	out := formatInt64(whole) + "." + padFrac(frac, decimals)
	if neg && (whole != 0 || frac != 0) {
		out = "-" + out
	}
	return out
}

func padFrac(frac int64, width int) string {
	s := formatInt64(frac)
	for len(s) < width {
		s = "0" + s
	}
	return s
}
