package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// GetConfig handles GET /api/v1/config. Returns the effective Config
// with secret-bearing fields redacted. Used by vornikctl config show
// (and future debugging UIs) so operators can see what the daemon
// actually loaded without having to decode YAML + env-var substitutions
// by hand.
//
// Redaction is implemented with a small allowlist of field-name tokens
// ("password", "api_key", "token", "secret", "bot_token"). A field
// matches if its lowercased JSON / map key contains any of those
// tokens. This is deliberately conservative: any future secret-bearing
// field that uses one of these obvious names is redacted automatically
// without requiring a coordinated code change. Non-secret fields that
// happen to contain "token" as a substring (e.g. max_tokens) are
// excluded via a short explicit denylist.
//
// Since 2026-09-03 the config served is the reload-written snapshot when one
// is wired (resolved-config provenance design §4.1): the boot-time pointer
// s.config is never swapped on hot reload, so before this the dump could show
// a config the daemon was no longer running. `?provenance=true` returns the
// per-key origin view instead of the plain dump — the plain dump's shape is
// unchanged for existing callers.
func (s *Server) GetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if !s.requireOperatorScope(w, r) {
		return
	}
	cfg, prov := s.effectiveConfig()
	if cfg == nil {
		respondError(w, http.StatusServiceUnavailable, "CONFIG_UNAVAILABLE", "config not wired into API server")
		return
	}
	if wantProvenance(r) || wantTrees(r) {
		var view ProvenanceView
		if wantProvenance(r) {
			if prov == nil {
				respondError(w, http.StatusServiceUnavailable, "PROVENANCE_UNAVAILABLE",
					"this daemon recorded no provenance for its configuration (started before the loader captured it, or loaded without a file)")
				return
			}
			view = provenanceView(cfg, prov)
		} else if prov != nil {
			view.ConfigPath, view.LoadedAt = prov.Path, prov.LoadedAt
		}
		if wantTrees(r) {
			if s.projectRegistry == nil {
				respondError(w, http.StatusServiceUnavailable, "TREES_UNAVAILABLE", "registry not wired into API server")
				return
			}
			view.Trees = s.projectRegistry.TreeIndex()
		}
		respondJSON(w, http.StatusOK, view)
		return
	}

	// Marshal/unmarshal through generic JSON so we don't have to mirror
	// the whole config schema with parallel "redacted" struct tags.
	raw, err := json.Marshal(cfg)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "encode config: "+err.Error())
		return
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "decode config: "+err.Error())
		return
	}
	redacted := redactSecrets(generic)
	respondJSON(w, http.StatusOK, redacted)
}

// effectiveConfig returns the config the daemon is running — the snapshot
// when wired, else the boot-time pointer — with its provenance if recorded.
func (s *Server) effectiveConfig() (*config.Config, *config.Provenance) {
	if snap := s.configSnapshot.Load(); snap != nil {
		return snap.Config, snap.Provenance
	}
	return s.config, nil
}

func wantProvenance(r *http.Request) bool { return queryFlag(r, "provenance") }

// wantTrees — `?trees=true` adds the registry's whole-tree index: which file
// supplied each project, swarm, workflow and role, from which layer, and the
// files the loader refused (design §4.2).
func wantTrees(r *http.Request) bool { return queryFlag(r, "trees") }

func queryFlag(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// ProvenanceEntry is one key of the provenance view: the resolved value
// (redacted by the same rule as the plain dump), and where it came from.
type ProvenanceEntry struct {
	Value  any           `json:"value"`
	Origin config.Origin `json:"origin"`
	Source string        `json:"source,omitempty"`
}

// ProvenanceView is the `?provenance=true` / `?trees=true` response. Values
// is present with provenance; Trees with trees; both when both are asked.
type ProvenanceView struct {
	ConfigPath string                     `json:"config_path,omitempty"`
	LoadedAt   time.Time                  `json:"loaded_at,omitempty"`
	Values     map[string]ProvenanceEntry `json:"values,omitempty"`
	Trees      *registry.TreeIndex        `json:"trees,omitempty"`
}

// provenanceView renders every walked leaf with its origin. Redaction goes
// through secrets.RedactConfig by KEY — the values are folded into a nested
// map by dotted path, redacted, and flattened back — so a redacted key keeps
// its origin and source: the variable NAME that supplied a password is not a
// secret; its value is.
func provenanceView(cfg *config.Config, prov *config.Provenance) ProvenanceView {
	nested := map[string]any{}
	config.WalkLeaves(reflect.ValueOf(cfg).Elem(), func(key string, _ reflect.StructField, v reflect.Value) {
		for v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		var val any
		if v.IsValid() && (v.Kind() != reflect.Pointer || !v.IsNil()) {
			val = v.Interface()
		}
		// Round-trip through JSON so nested collections render as the plain
		// dump renders them, and the redactor sees maps it understands.
		if b, err := json.Marshal(val); err == nil {
			var generic any
			if json.Unmarshal(b, &generic) == nil {
				val = generic
			}
		}
		setNested(nested, strings.Split(key, "."), val)
	})
	redacted, _ := redactSecrets(nested).(map[string]any)
	out := ProvenanceView{ConfigPath: prov.Path, LoadedAt: prov.LoadedAt, Values: map[string]ProvenanceEntry{}}
	for key, origin := range prov.Values {
		val, _ := getNested(redacted, strings.Split(key, "."))
		out.Values[key] = ProvenanceEntry{Value: val, Origin: origin.Origin, Source: origin.Source}
	}
	return out
}

func setNested(m map[string]any, path []string, val any) {
	for i, seg := range path {
		if i == len(path)-1 {
			m[seg] = val
			return
		}
		next, ok := m[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[seg] = next
		}
		m = next
	}
}

func getNested(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for _, seg := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// redactSecrets walks a JSON-decoded value and blanks any map values
// whose keys look secret. Lifted into internal/secrets.RedactConfig
// (2026-07, fix-it doctor task 3.1) so the grounding-bundle assembler
// reuses the exact same masking logic instead of forking it; this is a
// thin package-local alias so the two existing call sites in this
// package (GetConfig below, support_report_handlers.go) don't need to
// change.
func redactSecrets(v any) any {
	return secrets.RedactConfig(v)
}

// ensureConfigRefAvailable is a build-time reminder that the server
// must hold a config reference for this handler to work. It is a no-op
// at runtime and exists only so dropping the field from api.Server
// surfaces as a compile error here.
var _ = func(s *Server) *config.Config { return s.config }
