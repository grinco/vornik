package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseEnvironmentFileEntries_StripsOptionalMarker pins the
// contract that `-`-prefixed entries set Optional=true. systemd
// uses the prefix to mean "missing file is fine"; we honour the
// same semantics so the doctor check doesn't WARN about
// intentionally-empty categories like aws.env on a Vertex-only
// install.
func TestParseEnvironmentFileEntries_StripsOptionalMarker(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "vornik.service")
	require.NoError(t, os.WriteFile(unit, []byte(strings.Join([]string{
		"[Service]",
		"EnvironmentFile=/etc/vornik/required.env",
		"EnvironmentFile=-/etc/vornik/optional.env",
		"# comment line that mentions EnvironmentFile=/should/be/ignored",
		"EnvironmentFile=  /etc/vornik/whitespace.env  ",
		"",
	}, "\n")), 0o644))

	entries, err := parseEnvironmentFileEntries(unit)
	require.NoError(t, err)
	require.Len(t, entries, 3, "expected 3 entries (required, optional, whitespace), got %d", len(entries))

	byPath := map[string]envFileEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	assert.False(t, byPath["/etc/vornik/required.env"].Optional, "required.env must NOT be optional")
	assert.True(t, byPath["/etc/vornik/optional.env"].Optional, "optional.env must be optional (had `-` prefix)")
	assert.False(t, byPath["/etc/vornik/whitespace.env"].Optional)
}

// TestParseEnvironmentFileEntries_ExpandsHomeSpecifier pins %h
// expansion. The shipped contrib/systemd/vornik.service uses %h
// for every EnvironmentFile= line; if we don't expand it the
// downstream stat() always fails and the check is useless.
func TestParseEnvironmentFileEntries_ExpandsHomeSpecifier(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	dir := t.TempDir()
	unit := filepath.Join(dir, "vornik.service")
	require.NoError(t, os.WriteFile(unit, []byte(
		"EnvironmentFile=-%h/.config/vornik/secrets/vertex.env\n",
	), 0o644))

	entries, err := parseEnvironmentFileEntries(unit)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	want := filepath.Join(home, ".config/vornik/secrets/vertex.env")
	assert.Equal(t, want, entries[0].Path)
}

// TestParseEnvironmentFileEntries_SkipsRelativePaths — systemd
// rejects relative EnvironmentFile= paths; we should too, but
// silently so one bad entry doesn't poison the whole list.
func TestParseEnvironmentFileEntries_SkipsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "vornik.service")
	require.NoError(t, os.WriteFile(unit, []byte(strings.Join([]string{
		"EnvironmentFile=relative/path.env",
		"EnvironmentFile=/etc/vornik/absolute.env",
	}, "\n")), 0o644))

	entries, err := parseEnvironmentFileEntries(unit)
	require.NoError(t, err)
	require.Len(t, entries, 1, "relative entry should be silently dropped")
	assert.Equal(t, "/etc/vornik/absolute.env", entries[0].Path)
}

// TestExpandUnitSpecifiers covers the edge cases of the small
// expander we ship instead of pulling in a full systemd
// specifier table.
func TestExpandUnitSpecifiers(t *testing.T) {
	home := "/home/alice"
	cases := []struct {
		in   string
		want string
	}{
		{"%h/.config/vornik/x.env", "/home/alice/.config/vornik/x.env"},
		{"/literal/path", "/literal/path"},
		{"a%%b", "a%b"},                      // %% escapes
		{"%z/unknown", "%z/unknown"},         // unknown specifier left intact
		{"%h/%h", "/home/alice//home/alice"}, // multiple expansions
		{"%", "%"},                           // trailing bare %
	}
	for _, c := range cases {
		assert.Equal(t, c.want, expandUnitSpecifiers(c.in, home), "input=%q", c.in)
	}
}

// TestCheckEnvFileFreshness_FlagsFileNewerThanDaemon is the
// integration-flavoured test that locks the B-13 contract:
// touching an env file after the daemon started yields a WARNING
// with that file's path in Items.
//
// We construct a fake unit file in a temp dir, monkey-patch
// processStartedAt to the past, and call the check after
// writing a fresh env file. The unit-file locator is bypassed
// by writing the file at the well-known XDG_CONFIG_HOME slot.
func TestCheckEnvFileFreshness_FlagsFileNewerThanDaemon(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	unitDir := filepath.Join(xdg, "systemd/user")
	require.NoError(t, os.MkdirAll(unitDir, 0o755))

	envDir := filepath.Join(xdg, "vornik/secrets")
	require.NoError(t, os.MkdirAll(envDir, 0o700))
	stalePath := filepath.Join(envDir, "vertex.env")
	freshPath := filepath.Join(envDir, "chat.env")

	require.NoError(t, os.WriteFile(stalePath, []byte("GCP_API_KEY=old\n"), 0o600))
	require.NoError(t, os.WriteFile(freshPath, []byte("CHAT_API_KEY=fresh\n"), 0o600))

	unitPath := filepath.Join(unitDir, "vornik.service")
	require.NoError(t, os.WriteFile(unitPath, []byte(strings.Join([]string{
		"[Service]",
		"EnvironmentFile=-" + stalePath,
		"EnvironmentFile=-" + freshPath,
	}, "\n")), 0o644))

	pastModTime := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(stalePath, pastModTime, pastModTime))

	originalStart := processStartedAt
	t.Cleanup(func() { processStartedAt = originalStart })
	processStartedAt = time.Now().Add(-30 * time.Minute)

	require.NoError(t, os.Chtimes(freshPath, time.Now(), time.Now()))

	h := &DoctorHandlers{}
	got := h.checkEnvFileFreshness()

	assert.Equal(t, "env_file_freshness", got.Name)
	assert.Equal(t, "WARNING", got.Status, "message=%q items=%v", got.Message, got.Items)
	require.Len(t, got.Items, 1)
	assert.Contains(t, got.Items[0], freshPath)
	assert.NotContains(t, strings.Join(got.Items, "\n"), stalePath, "stale file (older than daemon) must NOT be flagged")
}

// TestCheckEnvFileFreshness_OKWhenAllOlder pins the happy path:
// every env file was edited before the daemon started, the
// daemon's environment is up to date, no warning.
func TestCheckEnvFileFreshness_OKWhenAllOlder(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	unitDir := filepath.Join(xdg, "systemd/user")
	require.NoError(t, os.MkdirAll(unitDir, 0o755))
	envDir := filepath.Join(xdg, "vornik/secrets")
	require.NoError(t, os.MkdirAll(envDir, 0o700))

	p1 := filepath.Join(envDir, "chat.env")
	p2 := filepath.Join(envDir, "vertex.env")
	require.NoError(t, os.WriteFile(p1, []byte("X=1"), 0o600))
	require.NoError(t, os.WriteFile(p2, []byte("Y=2"), 0o600))

	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(p1, past, past))
	require.NoError(t, os.Chtimes(p2, past, past))

	require.NoError(t, os.WriteFile(filepath.Join(unitDir, "vornik.service"), []byte(strings.Join([]string{
		"EnvironmentFile=-" + p1,
		"EnvironmentFile=-" + p2,
	}, "\n")), 0o644))

	originalStart := processStartedAt
	t.Cleanup(func() { processStartedAt = originalStart })
	processStartedAt = time.Now()

	h := &DoctorHandlers{}
	got := h.checkEnvFileFreshness()
	assert.Equal(t, "OK", got.Status, "message=%q items=%v", got.Message, got.Items)
	assert.Empty(t, got.Items)
}

// TestCheckEnvFileFreshness_OKWhenNoUnitFile — running outside
// a systemd setup (CI containers, macOS dev, podman exec into a
// container) must not produce a false WARNING.
func TestCheckEnvFileFreshness_OKWhenNoUnitFile(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", xdg) // also redirect %h so the home-based fallback misses

	h := &DoctorHandlers{}
	got := h.checkEnvFileFreshness()
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no vornik.service unit file found")
}

// TestCheckEnvFileFreshness_TreatsMissingOptionalAsFine pins
// that an optional file (`-` prefix) that doesn't exist on disk
// is silently skipped, mirroring systemd's behaviour. The
// non-existent path must not appear in Items.
func TestCheckEnvFileFreshness_TreatsMissingOptionalAsFine(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	unitDir := filepath.Join(xdg, "systemd/user")
	require.NoError(t, os.MkdirAll(unitDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(unitDir, "vornik.service"), []byte(
		"EnvironmentFile=-/nonexistent/optional.env\n",
	), 0o644))

	h := &DoctorHandlers{}
	got := h.checkEnvFileFreshness()
	assert.Equal(t, "OK", got.Status, "missing optional file should not WARN; got message=%q items=%v", got.Message, got.Items)
	for _, item := range got.Items {
		assert.NotContains(t, item, "nonexistent")
	}
}

// TestCheckEnvFileFreshness_ItemsSortedForStableOutput — the
// items slice is sorted alphabetically so JSON-mode doctor
// output diffs cleanly between runs and operators can rely on
// stable ordering when scripting against the report.
func TestCheckEnvFileFreshness_ItemsSortedForStableOutput(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	unitDir := filepath.Join(xdg, "systemd/user")
	envDir := filepath.Join(xdg, "vornik/secrets")
	require.NoError(t, os.MkdirAll(unitDir, 0o755))
	require.NoError(t, os.MkdirAll(envDir, 0o700))

	// Two files, fresh; create them in non-alphabetical order
	// to ensure sorting is doing real work.
	zPath := filepath.Join(envDir, "zzz.env")
	aPath := filepath.Join(envDir, "aaa.env")
	require.NoError(t, os.WriteFile(zPath, []byte("Z=1"), 0o600))
	require.NoError(t, os.WriteFile(aPath, []byte("A=1"), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(unitDir, "vornik.service"), []byte(strings.Join([]string{
		"EnvironmentFile=-" + zPath,
		"EnvironmentFile=-" + aPath,
	}, "\n")), 0o644))

	originalStart := processStartedAt
	t.Cleanup(func() { processStartedAt = originalStart })
	processStartedAt = time.Now().Add(-1 * time.Hour)

	now := time.Now()
	require.NoError(t, os.Chtimes(zPath, now, now))
	require.NoError(t, os.Chtimes(aPath, now, now))

	h := &DoctorHandlers{}
	got := h.checkEnvFileFreshness()
	assert.Equal(t, "WARNING", got.Status)
	require.Len(t, got.Items, 2)
	assert.True(t, sort.StringsAreSorted(got.Items), "items must be alphabetically sorted: %v", got.Items)
}
