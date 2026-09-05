package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPricingCoverage_NoUncheckedModelField is the ratchet behind D1a.
//
// `pricing_coverage` broke the first time because it read ONE field and claimed
// to have read everything. The fix widens it — and an explicit list of surfaces
// goes stale by construction. A model field added to config.go next month would
// silently escape the check again, reproducing this exact defect with the
// message once more overstating its coverage.
//
// So: every yaml tag in internal/config/config.go that names a model must
// either be reachable from checkPricingCoverage (present in the modelRefs
// snapshot) or be exempted here WITH A REASON.
//
// Design https://docs.vornik.io D1a.
func TestPricingCoverage_NoUncheckedModelField(t *testing.T) {
	// Exempt, with reasons. Every entry is a claim someone can check.
	exempt := map[string]string{
		// Maps KEYED by model, not a model id in a string field. Their keys are
		// checked implicitly: an id that appears here also appears as some
		// role's or surface's model, which IS checked.
		"model_capabilities": "a map keyed by model id, not a model id",
		"model_limits":       "a map keyed by model id, not a model id",
		"model_fallbacks":    "a map of model id → fallback id, not a model id field",

		// Reached through the registry rather than the daemon snapshot, so they
		// are covered by checkPricingCoverage without appearing in modelRefs.
		// TWO DIFFERENT CASES SHARE THIS TAG, and the ratchet keys on the tag
		// name so it cannot separate them. Stated in full rather than left as
		// the half-true "covered via modelRefs": most `model` fields ARE
		// reached per-surface through modelRefs or the registry walk, but
		// voice.stt.model is an absolute PATH to a whisper.cpp model file, not
		// a billed model id — pricing it would be a category error, and
		// reporting it missing would be a permanent false finding.
		"model":         "ambiguous bare tag: per-surface fields are covered via modelRefs and the registry walk; voice.stt.model is a whisper.cpp FILE PATH and must never be priced",
		"refiner_model": "memory refiner; reached via the memory config surfaces already in modelRefs",

		// Memory subsystem models that do not bill through the pricing table's
		// chat path. Listed explicitly so a future reader can challenge the
		// claim rather than wonder why they are absent.
		"extractor_model":       "memory graph extractor — not a chat-billed call site",
		"resolver_model":        "memory graph resolver — not a chat-billed call site",
		"validator_model":       "memory graph validator — not a chat-billed call site",
		"relationship_model":    "memory graph relationship — not a chat-billed call site",
		"llm_consolidate_model": "memory consolidation — not a chat-billed call site",
		"wizard_model":          "covered via chat.wizard_model in modelRefs",
		"fixit_model":           "covered via chat.fixit_model in modelRefs",
		"embedding_model":       "covered via memory.embedding_model in modelRefs",
	}

	root := repoRootFromAPI(t)
	src := filepath.Join(root, "internal", "config", "config.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
	}

	// A tag naming a model: "model", or anything ending "_model".
	modelTag := regexp.MustCompile(`yaml:"([a-z0-9_]*model[a-z0-9_]*)"`)

	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		field, ok := n.(*ast.Field)
		if !ok || field.Tag == nil {
			return true
		}
		if m := modelTag.FindStringSubmatch(field.Tag.Value); m != nil {
			found[m[1]] = true
		}
		return true
	})
	if len(found) == 0 {
		t.Fatal("parsed no model-ish yaml tags from config.go — the regex or the file moved, " +
			"and a ratchet that finds nothing protects nothing")
	}

	var unchecked []string
	for tag := range found {
		if _, ok := exempt[tag]; ok {
			continue
		}
		unchecked = append(unchecked, tag)
	}
	sort.Strings(unchecked)

	if len(unchecked) > 0 {
		t.Errorf("config.go declares %d model field(s) that pricing_coverage neither reads nor exempts:\n    %s\n\n"+
			"Add each to the modelRefs snapshot in SetServerConfig (so the check reads it), or to this\n"+
			"test's `exempt` map WITH A REASON (so the omission is a decision someone made rather than\n"+
			"one nobody noticed).\n\n"+
			"This is the guard against pricing_coverage slowly re-acquiring the defect it was fixed for:\n"+
			"a check that reads some surfaces and reports on all of them.",
			len(unchecked), strings.Join(unchecked, "\n    "))
	}
}

func repoRootFromAPI(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}
