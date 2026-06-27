package assetschema

// Collection declares a repeating sub-object an asset owns — a Swarm's
// ordered roles[] sequence, a Workflow's steps{} map. Each item is
// rendered as a collapsible card by the SAME generic renderer used for
// the top-level form, driven by ItemSchema. Surgical patching keys on
// IDField (a yaml key present in every item, e.g. "name"), so an edit
// survives reorder and preserves per-item comments.
type Collection struct {
	// Path is the top-level yaml key the collection occupies
	// (e.g. "roles", "steps"); the patch target for add/remove.
	Path string
	// Singular labels the add control ("Add role") and per-card UI.
	Singular string
	// Title / Help are the section heading + operator-facing blurb.
	Title string
	Help  string
	// IDField is the form-field key (relative) that carries an item's
	// identity. For a sequence (roles[]) it is a real item-schema field
	// matched by value (e.g. "name"). For a map (steps{}) it is the
	// synthetic map-key handle (e.g. "stepId") — NOT a struct leaf, so it
	// lives outside ItemSchema and is rendered/bound separately.
	IDField string
	// KeyIsMapKey is true for a map collection (steps{}): IDField names
	// the YAML map key, which is the item's identity and is never patched
	// into the item's value. False for a sequence: IDField is a real
	// value field.
	KeyIsMapKey bool
	// KeyLabel / KeyHelp label the synthetic key input rendered at the
	// top of each card for a map collection. Unused when KeyIsMapKey is
	// false (the identity field is part of ItemSchema).
	KeyLabel string
	KeyHelp  string
	// Ordered is true for a yaml sequence (roles[] — reorder matters)
	// and false for a map (steps{} — key is identity, order cosmetic).
	Ordered bool
	// ItemSchema is the sections+fields for ONE item; its field Paths
	// are relative to the item (e.g. "model", "runtime.image"). Reusing
	// AssetSchema lets the generic renderer draw a card with no new code.
	ItemSchema AssetSchema
	// ItemDeferredPaths are item leaves intentionally NOT given a form
	// field yet (opaque/complex blocks). Tracked by the drift-guard so a
	// deferred leaf isn't confused with a missing one — shrink over time.
	ItemDeferredPaths []string
}

// CollectionByPath returns the collection occupying the given top-level
// yaml key.
func (s AssetSchema) CollectionByPath(path string) (Collection, bool) {
	for _, c := range s.Collections {
		if c.Path == path {
			return c, true
		}
	}
	return Collection{}, false
}

// CollectionPaths returns the top-level keys collections occupy, in
// declaration order. The drift-guard treats these as covered — they are
// edited via the item editor, not as a scalar field.
func (s AssetSchema) CollectionPaths() []string {
	out := make([]string, 0, len(s.Collections))
	for _, c := range s.Collections {
		out = append(out, c.Path)
	}
	return out
}
