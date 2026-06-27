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

// Tests that lock the on-disk vornik-companion plugin contract to the
// daemon-side MCP server. Lives in internal/api/ so a regression in
// the tool surface (rename, removed tool) blows up the same package
// that owns the server contract — not "somewhere else under contrib/".
//
// The plugin directory is contrib/claude-code-companion/; relative
// from this test file that's ../../contrib/claude-code-companion/.

const (
	companionPluginDir     = "../../contrib/claude-code-companion"
	companionMarketplaceFp = "../../.claude-plugin/marketplace.json"
)

// TestCompanionMarketplace_Parses — the repo-root marketplace.json
// is what makes the plugin installable via /plugin marketplace add
// + /plugin install. Pin its shape so a future edit can't silently
// detach the marketplace from the plugin directory. Lint by hand
// with: claude plugin validate .
func TestCompanionMarketplace_Parses(t *testing.T) {
	body, err := os.ReadFile(companionMarketplaceFp)
	require.NoError(t, err, ".claude-plugin/marketplace.json must exist at repo root for /plugin marketplace add to discover it")

	var mp struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Owner       struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"owner"`
		Plugins []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      any    `json:"source"`
			Category    string `json:"category"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(body, &mp), "marketplace.json must be valid JSON")
	assert.Equal(t, "vornik", mp.Name,
		"marketplace name is part of the public contract — operators install as vornik-companion@vornik")
	assert.NotEmpty(t, mp.Owner.Name)
	require.NotEmpty(t, mp.Plugins, "marketplace must list at least one plugin")

	// The vornik-companion entry must exist + point at the
	// directory the plugin actually lives in. A typo here would
	// install a 'not found' entry.
	var found bool
	for _, p := range mp.Plugins {
		if p.Name != "vornik-companion" {
			continue
		}
		found = true
		src, ok := p.Source.(string)
		require.Truef(t, ok,
			"marketplace plugin source must be a string path; got %T", p.Source)
		assert.Equal(t, "./contrib/claude-code-companion", src,
			"source path must match the plugin's on-disk location relative to the marketplace.json")
		assert.NotEmpty(t, p.Description)
	}
	assert.True(t, found, "marketplace must list a 'vornik-companion' plugin entry")
}

// TestCompanionMarketplace_PluginSourceResolves — the source: path
// in the marketplace must actually resolve to a real plugin directory
// with a manifest. Catches the failure mode where someone renames the
// plugin dir but forgets to update the marketplace.
func TestCompanionMarketplace_PluginSourceResolves(t *testing.T) {
	body, err := os.ReadFile(companionMarketplaceFp)
	require.NoError(t, err)
	var mp struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source any    `json:"source"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(body, &mp))
	for _, p := range mp.Plugins {
		src, ok := p.Source.(string)
		if !ok {
			continue // git-subdir form, skipped
		}
		// Per Claude Code's marketplace spec, source paths resolve
		// relative to the marketplace ROOT (the directory containing
		// .claude-plugin/), NOT the .claude-plugin/ directory itself.
		// So strip one level off Dir(marketplaceFp) to get the
		// marketplace root, then join.
		marketplaceRoot := filepath.Dir(filepath.Dir(companionMarketplaceFp))
		resolved := filepath.Join(marketplaceRoot, src)
		manifest := filepath.Join(resolved, ".claude-plugin", "plugin.json")
		_, err := os.Stat(manifest)
		assert.NoErrorf(t, err,
			"marketplace plugin %q points at %q but no plugin manifest at %s",
			p.Name, src, manifest)
	}
}

// TestCompanionPlugin_ManifestParses — plugin.json must live at
// .claude-plugin/plugin.json (per the Claude Code plugin spec) and
// be valid JSON with the canonical name + version fields. Catches
// regressions in either the location (we shipped it at the wrong
// path in the first cut, 2026-05-27) or the contents.
func TestCompanionPlugin_ManifestParses(t *testing.T) {
	manifestPath := filepath.Join(companionPluginDir, ".claude-plugin", "plugin.json")
	body, err := os.ReadFile(manifestPath)
	require.NoErrorf(t, err,
		"manifest must exist at %s — Claude Code discovers plugins by this path; "+
			"a manifest at the plugin root won't be recognised",
		manifestPath)

	var manifest struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		Author      struct {
			Name string `json:"name"`
		} `json:"author"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest), "plugin.json must be valid JSON")
	assert.Equal(t, "vornik-companion", manifest.Name,
		"plugin name is part of the public contract; renaming requires marketplace coordination "+
			"AND would change the slash-command namespace (e.g. /vornik-companion:delegate)")
	assert.NotEmpty(t, manifest.Version)
	assert.NotEmpty(t, manifest.Description)
	assert.NotEmpty(t, manifest.Author.Name,
		"author must be a {name: ...} object per the plugin spec, not a bare string")
}

// TestCompanionPlugin_ManifestNotAtRoot — the docs are explicit: only
// .claude-plugin/plugin.json is recognised, never a bare plugin.json
// at the plugin root. Pin that here so a future operator doesn't
// "tidy up" by moving the manifest back and silently break discovery.
func TestCompanionPlugin_ManifestNotAtRoot(t *testing.T) {
	root := filepath.Join(companionPluginDir, "plugin.json")
	_, err := os.Stat(root)
	assert.Truef(t, os.IsNotExist(err),
		"plugin.json must NOT exist at the plugin root — the manifest belongs at "+
			".claude-plugin/plugin.json. Found unexpected root manifest at %s", root)
}

// TestCompanionPlugin_SlashCommandsPresent — every command the README
// references must exist on disk with YAML frontmatter. Command file
// names are intentionally short (`delegate.md`, not `vornik-delegate.md`)
// because Claude Code's plugin namespace already prefixes them as
// `/vornik-companion:delegate` — a redundant prefix would surface as
// /vornik-companion:vornik-delegate.
func TestCompanionPlugin_SlashCommandsPresent(t *testing.T) {
	commands := []string{
		"delegate.md",
		"peek.md",
		"status.md",
		"result.md",
		"review.md",
	}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(companionPluginDir, "commands", cmd))
			require.NoError(t, err, "command file missing: %s", cmd)
			// Frontmatter MUST start at byte 0 with a line of `---\n`.
			// The Claude Code parser is strict about this; a BOM or
			// a leading blank line silently breaks the command.
			require.True(t, strings.HasPrefix(string(body), "---\n"),
				"command file %s must start with YAML frontmatter (`---\\n`)", cmd)
			// `description:` is the user-facing palette entry; the
			// CLI lists commands by that field, so an empty line
			// here means an invisible command.
			assert.Containsf(t, string(body), "description:",
				"command %s must declare a description in its frontmatter", cmd)
		})
	}
}

// TestCompanionPlugin_MCPConfigShape — the .mcp.json is what wires
// Claude Code's MCP client at the daemon's companion endpoint. Both
// env var references (VORNIK_URL with the localhost fallback,
// VORNIK_COMPANION_TOKEN without one) are part of the operator
// contract documented in README.md; pin them so a tidy edit can't
// silently drop either.
func TestCompanionPlugin_MCPConfigShape(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(companionPluginDir, ".mcp.json"))
	require.NoError(t, err, ".mcp.json must ship at the plugin root")

	var mcp struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(body, &mcp), ".mcp.json must be valid JSON")

	srv, ok := mcp.MCPServers["vornik"]
	require.Truef(t, ok, ".mcp.json must declare an mcpServers.vornik entry; got: %v", mcp.MCPServers)
	assert.Equal(t, "http", srv.Type, "transport must be 'http'")
	assert.Contains(t, srv.URL, "/api/v1/mcp/companion",
		"URL must point at the daemon's companion endpoint")
	assert.Contains(t, srv.URL, "${VORNIK_URL",
		"URL must reference VORNIK_URL so operators can target a remote daemon")
	auth := srv.Headers["Authorization"]
	assert.Contains(t, auth, "Bearer ", "Authorization header must be a Bearer token")
	assert.Contains(t, auth, "${VORNIK_COMPANION_TOKEN",
		"token must come from VORNIK_COMPANION_TOKEN env var — never hard-coded")
}

// TestCompanionPlugin_SessionStartHookExecutable — the SessionStart
// hook is the keystone of cross-session continuity; if the file isn't
// executable, Claude Code silently skips it and the magic disappears.
func TestCompanionPlugin_SessionStartHookExecutable(t *testing.T) {
	path := filepath.Join(companionPluginDir, "hooks", "session-start.sh")
	info, err := os.Stat(path)
	require.NoError(t, err, "session-start.sh missing")
	mode := info.Mode().Perm()
	assert.NotZero(t, mode&0o100,
		"session-start.sh must have the owner-execute bit set (chmod +x); got mode %o", mode)
}

// TestCompanionPlugin_HookManifestWiresSessionStart — pins the
// 2026-05-28 "0 hooks reported" regression. The session-start.sh
// script existed on disk but no hooks/hooks.json declared it, so
// Claude Code's plugin loader never registered it and the
// SessionStart digest never fired. Operators ran "/reload-plugins"
// and silently lost the digest + the per-session repo_scope
// directive. The fix is a hooks/hooks.json file whose
// hooks.SessionStart[].hooks[].command points at the script via
// the ${CLAUDE_PLUGIN_ROOT} placeholder.
func TestCompanionPlugin_HookManifestWiresSessionStart(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(companionPluginDir, "hooks", "hooks.json"))
	require.NoError(t, err,
		"hooks/hooks.json missing — the SessionStart script won't fire even though session-start.sh is on disk")

	var manifest struct {
		Hooks struct {
			SessionStart []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"SessionStart"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest), "hooks.json must parse as JSON")
	require.NotEmpty(t, manifest.Hooks.SessionStart,
		"hooks.SessionStart must declare at least one hook group; got: %s", body)
	var foundCommand bool
	for _, group := range manifest.Hooks.SessionStart {
		for _, h := range group.Hooks {
			if h.Type == "command" && strings.Contains(h.Command, "session-start.sh") {
				foundCommand = true
			}
		}
	}
	assert.True(t, foundCommand,
		"no command hook references session-start.sh; the script is dead code without a declared entry: %s", body)
	// The command MUST use ${CLAUDE_PLUGIN_ROOT} (or %{...} variants)
	// so the hook resolves to the install-dir copy of the script,
	// not a hard-coded operator path. Hard-coded paths break
	// portability across operators / OSes.
	assert.Contains(t, string(body), "${CLAUDE_PLUGIN_ROOT}",
		"hook command must use ${CLAUDE_PLUGIN_ROOT} so the script path resolves relative to the install location")
}

// TestCompanionPlugin_SessionStartEmitsJSONEnvelope pins the
// 2026-05-28 / v2.1.153 finding: Claude Code v2.1+ no longer
// renders raw stdout from a SessionStart hook in the welcome
// banner. The script must wrap its markdown in
//
//	{ "hookSpecificOutput": { "hookEventName": "SessionStart",
//	  "additionalContext": "<markdown>" } }
//
// for both the visual banner AND the model's additionalContext
// slot to populate. Pre-0.4.5 the hook printed raw markdown that
// reached the model's context (proven on n8n-prod by Claude
// resolving the correct repo_scope) but never showed in the
// banner. The official learning-output-style + claude-plugin-dev
// example plugins use this envelope; we match.
//
// The test is a contract-pin: it greps for the literal JSON keys
// the harness expects, not a deep semantic check. A script that
// drifts away from the envelope (e.g. someone re-adds raw `echo`
// at the bottom of build_digest without the wrapper) fails here.
func TestCompanionPlugin_SessionStartEmitsJSONEnvelope(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(companionPluginDir, "hooks", "session-start.sh"))
	require.NoError(t, err)
	text := string(body)

	for _, marker := range []string{
		`hookSpecificOutput`,
		`hookEventName`,
		`"SessionStart"`,
		`additionalContext`,
	} {
		assert.Contains(t, text, marker,
			"session-start.sh must emit a v2 JSON envelope keyed by %q; raw stdout silently fails to render in Claude Code v2.1+ welcome banner", marker)
	}
	// The envelope must be wrapped via jq -n so the markdown is
	// safely JSON-encoded (newlines escaped, quotes escaped). A
	// hand-rolled echo with literal JSON would break the moment a
	// chunk snippet contains a quote or backslash.
	assert.Contains(t, text, "jq -n",
		"the JSON envelope must be built via `jq -n --arg ctx ...` so the markdown is safely escaped")
}

// TestCompanionPlugin_HookReferencesMCPPath — the hook script must
// hit the same MCP endpoint path the daemon serves. A drift here
// (someone edits the daemon path but forgets the hook, or vice
// versa) breaks the SessionStart digest silently — the hook just
// times out. Pin the path string in both places.
func TestCompanionPlugin_HookReferencesMCPPath(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(companionPluginDir, "hooks", "session-start.sh"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "/api/v1/mcp/companion",
		"session-start.sh must hit /api/v1/mcp/companion — the daemon-side path is committed via routes.go")
}

// TestCompanionPlugin_SlashCommandsHaveNoStrayBackticksInBashBlock
// — pins the 2026-05-28 regression where /vornik-rag-ingest broke
// because an inline-code comment INSIDE the Python heredoc closed
// the surrounding markdown code-span early. Claude Code's slash
// command parser wraps the `!`-prefixed bash invocation in a
// markdown backtick span; any backtick inside the bash body closes
// that span before the real terminator, truncating the Python
// source and producing a "here-document delimited by EOF" error.
//
// Rule enforced here: between the opening `!`<backtick> and the
// closing standalone-line <backtick>, there must be no other
// backticks. Markdown prose backticks ABOVE the bash block are
// fine; backticks INSIDE the bash block are not. The fix is to
// use plain double-quotes in Python comments and string literals.
func TestCompanionPlugin_SlashCommandsHaveNoStrayBackticksInBashBlock(t *testing.T) {
	commandsDir := filepath.Join(companionPluginDir, "commands")
	entries, err := os.ReadDir(commandsDir)
	require.NoError(t, err, "commands/ must exist for slash-command discovery")

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(commandsDir, name))
			require.NoError(t, err)
			text := string(body)

			// Find the bash invocation: the line that starts with !`
			// (markdown shell-block prefix). Not every command needs
			// one — recall/remember/etc. are model-driven and have
			// no bash. Skip files without an "!`" prefix.
			openIdx := strings.Index(text, "\n!`")
			if openIdx == -1 {
				t.Skipf("no !`...` bash block in %s — model-driven command, nothing to lint", name)
				return
			}
			// Advance past "\n!`" to point at the first byte of
			// the bash body.
			bodyStart := openIdx + len("\n!`")

			// The closing backtick is on its own line near the
			// file's end (the convention is a single ` on a line
			// by itself). Find the LAST single-backtick line in
			// the file; that's the terminator.
			closeIdx := strings.LastIndex(text, "\n`\n")
			if closeIdx == -1 || closeIdx <= bodyStart {
				// Fallback: terminating backtick may be at EOF
				// without a trailing newline; check that too.
				closeIdx = strings.LastIndex(text, "\n`")
			}
			require.Greaterf(t, closeIdx, bodyStart,
				"%s opens a bash block at offset %d but no terminating `\\n`\\n` line found; the markdown-parser will keep reading past EOF and Claude Code will reject the command",
				name, openIdx)

			bashBody := text[bodyStart:closeIdx]
			if strings.ContainsRune(bashBody, '`') {
				// Surface the first offending line so the failure
				// message points at the actual culprit, not a
				// boolean "somewhere".
				lines := strings.Split(bashBody, "\n")
				for i, line := range lines {
					if strings.ContainsRune(line, '`') {
						t.Fatalf("%s:line %d of bash block contains a backtick that will close the surrounding markdown code-span early and break slash-command parsing.\nLine: %q\nFix: replace the inline-code backticks with plain double-quotes — Python comments and string literals render fine without them.\n(2026-05-28 incident: this exact pattern broke /vornik-rag-ingest argument parsing.)",
							name, i+1, line)
					}
				}
			}
		})
	}
}

// TestCompanionPlugin_SkillPresent — the skills/delegate
// directory must contain SKILL.md with the canonical name. The
// skill is what teaches the host LLM when to reach for the
// companion rather than spending its own tokens. (The dir + name
// dropped the redundant `vornik-` prefix in plugin v0.5.0, commit
// 434166ed — the vornik-companion namespace already conveys it.)
func TestCompanionPlugin_SkillPresent(t *testing.T) {
	path := filepath.Join(companionPluginDir, "skills", "delegate", "SKILL.md")
	body, err := os.ReadFile(path)
	require.NoError(t, err, "SKILL.md missing — host LLMs won't know when to delegate")
	assert.True(t, strings.HasPrefix(string(body), "---\n"),
		"SKILL.md must lead with YAML frontmatter")
	assert.Contains(t, string(body), "name: delegate",
		"skill name field is the public contract — renaming requires marketplace coordination")
}
