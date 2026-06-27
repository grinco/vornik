package api

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
)

func TestCheckAPISecurityPosture_AuthEnabled(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetServerConfig(&config.Config{
		Server: config.ServerConfig{Address: "0.0.0.0:8080"},
		API:    config.APIConfig{AuthEnabled: true, APIKeys: []string{"real-strong-key"}},
	})
	got := h.checkAPISecurityPosture()
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "auth enabled")
}

func TestCheckAPISecurityPosture_LoopbackNoAuth(t *testing.T) {
	// Bare ":port" binds all interfaces (Go's net.Listen convention),
	// so it's intentionally NOT in this list — it's non-loopback.
	cases := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			h := &DoctorHandlers{}
			h.SetServerConfig(&config.Config{
				Server: config.ServerConfig{Address: addr},
				API:    config.APIConfig{AuthEnabled: false},
			})
			got := h.checkAPISecurityPosture()
			assert.Equal(t, "OK", got.Status, "loopback-only listen is acceptable for local dev")
		})
	}
}

func TestCheckAPISecurityPosture_BarePortIsNonLoopback(t *testing.T) {
	// `:8080` alias for 0.0.0.0:8080 in Go. Should flag as ERROR when
	// auth is off.
	h := &DoctorHandlers{}
	h.SetServerConfig(&config.Config{
		Server: config.ServerConfig{Address: ":8080"},
		API:    config.APIConfig{AuthEnabled: false},
	})
	got := h.checkAPISecurityPosture()
	assert.Equal(t, "ERROR", got.Status)
}

func TestCheckAPISecurityPosture_PublicNoAuth_IsError(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetServerConfig(&config.Config{
		Server: config.ServerConfig{Address: "0.0.0.0:8080"},
		API:    config.APIConfig{AuthEnabled: false},
	})
	got := h.checkAPISecurityPosture()
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "DISABLED")
}

func TestCheckAPIKeyStrength_WeakKeys(t *testing.T) {
	cases := []struct {
		name   string
		keys   []string
		want   string
		reason string
	}{
		{"short", []string{"abc123"}, "WARNING", "too short"},
		{"example", []string{"my-changeme-dev-key-placeholder-0000"}, "WARNING", "changeme"},
		{"whitespace", []string{"has a space in it and is long enough"}, "WARNING", "whitespace"},
		{"strong", []string{"zLkPQ7n8f8xZmWqR2sNv9tH4jYc3K6b1"}, "OK", ""},
		{"mixed", []string{"zLkPQ7nxf8xZmWqR9sNv4tH6jYc8K0bE", "changeme-this-is-long-enough-to-pass-length-check"}, "WARNING", "changeme"},
		{"empty", []string{}, "ERROR", "no api_keys"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			h.SetServerConfig(&config.Config{
				API: config.APIConfig{AuthEnabled: true, APIKeys: tc.keys},
			})
			got := h.checkAPIKeyStrength()
			assert.Equal(t, tc.want, got.Status)
			if tc.reason != "" && got.Items != nil {
				hasReason := false
				for _, item := range got.Items {
					if contains(item, tc.reason) {
						hasReason = true
						break
					}
				}
				if !hasReason && !contains(got.Message, tc.reason) {
					t.Errorf("expected reason %q in output, got: %v / %v", tc.reason, got.Message, got.Items)
				}
			}
		})
	}
}

func TestCheckAPIKeyStrength_AuthDisabledSkips(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetServerConfig(&config.Config{API: config.APIConfig{AuthEnabled: false}})
	got := h.checkAPIKeyStrength()
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "disabled")
}

func TestCheckSecretsPermissions_FlagsCredentialFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	secretsDir := filepath.Join(home, ".config", "vornik", "secrets", "assistant")
	require.NoError(t, os.MkdirAll(secretsDir, 0o700))
	creds := filepath.Join(secretsDir, "credentials.json")
	require.NoError(t, os.WriteFile(creds, []byte("{}"), 0o644)) // WORLD-READABLE

	h := &DoctorHandlers{}
	got := h.checkSecretsPermissions(false)
	assert.Equal(t, "WARNING", got.Status)
	var found bool
	for _, item := range got.Items {
		if contains(item, "credentials.json") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected credentials.json in output; got: %v", got.Items)

	// Confirm the file is still at 0644 after a preview-only run.
	info, err := os.Stat(creds)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestCheckSecretsPermissions_FixChmods(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	secretsDir := filepath.Join(home, ".config", "vornik", "secrets", "assistant")
	// Parent at 0755 (world-readable; should chmod to 0700).
	require.NoError(t, os.MkdirAll(secretsDir, 0o755))
	creds := filepath.Join(secretsDir, "credentials.json")
	require.NoError(t, os.WriteFile(creds, []byte("{}"), 0o644))
	// Also stage a workspace-mcp-style OAuth file matched by path heuristic.
	wsDir := filepath.Join(secretsDir, "workspace")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	oauth := filepath.Join(wsDir, "user@example.com.json")
	require.NoError(t, os.WriteFile(oauth, []byte("{}"), 0o644))

	h := &DoctorHandlers{}
	got := h.checkSecretsPermissions(true)
	assert.Equal(t, "OK", got.Status, "all fixable after --fix")
	assert.GreaterOrEqual(t, got.Fixed, 3, "expected parent dir + 2 files chmod'd")

	info, err := os.Stat(creds)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	info, err = os.Stat(oauth)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	info, err = os.Stat(secretsDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestCheckSecretsPermissions_PathHeuristicCatchesOAuthToken(t *testing.T) {
	// The real-world regression: workspace-mcp creates
	// secrets/<project>/workspace/<email>.json for Google OAuth tokens.
	// Filename has no "token" / "credential" substring — it's just the
	// user's email. The path-based heuristic picks it up because it
	// lives under /workspace/ and ends in .json.
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := filepath.Join(home, ".config", "vornik", "secrets", "assistant", "workspace")
	require.NoError(t, os.MkdirAll(path, 0o700))
	target := filepath.Join(path, "vadim@grinco.eu.json")
	require.NoError(t, os.WriteFile(target, []byte("{}"), 0o644)) // world-readable

	h := &DoctorHandlers{}
	got := h.checkSecretsPermissions(false)
	assert.Equal(t, "WARNING", got.Status)
	found := false
	for _, item := range got.Items {
		if contains(item, "vadim@grinco.eu.json") {
			found = true
		}
	}
	assert.True(t, found, "expected OAuth JSON to be flagged; got: %v", got.Items)
}

func TestCheckSecretsPermissions_IgnoresBrowserCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Stage a chromium extraction with world-readable mode. The top-level
	// project dir is restricted (0700); only the browser cache inside is
	// world-readable. That's the realistic shape: operator-managed parent
	// permissions are tight, the patchright extraction inside isn't.
	topDir := filepath.Join(home, ".config", "vornik", "secrets", "assistant")
	require.NoError(t, os.MkdirAll(topDir, 0o700))
	browserDir := filepath.Join(topDir, "linkedin", "patchright-browsers", "chromium-1208", "chrome-linux64")
	require.NoError(t, os.MkdirAll(browserDir, 0o755))
	for _, f := range []string{"chrome", "chrome_100_percent.pak", "libwidevinecdm.so"} {
		require.NoError(t, os.WriteFile(filepath.Join(browserDir, f), []byte("x"), 0o644))
	}

	h := &DoctorHandlers{}
	got := h.checkSecretsPermissions(false)
	// No credential-named files at all — the check returns OK with
	// "nothing to check" rather than flagging hundreds of browser binaries.
	assert.Equal(t, "OK", got.Status)
	for _, item := range got.Items {
		assert.NotContains(t, item, "chrome", "browser binaries should not appear in findings")
	}
}

func TestCheckSecretsPermissions_RestrictedCredentialOK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	secretsDir := filepath.Join(home, ".config", "vornik", "secrets", "assistant")
	require.NoError(t, os.MkdirAll(secretsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "credentials.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "gmail-token.json"), []byte("{}"), 0o600))

	h := &DoctorHandlers{}
	got := h.checkSecretsPermissions(false)
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "restricted")
}

func TestScanOrphanWorktrees_ClassifiesMissingAndTerminalTasks(t *testing.T) {
	root := t.TempDir()
	for _, taskID := range []string{"task-missing", "task-failed", "task-running"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "project-1", ".worktrees", taskID), 0o755))
	}

	findings, projectsChecked, err := scanOrphanWorktrees(root, func(taskID string) (string, error) {
		switch taskID {
		case "task-missing":
			return "", sql.ErrNoRows
		case "task-failed":
			return "FAILED", nil
		case "task-running":
			return "RUNNING", nil
		default:
			return "", sql.ErrNoRows
		}
	})
	require.NoError(t, err)
	assert.Equal(t, 1, projectsChecked)
	require.Len(t, findings, 2)

	var got []string
	for _, finding := range findings {
		got = append(got, finding.taskID)
	}
	assert.ElementsMatch(t, []string{"task-missing", "task-failed"}, got)
}

func TestCheckOrphanWorktrees_FixRemovesOnlyClassifiedOrphans(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "project-1", ".worktrees", "task-missing")
	failed := filepath.Join(root, "project-1", ".worktrees", "task-failed")
	running := filepath.Join(root, "project-1", ".worktrees", "task-running")
	for _, path := range []string{missing, failed, running} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}

	findings, _, err := scanOrphanWorktrees(root, func(taskID string) (string, error) {
		switch taskID {
		case "task-missing":
			return "", sql.ErrNoRows
		case "task-failed":
			return "FAILED", nil
		case "task-running":
			return "RUNNING", nil
		default:
			return "", sql.ErrNoRows
		}
	})
	require.NoError(t, err)

	fixed, items := fixOrphanWorktreeFindings(findings, root)
	assert.Equal(t, 2, fixed)
	assert.Len(t, items, 2)
	assert.NoDirExists(t, missing)
	assert.NoDirExists(t, failed)
	assert.DirExists(t, running)
}

// TestCheckScraperProfileFreshness_StaleWarns — A4 regression guard:
// a cookie file older than the configured cadence must produce a WARNING
// naming the stale profile and the re-login command.
func TestCheckScraperProfileFreshness_StaleWarns(t *testing.T) {
	dir := t.TempDir()
	// janka-default profile, cookie mtime 10 days old; cadence 7d
	p := filepath.Join(dir, "janka", "janka-default", "Default", "Network")
	require.NoError(t, os.MkdirAll(p, 0o755))
	cookie := filepath.Join(p, "Cookies")
	require.NoError(t, os.WriteFile(cookie, []byte("x"), 0o644))
	old := time.Now().Add(-10 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(cookie, old, old))
	h := &DoctorHandlers{scraperProfileRoot: dir, loginRequired: map[string]time.Duration{"janka-default": 7 * 24 * time.Hour}}
	got := h.checkScraperProfileFreshness(context.Background(), false)
	require.Equal(t, "scraper_profile_freshness", got.Name)
	require.Equal(t, "WARNING", got.Status)
	require.Contains(t, got.Message, "janka-default")
	require.Contains(t, got.Message, "vornikctl scraper login")
}

// TestCheckScraperProfileFreshness_FreshIsOK — a cookie file within cadence
// must return OK.
func TestCheckScraperProfileFreshness_FreshIsOK(t *testing.T) {
	dir := t.TempDir()
	// janka-default profile, cookie mtime 2 days old; cadence 7d — fresh
	p := filepath.Join(dir, "janka", "janka-default", "Default", "Network")
	require.NoError(t, os.MkdirAll(p, 0o755))
	cookie := filepath.Join(p, "Cookies")
	require.NoError(t, os.WriteFile(cookie, []byte("x"), 0o644))
	recent := time.Now().Add(-2 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(cookie, recent, recent))
	h := &DoctorHandlers{scraperProfileRoot: dir, loginRequired: map[string]time.Duration{"janka-default": 7 * 24 * time.Hour}}
	got := h.checkScraperProfileFreshness(context.Background(), false)
	require.Equal(t, "scraper_profile_freshness", got.Name)
	require.Equal(t, "OK", got.Status)
}

// TestCheckScraperProfileFreshness_NoRootIsOK — when scraperProfileRoot is
// unset, the check must return OK (not configured).
func TestCheckScraperProfileFreshness_NoRootIsOK(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkScraperProfileFreshness(context.Background(), false)
	require.Equal(t, "scraper_profile_freshness", got.Name)
	require.Equal(t, "OK", got.Status)
	require.Contains(t, got.Message, "not configured")
}

// contains is a trivial helper so these tests don't pull strings.Contains
// from the test file; matches the style of the existing handlers_test.go.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
