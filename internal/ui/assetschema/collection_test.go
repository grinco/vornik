package assetschema

import (
	"reflect"
	"testing"
)

// A Collection declares a repeating sub-object (Swarm roles[],
// Workflow steps{}) edited as a list of cards. Each item is rendered by
// the same generic renderer via the collection's ItemSchema; identity
// for surgical patching is the IDField. These tests pin the shape the
// renderer and drift-guard depend on.

func TestCollection_ItemSchemaAndLookup(t *testing.T) {
	s := AssetSchema{
		Asset: "swarm",
		Collections: []Collection{{
			Path:     "roles",
			Singular: "role",
			IDField:  "name",
			Ordered:  true,
			ItemSchema: AssetSchema{Sections: []Section{{Fields: []Field{
				{Path: "name", Kind: KindString, Required: true},
				{Path: "model", Kind: KindString},
			}}}},
		}},
	}

	c, ok := s.CollectionByPath("roles")
	if !ok {
		t.Fatal("CollectionByPath(roles) not found")
	}
	if c.IDField != "name" {
		t.Errorf("IDField = %q, want name", c.IDField)
	}
	if got := c.ItemSchema.Paths(); !reflect.DeepEqual(got, []string{"name", "model"}) {
		t.Errorf("ItemSchema.Paths() = %v, want [name model]", got)
	}
	if _, ok := s.CollectionByPath("missing"); ok {
		t.Error("CollectionByPath(missing) should not be found")
	}
}

// CollectionPaths returns the top-level keys collections occupy — the
// drift-guard treats those as covered (they're edited via the item
// editor, not as a scalar field).
func TestCollectionPaths(t *testing.T) {
	s := AssetSchema{Collections: []Collection{{Path: "roles"}, {Path: "steps"}}}
	if got := s.CollectionPaths(); !reflect.DeepEqual(got, []string{"roles", "steps"}) {
		t.Errorf("CollectionPaths() = %v, want [roles steps]", got)
	}
}
