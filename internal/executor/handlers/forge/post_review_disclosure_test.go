package forge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Regression tests for G6 finding A (six-surface Art 50 trace, 2026-07-29).
//
// forge.post_review posted the reviewer agent's prose straight to
// provider.PostReview with no AI disclosure of any kind, and the surface runs
// against a PUBLIC repository (grinco/headmatch). aidisclosure was wired
// only into dispatcher/channel_receiver.go and ui/chat.go, so this path reached
// a human — a developer reading a review on their pull request — outside the
// chokepoint the package doc claims no channel can bypass. Art 50(1) binds
// systems intended to interact directly with natural persons; it does not care
// that the text is a code review rather than public-interest information.
//
// Design: https://docs.vornik.io §4

// realDiscloser is the shipped policy, not a stub: these tests assert the
// wording that actually reaches GitHub. The package is a pure leaf with no I/O,
// so there is nothing to fake.
func realDiscloser() *aidisclosure.Service {
	return aidisclosure.New(aidisclosure.Config{}, nil)
}

func noticeText() string { return realDiscloser().PublicationNotice().Text }

// wantDisclosedBody is the exact body a review of `body` must post.
func wantDisclosedBody(body string) string {
	return body + "\n\n---\n" + noticeText()
}

func TestPostReview_G6A_BodyCarriesTheAIDisclosure(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	in := executor.SystemStepInput{
		Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 3}),
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"body":"looks good"}`),
	}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := prov.gotReview.Body
	if !strings.Contains(got, noticeText()) {
		t.Fatalf("posted body carries no AI disclosure:\n%s", got)
	}
	if got != wantDisclosedBody("looks good") {
		t.Errorf("body = %q, want %q", got, wantDisclosedBody("looks good"))
	}
	// The review must come first: a reader skimming the top of a comment sees
	// the substance, and the disclosure is a trailer, not a header.
	if !strings.HasPrefix(got, "looks good") {
		t.Errorf("review text must lead the body, got %q", got)
	}
}

// A nil disclosure means the binary was wired wrong. The conforming response to
// "I cannot prove I disclosed" is to refuse the post, not to publish
// undisclosed — the same fail-safe direction as CadenceFor's unknown-channel
// default. Critically: nothing may reach the provider.
func TestPostReview_G6A_NilDiscloserRefusesAndPostsNothing(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, nil)
	in := executor.SystemStepInput{
		Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 3}),
		PrevResult: json.RawMessage(`{"body":"looks good"}`),
	}
	if _, err := h.Execute(context.Background(), in); err == nil {
		t.Fatal("a nil discloser must refuse the post, got nil error")
	}
	if prov.gotReview.Body != "" {
		t.Errorf("nothing may be posted when the disclosure is unavailable, got %q", prov.gotReview.Body)
	}
}

// The notice is appended once, after the rendered structured review — not
// interleaved into the reviewer's markdown and not repeated per section.
func TestPostReview_G6A_NoticeAppendedOnceAfterStructuredReview(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	prev := `{"message":"{\"review\":{\"approved\":true,\"feedback\":\"Looks solid.\",\"summary\":\"Approved.\"}}"}`
	in := executor.SystemStepInput{
		Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 7}),
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(prev),
	}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := prov.gotReview.Body
	if n := strings.Count(got, noticeText()); n != 1 {
		t.Fatalf("notice appears %d times, want exactly 1:\n%s", n, got)
	}
	if !strings.HasSuffix(got, noticeText()) {
		t.Errorf("notice must be the trailer, got:\n%s", got)
	}
	// The rendered review survives intact ahead of it.
	for _, want := range []string{"✅ Approved", "Looks solid.", "Approved."} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered review lost %q:\n%s", want, got)
		}
	}
}

// An empty review body still errors, and still posts nothing — the disclosure
// must not become the body of an otherwise-empty review.
func TestPostReview_G6A_EmptyReviewDoesNotBecomeADisclosureOnlyComment(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	in := executor.SystemStepInput{
		Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 3}),
		PrevResult: json.RawMessage(`{}`),
	}
	if _, err := h.Execute(context.Background(), in); err == nil {
		t.Fatal("empty review body should error")
	}
	if prov.gotReview.Body != "" {
		t.Errorf("nothing should be posted, got %q", prov.gotReview.Body)
	}
}

// The disclosure is orthogonal to the gating decision: adding it must not
// change which forge review event is posted.
func TestPostReview_G6A_DisclosureDoesNotAffectGatingEvent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prev      string
		gating    bool
		wantEvent forgeapi.ReviewEvent
	}{
		{"approved + gating", `{"body":"ok","event":"approve"}`, true, forgeapi.ReviewApprove},
		{"approved + no gating", `{"body":"ok","event":"approve"}`, false, forgeapi.ReviewComment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{}
			h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
			in := executor.SystemStepInput{
				Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 3}),
				Step:       &registry.WorkflowStep{Handler: "forge.post_review", GatingReviews: tc.gating},
				PrevResult: json.RawMessage(tc.prev),
			}
			if _, err := h.Execute(context.Background(), in); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if prov.gotReview.Event != tc.wantEvent {
				t.Errorf("event = %q, want %q", prov.gotReview.Event, tc.wantEvent)
			}
		})
	}
}

// A missing forge job must refuse before the disclosure is even consulted —
// pins that the new dependency did not reorder the existing guards.
func TestPostReview_G6A_MissingJobStillRefuses(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{},
		PrevResult: json.RawMessage(`{"body":"x"}`),
	}); err == nil {
		t.Fatal("missing job should error")
	}
}
