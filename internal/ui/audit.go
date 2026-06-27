// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type AuditData struct {
	Title         string
	CurrentPage   string
	Entries       []*persistence.ToolAuditEntry
	WebhookEvents []*persistence.WebhookEvent
	Limit         int
	LimitOptions  []int
}

// Audit renders the tool audit log page.
//
// Audit is the canonical implementation of the "Show: 10 / 20 / 50 /
// 100" page-size pattern that every other list view inherits from.
// The validator (parsePageSize in page_size.go) and the option set
// (PageSizeOptions) live in one place so all pages stay in lock-step
// — see page_size.go's comment for why a strict allowlist matters.
func (s *Server) Audit(w http.ResponseWriter, r *http.Request) {
	limit := parsePageSize(r.URL.Query().Get("limit"))

	data := AuditData{
		Title:        "Audit Log",
		CurrentPage:  "audit",
		Limit:        limit,
		LimitOptions: PageSizeOptions,
	}

	if s.auditRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		entries, err := s.auditRepo.List(ctx, persistence.ToolAuditFilter{
			PageSize: limit,
		})
		if err != nil {
			s.logger.Warn().Err(err).Msg("failed to load audit entries")
		} else {
			data.Entries = entries
		}
	}
	if s.webhookEventRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		events, err := s.webhookEventRepo.List(ctx, persistence.WebhookEventFilter{
			PageSize: limit,
		})
		if err != nil {
			s.logger.Warn().Err(err).Msg("failed to load webhook audit events")
		} else {
			data.WebhookEvents = events
		}
	}

	s.render(w, "audit.html", data)
}

// ArtifactDownload serves an artifact file for download.
