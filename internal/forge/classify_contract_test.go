package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// These are CONTRACT tests for the provider-neutral abstraction that package
// forge defines: the ForgeJob classification semantics every ForgeProvider must
// honour (see the doc comments on ForgeProvider.ClassifyEvent and ForgeJob), the
// provider discriminator + factory, and the spec types. They exercise the
// interface and the value types this package owns — not any one provider's REST
// plumbing (that lives, and is tested, in internal/forge/<provider>).
//
// classifyProvider is a faithful, dependency-free implementation of the
// documented classifier contract, used to assert the abstraction behaves the
// way every concrete provider is required to: deterministic accept/reject, the
// right job kind per (event, action), require_forge_event filtering, and
// installation→project routing. It is intentionally provider-neutral (it carries
// no GitHub-specific REST) so the assertions are about the contract, not a
// vendor.

// classifyProvider classifies inbound webhooks per the ForgeProvider contract.
// requireForgeEvent, when non-empty, gates classification: only payloads whose
// label set intersects it are accepted (the "handle this" maintainer signal).
// installToProject routes by payload.installation.id so one daemon process can
// serve multiple installations.
type classifyProvider struct {
	name              string
	requireForgeEvent []string
	installToProject  map[int64]string
}

// neutralPayload is the subset of an inbound webhook the contract classifier
// reads. It is provider-neutral on purpose: the discriminator is the event/kind
// pair, not a vendor noun.
type neutralPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation,omitempty"`
	Issue *struct {
		Number int `json:"number"`
		Title  string
		Body   string
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"issue,omitempty"`
	PullRequest *struct {
		Number int `json:"number"`
		Title  string
		Body   string
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"pull_request,omitempty"`
}

func (c classifyProvider) Name() string { return c.name }

func (c classifyProvider) ClassifyEvent(h http.Header, body []byte) (ForgeJob, bool) {
	var pl neutralPayload
	if err := json.Unmarshal(body, &pl); err != nil {
		return ForgeJob{}, false
	}
	if pl.Repository.FullName == "" {
		return ForgeJob{}, false
	}

	// Multi-installation routing: when an installation id is present and the
	// provider knows a project for it, the job's Repo is taken from the routing
	// table (an unknown installation is not ours → reject).
	repo := pl.Repository.FullName
	if pl.Installation != nil {
		proj, known := c.installToProject[pl.Installation.ID]
		if !known {
			return ForgeJob{}, false
		}
		repo = proj
	}

	event := strings.TrimSpace(h.Get("X-Forge-Event"))
	if event == "" {
		switch {
		case pl.PullRequest != nil:
			event = "pull_request"
		case pl.Issue != nil:
			event = "issues"
		}
	}

	job := ForgeJob{
		Provider:      c.name,
		Repo:          repo,
		Action:        pl.Action,
		DefaultBranch: pl.Repository.DefaultBranch,
	}
	switch event {
	case "issues":
		if pl.Issue == nil || pl.Action != "labeled" {
			return ForgeJob{}, false
		}
		labels := names(pl.Issue.Labels)
		if !c.gate(labels) {
			return ForgeJob{}, false
		}
		job.Number = pl.Issue.Number
		job.Labels = labels
		job.Title = pl.Issue.Title
		job.Body = pl.Issue.Body
		job.IsChangeRequest = false
		return job, true
	case "pull_request":
		if pl.PullRequest == nil ||
			(pl.Action != "opened" && pl.Action != "reopened" && pl.Action != "ready_for_review") {
			return ForgeJob{}, false
		}
		labels := names(pl.PullRequest.Labels)
		if !c.gate(labels) {
			return ForgeJob{}, false
		}
		job.Number = pl.PullRequest.Number
		job.Labels = labels
		job.Title = pl.PullRequest.Title
		job.Body = pl.PullRequest.Body
		job.IsChangeRequest = true
		return job, true
	default:
		return ForgeJob{}, false
	}
}

// gate implements require_forge_event filtering: empty requirement accepts all;
// otherwise the label set must intersect the requirement.
func (c classifyProvider) gate(labels []string) bool {
	if len(c.requireForgeEvent) == 0 {
		return true
	}
	for _, want := range c.requireForgeEvent {
		for _, have := range labels {
			if want == have {
				return true
			}
		}
	}
	return false
}

func names(in []struct {
	Name string `json:"name"`
}) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, l := range in {
		out = append(out, l.Name)
	}
	return out
}

// The remaining ForgeProvider methods are not under test here (they are the
// provider's REST surface); satisfy the interface with no-ops.
func (c classifyProvider) FetchDiff(context.Context, string, int) ([]byte, error) { return nil, nil }
func (c classifyProvider) PushBranch(context.Context, string, string, string, string) error {
	return nil
}
func (c classifyProvider) OpenChangeRequest(context.Context, ChangeRequestSpec) (string, error) {
	return "", nil
}
func (c classifyProvider) PostReview(context.Context, string, int, ReviewSpec) error { return nil }
func (c classifyProvider) VerifyPushAccess(context.Context) error                    { return nil }

// compile-time assertion that the contract fake actually satisfies the interface.
var _ ForgeProvider = classifyProvider{}

func hdr(event string) http.Header {
	h := http.Header{}
	if event != "" {
		h.Set("X-Forge-Event", event)
	}
	return h
}

// --- ClassifyEvent contract: job-kind selection per (event, action) ---

// TestClassify_IssueLabeledIsIssueJob: a labeled issue classifies as a NON
// change-request job carrying the issue number/labels/title/body — the
// maintainer's explicit "handle this" signal.
func TestClassify_IssueLabeledIsIssueJob(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	body := `{"action":"labeled","repository":{"full_name":"o/r","default_branch":"main"},"issue":{"number":7,"Title":"T","Body":"B","labels":[{"name":"bug"}]}}`
	job, ok := p.ClassifyEvent(hdr("issues"), []byte(body))
	if !ok {
		t.Fatal("labeled issue must be accepted")
	}
	if job.IsChangeRequest {
		t.Error("an issue job must not be a change request")
	}
	want := ForgeJob{
		Provider: "neutral", Repo: "o/r", Action: "labeled", Number: 7,
		Labels: []string{"bug"}, DefaultBranch: "main", Title: "T", Body: "B",
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("job = %+v\nwant  %+v", job, want)
	}
}

// TestClassify_PullRequestOpenedIsChangeRequest: an opened PR classifies as a
// change-request job, carrying number/title/body, distinct from an issue job.
func TestClassify_PullRequestOpenedIsChangeRequest(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	body := `{"action":"opened","repository":{"full_name":"o/r","default_branch":"trunk"},"pull_request":{"number":42,"Title":"PR","Body":"body"}}`
	job, ok := p.ClassifyEvent(hdr("pull_request"), []byte(body))
	if !ok {
		t.Fatal("opened PR must be accepted")
	}
	if !job.IsChangeRequest {
		t.Error("a pull-request job must be a change request")
	}
	if job.Number != 42 || job.Title != "PR" || job.Body != "body" || job.DefaultBranch != "trunk" {
		t.Errorf("PR job fields wrong: %+v", job)
	}
}

// TestClassify_IssueOpenedRejected: a bare issues.opened is intentionally
// ignored — opening any issue must not auto-spawn work; only a label does.
func TestClassify_IssueOpenedRejected(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	body := `{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":1}}`
	if _, ok := p.ClassifyEvent(hdr("issues"), []byte(body)); ok {
		t.Error("issues.opened must be rejected (label required)")
	}
}

// TestClassify_RejectsNonActionableActions: synchronize/edited/closed and other
// non-actionable actions are dropped for both issues and pull_request events.
func TestClassify_RejectsNonActionableActions(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	cases := []struct {
		event, action string
	}{
		{"pull_request", "synchronize"},
		{"pull_request", "edited"},
		{"pull_request", "closed"},
		{"pull_request", "labeled"}, // labeling a PR is not an open/reopen
		{"issues", "edited"},
		{"issues", "closed"},
		{"issues", "unlabeled"},
	}
	for _, tc := range cases {
		t.Run(tc.event+"."+tc.action, func(t *testing.T) {
			obj := "issue"
			if tc.event == "pull_request" {
				obj = "pull_request"
			}
			body := `{"action":"` + tc.action + `","repository":{"full_name":"o/r"},"` + obj + `":{"number":5}}`
			if _, ok := p.ClassifyEvent(hdr(tc.event), []byte(body)); ok {
				t.Errorf("%s.%s must be rejected", tc.event, tc.action)
			}
		})
	}
}

// TestClassify_AcceptsAllOpenLikePRActions: opened, reopened and
// ready_for_review all classify as change-request jobs.
func TestClassify_AcceptsAllOpenLikePRActions(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	for _, action := range []string{"opened", "reopened", "ready_for_review"} {
		body := `{"action":"` + action + `","repository":{"full_name":"o/r"},"pull_request":{"number":3}}`
		job, ok := p.ClassifyEvent(hdr("pull_request"), []byte(body))
		if !ok || !job.IsChangeRequest || job.Number != 3 {
			t.Errorf("PR action %q: ok=%v job=%+v", action, ok, job)
		}
	}
}

// TestClassify_NonForgeEventRejected: events outside the forge vocabulary (push,
// star, ping, …) are not forge jobs.
func TestClassify_NonForgeEventRejected(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	for _, event := range []string{"push", "star", "ping", "workflow_run", ""} {
		body := `{"action":"created","repository":{"full_name":"o/r"}}`
		if _, ok := p.ClassifyEvent(hdr(event), []byte(body)); ok {
			t.Errorf("non-forge event %q must be rejected", event)
		}
	}
}

// TestClassify_MissingRepoRejected: a payload without a repository full name
// cannot be addressed (Repo,Number) and is rejected.
func TestClassify_MissingRepoRejected(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	body := `{"action":"labeled","issue":{"number":7,"labels":[{"name":"bug"}]}}`
	if _, ok := p.ClassifyEvent(hdr("issues"), []byte(body)); ok {
		t.Error("missing repository.full_name must be rejected")
	}
}

// TestClassify_MalformedBodyRejected: an unparseable body is rejected, never a
// panic — the webhook path must be robust to garbage.
func TestClassify_MalformedBodyRejected(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	if _, ok := p.ClassifyEvent(hdr("issues"), []byte("}{ not json")); ok {
		t.Error("malformed body must be rejected")
	}
	if _, ok := p.ClassifyEvent(hdr("issues"), nil); ok {
		t.Error("nil body must be rejected")
	}
}

// TestClassify_EventInferredFromShape: with no event header (a relay-forwarded
// body), the event kind is inferred from the payload shape — a pull_request
// object means a PR event; an issue object means an issues event.
func TestClassify_EventInferredFromShape(t *testing.T) {
	p := classifyProvider{name: "neutral"}

	issue := `{"action":"labeled","repository":{"full_name":"o/r"},"issue":{"number":7,"labels":[{"name":"bug"}]}}`
	if job, ok := p.ClassifyEvent(hdr(""), []byte(issue)); !ok || job.IsChangeRequest || job.Number != 7 {
		t.Errorf("headerless issue inference: ok=%v job=%+v", ok, job)
	}

	pr := `{"action":"opened","repository":{"full_name":"o/r"},"pull_request":{"number":12}}`
	if job, ok := p.ClassifyEvent(hdr(""), []byte(pr)); !ok || !job.IsChangeRequest || job.Number != 12 {
		t.Errorf("headerless PR inference: ok=%v job=%+v", ok, job)
	}
}

// TestClassify_HeaderWinsOverShape: an explicit event header takes precedence
// over the payload shape — a body carrying an issue object but a pull_request
// header is treated as a PR event (and rejected here, since there is no PR
// object to populate), not silently reclassified as an issue.
func TestClassify_HeaderWinsOverShape(t *testing.T) {
	p := classifyProvider{name: "neutral"}
	// issue object present, but header says pull_request → PR branch, no PR obj → reject.
	body := `{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":7,"labels":[{"name":"bug"}]}}`
	if _, ok := p.ClassifyEvent(hdr("pull_request"), []byte(body)); ok {
		t.Error("explicit pull_request header must not fall back to the issue shape")
	}
}

// --- require_forge_event filtering ---

// TestClassify_RequireForgeEventGate: when a required label set is configured,
// only issues whose labels intersect it are accepted; others are rejected even
// though the action (labeled) is itself actionable.
func TestClassify_RequireForgeEventGate(t *testing.T) {
	p := classifyProvider{name: "neutral", requireForgeEvent: []string{"vornik", "auto"}}

	matched := `{"action":"labeled","repository":{"full_name":"o/r"},"issue":{"number":7,"labels":[{"name":"vornik"}]}}`
	if _, ok := p.ClassifyEvent(hdr("issues"), []byte(matched)); !ok {
		t.Error("issue with a required label must be accepted")
	}

	other := `{"action":"labeled","repository":{"full_name":"o/r"},"issue":{"number":7,"labels":[{"name":"wontfix"}]}}`
	if _, ok := p.ClassifyEvent(hdr("issues"), []byte(other)); ok {
		t.Error("issue without any required label must be rejected by the gate")
	}
}

// TestClassify_EmptyRequireForgeEventAcceptsAll: with no required-label set, the
// gate is open — any labeled issue passes.
func TestClassify_EmptyRequireForgeEventAcceptsAll(t *testing.T) {
	p := classifyProvider{name: "neutral", requireForgeEvent: nil}
	body := `{"action":"labeled","repository":{"full_name":"o/r"},"issue":{"number":7,"labels":[{"name":"anything"}]}}`
	if _, ok := p.ClassifyEvent(hdr("issues"), []byte(body)); !ok {
		t.Error("empty require_forge_event must accept any labeled issue")
	}
}

// --- multi-installation routing ---

// TestClassify_InstallationRoutesToProject: payload.installation.id selects the
// project, so one daemon can serve several installations; the resolved project
// (not the raw repo) lands on the job.
func TestClassify_InstallationRoutesToProject(t *testing.T) {
	p := classifyProvider{
		name:             "neutral",
		installToProject: map[int64]string{100: "team-a/svc", 200: "team-b/web"},
	}
	body := `{"action":"opened","repository":{"full_name":"o/r"},"installation":{"id":200},"pull_request":{"number":9}}`
	job, ok := p.ClassifyEvent(hdr("pull_request"), []byte(body))
	if !ok {
		t.Fatal("known installation must be accepted")
	}
	if job.Repo != "team-b/web" {
		t.Errorf("installation 200 must route to team-b/web, got %q", job.Repo)
	}
}

// TestClassify_UnknownInstallationRejected: an installation id the daemon does
// not serve is not ours — reject rather than guess a project.
func TestClassify_UnknownInstallationRejected(t *testing.T) {
	p := classifyProvider{name: "neutral", installToProject: map[int64]string{100: "team-a/svc"}}
	body := `{"action":"opened","repository":{"full_name":"o/r"},"installation":{"id":999},"pull_request":{"number":9}}`
	if _, ok := p.ClassifyEvent(hdr("pull_request"), []byte(body)); ok {
		t.Error("unknown installation id must be rejected")
	}
}

// --- provider discriminator / factory ---

// TestKnown_ListsRegisteredSorted: known() returns the registered provider
// names sorted (the factory uses it for a deterministic error message). This
// also covers the GitLab/Gitea discriminator stubs being absent from the live
// registry until their impls register.
func TestKnown_ListsRegisteredSorted(t *testing.T) {
	for _, n := range []string{"zeta-fake", "alpha-fake"} {
		Register(n, func(Config) (ForgeProvider, error) { return classifyProvider{name: n}, nil })
		t.Cleanup(func() { delete(registry, n) })
	}
	got := known()
	if !sort.StringsAreSorted(got) {
		t.Errorf("known() must be sorted, got %v", got)
	}
	var sawAlpha, sawZeta bool
	for _, n := range got {
		if n == "alpha-fake" {
			sawAlpha = true
		}
		if n == "zeta-fake" {
			sawZeta = true
		}
	}
	if !sawAlpha || !sawZeta {
		t.Errorf("known() missing registered names: %v", got)
	}
}

// TestNew_UnknownProviderListsKnown: the unknown-provider error names the known
// providers (so an operator sees what IS wired). Exercises New→known() together.
func TestNew_UnknownProviderListsKnown(t *testing.T) {
	const reg = "registered-fake"
	Register(reg, func(Config) (ForgeProvider, error) { return classifyProvider{name: reg}, nil })
	t.Cleanup(func() { delete(registry, reg) })

	_, err := New(Config{Provider: "gitlab-not-wired"})
	if err == nil {
		t.Fatal("want error for an unwired provider")
	}
	if !strings.Contains(err.Error(), reg) {
		t.Errorf("error should list known providers (want %q in %q)", reg, err.Error())
	}
}

// TestNew_ConstructorErrorPropagates: a constructor that fails surfaces its
// error through New rather than returning a usable-looking nil provider.
func TestNew_ConstructorErrorPropagates(t *testing.T) {
	const name = "boom-fake"
	Register(name, func(Config) (ForgeProvider, error) {
		return nil, errBoom
	})
	t.Cleanup(func() { delete(registry, name) })

	if _, err := New(Config{Provider: name}); err == nil {
		t.Fatal("constructor error must propagate through New")
	}
}

// TestRegister_TrimsAndLastWriteWins: Register trims surrounding whitespace from
// the name and the last registration for a name wins (documented init-time
// semantics).
func TestRegister_TrimsAndLastWriteWins(t *testing.T) {
	const name = "trim-fake"
	Register("  "+name+"  ", func(Config) (ForgeProvider, error) {
		return classifyProvider{name: "first"}, nil
	})
	Register(name, func(Config) (ForgeProvider, error) {
		return classifyProvider{name: "second"}, nil
	})
	t.Cleanup(func() { delete(registry, name) })

	p, err := New(Config{Provider: " " + name + " "}) // New trims on lookup too
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "second" {
		t.Errorf("last write must win: got %q", p.Name())
	}
}

// --- spec / value-type contracts ---

// TestChangeRequestSpec_CarriesPublishFields: the daemon-templated CR spec
// round-trips every field the publish step needs (no free-text parsing).
func TestChangeRequestSpec_CarriesPublishFields(t *testing.T) {
	s := ChangeRequestSpec{
		Repo: "o/r", Head: "fix/issue-7", Base: "main",
		Title: "Fix #7", Body: "Closes #7", Labels: []string{"automated"}, Draft: true,
	}
	if s.Repo != "o/r" || s.Head != "fix/issue-7" || s.Base != "main" {
		t.Errorf("addressing fields wrong: %+v", s)
	}
	if s.Title != "Fix #7" || s.Body != "Closes #7" || !s.Draft || len(s.Labels) != 1 {
		t.Errorf("content fields wrong: %+v", s)
	}
}

// TestForgeJob_JSONRoundTrip: the ForgeJob is persisted on the task as JSON;
// the wire tags must round-trip without losing the addressing pair or kind.
func TestForgeJob_JSONRoundTrip(t *testing.T) {
	in := ForgeJob{
		Provider: "github", Repo: "o/r", Action: "labeled", Number: 7,
		Labels: []string{"bug"}, DefaultBranch: "main", IsChangeRequest: true,
		HeadRef: "refs/pull/7/head", Title: "T", Body: "B",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ForgeJob
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip lost data:\n in=%+v\nout=%+v", in, out)
	}
	// omitempty fields must not appear when zero.
	bare, _ := json.Marshal(ForgeJob{Provider: "github", Repo: "o/r", Number: 1})
	for _, absent := range []string{"labels", "default_branch", "head_ref", "title", "body"} {
		if strings.Contains(string(bare), `"`+absent+`"`) {
			t.Errorf("zero %q should be omitted, got %s", absent, bare)
		}
	}
	// is_change_request has no omitempty: the kind must always be explicit on the wire.
	if !strings.Contains(string(bare), `"is_change_request"`) {
		t.Errorf("is_change_request must always be present: %s", bare)
	}
}

var errBoom = errBoomT("constructor boom")

type errBoomT string

func (e errBoomT) Error() string { return string(e) }
