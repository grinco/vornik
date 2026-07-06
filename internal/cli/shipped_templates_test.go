package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"vornik.io/vornik/internal/templates"
)

// shippedTemplateSamples supplies one representative parameter set
// per shipped template. EVERY template in configs/project-templates
// MUST have an entry — the harness fails otherwise, so adding a
// template forces adding its smoke coverage (spec §Testing Phase 1).
var shippedTemplateSamples = map[string]map[string][]string{
	"news-feed": {
		"projectId": {"smoke-news"}, "displayName": {"Smoke"},
		"topic": {"testing"}, "interval": {"4h"}, "llmModel": {""},
	},
	"personal-assistant": {
		"projectId": {"smoke-pa"}, "displayName": {"Smoke"},
		"llmModel": {""},
	},
	"companion": {
		"projectId": {"smoke-comp"}, "displayName": {"Smoke"},
		"defaultModel": {""},
	},
	"report-pipeline": {
		"projectId": {"smoke-report"}, "displayName": {"Smoke Report"},
		"sources": {"https://example.com/feed-a", "https://example.com/feed-b"},
		"cadence": {"24h"}, "llmModel": {""},
	},
	"docs-rag-sync": {
		"projectId": {"smoke-docs"}, "displayName": {"Smoke Docs"},
		"docSources": {"https://docs.example.com"}, "cadence": {"24h"}, "llmModel": {""},
	},
	"tool-assistant": {
		"projectId": {"smoke-tools"}, "displayName": {"Smoke Tools"},
		"assistantPurpose": {"testing assistant"},
		"mcpServers":       {}, // offline CLI: multiselect optional, may be empty
		"llmModel":         {""},
	},
	"code-reviewer": {
		"projectId": {"smoke-reviewer"}, "displayName": {"Smoke Reviewer"},
		"repo": {"acme/widgets"}, "llmModel": {""},
	},
	"custom-base": {
		"projectId": {"smoke-custom"}, "displayName": {"Smoke Custom"}, "llmModel": {""},
	},
}

func repoConfigsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "configs")
}

// allTemplateSlugs lists every template directory under
// configs/project-templates that has a template.yaml, regardless of
// its `hidden` flag.
//
// Regression: template-bundles-v2 final review — the smoke gate used
// to derive its slug set from Catalog.List(), which deliberately
// excludes hidden templates (gallery filtering). That left a blind
// spot: a future hidden template would ship with zero smoke coverage
// and no test failure to catch it. Walking the directory directly
// (instead of adding an exported Catalog.ListAll) keeps templates'
// public API unchanged while still finding every manifest on disk.
func allTemplateSlugs(t *testing.T, configsDir string) []string {
	t.Helper()
	root := filepath.Join(configsDir, "project-templates")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	var slugs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(root, entry.Name(), "template.yaml")); statErr != nil {
			continue
		}
		slugs = append(slugs, entry.Name())
	}
	return slugs
}

// TestShippedTemplatesMaterialiseAndValidate is the CI smoke gate
// from project-creation-e2e-design §Testing: every shipped template
// must materialise with representative params and pass sandbox
// registry validation (project + swarm + workflow cross-refs).
func TestShippedTemplatesMaterialiseAndValidate(t *testing.T) {
	configsDir := repoConfigsDir(t)
	cat, err := templates.Load(filepath.Join(configsDir, "project-templates"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	seen := map[string]bool{}
	for _, slug := range allTemplateSlugs(t, configsDir) {
		seen[slug] = true
	}
	for slug := range shippedTemplateSamples {
		if _, ok := cat.Get(slug); !ok {
			t.Errorf("sample exists for %q but template is missing", slug)
		}
	}
	for slug := range seen {
		params, ok := shippedTemplateSamples[slug]
		if !ok {
			t.Errorf("template %q has no smoke sample — add it to shippedTemplateSamples", slug)
			continue
		}
		m, _ := cat.Get(slug)
		var rendered map[string]string
		var rerr error
		if m.NeedsMultiValue() {
			rendered, rerr = cat.MaterialiseFilesMulti(m, params, nil)
		} else {
			flat := map[string]string{}
			for k, v := range params {
				if len(v) > 0 {
					flat[k] = v[len(v)-1]
				}
			}
			rendered, rerr = cat.MaterialiseFiles(m, flat)
		}
		if rerr != nil {
			t.Errorf("%s: materialise: %v", slug, rerr)
			continue
		}
		if err := validateRenderedTemplate(configsDir, rendered); err != nil {
			t.Errorf("%s: registry validation: %v", slug, err)
		}
	}
}

// TestToolAssistantEmptyMCPServers verifies the critical render behavior:
// with mcpServers empty, the rendered project.yaml contains NO mcp: block.
func TestToolAssistantEmptyMCPServers(t *testing.T) {
	configsDir := repoConfigsDir(t)
	cat, err := templates.Load(filepath.Join(configsDir, "project-templates"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	m, ok := cat.Get("tool-assistant")
	if !ok {
		t.Fatal("tool-assistant template not found")
	}

	// Render with empty mcpServers
	params := map[string][]string{
		"projectId":        {"test-empty"},
		"displayName":      {"Test Empty"},
		"assistantPurpose": {"test purpose"},
		"mcpServers":       {}, // empty
		"llmModel":         {""},
	}

	rendered, err := cat.MaterialiseFilesMulti(m, params, nil)
	if err != nil {
		t.Fatalf("materialise with empty mcpServers: %v", err)
	}

	projectFile := rendered["projects/test-empty.yaml"]
	if strings.Contains(projectFile, "mcp:") {
		t.Errorf("empty mcpServers: mcp: block should not appear in rendered project.yaml, but found:\n%s", projectFile)
	}
}

// TestToolAssistantWithMCPServers verifies the render with servers:
// selected servers appear as - name: "<server>" entries under
// mcp.servers, AND — regression: template-bundles-v2 final review —
// that this shape actually passes sandbox registry validation. The
// main smoke gate above only exercises tool-assistant with an EMPTY
// mcpServers sample (offline CLI default), so the with-servers render
// never went through validateRenderedTemplate; a future manifest or
// template change that broke the populated-servers path would have
// shipped undetected.
func TestToolAssistantWithMCPServers(t *testing.T) {
	configsDir := repoConfigsDir(t)
	cat, err := templates.Load(filepath.Join(configsDir, "project-templates"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	m, ok := cat.Get("tool-assistant")
	if !ok {
		t.Fatal("tool-assistant template not found")
	}

	// Render with non-empty mcpServers
	params := map[string][]string{
		"projectId":        {"test-with-servers"},
		"displayName":      {"Test With Servers"},
		"assistantPurpose": {"test purpose"},
		"mcpServers":       {"server1", "server2"},
		"llmModel":         {""},
	}

	rendered, err := cat.MaterialiseFilesMulti(m, params, nil)
	if err != nil {
		t.Fatalf("materialise with servers: %v", err)
	}

	projectFile := rendered["projects/test-with-servers.yaml"]
	if !strings.Contains(projectFile, "mcp:") {
		t.Errorf("mcp: block missing when servers are selected")
	}
	if !strings.Contains(projectFile, `- name: "server1"`) {
		t.Errorf("server1 entry missing in mcp.servers")
	}
	if !strings.Contains(projectFile, `- name: "server2"`) {
		t.Errorf("server2 entry missing in mcp.servers")
	}

	if err := validateRenderedTemplate(configsDir, rendered); err != nil {
		t.Errorf("tool-assistant with mcpServers set: registry validation: %v", err)
	}
}

// TestCustomBase_HiddenFromGalleryButGettable verifies the hidden:true
// semantics: custom-base must not appear in Catalog.List() but must
// be resolvable via Catalog.Get().
func TestCustomBase_HiddenFromGalleryButGettable(t *testing.T) {
	configsDir := repoConfigsDir(t)
	cat, err := templates.Load(configsDir + "/project-templates")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	for _, m := range cat.List() {
		if m.Slug == "custom-base" {
			t.Error("custom-base must not appear in the gallery list")
		}
	}
	_, ok := cat.Get("custom-base")
	if !ok {
		t.Fatal("custom-base must be resolvable by slug")
	}
}
