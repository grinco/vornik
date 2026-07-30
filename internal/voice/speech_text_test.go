package voice

import "testing"

// OPERATOR REQUEST 2026-07-30: rich text everywhere EXCEPT audio — "it needs to be
// converted to plain text and handled with more care (also links probably shouldn't be
// dictated)."
func TestForSpeech(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a labelled link becomes the words a human would have said",
			in:   "Prepared it — <https://github.com/grinco/vornik/issues/new?body=%23%23|open this link> and submit.",
			want: "Prepared it — open this link and submit.",
		},
		{
			name: "a markdown link keeps its label too",
			in:   "See [the report](https://example.com/a/b) for detail.",
			want: "See the report for detail.",
		},
		{
			name: "a line that is only a link is dropped entirely",
			in:   "Finished the job.\nhttps://vornik.example/ui/projects/p/tasks/task_42\nAsk me about it.",
			want: "Finished the job.\nAsk me about it.",
		},
		{
			name: "an angle-wrapped link alone on a line goes as well",
			in:   "Done.\n<https://vornik.example/ui/tasks/1>\nThanks.",
			want: "Done.\nThanks.",
		},
		{
			name: "an inline bare url keeps the sentence readable",
			in:   "Check https://example.com/x before you submit.",
			want: "Check a link before you submit.",
		},
		{
			name: "formatting markers go and the words stay",
			in:   "*Finished* the _weekly_ report — see ~~old~~ `task_42`.",
			want: "Finished the weekly report — see old task_42.",
		},
		{
			name: "emoji shortcodes and glyphs are not dictated",
			in:   ":tada: Done ✅ — all good 🎉",
			want: "Done — all good",
		},
		{
			name: "slack user and channel refs are dropped rather than spelled",
			in:   "<@U0BLPMBQXDL> asked in <#C03HTMUL2S1|general>, <!here> please look",
			want: "asked in , please look",
		},
		{
			name: "headings, quotes and bullets read as prose",
			in:   "### Summary\n\n> quoted line\n\n- first\n- second",
			want: "Summary\n\nquoted line\n\nfirst\nsecond",
		},
		{
			name: "a fenced code block loses its fence and language",
			in:   "Run this:\n```sh\nvornikctl report\n```\nthen submit.",
			want: "Run this:\nvornikctl report\nthen submit.",
		},
		{
			name: "plain text is untouched",
			in:   "The meeting is at three on Tuesday.",
			want: "The meeting is at three on Tuesday.",
		},
		{
			name: "a message that was only a link and formatting ends up empty",
			in:   "*<https://example.com/only|x>*",
			want: "x",
		},
		{
			name: "blank runs collapse so the voice does not pause forever",
			in:   "One.\n\n\n\nTwo.",
			want: "One.\n\nTwo.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForSpeech(tc.in); got != tc.want {
				t.Errorf("ForSpeech()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// The completion notice is daemon-composed and carries a task URL on its own line — the
// exact shape that must not be dictated.
func TestForSpeech_CompletionNoticeIsListenable(t *testing.T) {
	in := "Finished the job you asked for — task `task_42` in project `p`.\n" +
		"<https://vornik.example/ui/projects/p/tasks/task_42|open the task>\n\n" +
		"Ask me about it and I can summarise the result."
	got := ForSpeech(in)
	for _, unwanted := range []string{"http", "`", "|", "<", ">"} {
		if contains(got, unwanted) {
			t.Errorf("speech text still contains %q: %q", unwanted, got)
		}
	}
	if !contains(got, "open the task") {
		t.Errorf("the link's label was lost, so the sentence lost its object: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Empty and whitespace input must not panic or produce stray output — the TTS path calls
// this before deciding whether it has anything to synthesise.
func TestForSpeech_EmptyInput(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n", "*_~`*"} {
		if got := ForSpeech(in); got != "" {
			t.Errorf("ForSpeech(%q) = %q, want empty", in, got)
		}
	}
}
