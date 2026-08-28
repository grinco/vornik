package api

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/imagemanifest"
)

// The six-scenario truth table from the design (§10), adopted verbatim from
// round-3 review, which verified the dual comparison by tracing every
// combination rather than reasoning about it.
//
// Row "upgraded, not restarted" is the one draft v2 of the design got WRONG:
// an rpm upgrade replaces the binary on disk while the running process keeps
// executing the old one, so record=N image=N daemon=N-1 is ordinary — and a
// single image↔record comparison reports OK on a genuinely half-applied host.

const (
	commitN    = "4b343821000000000000000000000000000000ab"
	commitPrev = "9f3c1a2000000000000000000000000000000cd0"
	digestN    = "sha256:8b41a998f6080f06462866a2ae50ad40c1ca9bc11ae06f991044e5a6e6d24393"
)

// recordFor builds a one-image record declaring commit.
func recordFor(commit string) func() (*imagemanifest.ReleaseRecord, error) {
	return func() (*imagemanifest.ReleaseRecord, error) {
		return &imagemanifest.ReleaseRecord{
			Version: imagemanifest.RecordVersion,
			Count:   1,
			Images: []imagemanifest.ImageRecord{{
				Tag:          imagemanifest.AgentImageTag,
				Digest:       digestN,
				SourceCommit: commit,
			}},
		}, nil
	}
}

// onlyAgentProber restricts Deployable() to the agent image, so the table
// exercises the comparison rather than this host's optional stacks.
type onlyAgentProber struct{}

func (onlyAgentProber) UnitEnabled(string) bool        { return false }
func (onlyAgentProber) StackHasContainers(string) bool { return false }

func freshnessFor(t *testing.T, imageCommit, recordCommit, daemonCommit string) DoctorCheck {
	t.Helper()
	h := &DoctorHandlers{
		imageProber:        onlyAgentProber{},
		imageRecordFunc:    recordFor(recordCommit),
		daemonRevisionFunc: func() (string, bool) { return daemonCommit, true },
		imageRevisionFunc: func(_ context.Context, _ string) (string, bool, error) {
			return imageCommit, true, nil
		},
	}
	return h.checkImageFreshness(context.Background())
}

func TestImageFreshness_SixScenarioTable(t *testing.T) {
	cases := []struct {
		name                  string
		image, record, daemon string
		want                  string
		wantMentions          string
	}{
		{"fresh package install", commitN, commitN, commitN, "OK", ""},
		// The row draft v2 got wrong.
		{"upgraded, not restarted", commitN, commitN, commitPrev, "WARNING", "restart"},
		{"staged rollout, daemon newer", commitPrev, commitN, commitN, "WARNING", "declares"},
		{"staged rollout, image newer", commitN, commitN, commitPrev, "WARNING", "restart"},
		// A deliberate pin still warns: the check REPORTS drift rather than
		// enforcing it, and the operator chose a state it exists to report.
		{"deliberate pin", commitPrev, commitN, commitN, "WARNING", "declares"},
		{"local rebuild, same commit", commitN, commitN, commitN, "OK", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := freshnessFor(t, tc.image, tc.record, tc.daemon)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q\nmessage: %s\nitems: %v",
					got.Status, tc.want, got.Message, got.Items)
			}
			if tc.wantMentions != "" {
				joined := got.Message + " " + strings.Join(got.Items, " ")
				if !strings.Contains(joined, tc.wantMentions) {
					t.Errorf("expected the report to mention %q, got: %s", tc.wantMentions, joined)
				}
			}
		})
	}
}

// The restart-pending message must name its own remedy. A generic drift notice
// is what the previous check emitted and is precisely what erodes.
func TestImageFreshness_RestartPendingNamesTheFix(t *testing.T) {
	got := freshnessFor(t, commitN, commitN, commitPrev)
	joined := got.Message + " " + strings.Join(got.Items, " ")
	if !strings.Contains(joined, "systemctl restart vornik-enterprise") {
		t.Errorf("the restart-pending finding must name the command; got: %s", joined)
	}
}

// CORRUPT IS NOT ABSENT. A corrupt record must say the check is disabled, not
// degrade silently into the legacy path — that is a guard that looks protective
// and is not.
func TestImageFreshness_CorruptRecordSaysCheckIsDisabled(t *testing.T) {
	h := &DoctorHandlers{
		imageProber:        onlyAgentProber{},
		daemonRevisionFunc: func() (string, bool) { return commitN, true },
		imageRecordFunc: func() (*imagemanifest.ReleaseRecord, error) {
			return nil, &imagemanifest.CorruptRecordError{Reason: "count says 3 but 2 present"}
		},
	}
	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("corrupt record must WARN, got %q", got.Status)
	}
	if !strings.Contains(got.Message, "DISABLED") {
		t.Errorf("the operator must be told the check is not running; got: %s", got.Message)
	}
}

// The record-absent path is the one every pre-existing host lands on, so it
// must behave exactly as it did before the record existed.
func TestImageFreshness_AbsentRecordFallsBackToLegacy(t *testing.T) {
	h := &DoctorHandlers{
		imageProber:        onlyAgentProber{},
		daemonRevisionFunc: func() (string, bool) { return commitN, true },
		imageRecordFunc: func() (*imagemanifest.ReleaseRecord, error) {
			return nil, imagemanifest.ErrRecordAbsent
		},
		imageRevisionFunc: func(_ context.Context, _ string) (string, bool, error) {
			return commitPrev, true, nil
		},
	}
	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("legacy path should still warn on a stale image, got %q", got.Status)
	}
	// The legacy message is the pre-record wording, not the record wording.
	if strings.Contains(got.Message, "this release declares") {
		t.Errorf("absent-record path must use the legacy message, got: %s", got.Message)
	}
}

// One image passing must not read as the deployment passing.
func TestImageFreshness_AggregateIsNeverGreenerThanWorstRow(t *testing.T) {
	h := &DoctorHandlers{
		imageProber:        onlyAgentProber{},
		daemonRevisionFunc: func() (string, bool) { return commitN, true },
		imageRecordFunc: func() (*imagemanifest.ReleaseRecord, error) {
			return &imagemanifest.ReleaseRecord{
				Version: imagemanifest.RecordVersion, Count: 1,
				Images: []imagemanifest.ImageRecord{{
					Tag: imagemanifest.AgentImageTag, Digest: digestN, SourceCommit: commitN,
				}},
			}, nil
		},
		// The agent image is stale.
		imageRevisionFunc: func(_ context.Context, _ string) (string, bool, error) {
			return commitPrev, true, nil
		},
	}
	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("any warning row must make the aggregate WARNING, got %q", got.Status)
	}
	if !strings.Contains(got.Message, "WARNING") {
		t.Errorf("the aggregate line must state the warning count; got: %s", got.Message)
	}
}

// THE CROSS-EDITION CASE, and the one that makes Stage 2 usable at all.
//
// An image PULLED from GHCR was built in the public CE repo, so its
// org.opencontainers.image.revision label is a CE commit — while the release
// record shipped in the EE package declares an EE commit. Those can never
// match, because the export maps one tree onto the other.
//
// A digest needs no such mapping. When the image's digest IS the digest this
// release declares, that is the strongest statement available and the commit
// label is irrelevant. Without this, every pulled image warns forever and the
// registry path is unusable.
func TestImageFreshness_DigestMatchWinsOverForeignCommitLabel(t *testing.T) {
	const ceCommit = "1111111111111111111111111111111111111111"
	h := &DoctorHandlers{
		imageProber:        onlyAgentProber{},
		daemonRevisionFunc: func() (string, bool) { return commitN, true },
		imageRecordFunc:    recordFor(commitN),
		// The label is the CE commit — deliberately NOT the record's commit.
		imageRevisionFunc: func(_ context.Context, _ string) (string, bool, error) {
			return ceCommit, true, nil
		},
		// The digest is exactly what the release declares.
		imageDigestFunc: func(_ context.Context, _ string) (string, error) {
			return digestN, nil
		},
	}
	got := h.checkImageFreshness(context.Background())
	if got.Status != "OK" {
		t.Fatalf("a digest-exact image must be OK regardless of its commit label, got %q\n%s\n%v",
			got.Status, got.Message, got.Items)
	}
}

// A digest MISMATCH must not be silently forgiven by the commit falling back to
// a match — the fallback exists for local rebuilds, not for a wrong artifact.
func TestImageFreshness_DigestMismatchStillFallsBackToCommit(t *testing.T) {
	h := &DoctorHandlers{
		imageProber:        onlyAgentProber{},
		daemonRevisionFunc: func() (string, bool) { return commitN, true },
		imageRecordFunc:    recordFor(commitN),
		// Same commit as the record — a legitimate local rebuild.
		imageRevisionFunc: func(_ context.Context, _ string) (string, bool, error) {
			return commitN, true, nil
		},
		// Different digest, because a rebuild is not bit-reproducible.
		imageDigestFunc: func(_ context.Context, _ string) (string, error) {
			return "sha256:" + strings.Repeat("f", 64), nil
		},
	}
	got := h.checkImageFreshness(context.Background())
	if got.Status != "OK" {
		t.Fatalf("a local rebuild of the declared commit must stay OK, got %q: %s",
			got.Status, got.Message)
	}
}
