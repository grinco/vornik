package cli

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeWebWriteRepo is an in-memory WebWriteRepo capturing the Resolve call and
// returning a configurable error. Only Resolve is exercised by the CLI; the
// other methods satisfy the interface and fail loudly if unexpectedly called.
type fakeWebWriteRepo struct {
	gotID      string
	gotStatus  string
	calls      int
	resolveErr error
}

func (f *fakeWebWriteRepo) Resolve(_ context.Context, submissionID, status string) error {
	f.calls++
	f.gotID = submissionID
	f.gotStatus = status
	return f.resolveErr
}

func (f *fakeWebWriteRepo) Create(context.Context, *persistence.WebWriteAction) error {
	panic("Create not expected")
}
func (f *fakeWebWriteRepo) Get(context.Context, string) (*persistence.WebWriteAction, error) {
	panic("Get not expected")
}
func (f *fakeWebWriteRepo) Approve(context.Context, string, string, string) error {
	panic("Approve not expected")
}
func (f *fakeWebWriteRepo) CASToSubmitting(context.Context, string) (bool, error) {
	panic("CASToSubmitting not expected")
}
func (f *fakeWebWriteRepo) Finalize(context.Context, string, string) error {
	panic("Finalize not expected")
}
func (f *fakeWebWriteRepo) Reject(context.Context, string, string) error {
	panic("Reject not expected")
}

// TestWebWriteResolveStatus covers the mutually-exclusive flag mapping: neither
// and both are usage errors; each single flag maps to its terminal status.
func TestWebWriteResolveStatus(t *testing.T) {
	cases := []struct {
		name              string
		submitted, failed bool
		want              string
		wantErr           bool
	}{
		{"neither", false, false, "", true},
		{"both", true, true, "", true},
		{"submitted", true, false, "submitted", false},
		{"failed", false, true, "failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := webWriteResolveStatus(tc.submitted, tc.failed)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want usage error, got status=%q nil err", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWebWriteResolve_Submitted: --submitted derives status "submitted" and
// forwards it to WebWriteRepo.Resolve for the given submission id.
func TestWebWriteResolve_Submitted(t *testing.T) {
	status, err := webWriteResolveStatus(true, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	repo := &fakeWebWriteRepo{}
	if err := resolveWebWrite(context.Background(), repo, "webwrite_s1", status); err != nil {
		t.Fatalf("resolveWebWrite: %v", err)
	}
	if repo.calls != 1 || repo.gotID != "webwrite_s1" || repo.gotStatus != "submitted" {
		t.Fatalf("Resolve got (id=%q status=%q calls=%d), want (webwrite_s1, submitted, 1)",
			repo.gotID, repo.gotStatus, repo.calls)
	}
}

// TestWebWriteResolve_Failed: --failed derives status "failed" and forwards it.
func TestWebWriteResolve_Failed(t *testing.T) {
	status, err := webWriteResolveStatus(false, true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	repo := &fakeWebWriteRepo{}
	if err := resolveWebWrite(context.Background(), repo, "webwrite_s2", status); err != nil {
		t.Fatalf("resolveWebWrite: %v", err)
	}
	if repo.gotStatus != "failed" || repo.gotID != "webwrite_s2" {
		t.Fatalf("Resolve got (id=%q status=%q), want (webwrite_s2, failed)", repo.gotID, repo.gotStatus)
	}
}

// TestWebWriteResolve_NotResolvable: a row not in submitting/unknown makes the
// repo return ErrNoTransition; the CLI surfaces a friendly, actionable message
// (not the raw sentinel) and reports nothing was changed.
func TestWebWriteResolve_NotResolvable(t *testing.T) {
	repo := &fakeWebWriteRepo{resolveErr: persistence.ErrNoTransition}
	err := resolveWebWrite(context.Background(), repo, "webwrite_s3", "submitted")
	if err == nil {
		t.Fatal("want an error when the row is not in a resolvable state")
	}
	msg := err.Error()
	for _, want := range []string{"webwrite_s3", "not in a resolvable state", "submitting/unknown"} {
		if !strings.Contains(msg, want) {
			t.Errorf("friendly error missing %q: %s", want, msg)
		}
	}
	// It must not leak the raw sentinel text.
	if strings.Contains(msg, "valid source state for transition") {
		t.Errorf("error should be friendly, not the raw sentinel: %s", msg)
	}
}
