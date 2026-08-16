package runtime

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// Regression: grinco/vornik-ee#66 CI, 2026-08-16. The E2E lane failed with
//
//	podman availability check failed  error="signal: killed"
//	output={"Client":{"APIVersion":"5.8.4","Version":"5.8.4",...}}
//
// Podman ANSWERED CORRECTLY and was then killed — the 5s version-check timeout
// expired on a loaded nested-podman runner after the output had been produced.
// The daemon concluded podman was unavailable, scheduler init failed, /healthz
// never came up, and the whole lane timed out at 40s.
//
// A slow-but-functional podman must not take the daemon down. If the output
// parses as a podman version document, podman is demonstrably working whatever
// the exit status says.
func TestPodmanVersionUsable(t *testing.T) {
	const realOutput = `{"Client":{"APIVersion":"5.8.4","Version":"5.8.4","GoVersion":"go1.25.11","GitCommit":"","BuiltTime":"Thu Jan  1 00:00:00 1970","Built":0,"OsArch":"linux/amd64","Os":"linux"}}`

	tests := []struct {
		name   string
		output string
		err    error
		want   bool
	}{
		{
			name:   "clean success",
			output: realOutput,
			err:    nil,
			want:   true,
		},
		{
			// The incident: valid document, process signalled.
			name:   "killed after producing a valid version document",
			output: realOutput,
			err:    errors.New("signal: killed"),
			want:   true,
		},
		{
			name:   "context deadline after producing a valid document",
			output: realOutput,
			err:    context.DeadlineExceeded,
			want:   true,
		},
		{
			// Genuinely broken: no usable output. Must still fail, or the
			// check stops being a check.
			name:   "killed with no output",
			output: "",
			err:    errors.New("signal: killed"),
			want:   false,
		},
		{
			name:   "binary missing",
			output: "",
			err:    exec.ErrNotFound,
			want:   false,
		},
		{
			name:   "error with non-JSON noise",
			output: "podman: command not found",
			err:    errors.New("exit status 127"),
			want:   false,
		},
		{
			// Well-formed JSON that is not a version document proves nothing.
			name:   "unrelated JSON",
			output: `{"something":"else"}`,
			err:    errors.New("signal: killed"),
			want:   false,
		},
		{
			// Truncated output — killed mid-write. Cannot be trusted.
			name:   "truncated document",
			output: `{"Client":{"APIVersion":"5.8`,
			err:    errors.New("signal: killed"),
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := podmanVersionUsable([]byte(tc.output), tc.err); got != tc.want {
				t.Errorf("podmanVersionUsable(%q, %v) = %v, want %v", tc.output, tc.err, got, tc.want)
			}
		})
	}
}
