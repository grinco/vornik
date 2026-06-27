package ui

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// /ui/projects/<id>/keys — per-project API-key management panel.
//
// Rendering rules:
//   - List view shows ID, name, prefix, created, last used,
//     expires, status (active/revoked). Revoked rows render dimmed.
//   - Create form posts to the same path; on success we render the
//     SAME template with `NewSecret` populated so the operator sees
//     the secret exactly once. A page reload clears it.
//   - Revoke / rotate post to the action paths; success refreshes
//     the list (the new secret again rides back via `NewSecret`).
//
// IDOR / auth concerns are delegated to the AuthMiddleware that
// wraps the UI subtree (service container wires the same DB-keys
// auth as the API path). This handler trusts the auth context.

// ProjectKeysData backs project_keys.html.
type ProjectKeysData struct {
	Title       string
	CurrentPage string
	ProjectID   string

	Keys []ProjectKeyRow

	// Status is the active/all filter for the list. Defaults to
	// "active" — one-time task keys get revoked after use and made the
	// unfiltered list endless, so the default view hides revoked and
	// expired keys. "all" shows everything (paginated).
	Status string
	// Paginator drives the shared {{template "pagination"}} controls.
	Paginator Paginator

	// NewSecret is non-empty exactly once per (create | rotate)
	// response. The template renders it inside a copy-this-now
	// banner and clears on page reload.
	NewSecret   string
	NewSecretID string

	Error   string
	Success string
}

// ProjectKeyRow is a render-friendly form of persistence.APIKey.
// Times are pre-formatted so the template stays declarative.
type ProjectKeyRow struct {
	ID        string
	Name      string
	Prefix    string
	Created   string
	LastUsed  string
	Expires   string
	Status    string // "active" or "revoked"
	CreatedBy string

	// AllowPush is true when this key may push to the git-over-HTTPS
	// workspace endpoint. Operators enable it per-key via the
	// enable-push / disable-push toggle actions.
	AllowPush bool

	// Rate-limit headroom (P1 UI batch from the 7-day sweep). Each
	// row carries the nominal cap from the api_keys row PLUS the
	// live bucket headroom from the in-process APIKeyLimiter.
	// Operators scan the column to spot a key that's chronically
	// at zero — the upstream HA loop is hammering and needs
	// either a higher cap or backoff in the caller.
	RateLimited     bool   // true when the persisted row carries non-zero rps + burst
	RateLimitRPS    int    // configured per-second cap (0 = unlimited)
	RateLimitBurst  int    // configured burst (0 = unlimited)
	TokensRemaining string // pre-rendered "12.4" or "—" so the template stays declarative
	LastRefillAgo   string // "3m 24s ago" or "—" for fresh-bucket / no-bucket
}

// ProjectKeys renders the page. GET shows the list; POST handles
// create / rotate / revoke based on the form's `action` field.
func (s *Server) ProjectKeys(w http.ResponseWriter, r *http.Request, projectID string) {
	if projectID == "" {
		http.Error(w, "project id required", http.StatusBadRequest)
		return
	}
	if s.apiKeyRepo == nil {
		http.Error(w, "api-key surface not configured", http.StatusServiceUnavailable)
		return
	}

	data := ProjectKeysData{
		Title:       "API Keys: " + projectID,
		CurrentPage: "projects",
		ProjectID:   projectID,
	}

	if r.Method == http.MethodPost {
		// D2 (audit 2026-06-10): minting/rotating/revoking project API
		// keys issues long-lived bearer credentials that survive session
		// revocation — a credential-issuer privilege. Restrict the
		// mutating POST actions to admin scope; the read-only GET list
		// (values already redacted) stays available to project members.
		if !s.uiRequireAdminMutation(w, r) {
			return
		}
		if err := r.ParseForm(); err != nil {
			data.Error = "failed to parse form: " + err.Error()
		} else {
			s.handleProjectKeysAction(r, &data, projectID)
		}
	}

	// Active-only by default; ?status=all reveals revoked/expired keys.
	status := r.URL.Query().Get("status")
	if status != "all" {
		status = "active"
	}
	data.Status = status

	rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
	if err != nil {
		// Surface the error inline but keep rendering — the operator
		// still needs the create form. Half-broken page > total 500.
		if data.Error == "" {
			data.Error = "failed to load keys: " + err.Error()
		}
	} else {
		if status == "active" {
			rows = filterActiveKeys(rows, time.Now().UTC())
		}
		sortKeysNewestFirst(rows)
		// Build the paginator over the full (filtered) set, then slice
		// to the current page. Same page value feeds both so the window
		// and the controls agree.
		data.Paginator = NewPaginator(len(rows), parsePageParam(r.URL.Query()),
			defaultPerPage, r.URL.Path, r.URL.Query())
		rows = pageWindow(rows, data.Paginator.Page, defaultPerPage)
		data.Keys = s.renderKeyRowsWithLimiter(rows)
	}

	s.render(w, "project_keys.html", data)
}

// handleProjectKeysAction dispatches create / rotate / revoke
// based on the form's `action` field. Result drops onto `data`
// (Error or Success + optional NewSecret).
func (s *Server) handleProjectKeysAction(r *http.Request, data *ProjectKeysData, projectID string) {
	switch r.FormValue("action") {
	case "create":
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			data.Error = "name is required"
			return
		}
		secret, err := apikey.Generate(projectID)
		if err != nil {
			data.Error = "could not mint key for project: " + err.Error()
			return
		}
		row := &persistence.APIKey{
			ID:        persistence.GenerateID("akey"),
			ProjectID: projectID,
			Name:      name,
			KeyHash:   apikey.Hash(secret),
			KeyPrefix: apikey.DisplayPrefix(secret),
			CreatedAt: time.Now().UTC(),
			CreatedBy: "ui",
		}
		if err := s.apiKeyRepo.Create(r.Context(), row); err != nil {
			data.Error = "failed to create key: " + err.Error()
			return
		}
		data.Success = "Created key " + row.Name + "."
		data.NewSecret = secret
		data.NewSecretID = row.ID
	case "rotate":
		keyID := r.FormValue("key_id")
		if keyID == "" {
			data.Error = "key_id required"
			return
		}
		// IDOR guard: confirm the key belongs to THIS project.
		rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
		if err != nil {
			data.Error = "failed to load keys for rotate: " + err.Error()
			return
		}
		var prior *persistence.APIKey
		for _, k := range rows {
			if k.ID == keyID {
				prior = k
				break
			}
		}
		if prior == nil {
			data.Error = "key not found in this project"
			return
		}
		if prior.RevokedAt != nil {
			data.Error = "cannot rotate a revoked key"
			return
		}
		secret, err := apikey.Generate(projectID)
		if err != nil {
			data.Error = "could not mint replacement: " + err.Error()
			return
		}
		// Carry over EVERY scope/limit/capability column via the shared
		// helper — a hand-built struct here once dropped the companion
		// scope block and demoted UI-rotated companion keys (2026-06-27).
		fresh := prior.RotatedCopy(
			persistence.GenerateID("akey"),
			apikey.Hash(secret),
			apikey.DisplayPrefix(secret),
			"ui",
			time.Now().UTC(),
		)
		if err := s.apiKeyRepo.Create(r.Context(), fresh); err != nil {
			data.Error = "failed to mint rotated key: " + err.Error()
			return
		}
		_ = s.apiKeyRepo.Revoke(r.Context(), prior.ID)
		data.Success = "Rotated key " + prior.Name + "."
		data.NewSecret = secret
		data.NewSecretID = fresh.ID
	case "revoke":
		keyID := r.FormValue("key_id")
		if keyID == "" {
			data.Error = "key_id required"
			return
		}
		// IDOR guard mirror of rotate.
		rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
		if err != nil {
			data.Error = "failed to load keys for revoke: " + err.Error()
			return
		}
		found := false
		for _, k := range rows {
			if k.ID == keyID {
				found = true
				break
			}
		}
		if !found {
			data.Error = "key not found in this project"
			return
		}
		if err := s.apiKeyRepo.Revoke(r.Context(), keyID); err != nil {
			data.Error = "failed to revoke: " + err.Error()
			return
		}
		data.Success = "Revoked key " + keyID + "."
	case "enable-push", "disable-push":
		allow := r.FormValue("action") == "enable-push"
		keyID := r.FormValue("key_id")
		if keyID == "" {
			data.Error = "key_id required"
			return
		}
		// IDOR guard: confirm the key belongs to THIS project.
		rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
		if err != nil {
			data.Error = "failed to load keys for push-toggle: " + err.Error()
			return
		}
		found := false
		for _, k := range rows {
			if k.ID == keyID {
				found = true
				break
			}
		}
		if !found {
			data.Error = "key not found in this project"
			return
		}
		if err := s.apiKeyRepo.UpdateAllowPush(r.Context(), keyID, allow); err != nil {
			data.Error = "failed to update push permission: " + err.Error()
			return
		}
		if allow {
			data.Success = "Enabled git push for key " + keyID + "."
		} else {
			data.Success = "Disabled git push for key " + keyID + "."
		}
	default:
		data.Error = "unknown action " + r.FormValue("action")
	}
}

// filterActiveKeys keeps only keys that are neither revoked nor
// expired — the default list view. One-time task keys are revoked once
// the task finishes, so without this the list grows without bound. A
// nil ExpiresAt means "never expires" and stays active.
func filterActiveKeys(in []*persistence.APIKey, now time.Time) []*persistence.APIKey {
	out := make([]*persistence.APIKey, 0, len(in))
	for _, k := range in {
		if k.RevokedAt != nil {
			continue
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// sortKeysNewestFirst orders keys by creation time descending so the
// most recently minted keys lead the list (stable pagination + the
// just-created key is on page 1). Sorts in place.
func sortKeysNewestFirst(in []*persistence.APIKey) {
	sort.SliceStable(in, func(i, j int) bool {
		return in[i].CreatedAt.After(in[j].CreatedAt)
	})
}

// renderKeyRows converts persistence rows to the render-friendly
// shape the template consumes. Times collapse to relative-ish
// strings (RFC3339) for monospace alignment; the page is operator-
// facing, not customer-facing — pretty-printing is not the goal.
//
// Pure-function variant kept so unit tests can build rows without a
// Server. Production callers go through renderKeyRowsWithLimiter
// so the per-row rate-limit headroom columns are populated.
func renderKeyRows(in []*persistence.APIKey) []ProjectKeyRow {
	out := make([]ProjectKeyRow, 0, len(in))
	const layout = "2006-01-02 15:04 MST"
	for _, k := range in {
		status := "active"
		if k.RevokedAt != nil {
			status = "revoked"
		}
		row := ProjectKeyRow{
			ID:        k.ID,
			Name:      k.Name,
			Prefix:    k.KeyPrefix,
			Created:   k.CreatedAt.UTC().Format(layout),
			Status:    status,
			CreatedBy: k.CreatedBy,
			AllowPush: k.AllowPush,
		}
		row.LastUsed = "—"
		if k.LastUsedAt != nil {
			row.LastUsed = k.LastUsedAt.UTC().Format(layout)
		}
		row.Expires = "—"
		if k.ExpiresAt != nil {
			row.Expires = k.ExpiresAt.UTC().Format(layout)
		}
		// Nominal rate-limit cap from the persisted row. Live
		// bucket headroom is filled in by
		// renderKeyRowsWithLimiter when the Server has a limiter
		// wired — that wraps this function so unit tests can call
		// the pure path without an in-process bucket store.
		if k.RateLimitRPS != nil && *k.RateLimitRPS > 0 {
			row.RateLimited = true
			row.RateLimitRPS = *k.RateLimitRPS
		}
		if k.RateLimitBurst != nil && *k.RateLimitBurst > 0 {
			row.RateLimited = true
			row.RateLimitBurst = *k.RateLimitBurst
		}
		row.TokensRemaining = "—"
		row.LastRefillAgo = "—"
		out = append(out, row)
	}
	return out
}

// renderKeyRowsWithLimiter is the production-side wrapper: pulls
// the pure renderKeyRows shape, then asks the wired APIKeyLimiter
// for each key's current bucket level. Renders "tokens" and "last
// refill ago" as strings so the template stays declarative.
func (s *Server) renderKeyRowsWithLimiter(in []*persistence.APIKey) []ProjectKeyRow {
	rows := renderKeyRows(in)
	if s == nil || s.apiKeyLimiter == nil {
		return rows
	}
	now := time.Now()
	for i := range rows {
		if !rows[i].RateLimited {
			continue
		}
		snap, ok := s.apiKeyLimiter.SnapshotFor(rows[i].ID)
		if !ok {
			// Bucket hasn't been allocated yet (no Allow call
			// since boot). Render "full" so the operator sees
			// "limit configured, no traffic yet" rather than an
			// alarming zero.
			rows[i].TokensRemaining = fmt.Sprintf("%d / %d (idle)", rows[i].RateLimitBurst, rows[i].RateLimitBurst)
			continue
		}
		rows[i].TokensRemaining = fmt.Sprintf("%.1f / %d", snap.Tokens, rows[i].RateLimitBurst)
		if !snap.LastRefill.IsZero() {
			rows[i].LastRefillAgo = relativeShort(now.Sub(snap.LastRefill))
		}
	}
	return rows
}

// relativeShort renders a Duration as a short "3m 24s" style label
// for the template. Sub-second collapses to "<1s"; multi-hour stays
// in m for compactness (the panel polls every page reload, so
// anything over an hour idle is "bucket reset already" anyway).
func relativeShort(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Second {
		return "<1s ago"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours())/24)
}
