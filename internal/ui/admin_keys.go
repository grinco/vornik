package ui

// Daemon-wide DB-backed API key inventory at /ui/admin/keys.
// Aggregates every project's keys into one operator view so
// the per-project-API-key migration's progress + rotation
// hygiene is visible without clicking through each project.
//
// Per-project /ui/projects/{id}/keys remains the editing
// surface; this page is read-only (no rotate / revoke /
// create) — admin actions land on the per-project page where
// they already exist + are tested.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminKeysData backs the /ui/admin/keys template.
type AdminKeysData struct {
	adminCommonData
	Available     bool
	Rows          []AdminKeyRow
	Counts        AdminKeysCounts
	FilterStatus  string
	FilterProject string
	StatusOptions []string
	Error         string
}

// AdminKeysCounts summarises the row distribution so the header
// renders "12 active · 3 revoked · 1 expired" without summing
// the slice client-side.
type AdminKeysCounts struct {
	Total   int
	Active  int
	Revoked int
	Expired int
}

// AdminKeyRow is one key row — pre-formatted timestamps + status
// badge class. Same shape as the per-project /keys row so a future
// refactor could share the template partial.
type AdminKeyRow struct {
	ID             string
	ProjectID      string
	Name           string
	KeyPrefix      string
	Status         string // "active" / "revoked" / "expired"
	StatusBadge    string
	CreatedAt      string
	CreatedAgo     string
	LastUsedAt     string
	LastUsedAgo    string
	ExpiresAt      string
	RevokedAt      string
	CreatedBy      string
	RateLimitLabel string // "60 rps / 10 burst" or "unlimited"
}

// AdminKeys renders /ui/admin/keys. Filter axes: status,
// project. Defaults to status=active so the most common view
// (live keys) loads first.
func (s *Server) AdminKeys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := AdminKeysData{
		adminCommonData: adminCommonData{
			Title:       "API keys",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:     s.apiKeyRepo != nil && s.projectReg != nil,
		FilterStatus:  strings.TrimSpace(q.Get("status")),
		FilterProject: strings.TrimSpace(q.Get("project")),
		StatusOptions: []string{"", "active", "revoked", "expired"},
	}
	if !data.Available {
		s.render(w, "admin_keys.html", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	projects := s.projectReg.ListProjects()
	now := time.Now()
	for _, p := range projects {
		if p == nil {
			continue
		}
		if data.FilterProject != "" && p.ID != data.FilterProject {
			continue
		}
		rows, err := s.apiKeyRepo.ListByProject(ctx, p.ID)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", p.ID).Msg("admin keys: list failed")
			data.Error = "Some projects failed to load: " + err.Error()
			continue
		}
		for _, k := range rows {
			if k == nil {
				continue
			}
			row := adminKeyToRow(k, now)
			if data.FilterStatus != "" && row.Status != data.FilterStatus {
				continue
			}
			data.Rows = append(data.Rows, row)
			data.Counts.Total++
			switch row.Status {
			case "active":
				data.Counts.Active++
			case "revoked":
				data.Counts.Revoked++
			case "expired":
				data.Counts.Expired++
			}
		}
	}
	// Newest-first global order so recent rotations land at the top.
	sort.Slice(data.Rows, func(i, j int) bool {
		return data.Rows[i].CreatedAt > data.Rows[j].CreatedAt
	})

	s.render(w, "admin_keys.html", data)
}

// adminKeyToRow converts the persistence row to the template-
// friendly shape. Pure — no I/O.
func adminKeyToRow(k *persistence.APIKey, now time.Time) AdminKeyRow {
	row := AdminKeyRow{
		ID:        k.ID,
		ProjectID: k.ProjectID,
		Name:      k.Name,
		KeyPrefix: k.KeyPrefix,
		CreatedAt: k.CreatedAt.Format("2006-01-02 15:04:05"),
		CreatedBy: k.CreatedBy,
	}
	row.CreatedAgo = humanDuration(now.Sub(k.CreatedAt)) + " ago"
	switch {
	case k.RevokedAt != nil:
		row.Status = "revoked"
		row.StatusBadge = "pill pill-danger"
		row.RevokedAt = k.RevokedAt.Format("2006-01-02 15:04:05")
	case k.ExpiresAt != nil && k.ExpiresAt.Before(now):
		row.Status = "expired"
		row.StatusBadge = "pill pill-warn"
		row.ExpiresAt = k.ExpiresAt.Format("2006-01-02 15:04:05")
	default:
		row.Status = "active"
		row.StatusBadge = "pill pill-ok"
		if k.ExpiresAt != nil {
			row.ExpiresAt = k.ExpiresAt.Format("2006-01-02 15:04:05")
		}
	}
	if k.LastUsedAt != nil {
		row.LastUsedAt = k.LastUsedAt.Format("2006-01-02 15:04:05")
		row.LastUsedAgo = humanDuration(now.Sub(*k.LastUsedAt)) + " ago"
	} else {
		row.LastUsedAgo = "never"
	}
	switch {
	case k.RateLimitRPS != nil && *k.RateLimitRPS > 0:
		burst := 0
		if k.RateLimitBurst != nil {
			burst = *k.RateLimitBurst
		}
		row.RateLimitLabel = formatRateLimit(*k.RateLimitRPS, burst)
	default:
		row.RateLimitLabel = "unlimited"
	}
	return row
}

// formatRateLimit returns "60 rps" or "60 rps / 10 burst".
// Kept here rather than in template_helpers so the admin-keys
// formatting stays close to its consumer.
func formatRateLimit(rps, burst int) string {
	if burst > 0 {
		return strItoa(rps) + " rps / " + strItoa(burst) + " burst"
	}
	return strItoa(rps) + " rps"
}

// strItoa is a one-line wrapper that exists so the template-
// adjacent code in this file doesn't pull strconv into its
// imports — keeps the surface tight.
func strItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
