package forge

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forgeci"
	"vornik.io/vornik/internal/persistence"
)

// forgeci.NeedsChangeRequest carries a list of the handlers that refuse a job
// with no pull request. That list lives in another package and nothing in the
// compiler ties it to what the handlers DO — so this test executes every forge
// system handler against a PR-less job and asserts that the list agrees with
// the behaviour, in both directions (review-20260909-f95b, MEDIUM: "the test
// does not exist"). A handler added here that requires a pull request and is
// not on the list re-creates headmatch task_20260909150605_13cc14a3d079dc09:
// the success trigger enqueues a workflow whose first step must refuse.
func TestChangeRequestHandlerListMatchesTheHandlers(t *testing.T) {
	prLess := taskWithJob(forgeapi.ForgeJob{Repo: "acme/infra", HeadSHA: "320683a8"})
	in := executor.SystemStepInput{Task: prLess}
	prov := &fakeProvider{}
	handlers := []executor.SystemHandler{
		NewFetchDiffHandler(fakeResolver{p: prov}),
		NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()),
		NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, nil, realDiscloser()),
		NewFetchCIHandler(stubCIOutcomes{rows: []*persistence.ForgeCIOutcome{{RunID: 1, Conclusion: "success", HeadSHA: "320683a8"}}}),
	}
	for _, h := range handlers {
		name := h.Name()
		_, err := h.Execute(context.Background(), in)
		_, refuses := forgeapi.AsPermanent(err)
		listed := forgeci.NeedsChangeRequest([]string{name})
		switch {
		case refuses && !listed:
			t.Errorf("%s refuses a PR-less job but forgeci.NeedsChangeRequest does not list it — "+
				"the success trigger would enqueue a workflow that must fail", name)
		case !refuses && listed:
			t.Errorf("%s is listed as needing a pull request but accepted a PR-less job (err=%v) — "+
				"the success trigger would withhold a workflow that can run", name, err)
		}
	}
}
