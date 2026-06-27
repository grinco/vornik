package ui

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/ui/assetschema"
)

// schemaAssetSpec parameterises saveSchemaAsset with the per-asset bits of the
// schema-driven save pipeline.
//
// SCOPE: collection-based schema saves ONLY (swarm roles, workflow steps).
// ProjectSchemaConfigSave and any future non-collection asset must NOT adopt
// this primitive — they have no collection reconcile or body-prompt rewrite,
// which is the bulk of what this skeleton centres on. Forcing them in with
// no-op hooks would trade real clarity for a false sense of unification.
//
// Error-message contract: parseValidate returns an error whose .Error() is
// ALREADY the exact user-facing message (parser-prefix trimmed on a parse
// error, raw on a validate error) — the skeleton renders it verbatim. reconcile
// returns its RAW error; the skeleton wraps it with the collection name
// ("Failed to reconcile <collPath>: …"). The other skeleton-OWNED steps
// (read/split/apply/write) format their own messages with assetNoun to
// reproduce the original per-asset wording byte-for-byte.
type schemaAssetSpec struct {
	template     string                  // server-rendered template name
	schema       assetschema.AssetSchema // SwarmSchema() | WorkflowSchema()
	collPath     string                  // "roles" | "steps"
	bindKeyValue bool                    // workflow binds the synthetic map-key value; swarm doesn't
	assetPath    string                  // on-disk file: data.SwarmPath | data.WorkflowPath
	assetNoun    string                  // "swarm" | "workflow" — reproduces error text only

	split          func(content []byte, base string) (fm, body []byte, err error)
	join           func(fm, body []byte) []byte
	reconcile      func(fm []byte, coll assetschema.Collection, items []boundRoleItem) (outFM []byte, prompts map[string]string, err error)
	replacePrompts func(body []byte, prompts map[string]string, keep map[string]bool) ([]byte, error)
	parseValidate  func(joined []byte, base string) error

	renderErr func(code int, msg string, values, errs map[string]string) // closes over the concrete data
	renderOK  func(backupPath string)                                    // closes over the concrete data
	audit     func(itemCount int)                                        // closes over r + asset id
}

// saveSchemaAsset runs the shared schema-driven save pipeline for a
// collection-based asset. Behaviour-preserving extraction of
// SwarmSchemaConfigSave / WorkflowSchemaConfigSave (steps 3–14 of the pipeline
// documented in https://docs.vornik.io).
//
// PRECONDITION: the caller has already run the admin guard, loaded the editor
// data successfully (no data.Error), and handled the 404 render. This method
// assumes a valid, existing asset on disk. The response writer is not a
// parameter — every render path goes through the spec's renderErr / renderOK
// closures (which capture the caller's w), so the skeleton never touches it.
func (s *Server) saveSchemaAsset(r *http.Request, spec schemaAssetSpec) {
	base := filepath.Base(spec.assetPath)
	coll, _ := spec.schema.CollectionByPath(spec.collPath)

	// Step 3: parse the form.
	if err := r.ParseForm(); err != nil {
		spec.renderErr(http.StatusBadRequest, "Failed to parse form: "+err.Error(), nil, nil)
		return
	}

	// Step 4: bind top-level fields + collection items into values/errs.
	values, errs, topValues, items, topErrs, itemErrs := s.bindSchemaSaveForm(r, spec, coll)

	// Step 5: reject on field errors (all collected so the operator sees them at once).
	if len(errs) > 0 {
		spec.renderErr(http.StatusBadRequest, joinFieldErrors(topErrs, itemErrs), values, errs)
		return
	}

	// Step 6: read the on-disk file.
	existing, err := os.ReadFile(spec.assetPath)
	if err != nil {
		spec.renderErr(http.StatusInternalServerError, fmt.Sprintf("Failed to read existing %s: %v", spec.assetNoun, err), values, errs)
		return
	}

	// Step 7: split frontmatter / body.
	frontmatter, body, err := spec.split(existing, base)
	if err != nil {
		spec.renderErr(http.StatusBadRequest, fmt.Sprintf("Failed to split %s file: %v", spec.assetNoun, err), values, errs)
		return
	}

	// Step 8: apply top-level frontmatter patches under the field-allowlist guard.
	fmPatches := schemaPatches(spec.schema, topValues)
	if err := schemaTopLevelGuard(spec.schema).Check(topLevelPatchKeys(fmPatches)); err != nil {
		spec.renderErr(http.StatusBadRequest, "Refused: "+err.Error(), values, errs)
		return
	}
	newFM, err := applyYAMLPatches(frontmatter, fmPatches)
	if err != nil {
		spec.renderErr(http.StatusBadRequest, fmt.Sprintf("Failed to apply %s edits: %v", spec.assetNoun, err), values, errs)
		return
	}

	// Step 9: reconcile the collection into the frontmatter + collect body
	// prompt updates. The hook returns the raw reconcile error; the skeleton
	// wraps it with the collection name to reproduce the original per-asset
	// message ("Failed to reconcile roles/steps: <err>").
	newFM, prompts, err := spec.reconcile(newFM, coll, items)
	if err != nil {
		spec.renderErr(http.StatusBadRequest, fmt.Sprintf("Failed to reconcile %s: %v", spec.collPath, err), values, errs)
		return
	}

	// Step 10: rewrite body prompts, keeping only surviving items.
	keep := make(map[string]bool, len(items))
	for _, it := range items {
		keep[it.ID] = true
	}
	newBody, err := spec.replacePrompts(body, prompts, keep)
	if err != nil {
		spec.renderErr(http.StatusBadRequest, "Failed to apply prompt edits: "+err.Error(), values, errs)
		return
	}

	// Step 11: join → parse → validate. parseValidate's error is user-facing
	// and pre-formatted (parser prefix trimmed on parse, raw on validate).
	joined := spec.join(newFM, newBody)
	if err := spec.parseValidate(joined, base); err != nil {
		spec.renderErr(http.StatusBadRequest, err.Error(), values, errs)
		return
	}

	// Step 12: atomic write (returns backup path).
	backupPath, err := writeProjectConfigAtomic(spec.assetPath, joined)
	if err != nil {
		spec.renderErr(http.StatusInternalServerError, fmt.Sprintf("Failed to write %s: %v", spec.assetNoun, err), values, errs)
		return
	}

	// Step 13: hot-reload (full reloader if wired, else registry reload).
	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			spec.renderErr(http.StatusConflict, reloadErrMsg(err, backupPath), values, errs)
			return
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			spec.renderErr(http.StatusConflict, reloadErrMsg(err, backupPath), values, errs)
			return
		}
	}

	// Step 14: audit + success render.
	spec.audit(len(items))
	spec.renderOK(backupPath)
}

// bindSchemaSaveForm binds the submitted form into the trimmed values map (for
// re-render), the field-error map, and the structured slices the pipeline
// needs downstream (topValues for patches; items for reconcile; topErrs +
// itemErrs for the combined error message). Extracted from saveSchemaAsset so
// the skeleton stays under the complexity ratchet; behaviour is identical to
// the binding block both handlers ran inline.
func (s *Server) bindSchemaSaveForm(r *http.Request, spec schemaAssetSpec, coll assetschema.Collection) (
	values, errs map[string]string,
	topValues []assetschema.FieldValue,
	items []boundRoleItem,
	topErrs, itemErrs []assetschema.FieldError,
) {
	values = map[string]string{}
	errs = map[string]string{}

	topValues, topErrs = assetschema.BindForm(spec.schema, r.FormValue)
	for _, f := range spec.schema.Fields() {
		if f.ReadOnly {
			continue
		}
		values[f.Path] = strings.TrimSpace(r.FormValue(f.Path))
	}
	for _, fe := range topErrs {
		errs[fe.Path] = fe.Message
	}

	items, itemErrs = bindCollectionItems(coll, r)
	for _, it := range items {
		if spec.bindKeyValue {
			full := fmt.Sprintf("%s[%d].%s", coll.Path, it.Index, coll.IDField)
			values[full] = strings.TrimSpace(r.FormValue(full))
		}
		for _, fv := range it.Values {
			full := fmt.Sprintf("%s[%d].%s", coll.Path, it.Index, fv.Path)
			values[full] = strings.TrimSpace(r.FormValue(full))
		}
	}
	for _, fe := range itemErrs {
		errs[fe.Path] = fe.Message
	}
	return values, errs, topValues, items, topErrs, itemErrs
}
