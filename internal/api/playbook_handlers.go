package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/playbook"
)

// Playbook handles GET /api/v1/playbook and GET /api/v1/playbook/{class}.
// The classless form returns the full corpus for the failed-task UI's
// class index; the per-class form is what `vornikctl playbook show` calls.
//
// No auth or project scoping — playbook content is daemon-version-pinned
// metadata, not per-tenant data. Adding scoping later (e.g. per-deploy
// custom suggestions) won't break the wire shape; consumers ignore
// unknown fields.
func (s *Server) Playbook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Path is /api/v1/playbook or /api/v1/playbook/{class}. Trim the
	// fixed prefix and treat anything left as the class identifier.
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/playbook")
	rest = strings.TrimPrefix(rest, "/")
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if rest == "" {
		_ = enc.Encode(map[string]any{"entries": playbook.All()})
		return
	}
	_ = enc.Encode(playbook.Lookup(rest))
}
