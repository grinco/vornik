package telegram

import (
	"testing"
	"time"
)

// TestParseTelegramRetryAfter covers the 429 backoff parser that
// sendDocumentToForum reads to pace its retries. Pre-fix, 429s
// fanning out 4 artifacts to one thread dropped every document
// silently (observed 2026-05-18 on janka's CV delivery — the bot
// logged "forum: failed to send artifact" four times and moved on,
// leaving the operator empty-handed).
func TestParseTelegramRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{
			name: "canonical 429 with retry_after",
			body: `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 19","parameters":{"retry_after":19}}`,
			want: 19 * time.Second,
		},
		{
			name: "single-second retry",
			body: `{"parameters":{"retry_after":1}}`,
			want: time.Second,
		},
		{
			name: "missing parameters → 1s default",
			body: `{"ok":false,"error_code":429,"description":"Too Many Requests"}`,
			want: time.Second,
		},
		{
			name: "malformed JSON → 1s default",
			body: `not json`,
			want: time.Second,
		},
		{
			name: "empty body → 1s default",
			body: ``,
			want: time.Second,
		},
		{
			name: "zero retry_after → 1s default (treat zero as missing)",
			body: `{"parameters":{"retry_after":0}}`,
			want: time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTelegramRetryAfter([]byte(tc.body))
			if got != tc.want {
				t.Errorf("parseTelegramRetryAfter(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
