package service

// Coverage-uplift sweep (2026-06-18). Complements
// project_wizard_template_test.go (which drives catalogTemplateSource
// end-to-end against a loaded catalog) and project_wizard_model_test.go
// (resolveWizardModel) by pinning the defensive branches the happy-path
// tests don't reach:
//   - catalogTemplateSource with a nil catalog: Lookup → (zero, false),
//     Materialise → error (the degraded "templates not installed" path).
//   - catalogTemplateSource.Materialise on an unknown slug → error.
//   - newProjectWizardAdapter(nil) → nil (handler 503 contract).
//
// No filesystem fixtures needed for the nil-cat paths; the unknown-slug
// case reuses a minimal loaded catalog. No DB/LLM.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/templates"
)

func TestCatalogTemplateSource_NilCatalog(t *testing.T) {
	src := catalogTemplateSource{cat: nil}

	spec, ok := src.Lookup("anything")
	assert.False(t, ok, "nil catalog must not resolve a slug")
	assert.Empty(t, spec.Slug)

	_, err := src.Materialise("anything", map[string]string{"x": "y"})
	require.Error(t, err, "nil catalog must refuse materialisation")
}

func TestCatalogTemplateSource_UnknownSlug(t *testing.T) {
	// Load an empty catalog (missing dir → empty, no error) so cat is
	// non-nil but Get(slug) misses.
	cat, err := templates.Load(t.TempDir())
	require.NoError(t, err)
	src := catalogTemplateSource{cat: cat}

	_, ok := src.Lookup("missing")
	assert.False(t, ok)

	_, err = src.Materialise("missing", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown template")
}

func TestNewProjectWizardAdapter_NilWizardReturnsNil(t *testing.T) {
	// nil wizard → nil adapter so the commit/converse handlers 503
	// rather than nil-deref into a missing wizard.
	assert.Nil(t, newProjectWizardAdapter(nil))
}
