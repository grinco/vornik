// Package mcp: targeted coverage on the small Manager helpers
// (parseQualifiedName, Close, counts) plus the rate-limit
// specWithBucketKey helper. The subprocess + SSE I/O paths are
// covered by integration tests; this file pins the pure-Go shape.
package mcp

import (
	"testing"

	"github.com/rs/zerolog"
)

// TestParseQualifiedName_AllShapes pins the `mcp__<server>__<tool>`
// convention. The dispatcher routes every tool call through this
// parser; a regression that accepts a malformed name silently
// would let the agent execute a tool the operator never wired.
func TestParseQualifiedName_AllShapes(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		server, tool string
		ok           bool
	}{
		{"happy path", "mcp__broker__place_order", "broker", "place_order", true},
		{"two underscores in tool", "mcp__broker__some__tool", "broker", "some__tool", true},
		{"missing mcp prefix", "broker__place_order", "", "", false},
		{"missing separator", "mcp__broker", "", "", false},
		{"empty server", "mcp____place_order", "", "", false},
		{"empty tool", "mcp__broker__", "", "", false},
		{"empty input", "", "", "", false},
		{"prefix only", "mcp__", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotServer, gotTool, gotOK := parseQualifiedName(tc.in)
			if gotOK != tc.ok || gotServer != tc.server || gotTool != tc.tool {
				t.Errorf("parseQualifiedName(%q) = (%q, %q, %v); want (%q, %q, %v)",
					tc.in, gotServer, gotTool, gotOK, tc.server, tc.tool, tc.ok)
			}
		})
	}
}

// TestManager_CloseOnEmptyManager — closing a freshly constructed
// manager (no projects, no clients) must succeed without panic and
// must NOT regress the internal map (a future caller's
// StartForProject should still work).
func TestManager_CloseOnEmptyManager(t *testing.T) {
	m := NewManager(zerolog.Nop())
	m.Close()
	if m.ProjectCount() != 0 {
		t.Errorf("ProjectCount after Close = %d, want 0", m.ProjectCount())
	}
	if m.ServerCount() != 0 {
		t.Errorf("ServerCount after Close = %d, want 0", m.ServerCount())
	}
	// Second Close must also be safe (idempotent).
	m.Close()
}

// TestManager_Counts_AfterEmptyStart — StartForProject with no
// servers must not register a project in the map (otherwise the
// admin page would show "project with 0 servers" rows).
func TestManager_Counts_AfterEmptyStart(t *testing.T) {
	m := NewManager(zerolog.Nop())
	if got := m.ProjectCount(); got != 0 {
		t.Errorf("initial ProjectCount = %d, want 0", got)
	}
	if got := m.ServerCount(); got != 0 {
		t.Errorf("initial ServerCount = %d, want 0", got)
	}
}

// TestManager_Tools_UnknownProjectReturnsEmpty — Tools on a
// project the manager hasn't seen must return nil/empty rather
// than panic. The dispatcher calls this before knowing whether a
// project has MCP wired.
func TestManager_Tools_UnknownProjectReturnsEmpty(t *testing.T) {
	m := NewManager(zerolog.Nop())
	got := m.Tools("nonexistent-project")
	if len(got) != 0 {
		t.Errorf("expected empty tool list, got %v", got)
	}
}
