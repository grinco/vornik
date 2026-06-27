package ui

// Schema-driven swarm editor — UI asset-management feature phase 2 (P2).
// See https://docs.vornik.io
//
// Adds full, collection-aware swarm editing on top of the schema spine:
// the top-level swarm fields plus a card per role, every role field
// rendered from the SwarmRole item-schema. The save reconciles the whole
// roles[] sequence (add / remove / reorder + per-item patches, by name)
// and routes each role's systemPrompt to the SWARM.md body — reusing the
// existing split → patch frontmatter → reconcile → body-surgery → join →
// validate → write → reload pipeline. Comments, key order, and unrelated
// body sections survive because nothing unmarshal/remarshals the file.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/ui/assetschema"
)

// schemaFieldRow is one rendered field: the schema Field plus its full
// HTML name (path, or roles[i].path for a collection item), current
// value, and any per-field error. The template branches on Field's kind
// predicates; it never builds names or looks up values itself.
type schemaFieldRow struct {
	Field assetschema.Field
	Name  string
	Value string
	Err   string
}

// schemaSectionView groups rows under a heading for the renderer.
type schemaSectionView struct {
	Title    string
	Help     string
	Advanced bool
	Rows     []schemaFieldRow
}

// swarmRoleCard is one role's editable card.
type swarmRoleCard struct {
	Index    int
	Name     string
	Sections []schemaSectionView
}

// SwarmSchemaConfigData backs the schema-driven swarm editor template.
type SwarmSchemaConfigData struct {
	Title       string
	CurrentPage string
	SwarmID     string
	SwarmPath   string
	Error       string
	Success     string
	BackupPath  string

	AssistProjectID string

	TopSections    []schemaSectionView
	RolesTitle     string
	RolesHelp      string
	RolesSingular  string
	RoleCards      []swarmRoleCard
	BlankCardIndex int
}

// buildSchemaSections renders an asset (or item) schema's sections to
// view rows. prefix is "" for the top-level form or "roles[<i>]" for a
// collection item; values/errs are keyed by the resulting full name.
func buildSchemaSections(schema assetschema.AssetSchema, prefix string, values, errs map[string]string) []schemaSectionView {
	out := make([]schemaSectionView, 0, len(schema.Sections))
	for _, sec := range schema.Sections {
		sv := schemaSectionView{Title: sec.Title, Help: sec.Help, Advanced: sec.Advanced}
		for _, f := range sec.Fields {
			name := f.Path
			if prefix != "" {
				name = prefix + "." + f.Path
			}
			sv.Rows = append(sv.Rows, schemaFieldRow{Field: f, Name: name, Value: values[name], Err: errs[name]})
		}
		out = append(out, sv)
	}
	return out
}

// SwarmSchemaConfigEdit renders the schema-driven swarm editor (GET).
func (s *Server) SwarmSchemaConfigEdit(w http.ResponseWriter, r *http.Request, swarmID string) {
	data := s.swarmSchemaConfigData(swarmID)
	data.AssistProjectID = r.URL.Query().Get("projectId")
	data.AssistProjectID = assistProjectFromRequest(data.AssistProjectID, s.defaultAssistProjectForSwarm(swarmID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "swarm_schema_config.html", data)
}

// SwarmSchemaRoleCard renders a single blank role card for the htmx
// "Add role" control. The index comes from ?index= so the new card's
// field names don't collide with existing ones.
func (s *Server) SwarmSchemaRoleCard(w http.ResponseWriter, r *http.Request, swarmID string) {
	idx, _ := strconv.Atoi(r.URL.Query().Get("index"))
	if idx < 0 {
		idx = 0
	}
	coll, _ := assetschema.SwarmSchema().CollectionByPath("roles")
	prefix := fmt.Sprintf("roles[%d]", idx)
	card := swarmRoleCard{
		Index:    idx,
		Sections: buildSchemaSections(coll.ItemSchema, prefix, defaultsAsValues(coll.ItemSchema, prefix), nil),
	}
	s.render(w, "swarm_role_card.html", card)
}

// SwarmSchemaConfigSave binds the top-level fields + every role card,
// reconciles the roles sequence, rewrites body prompts, validates, writes
// atomically, reloads, and audits. Any failure re-renders with the
// operator's input + a precise error; nothing is written unless every
// step succeeds.
func (s *Server) SwarmSchemaConfigSave(w http.ResponseWriter, r *http.Request, swarmID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	data := s.swarmSchemaConfigData(swarmID)
	data.AssistProjectID = r.URL.Query().Get("projectId")
	data.AssistProjectID = assistProjectFromRequest(data.AssistProjectID, s.defaultAssistProjectForSwarm(swarmID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "swarm_schema_config.html", data)
		return
	}
	s.saveSchemaAsset(r, schemaAssetSpec{
		template:  "swarm_schema_config.html",
		schema:    assetschema.SwarmSchema(),
		collPath:  "roles",
		assetPath: data.SwarmPath,
		assetNoun: "swarm",
		split:     registry.SplitSwarmContent,
		join:      registry.JoinSwarmContent,
		reconcile: func(fm []byte, coll assetschema.Collection, items []boundRoleItem) ([]byte, map[string]string, error) {
			reconcileItems, prompts := roleReconcile(coll, items)
			// Keep only the surviving roles' prompt bodies — a removed role's
			// `### name` subsection must go too, or the parser rejects the orphan.
			out, err := reconcileYAMLSequence(fm, coll.Path, coll.IDField, reconcileItems)
			if err != nil {
				return nil, nil, err // skeleton wraps with "Failed to reconcile <collPath>: …"
			}
			return out, prompts, nil
		},
		replacePrompts: registry.ReplaceSwarmRolePromptsKeeping,
		parseValidate: func(joined []byte, base string) error {
			parsed, err := registry.ParseSwarmMarkdown(joined, base)
			if err != nil {
				return fmt.Errorf("%s", trimSwarmParserPrefix(err.Error()))
			}
			return parsed.Validate(base)
		},
		renderErr: func(code int, msg string, values, errs map[string]string) {
			s.renderSwarmSchemaError(w, &data, code, msg, values, errs)
		},
		renderOK: func(backupPath string) {
			data.BackupPath = backupPath
			data.Success = "Swarm saved and reloaded."
			if backupPath != "" {
				data.Success += " Backup: " + backupPath
			}
			// Re-render from the freshly-reloaded swarm.
			if sw := s.projectReg.GetSwarm(swarmID); sw != nil {
				s.populateSwarmSchemaData(&data, sw)
			}
			s.render(w, "swarm_schema_config.html", data)
		},
		audit: func(itemCount int) {
			s.writeSwarmSaveAudit(r, swarmID, itemCount)
		},
	})
}

// renderSwarmSchemaError re-renders the editor with an error banner, the
// operator's submitted values, and per-field errors. Rebuilds the cards
// from the submitted state so input is preserved on a rejected save.
func (s *Server) renderSwarmSchemaError(w http.ResponseWriter, data *SwarmSchemaConfigData, code int, msg string, values, errs map[string]string) {
	data.Error = msg
	// Rebuild the form from the submitted state so input is preserved on a
	// rejected save. nil values means the caller has nothing to re-bind (e.g.
	// a parse-form failure before any binding) — leave the data the loader
	// already populated untouched. Mirrors renderWorkflowSchemaError.
	if values != nil {
		schema := assetschema.SwarmSchema()
		coll, _ := schema.CollectionByPath("roles")
		data.TopSections = buildSchemaSections(schema, "", values, errs)
		data.RoleCards = cardsFromValues(coll, values, errs)
		data.BlankCardIndex = len(data.RoleCards)
	}
	w.WriteHeader(code)
	s.render(w, "swarm_schema_config.html", *data)
}

// swarmSchemaConfigData builds the initial render-state. Mirrors
// swarmEditData's validation (invalid id / missing registry / missing
// swarm → Error set; caller short-circuits).
func (s *Server) swarmSchemaConfigData(swarmID string) SwarmSchemaConfigData {
	data := SwarmSchemaConfigData{
		Title:       "Swarm (schema): " + swarmID,
		CurrentPage: "swarms",
		SwarmID:     swarmID,
	}
	if swarmID == "" || strings.Contains(swarmID, "/") || strings.Contains(swarmID, string(os.PathSeparator)) {
		data.Error = "Invalid swarm id"
		return data
	}
	configDir := s.configDir()
	if configDir == "" {
		data.Error = "Registry config directory is not configured"
		return data
	}
	if s.projectReg == nil {
		data.Error = "Project registry not configured"
		return data
	}
	sw := s.projectReg.GetSwarm(swarmID)
	if sw == nil {
		data.Error = "Swarm not found"
		return data
	}
	data.SwarmPath = filepath.Join(configDir, "swarms", swarmID+".md")
	if _, err := os.Stat(data.SwarmPath); err != nil {
		data.Error = "Swarm file not found: " + err.Error()
		return data
	}
	s.populateSwarmSchemaData(&data, sw)
	return data
}

// populateSwarmSchemaData fills the render model from a parsed *Swarm.
func (s *Server) populateSwarmSchemaData(data *SwarmSchemaConfigData, sw *registry.Swarm) {
	schema := assetschema.SwarmSchema()
	coll, _ := schema.CollectionByPath("roles")

	values := map[string]string{
		"swarmId":     sw.ID,
		"displayName": sw.DisplayName,
		"leadRole":    sw.LeadRole,
		"rolePrelude": sw.RolePrelude,
	}
	cards := make([]swarmRoleCard, 0, len(sw.Roles))
	for i, role := range sw.Roles {
		prefix := fmt.Sprintf("roles[%d]", i)
		for rel, val := range structToValues(coll.ItemSchema, role) {
			values[prefix+"."+rel] = val
		}
		cards = append(cards, swarmRoleCard{
			Index:    i,
			Name:     role.Name,
			Sections: buildSchemaSections(coll.ItemSchema, prefix, values, nil),
		})
	}
	data.TopSections = buildSchemaSections(schema, "", values, nil)
	data.RolesTitle = coll.Title
	data.RolesHelp = coll.Help
	data.RolesSingular = coll.Singular
	data.RoleCards = cards
	data.BlankCardIndex = len(sw.Roles)
}

// structToValues maps any registry struct to schema-relative display
// values by round-tripping it through YAML — reusing
// assetschema.CurrentValues so list/scalar formatting matches the
// top-level form exactly. Used for swarm roles, workflow steps, and
// top-level workflow fields.
func structToValues(schema assetschema.AssetSchema, v any) map[string]string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return map[string]string{}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return map[string]string{}
	}
	return assetschema.CurrentValues(schema, doc)
}

// defaultsAsValues seeds a blank card's values from each field's Default.
func defaultsAsValues(itemSchema assetschema.AssetSchema, prefix string) map[string]string {
	out := map[string]string{}
	for _, f := range itemSchema.Fields() {
		if f.Default != "" {
			out[prefix+"."+f.Path] = f.Default
		}
	}
	return out
}

// cardsFromValues rebuilds role cards from a submitted values map (used
// on a rejected save so the operator's input is preserved). Card indices
// are recovered from the value keys, in ascending order.
func cardsFromValues(coll assetschema.Collection, values, errs map[string]string) []swarmRoleCard {
	idxSet := map[int]bool{}
	prefix := coll.Path + "["
	for k := range values {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		end := strings.IndexByte(rest, ']')
		if end <= 0 {
			continue
		}
		if n, err := strconv.Atoi(rest[:end]); err == nil {
			idxSet[n] = true
		}
	}
	idxs := make([]int, 0, len(idxSet))
	for i := range idxSet {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	cards := make([]swarmRoleCard, 0, len(idxs))
	for _, i := range idxs {
		p := fmt.Sprintf("%s[%d]", coll.Path, i)
		cards = append(cards, swarmRoleCard{
			Index:    i,
			Name:     values[p+"."+coll.IDField],
			Sections: buildSchemaSections(coll.ItemSchema, p, values, errs),
		})
	}
	return cards
}

// boundRoleItem is one bound role card: its submitted index, identity
// value, and type-checked field values.
type boundRoleItem struct {
	Index  int
	ID     string
	Values []assetschema.FieldValue
}

// bindCollectionItems binds every submitted card of a collection against
// its item-schema. Field errors are namespaced by full HTML name so the
// renderer can attach them to the right input.
func bindCollectionItems(coll assetschema.Collection, r *http.Request) (items []boundRoleItem, errs []assetschema.FieldError) {
	for _, idx := range collectionIndices(r.Form, coll.Path) {
		get := func(p string) string {
			return r.FormValue(fmt.Sprintf("%s[%d].%s", coll.Path, idx, p))
		}
		vals, ferrs := assetschema.BindForm(coll.ItemSchema, get)
		id := strings.TrimSpace(get(coll.IDField))
		// For a map collection the identity is the synthetic map key,
		// which lives outside ItemSchema (so BindForm doesn't validate
		// it) — enforce required here.
		if coll.KeyIsMapKey && id == "" {
			ferrs = append(ferrs, assetschema.FieldError{
				Path: coll.IDField, Label: coll.KeyLabel,
				Message: coll.KeyLabel + " is required",
			})
		}
		if len(ferrs) > 0 {
			for _, fe := range ferrs {
				fe.Path = fmt.Sprintf("%s[%d].%s", coll.Path, idx, fe.Path)
				errs = append(errs, fe)
			}
			continue
		}
		items = append(items, boundRoleItem{Index: idx, ID: id, Values: vals})
	}
	return items, errs
}

// collectionIndices returns the distinct card indices present in the form
// for a collection path, ascending — the submitted order of the cards.
func collectionIndices(form map[string][]string, path string) []int {
	prefix := path + "["
	seen := map[int]bool{}
	for k := range form {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		end := strings.IndexByte(rest, ']')
		if end <= 0 {
			continue
		}
		if n, err := strconv.Atoi(rest[:end]); err == nil && n >= 0 {
			seen[n] = true
		}
	}
	out := make([]int, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// roleReconcile splits each bound item into a frontmatter reconcile item
// (its non-body field patches) and a body prompt update (its body-backed
// field, e.g. systemPrompt) keyed by role name.
func roleReconcile(coll assetschema.Collection, items []boundRoleItem) ([]collectionItem, map[string]string) {
	reconcile := make([]collectionItem, 0, len(items))
	prompts := make(map[string]string, len(items))
	for _, it := range items {
		reconcile = append(reconcile, collectionItem{ID: it.ID, Patches: schemaPatches(coll.ItemSchema, it.Values)})
		for _, fv := range it.Values {
			if f, ok := coll.ItemSchema.FieldByPath(fv.Path); ok && f.IsBody() {
				s, _ := fv.Value.(string)
				prompts[it.ID] = s
			}
		}
	}
	return reconcile, prompts
}

// joinFieldErrors flattens top-level + item field errors into one banner
// string.
func joinFieldErrors(a, b []assetschema.FieldError) string {
	msgs := make([]string, 0, len(a)+len(b))
	for _, fe := range a {
		msgs = append(msgs, fe.Message)
	}
	for _, fe := range b {
		msgs = append(msgs, fe.Message)
	}
	return strings.Join(msgs, "; ")
}

// reloadErrMsg formats the saved-but-reload-failed message with the
// backup path for recovery.
func reloadErrMsg(err error, backupPath string) string {
	msg := "Saved, but reload failed: " + err.Error()
	if backupPath != "" {
		msg += "\nBackup: " + backupPath
	}
	return msg
}

// writeSwarmSaveAudit records one admin-audit row for a successful save.
func (s *Server) writeSwarmSaveAudit(r *http.Request, swarmID string, roleCount int) {
	if s.adminAuditRepo == nil {
		return
	}
	principal := adminPrincipal(r)
	if principal == "" || principal == "unknown" {
		principal = "ui-admin"
	}
	_ = s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: principal,
		Source:    "ui",
		Action:    "swarm.save",
		Target:    swarmID,
		After:     fmt.Sprintf(`{"roles":%d}`, roleCount),
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
}
