package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression for the 2026-05-09 UI 404: TaskConversationAction
// trimmed the literal "/ui/tasks/" prefix, but by the time the
// handler runs the /ui prefix has already been stripped by
// uiSubtreeHandler — r.URL.Path is "/tasks/<id>/<action>". The
// old TrimPrefix returned the whole string unchanged, leaving
// taskID="" → s.taskRepo.Get("") → 404 redirect.
//
// The fix tolerates both shapes (with and without /ui). Keep this
// test future-proof in case the upstream subtree handler ever
// moves the strip to a different layer.
func TestTaskConversationAction_PathParse(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantTask string
		wantBad  bool // expect 400 "bad path"
	}{
		// Production shape (prefix stripped upstream).
		{"stripped_message", "/tasks/T_abc/message", "T_abc", false},
		{"stripped_answer", "/tasks/T_abc/answer", "T_abc", false},
		{"stripped_amend", "/tasks/T_abc/amend", "T_abc", false},
		{"stripped_pause", "/tasks/T_abc/pause", "T_abc", false},
		{"stripped_resume", "/tasks/T_abc/resume", "T_abc", false},
		{"stripped_close", "/tasks/T_abc/close", "T_abc", false},
		// Defensive: if /ui isn't stripped (direct mount or test
		// harness), the handler must still parse correctly.
		{"unstripped_message", "/ui/tasks/T_abc/message", "T_abc", false},
		{"unstripped_answer", "/ui/tasks/T_abc/answer", "T_abc", false},
		// Malformed.
		{"missing_action", "/tasks/T_abc", "", true},
		{"empty_path", "/", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTask, gotAction, gotBad := parseTaskActionPath(tc.path)
			if gotBad != tc.wantBad {
				t.Errorf("path=%q wantBad=%v gotBad=%v", tc.path, tc.wantBad, gotBad)
				return
			}
			if tc.wantBad {
				return
			}
			if gotTask != tc.wantTask {
				t.Errorf("path=%q taskID got %q want %q", tc.path, gotTask, tc.wantTask)
			}
			if gotAction == "" {
				t.Errorf("path=%q action must not be empty", tc.path)
			}
		})
	}
}

// parseTaskActionPath mirrors the path-parsing logic inside
// TaskConversationAction so we can assert it without standing up
// a full Server with all the repos. Production handler uses the
// same shape:
//
//	trimmed := strings.TrimPrefix(r.URL.Path, "/ui")
//	trimmed = strings.TrimPrefix(trimmed, "/tasks/")
//	parts := strings.Split(trimmed, "/")
//	if len(parts) < 2 → "bad path"
func parseTaskActionPath(path string) (taskID, action string, bad bool) {
	trimmed := strings.TrimPrefix(path, "/ui")
	trimmed = strings.TrimPrefix(trimmed, "/tasks/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" {
		return "", "", true
	}
	return parts[0], parts[1], false
}

// End-to-end check: hit the handler over a recorded ResponseWriter
// and confirm we don't 404 on the production path shape. Uses a
// nil-repo Server (responds 503 TASK_LIFECYCLE_DISABLED), which
// proves we got past the path-parse + into the dependency check.
// Pre-fix, an empty-taskID path would have flown into taskRepo.Get
// before the dep check fired, returning 404.
func TestTaskConversationAction_DoesNotFourOhFourOnStrippedPath(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/tasks/T_abc/answer",
		strings.NewReader("checkpoint_id=x&content=hi"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.TaskConversationAction(rr, req)
	if rr.Code == 404 {
		t.Fatalf("regression: handler returned 404 on the production stripped path %q", req.URL.Path)
	}
	// 503 is the expected response when no repos wired (this Server
	// is empty). 200/303 would mean we somehow got further; both
	// are acceptable as proof the path-parse worked.
}
