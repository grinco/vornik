package ui

import (
	"net/http/httptest"
	"testing"
)

// TestSortBaseURL_ReattachesUIPrefix — the regression: the subtree
// middleware strips /ui from r.URL.Path before handlers run, so a raw
// r.URL-derived link omits the prefix and 404s when the browser
// re-requests it. sortBaseURL must reattach /ui to match the rest of
// the UI's URL-emission convention.
func TestSortBaseURL_ReattachesUIPrefix(t *testing.T) {
	cases := []struct {
		name string
		path string
		raw  string
		want string
	}{
		{
			name: "project detail no other params",
			path: "/projects/assistant",
			raw:  "sort=cost&dir=asc",
			want: "/ui/projects/assistant",
		},
		{
			name: "tasks list with filter preserved",
			path: "/tasks",
			raw:  "status=RUNNING&sort=created&dir=desc",
			want: "/ui/tasks?status=RUNNING",
		},
		{
			name: "task detail no params",
			path: "/tasks/task_abc",
			raw:  "sort=in&dir=desc",
			want: "/ui/tasks/task_abc",
		},
		{
			name: "no sort/dir to strip — preserved as-is",
			path: "/projects/p",
			raw:  "limit=50",
			want: "/ui/projects/p?limit=50",
		},
		{
			name: "already prefixed — not double-prefixed",
			path: "/ui/projects/p",
			raw:  "sort=x",
			want: "/ui/projects/p",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path+"?"+tc.raw, nil)
			got := sortBaseURL(r)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
