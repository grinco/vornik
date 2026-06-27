package ui

// Schema-driven workflow editor — UI asset-management feature phase 3 (P3).
// See https://docs.vornik.io
//
// Collection-aware workflow editing over the steps{} MAP: top-level
// workflow fields plus a card per step, every step field rendered from
// the WorkflowStep item-schema. The step id is the YAML map key (a
// synthetic identity field, not a struct leaf), so add / remove / rename
// reconcile the steps mapping by key. A step's prompt is body-backed —
// routed to the WORKFLOW.md ## Prompts body, with the inline frontmatter
// `prompt:` removed so the body is canonical (the parser otherwise lets a
// frontmatter prompt silently override the body).
//
// Reuses the swarm editor's generic helpers: buildSchemaSections,
// bindCollectionItems, schemaPatches, schemaTopLevelGuard. The collection
// reconcile uses reconcileYAMLMapping (map) rather than the sequence path.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/ui/assetschema"
)

// WorkflowSchemaConfigData backs the schema-driven workflow editor.
type WorkflowSchemaConfigData struct {
	Title        string
	CurrentPage  string
	WorkflowID   string
	WorkflowPath string
	Error        string
	Success      string
	BackupPath   string

	AssistProjectID string

	TopSections    []schemaSectionView
	StepsTitle     string
	StepsHelp      string
	StepsSingular  string
	StepCards      []swarmRoleCard
	BlankCardIndex int
}

// WorkflowSchemaConfigEdit renders the schema-driven workflow editor (GET).
func (s *Server) WorkflowSchemaConfigEdit(w http.ResponseWriter, r *http.Request, workflowID string) {
	data := s.workflowSchemaConfigData(workflowID)
	data.AssistProjectID = assistProjectFromRequest(r.URL.Query().Get("projectId"), s.defaultAssistProjectForWorkflow(workflowID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "workflow_schema_config.html", data)
}

// WorkflowSchemaStepCard renders a blank step card for the htmx "Add
// step" control at the given index.
func (s *Server) WorkflowSchemaStepCard(w http.ResponseWriter, r *http.Request, workflowID string) {
	idx, _ := strconv.Atoi(r.URL.Query().Get("index"))
	if idx < 0 {
		idx = 0
	}
	coll, _ := assetschema.WorkflowSchema().CollectionByPath("steps")
	card := stepCard(coll, idx, defaultsAsValues(coll.ItemSchema, fmt.Sprintf("steps[%d]", idx)), nil)
	s.render(w, "workflow_step_card.html", card)
}

// WorkflowSchemaConfigSave binds top-level fields + every step card,
// reconciles the steps mapping (add/remove/rename by key), rewrites body
// prompts, validates, writes, reloads, audits.
func (s *Server) WorkflowSchemaConfigSave(w http.ResponseWriter, r *http.Request, workflowID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	data := s.workflowSchemaConfigData(workflowID)
	data.AssistProjectID = assistProjectFromRequest(r.URL.Query().Get("projectId"), s.defaultAssistProjectForWorkflow(workflowID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "workflow_schema_config.html", data)
		return
	}
	s.saveSchemaAsset(r, schemaAssetSpec{
		template:     "workflow_schema_config.html",
		schema:       assetschema.WorkflowSchema(),
		collPath:     "steps",
		bindKeyValue: true, // the step id is a synthetic map key, bound separately
		assetPath:    data.WorkflowPath,
		assetNoun:    "workflow",
		split:        registry.SplitWorkflowContent,
		join:         registry.JoinWorkflowContent,
		reconcile: func(fm []byte, coll assetschema.Collection, items []boundRoleItem) ([]byte, map[string]string, error) {
			mappingItems, prompts := stepReconcile(coll, items)
			out, err := reconcileYAMLMapping(fm, coll.Path, mappingItems)
			if err != nil {
				return nil, nil, err // skeleton wraps with "Failed to reconcile <collPath>: …"
			}
			return out, prompts, nil
		},
		replacePrompts: registry.ReplaceWorkflowStepPromptsKeeping,
		parseValidate: func(joined []byte, base string) error {
			parsed, err := registry.ParseWorkflowMarkdown(joined, base)
			if err != nil {
				return fmt.Errorf("%s", trimWorkflowParserPrefix(err.Error()))
			}
			return parsed.Validate(base)
		},
		renderErr: func(code int, msg string, values, errs map[string]string) {
			s.renderWorkflowSchemaError(w, &data, code, msg, values, errs)
		},
		renderOK: func(backupPath string) {
			data.BackupPath = backupPath
			data.Success = "Workflow saved and reloaded."
			if backupPath != "" {
				data.Success += " Backup: " + backupPath
			}
			if wf := s.projectReg.GetWorkflow(workflowID); wf != nil {
				s.populateWorkflowSchemaData(&data, wf)
			}
			s.render(w, "workflow_schema_config.html", data)
		},
		audit: func(itemCount int) {
			s.writeWorkflowSaveAudit(r, workflowID, itemCount)
		},
	})
}

func (s *Server) renderWorkflowSchemaError(w http.ResponseWriter, data *WorkflowSchemaConfigData, code int, msg string, values, errs map[string]string) {
	data.Error = msg
	schema := assetschema.WorkflowSchema()
	coll, _ := schema.CollectionByPath("steps")
	if values != nil {
		data.TopSections = buildSchemaSections(schema, "", values, errs)
		data.StepCards = stepCardsFromValues(coll, values, errs)
		data.BlankCardIndex = len(data.StepCards)
	}
	w.WriteHeader(code)
	s.render(w, "workflow_schema_config.html", *data)
}

func (s *Server) workflowSchemaConfigData(workflowID string) WorkflowSchemaConfigData {
	data := WorkflowSchemaConfigData{
		Title:       "Workflow (schema): " + workflowID,
		CurrentPage: "workflows",
		WorkflowID:  workflowID,
	}
	if workflowID == "" || strings.Contains(workflowID, "/") || strings.Contains(workflowID, string(os.PathSeparator)) {
		data.Error = "Invalid workflow id"
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
	wf := s.projectReg.GetWorkflow(workflowID)
	if wf == nil {
		data.Error = "Workflow not found"
		return data
	}
	data.WorkflowPath = filepath.Join(configDir, "workflows", workflowID+".md")
	if _, err := os.Stat(data.WorkflowPath); err != nil {
		data.Error = "Workflow file not found: " + err.Error()
		return data
	}
	s.populateWorkflowSchemaData(&data, wf)
	return data
}

func (s *Server) populateWorkflowSchemaData(data *WorkflowSchemaConfigData, wf *registry.Workflow) {
	schema := assetschema.WorkflowSchema()
	coll, _ := schema.CollectionByPath("steps")

	values := map[string]string{
		"workflowId": wf.ID,
	}
	// Top-level values via YAML round-trip → CurrentValues (matches the
	// project/swarm formatting for lists/scalars).
	for k, v := range structToValues(schema, wf) {
		values[k] = v
	}

	// Stable, lexicographic step order so the form doesn't reshuffle.
	ids := make([]string, 0, len(wf.Steps))
	for id := range wf.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	cards := make([]swarmRoleCard, 0, len(ids))
	for i, id := range ids {
		prefix := fmt.Sprintf("steps[%d]", i)
		values[prefix+"."+coll.IDField] = id
		step := wf.Steps[id]
		for rel, val := range structToValues(coll.ItemSchema, step) {
			values[prefix+"."+rel] = val
		}
		cards = append(cards, stepCard(coll, i, values, nil))
		_ = step
	}
	data.TopSections = buildSchemaSections(schema, "", values, nil)
	data.StepsTitle = coll.Title
	data.StepsHelp = coll.Help
	data.StepsSingular = coll.Singular
	data.StepCards = cards
	data.BlankCardIndex = len(ids)
}

// stepCard builds one step card: a synthetic key row (the map key) on top
// of the item-schema sections.
func stepCard(coll assetschema.Collection, index int, values, errs map[string]string) swarmRoleCard {
	prefix := fmt.Sprintf("%s[%d]", coll.Path, index)
	sections := []schemaSectionView{keySectionView(coll, prefix, values, errs)}
	sections = append(sections, buildSchemaSections(coll.ItemSchema, prefix, values, errs)...)
	return swarmRoleCard{
		Index:    index,
		Name:     values[prefix+"."+coll.IDField],
		Sections: sections,
	}
}

// keySectionView renders the synthetic map-key field as a one-row
// "Identity" section.
func keySectionView(coll assetschema.Collection, prefix string, values, errs map[string]string) schemaSectionView {
	f := assetschema.Field{
		Path: coll.IDField, Label: coll.KeyLabel, Kind: assetschema.KindString,
		Required: true, Help: coll.KeyHelp,
	}
	name := prefix + "." + coll.IDField
	return schemaSectionView{
		Title: "Identity",
		Rows:  []schemaFieldRow{{Field: f, Name: name, Value: values[name], Err: errs[name]}},
	}
}

// stepCardsFromValues rebuilds step cards from a submitted values map (on
// a rejected save) so operator input is preserved.
func stepCardsFromValues(coll assetschema.Collection, values, errs map[string]string) []swarmRoleCard {
	idxs := submittedIndices(coll.Path, values)
	cards := make([]swarmRoleCard, 0, len(idxs))
	for _, i := range idxs {
		cards = append(cards, stepCard(coll, i, values, errs))
	}
	return cards
}

// submittedIndices recovers the distinct card indices present in a values
// map for a collection path, ascending.
func submittedIndices(path string, values map[string]string) []int {
	prefix := path + "["
	seen := map[int]bool{}
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

// stepReconcile splits each bound step into a mapping reconcile item (its
// non-body frontmatter patches, plus a removal of any inline body-field
// key so the body stays canonical) and a body prompt update keyed by step
// id.
func stepReconcile(coll assetschema.Collection, items []boundRoleItem) ([]mappingItem, map[string]string) {
	reconcile := make([]mappingItem, 0, len(items))
	prompts := make(map[string]string, len(items))
	for _, it := range items {
		patches := schemaPatches(coll.ItemSchema, it.Values)
		for _, fv := range it.Values {
			f, ok := coll.ItemSchema.FieldByPath(fv.Path)
			if !ok || !f.IsBody() {
				continue
			}
			// Body-backed field: clear any inline frontmatter key so the
			// body wins, and route the value to the body editor.
			patches = append(patches, yamlPatch{Path: strings.Split(fv.Path, "."), Value: "", RemoveIfEmpty: true})
			sval, _ := fv.Value.(string)
			prompts[it.ID] = sval
		}
		reconcile = append(reconcile, mappingItem{Key: it.ID, Patches: patches})
	}
	return reconcile, prompts
}

func (s *Server) writeWorkflowSaveAudit(r *http.Request, workflowID string, stepCount int) {
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
		Action:    "workflow.save",
		Target:    workflowID,
		After:     fmt.Sprintf(`{"steps":%d}`, stepCount),
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
}
