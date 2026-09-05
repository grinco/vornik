package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderExecutionPrompt(t *testing.T) {
	hashes := map[string]string{"system": "aaa", "user": "bbb", "tools": "ccc"}
	parts := map[string]string{"system": "You are the planner.", "user": "Plan it.", "tools": ""}
	var out bytes.Buffer
	if err := renderExecutionPrompt(&out, "exec_1", "plan", "2026-09-04T07:00:00Z", hashes, parts, ""); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"execution exec_1  step plan", "=== system  (sha256 aaa)", "You are the planner.", "=== user  (sha256 bbb)", "=== tools  (sha256 ccc)", "pruned by retention"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Order is the order the model saw them.
	if strings.Index(got, "=== system") > strings.Index(got, "=== user") || strings.Index(got, "=== user") > strings.Index(got, "=== tools") {
		t.Errorf("parts out of order:\n%s", got)
	}
	// --part prints the bare body, pipeable.
	out.Reset()
	if err := renderExecutionPrompt(&out, "exec_1", "plan", "", hashes, parts, "user"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Plan it.\n" {
		t.Errorf("--part user = %q", out.String())
	}
	if err := renderExecutionPrompt(&out, "exec_1", "plan", "", hashes, parts, "nope"); err == nil {
		t.Error("an unknown part must error")
	}
}
