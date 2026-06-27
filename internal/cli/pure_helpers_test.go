// Coverage for the pure helper functions across the cli package.
// Targets: shortenID, formatTimeOrDash, statusIcon, isTerminalTaskStatus,
// presetDisplayName. These are all data-in / data-out functions with no
// external dependencies, so the tests stay narrow and fast.

package cli

import (
	"strings"
	"testing"
	"time"
)

// TestShortenID pins the tail-hex extraction. Vornik ID shape is
// {prefix}_{timestamp}_{hex} and the helper keeps just the trailing
// hex chunk so table rows stay readable. The fallback path
// (no underscore at all) is also exercised.
func TestShortenID(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"canonical task id", "task_20260423233059_abcd1234", "abcd1234"},
		{"canonical exec id", "exec_20260423233059_deadbeef", "deadbeef"},
		{"single underscore", "x_y", "y"},
		{"no underscore long", "abcdefghijklmnop", "efghijklmnop"},
		{"no underscore short", "short", "short"},
		{"empty", "", ""},
		{"trailing underscore", "prefix_", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortenID(tc.in); got != tc.want {
				t.Errorf("shortenID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFormatTimeOrDash covers the three branches: empty input
// returns the em-dash placeholder, RFC3339 timestamps render
// with a relative-time suffix, and unparseable values pass
// through unchanged so the CLI surface never lies about what
// the daemon sent.
func TestFormatTimeOrDash(t *testing.T) {
	if got := formatTimeOrDash(""); got != "—" {
		t.Errorf("empty input: got %q, want em-dash", got)
	}
	if got := formatTimeOrDash("not a timestamp"); got != "not a timestamp" {
		t.Errorf("unparseable: got %q, want pass-through", got)
	}
	past := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	got := formatTimeOrDash(past)
	if !strings.Contains(got, past) || !strings.Contains(got, "ago") {
		t.Errorf("RFC3339: got %q, want format containing %q + 'ago'", got, past)
	}
}

// TestStatusIcon pins the four branches plus the unknown-default
// fallback. The icon strings are part of the doctor command's
// terminal output contract so callers can grep for known prefixes.
func TestStatusIcon(t *testing.T) {
	cases := map[string]string{
		"OK":      "OK ",
		"ok":      "OK ", // case insensitive
		"WARNING": "!! ",
		"warning": "!! ",
		"ERROR":   "ERR",
		"error":   "ERR",
		"unknown": "?  ",
		"":        "?  ",
	}
	for in, want := range cases {
		if got := statusIcon(in); got != want {
			t.Errorf("statusIcon(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsTerminalTaskStatus_CLI mirrors the scheduler's same
// predicate but at the cli layer. Lock the three-state contract
// (COMPLETED, FAILED, CANCELLED) and reject everything else —
// the eval runner's polling loop relies on this to know when to
// stop waiting.
func TestIsTerminalTaskStatus_CLI(t *testing.T) {
	for _, terminal := range []string{"COMPLETED", "FAILED", "CANCELLED"} {
		if !isTerminalTaskStatus(terminal) {
			t.Errorf("%s must be terminal", terminal)
		}
	}
	for _, active := range []string{
		"QUEUED", "LEASED", "RUNNING", "PENDING",
		"WAITING_FOR_CHILDREN", "AWAITING_INPUT",
		"", "completed", // case sensitive
	} {
		if isTerminalTaskStatus(active) {
			t.Errorf("%s must NOT be terminal", active)
		}
	}
}

// TestPresetDisplayName covers the linear-scan extractor used by
// the --list output. Sliding through three shapes: a clean
// quoted value, an unquoted value, and a missing key.
func TestPresetDisplayName(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			"quoted value",
			"id: snake\ndisplayName: \"Snake Game\"\nswarmId: dev",
			"Snake Game",
		},
		{
			"unquoted value",
			"id: x\ndisplayName: My Preset\n",
			"My Preset",
		},
		{
			"key with leading whitespace",
			"id: x\n  displayName: \"Indented\"\n",
			"Indented",
		},
		{
			"missing key",
			"id: x\nswarmId: dev\n",
			"(no description)",
		},
		{"empty body", "", "(no description)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := presetDisplayName(tc.in); got != tc.want {
				t.Errorf("presetDisplayName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
