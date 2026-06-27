package autonomy

import (
	"strings"
	"testing"
)

// TestConsumeNextBacklogItem pins the markdown-checklist parser
// that backs Mode="backlog" autonomy ticks. The function MUST
// preserve all surrounding content (headers, prose, completed
// items, blank lines, indented notes) so the file remains
// operator-readable in git after the daemon rewrites it.
func TestConsumeNextBacklogItem(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantOK    bool
		wantPromp string
		wantBody  string
	}{
		{
			name: "single_pending_item",
			input: "# Backlog\n" +
				"\n" +
				"- [ ] Implement login\n",
			wantOK:    true,
			wantPromp: "Implement login",
			wantBody: "# Backlog\n" +
				"\n" +
				"- [x] Implement login\n",
		},
		{
			name: "skips_completed_items",
			input: "- [x] First\n" +
				"- [x] Second\n" +
				"- [ ] Third\n",
			wantOK:    true,
			wantPromp: "Third",
			wantBody: "- [x] First\n" +
				"- [x] Second\n" +
				"- [x] Third\n",
		},
		{
			name: "preserves_indent_and_prose",
			input: "## Sprint\n" +
				"\n" +
				"Notes for context:\n" +
				"- [x] done thing\n" +
				"- [ ] Refactor auth\n" +
				"  - nested note (untouched)\n" +
				"- [ ] later item\n",
			wantOK:    true,
			wantPromp: "Refactor auth",
			wantBody: "## Sprint\n" +
				"\n" +
				"Notes for context:\n" +
				"- [x] done thing\n" +
				"- [x] Refactor auth\n" +
				"  - nested note (untouched)\n" +
				"- [ ] later item\n",
		},
		{
			name:      "accepts_star_bullet",
			input:     "* [ ] Starred item\n",
			wantOK:    true,
			wantPromp: "Starred item",
			wantBody:  "* [x] Starred item\n",
		},
		{
			name:      "indented_item_picked_up",
			input:     "   - [ ] indented work\n",
			wantOK:    true,
			wantPromp: "indented work",
			wantBody:  "   - [x] indented work\n",
		},
		{
			name:   "empty_body_returns_false",
			input:  "",
			wantOK: false,
		},
		{
			name: "all_completed_returns_false",
			input: "- [x] one\n" +
				"- [x] two\n",
			wantOK: false,
		},
		{
			name: "prose_only_returns_false",
			input: "# Some thoughts\n" +
				"\n" +
				"No tasks here yet.\n",
			wantOK: false,
		},
		{
			name: "skips_empty_checkbox_text",
			input: "- [ ]\n" +
				"- [ ]   \n" +
				"- [ ] real task\n",
			wantOK:    true,
			wantPromp: "real task",
			wantBody: "- [ ]\n" +
				"- [ ]   \n" +
				"- [x] real task\n",
		},
		{
			name: "checked_x_lowercase_only_skipped",
			input: "- [X] Counts as complete in github-flavoured md (case-insensitive)\n" +
				"- [ ] take this\n",
			// My regex matches only `[ ]` (space) for pending,
			// so [X]/[x] are both "not pending". Pin that.
			wantOK:    true,
			wantPromp: "take this",
			wantBody: "- [X] Counts as complete in github-flavoured md (case-insensitive)\n" +
				"- [x] take this\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prompt, newBody, ok := consumeNextBacklogItem(c.input)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (prompt=%q)", ok, c.wantOK, prompt)
			}
			if !ok {
				return
			}
			if prompt != c.wantPromp {
				t.Errorf("prompt = %q, want %q", prompt, c.wantPromp)
			}
			if newBody != c.wantBody {
				t.Errorf("body mismatch:\nGOT:\n%s\nWANT:\n%s", newBody, c.wantBody)
			}
		})
	}
}

// TestConsumeNextBacklogItem_DoesNotMutateUnrelatedBoxes guards
// against a subtle regression: the rewriter replaces the first
// `[ ]` on the matched line, so it must NOT touch other [ ]'s
// elsewhere in the file when more than one pending item exists.
func TestConsumeNextBacklogItem_DoesNotMutateUnrelatedBoxes(t *testing.T) {
	in := "- [ ] first\n- [ ] second\n- [ ] third\n"
	_, body, ok := consumeNextBacklogItem(in)
	if !ok {
		t.Fatal("expected ok")
	}
	// Exactly one line should have flipped to [x].
	xs := strings.Count(body, "[x]")
	if xs != 1 {
		t.Errorf("expected exactly one [x], got %d:\n%s", xs, body)
	}
	if !strings.Contains(body, "- [x] first") {
		t.Errorf("first item not consumed:\n%s", body)
	}
	if !strings.Contains(body, "- [ ] second") || !strings.Contains(body, "- [ ] third") {
		t.Errorf("subsequent items must remain pending:\n%s", body)
	}
}
