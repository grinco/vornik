package dispatcher

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDispatcherTools_CreateTaskInputFilesDescribesArtifactIDs pins
// the create_task schema language that tells the LLM input_files
// accepts artifact_id values, not just host paths. The 2026-05-21
// EPUB regression on task_20260521111852_8016a4a902b4f959 was the
// LLM reading "File paths to attach..." and concluding artifact_id
// strings didn't belong there — so it baked the ID into the prompt
// instead of input_files, the staging chain never fired, and the
// worker container had no file to read.
func TestDispatcherTools_CreateTaskInputFilesDescribesArtifactIDs(t *testing.T) {
	var params struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	var found bool
	for _, tool := range DispatcherTools() {
		if tool.Function.Name != "create_task" {
			continue
		}
		if err := json.Unmarshal(tool.Function.Parameters, &params); err != nil {
			t.Fatalf("unmarshal create_task params: %v", err)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("create_task tool not found in DispatcherTools()")
	}
	desc := params.Properties["input_files"].Description
	for _, want := range []string{"artifact ID", "[Attached files]", "artifact_id"} {
		if !strings.Contains(desc, want) {
			t.Errorf("input_files description missing %q; got: %s", want, desc)
		}
	}
}

// TestProjectAllowed locks in the project-scoping helper that every
// tool consults. This is the hot path for cross-project-leak
// prevention — a regression here silently widens every user's reach.
func TestProjectAllowed(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		allowed   []string
		want      bool
	}{
		// "No restriction" shapes (dev mode, wildcard users).
		{"nil allow list = no restriction", "anything", nil, true},
		{"empty allow list = no restriction", "anything", []string{}, true},
		{"wildcard entry matches any id", "assistant", []string{"*"}, true},
		{"wildcard with other entries still wildcards", "something", []string{"snake", "*"}, true},

		// Scoped shapes.
		{"exact match", "snake", []string{"snake"}, true},
		{"exact match in multi", "snake", []string{"snake", "assistant"}, true},
		{"not in list", "assistant", []string{"snake"}, false},
		{"empty projectID never allowed in scoped list", "", []string{"snake"}, false},

		// A project named "*" (hypothetical) shouldn't bypass scoping.
		{"literal-star projectID only matches wildcard-capable list",
			"*", []string{"snake"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectAllowed(tc.projectID, tc.allowed)
			if got != tc.want {
				t.Errorf("projectAllowed(%q, %v) = %v, want %v",
					tc.projectID, tc.allowed, got, tc.want)
			}
		})
	}
}

// TestResolveProjectAllowed confirms the error-or-resolve contract
// every project_id-accepting tool relies on. In particular: an
// explicit project_id that isn't in the allowlist must ERROR, not
// silently fall back to the active project — otherwise a scoped user
// passing project_id="assistant" would appear to succeed from the
// model's perspective.
func TestResolveProjectAllowed(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		active   string
		allowed  []string
		wantID   string
		wantErr  bool
	}{
		{
			name:     "explicit allowed",
			explicit: "snake",
			active:   "", // no active; explicit must survive
			allowed:  []string{"snake", "assistant"},
			wantID:   "snake",
		},
		{
			name:     "explicit NOT in allowlist — hard error",
			explicit: "assistant",
			active:   "snake",
			allowed:  []string{"snake"},
			wantErr:  true,
		},
		{
			name:    "fall back to active when explicit empty",
			active:  "snake",
			allowed: []string{"snake"},
			wantID:  "snake",
		},
		{
			name:    "active not allowed either — error",
			active:  "assistant",
			allowed: []string{"snake"},
			wantErr: true,
		},
		{
			name:    "nothing resolvable — error",
			allowed: []string{"snake"},
			wantErr: true,
		},
		{
			name:     "nil allowed list passes explicit through",
			explicit: "anything",
			wantID:   "anything",
		},
		{
			name:    "wildcard user resolves active transparently",
			active:  "assistant",
			allowed: []string{"*"},
			wantID:  "assistant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProjectAllowed(tc.explicit, tc.active, tc.allowed)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got resolved=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantID {
				t.Errorf("resolved = %q, want %q", got, tc.wantID)
			}
		})
	}
}
