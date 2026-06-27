package executor

import "testing"

func TestStripReasoning(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no tags", "hello world", "hello world"},
		{"plain think", "<think>deliberation</think>answer", "answer"},
		{"thinking variant", "<thinking>x</thinking>y", "y"},
		{"reasoning variant", "<reasoning>r</reasoning>ok", "ok"},
		{"multiline block", "<think>line1\nline2</think>\nfinal", "final"},
		{"multiple blocks", "<think>a</think>mid<think>b</think>end", "midend"},
		{"leading whitespace stripped", "<think>x</think>\n\n  answer", "answer"},
		{"tag-only input", "<think>solo</think>", ""},
		{"tag in middle keeps surrounding", "before<think>x</think>after", "beforeafter"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripReasoning(tc.in)
			if got != tc.want {
				t.Errorf("stripReasoning(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
