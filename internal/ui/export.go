// Helpers for ?format=csv|json export of spend tables.

package ui

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
)

// exportFormat reads ?format= from the request and returns "csv", "json",
// or "" for the default HTML render.
func exportFormat(r *http.Request) string {
	switch r.URL.Query().Get("format") {
	case "csv":
		return "csv"
	case "json":
		return "json"
	default:
		return ""
	}
}

// writeCSV writes rows with headers as CSV. The first slice in rows is
// treated as the header line.
func writeCSV(w http.ResponseWriter, filename string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	cw := csv.NewWriter(w)
	for _, r := range rows {
		_ = cw.Write(r)
	}
	cw.Flush()
}

// writeJSON marshals v with indentation and serves it as application/json.
// Errors fall back to a 500 — the caller has already logged.
func writeJSON(w http.ResponseWriter, filename string, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}
