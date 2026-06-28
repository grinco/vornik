package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	codexCompanionPluginDir       = "../../contrib/codex-companion"
	codexCompanionMarketplacePath = "../../.agents/plugins/marketplace.json"
)

func TestCodexCompanionMarketplace_Parses(t *testing.T) {
	body, err := os.ReadFile(codexCompanionMarketplacePath)
	require.NoError(t, err, ".agents/plugins/marketplace.json must exist for Codex marketplace installs")

	var mp struct {
		Name      string `json:"name"`
		Interface struct {
			DisplayName string `json:"displayName"`
		} `json:"interface"`
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Source string `json:"source"`
				Path   string `json:"path"`
			} `json:"source"`
			Policy struct {
				Installation   string `json:"installation"`
				Authentication string `json:"authentication"`
			} `json:"policy"`
			Category string `json:"category"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(body, &mp), "Codex marketplace must be valid JSON")
	assert.Equal(t, "vornik", mp.Name)
	assert.Equal(t, "Vornik", mp.Interface.DisplayName)
	require.NotEmpty(t, mp.Plugins)

	var found bool
	for _, p := range mp.Plugins {
		if p.Name != "codex-companion" {
			continue
		}
		found = true
		assert.Equal(t, "local", p.Source.Source)
		assert.Equal(t, "./contrib/codex-companion", p.Source.Path)
		assert.Equal(t, "AVAILABLE", p.Policy.Installation)
		assert.Equal(t, "ON_INSTALL", p.Policy.Authentication)
		assert.NotEmpty(t, p.Category)
	}
	assert.True(t, found, "Codex marketplace must list codex-companion")
}

func TestCodexCompanionMarketplace_PluginSourceResolves(t *testing.T) {
	body, err := os.ReadFile(codexCompanionMarketplacePath)
	require.NoError(t, err)

	var mp struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Source string `json:"source"`
				Path   string `json:"path"`
			} `json:"source"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(body, &mp))

	marketplaceRoot := filepath.Dir(filepath.Dir(filepath.Dir(codexCompanionMarketplacePath)))
	for _, p := range mp.Plugins {
		if p.Source.Source != "local" {
			continue
		}
		resolved := filepath.Clean(filepath.Join(marketplaceRoot, p.Source.Path))
		manifest := filepath.Join(resolved, ".codex-plugin", "plugin.json")
		mcpConfig := filepath.Join(resolved, ".mcp.json")
		assert.NoErrorf(t, statRegularFile(manifest),
			"Codex marketplace plugin %q points at %q but no Codex manifest exists at %s",
			p.Name, p.Source.Path, manifest)
		assert.NoErrorf(t, statRegularFile(mcpConfig),
			"Codex marketplace plugin %q points at %q but no MCP config exists at %s",
			p.Name, p.Source.Path, mcpConfig)
	}
}

func statRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}

func TestCodexCompanionPlugin_ManifestParses(t *testing.T) {
	manifestPath := filepath.Join(codexCompanionPluginDir, ".codex-plugin", "plugin.json")
	body, err := os.ReadFile(manifestPath)
	require.NoErrorf(t, err,
		"manifest must exist at %s; Codex discovers plugins by .codex-plugin/plugin.json",
		manifestPath)

	var manifest struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		Skills      string `json:"skills"`
		MCPServers  string `json:"mcpServers"`
		Author      struct {
			Name string `json:"name"`
		} `json:"author"`
		Interface struct {
			DisplayName      string   `json:"displayName"`
			ShortDescription string   `json:"shortDescription"`
			LongDescription  string   `json:"longDescription"`
			DeveloperName    string   `json:"developerName"`
			Category         string   `json:"category"`
			Capabilities     []string `json:"capabilities"`
			DefaultPrompt    string   `json:"defaultPrompt"`
		} `json:"interface"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest), "plugin.json must be valid JSON")
	assert.Equal(t, "codex-companion", manifest.Name)
	assert.NotEmpty(t, manifest.Version)
	assert.NotEmpty(t, manifest.Description)
	assert.Equal(t, "./skills/", manifest.Skills)
	assert.Equal(t, "./.mcp.json", manifest.MCPServers)
	assert.Equal(t, "vornik", manifest.Author.Name)
	assert.Equal(t, "Vornik Companion for Codex", manifest.Interface.DisplayName)
	assert.NotEmpty(t, manifest.Interface.ShortDescription)
	assert.NotEmpty(t, manifest.Interface.LongDescription)
	assert.NotEmpty(t, manifest.Interface.DeveloperName)
	assert.NotEmpty(t, manifest.Interface.Category)
	assert.NotEmpty(t, manifest.Interface.Capabilities)
	assert.NotEmpty(t, manifest.Interface.DefaultPrompt)
}

func TestCodexCompanionPlugin_ManifestNotAtRoot(t *testing.T) {
	root := filepath.Join(codexCompanionPluginDir, "plugin.json")
	_, err := os.Stat(root)
	assert.Truef(t, os.IsNotExist(err),
		"plugin.json must not exist at the plugin root; the Codex manifest belongs at .codex-plugin/plugin.json")
}

func TestCodexCompanionPlugin_MCPConfigShape(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(codexCompanionPluginDir, ".mcp.json"))
	require.NoError(t, err, ".mcp.json must ship at the plugin root")

	var mcp struct {
		MCPServers map[string]struct {
			Type              string `json:"type"`
			URL               string `json:"url"`
			BearerTokenEnvVar string `json:"bearer_token_env_var"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(body, &mcp), ".mcp.json must be valid JSON")

	srv, ok := mcp.MCPServers["vornik"]
	require.Truef(t, ok, ".mcp.json must declare mcpServers.vornik; got: %v", mcp.MCPServers)
	assert.Equal(t, "http", srv.Type)
	assert.Equal(t, "http://localhost:8080/api/v1/mcp/companion", srv.URL)
	assert.Equal(t, "VORNIK_COMPANION_TOKEN", srv.BearerTokenEnvVar)
}

func TestCodexCompanionPlugin_SkillPresent(t *testing.T) {
	path := filepath.Join(codexCompanionPluginDir, "skills", "delegate", "SKILL.md")
	body, err := os.ReadFile(path)
	require.NoError(t, err, "Codex delegate skill missing")
	text := string(body)
	assert.True(t, strings.HasPrefix(text, "---\n"), "SKILL.md must lead with YAML frontmatter")
	assert.Contains(t, text, "name: delegate")
	assert.Contains(t, text, "mcp__vornik__delegate")
	assert.Contains(t, text, "inputArtifacts")
	assert.Contains(t, text, "repo_scope")
}

// TestCodexCompanionPlugin_ScopeDerivationGuidance pins the Part-B client
// hardening: because Codex ships no SessionStart scope injector, the
// always-on defaultPrompt AND the delegate skill must both teach the model
// to DERIVE the canonical repo_scope from the git remote (not hand-guess
// it). This is the regression guard for the NULL/drifted-scope leak that
// produced github.com/easeit/vornik-ee chunks (2026-06-28).
func TestCodexCompanionPlugin_ScopeDerivationGuidance(t *testing.T) {
	// defaultPrompt must carry the always-on scope rule with a concrete
	// derivation source — it fires even when the delegate skill isn't loaded.
	manifestPath := filepath.Join(codexCompanionPluginDir, ".codex-plugin", "plugin.json")
	body, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	var manifest struct {
		Interface struct {
			DefaultPrompt string `json:"defaultPrompt"`
		} `json:"interface"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest))
	dp := manifest.Interface.DefaultPrompt
	assert.Contains(t, dp, "repo_scope", "defaultPrompt must mention repo_scope")
	assert.Contains(t, dp, "remote.origin.url",
		"defaultPrompt must point at the git remote as the scope-derivation source")

	// The delegate skill must spell out the normalization recipe so the
	// token matches the canonical scope already in memory.
	skillPath := filepath.Join(codexCompanionPluginDir, "skills", "delegate", "SKILL.md")
	sb, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	skill := string(sb)
	assert.Contains(t, skill, "remote.origin.url",
		"skill must instruct deriving the scope from the git remote")
	assert.Contains(t, skill, ".git",
		"skill must describe stripping the trailing .git in normalization")
}

func TestCodexCompanionPlugin_NoClaudeOnlySurfaces(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("hooks", "hooks.json"),
		filepath.Join("hooks", "session-start.sh"),
		"commands",
		".claude-plugin",
	} {
		path := filepath.Join(codexCompanionPluginDir, rel)
		_, err := os.Stat(path)
		assert.Truef(t, os.IsNotExist(err),
			"Codex plugin should not ship Claude-only surface %s; use MCP tools and skills instead", path)
	}
}
