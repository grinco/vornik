package forge

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyPushOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   PushRejectionKind
	}{
		{
			// The exact 2026-07-11 headmatch rejection.
			name: "github app missing workflows permission",
			output: " ! [remote rejected] 1c5018a -> backlog/task-112 (refusing to allow a GitHub App to " +
				"create or update workflow `.github/workflows/coverage.yml` without `workflows` permission)\n" +
				"error: failed to push some refs to 'https://github.com/grinco/headmatch'",
			want: PushRejectionPermission,
		},
		{"resource not accessible", "remote: Resource not accessible by integration", PushRejectionPermission},
		{"protected branch", " ! [remote rejected] main -> main (protected branch hook declined)", PushRejectionProtected},
		{"gh006", "error: GH006: Protected branch update failed", PushRejectionProtected},
		{"generic remote rejected", " ! [remote rejected] x -> y (some other reason)", PushRejectionOther},
		{"non-fast-forward is not a remote rejection", "! [rejected]        main -> main (non-fast-forward)", PushRejectionOther},
		{"transient network", "fatal: unable to access 'https://...': Could not resolve host", PushRejectionNone},
		{"empty", "", PushRejectionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPushOutput(tc.output); got != tc.want {
				t.Errorf("ClassifyPushOutput() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAsPushRejected(t *testing.T) {
	base := &PushRejectedError{Branch: "b", Kind: PushRejectionPermission, Output: "denied", Err: errors.New("exit status 1")}
	wrapped := fmt.Errorf("publish: %w", base)

	got, ok := AsPushRejected(wrapped)
	if !ok {
		t.Fatalf("AsPushRejected did not unwrap a wrapped *PushRejectedError")
	}
	if got.Kind != PushRejectionPermission || got.Branch != "b" {
		t.Errorf("unwrapped = %+v, want Kind=permission Branch=b", got)
	}
	if !errors.Is(wrapped, base.Err) {
		t.Errorf("Unwrap chain broken: errors.Is(wrapped, base.Err) = false")
	}

	if _, ok := AsPushRejected(errors.New("plain")); ok {
		t.Errorf("AsPushRejected(plain error) = true, want false")
	}
	if base.Kind.String() != "permission" || base.Kind.Remediation() == "" {
		t.Errorf("Kind String()/Remediation() unexpected: %q / %q", base.Kind.String(), base.Kind.Remediation())
	}
}
