package forge

import (
	"context"
	"net/http"
	"testing"
)

// fakeProvider is a minimal ForgeProvider for exercising the registry/factory.
type fakeProvider struct{ name string }

func (f fakeProvider) Name() string                                           { return f.name }
func (f fakeProvider) ClassifyEvent(http.Header, []byte) (ForgeJob, bool)     { return ForgeJob{}, false }
func (f fakeProvider) FetchDiff(context.Context, string, int) ([]byte, error) { return nil, nil }
func (f fakeProvider) PushBranch(context.Context, string, string, string, string) error {
	return nil
}
func (f fakeProvider) OpenChangeRequest(context.Context, ChangeRequestSpec) (string, error) {
	return "", nil
}
func (f fakeProvider) PostReview(context.Context, string, int, ReviewSpec) error { return nil }
func (f fakeProvider) VerifyPushAccess(context.Context) error                    { return nil }

// TestNew_DispatchesByProvider: a registered constructor is selected by name and
// receives the config it was asked for.
func TestNew_DispatchesByProvider(t *testing.T) {
	const name = "fake-dispatch"
	var gotCfg Config
	Register(name, func(c Config) (ForgeProvider, error) {
		gotCfg = c
		return fakeProvider{name: name}, nil
	})
	t.Cleanup(func() { delete(registry, name) })

	p, err := New(Config{Provider: name, GitHub: GitHubConfig{AppID: 7}})
	if err != nil {
		t.Fatalf("New: unexpected error %v", err)
	}
	if p.Name() != name {
		t.Errorf("want provider %q, got %q", name, p.Name())
	}
	if gotCfg.GitHub.AppID != 7 {
		t.Errorf("constructor did not receive the config (AppID=%d)", gotCfg.GitHub.AppID)
	}
}

// TestNew_UnknownProvider: an unregistered provider name is a clear error, not a
// nil panic — so a typo'd config fails loudly at wire time.
func TestNew_UnknownProvider(t *testing.T) {
	_, err := New(Config{Provider: "bitbucket-nope"})
	if err == nil {
		t.Fatal("want error for unknown provider, got nil")
	}
}

// TestNew_EmptyProvider: no provider configured is an error, not a silent nil.
func TestNew_EmptyProvider(t *testing.T) {
	if _, err := New(Config{Provider: "  "}); err == nil {
		t.Fatal("want error for empty provider, got nil")
	}
}

// TestReviewEvents: the three review events have stable string encodings (the
// GitHub impl maps these onto the Reviews API; forges without reviews map them
// onto notes/approvals).
func TestReviewEvents(t *testing.T) {
	for ev, want := range map[ReviewEvent]string{
		ReviewComment:        "comment",
		ReviewApprove:        "approve",
		ReviewRequestChanges: "request_changes",
	} {
		if string(ev) != want {
			t.Errorf("ReviewEvent %v: want %q", ev, want)
		}
	}
}
