package telegram

import "testing"

func TestRenderTelegramHTML(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantHTML bool
		want     string
	}{
		{
			name:     "plain message untouched",
			in:       "Your task is running.",
			wantHTML: false,
			want:     "Your task is running.",
		},
		{
			name:     "bot command wrapped as copyable code",
			in:       "Check /tasks for the list.",
			wantHTML: true,
			want:     "Check <code>/tasks</code> for the list.",
		},
		{
			name:     "command with id argument wrapped whole",
			in:       "Poll it with /status task_123 anytime.",
			wantHTML: true,
			want:     "Poll it with <code>/status task_123</code> anytime.",
		},
		{
			name:     "backtick span becomes copyable code (password carryover)",
			in:       "Viewing password: `hunter2-xY9`",
			wantHTML: true,
			want:     "Viewing password: <code>hunter2-xY9</code>",
		},
		{
			name:     "unknown slash token (file path) is not wrapped",
			in:       "Wrote it to /tmp/out.txt on disk.",
			wantHTML: false,
			want:     "Wrote it to /tmp/out.txt on disk.",
		},
		{
			name:     "html-special chars are escaped when rendering",
			in:       "Run /status <id> to check A&B.",
			wantHTML: true,
			// "<id>" is not an ID-like arg, so only "/status" is wrapped;
			// the placeholder and ampersand are escaped.
			want: "Run <code>/status</code> &lt;id&gt; to check A&amp;B.",
		},
		{
			name:     "command inside backtick span not double-wrapped",
			in:       "Type `/status abc` here.",
			wantHTML: true,
			want:     "Type <code>/status abc</code> here.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, isHTML := renderTelegramHTML(tc.in)
			if isHTML != tc.wantHTML {
				t.Errorf("isHTML = %v, want %v", isHTML, tc.wantHTML)
			}
			if got != tc.want {
				t.Errorf("render mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
