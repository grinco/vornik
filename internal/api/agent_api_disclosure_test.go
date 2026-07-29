package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// Regression tests for G6 finding B (six-surface Art 50 trace, 2026-07-29).
//
// The `moltbook` gateway provider autonomously publishes to a public social
// platform — comments on strangers' threads every 6h, an original post every
// 24h — through a single-step workflow with no review step and no draft. It is
// a human-facing Art 50(1) surface that never touches
// dispatcher/channel_receiver.go, so the channel chokepoint does not cover it.
// Disclosure existed only as Phase 4 of the `moltbook-engagement` knowledge
// skill: an instruction, with no code enforcement and no test. A model that
// skipped it published anyway.
//
// Design: https://docs.vornik.io §5

const disclosureTestProvider = "moltbook"

func publicationNoticeText() string {
	return aidisclosure.New(aidisclosure.Config{}, nil).PublicationNotice().Text
}

// disclosureTestServer builds a server whose `moltbook` provider is a
// publication surface gated on the `content` field, with writes permitted so the
// disclosure gate is the only thing that can refuse.
func disclosureTestServer(t *testing.T, gw *fakeQueryGateway, metrics *AgentAPIWriteMetrics) *Server {
	t.Helper()
	reg := loadAPIQueryTestRegistry(t, nil) // empty ⇒ all providers allowed
	repo := newAgentWriteTaskRepo(task(agentWriteTestTaskID, "", persistence.TaskCreationSourceUser))
	cfg := &config.Config{}
	cfg.Gateway = config.GatewayConfig{
		Enabled: true,
		Providers: map[string]config.ProviderConfig{
			disclosureTestProvider: {
				BasePath:      "/moltbook",
				WritesEnabled: true,
				Disclosure: config.ProviderDisclosureConfig{
					Required:      true,
					ContentFields: []string{"content"},
				},
			},
			// A non-publication provider in the same config: proves the gate is
			// opt-in and does not leak onto every provider.
			"maps": {BasePath: "/maps", WritesEnabled: true},
		},
	}
	return &Server{
		logger: zerolog.Nop(), apiGatewayClient: gw, projectRegistry: reg,
		toolAuditRepo: &stubAuditRepo{}, taskRepo: repo,
		agentWritesMode: "all", agentWriteMetrics: metrics,
		config: cfg, aiDisclosure: aidisclosure.New(aidisclosure.Config{}, nil),
	}
}

func postToProvider(t *testing.T, srv *Server, provider string, body map[string]any) AgentQueryResponse {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"provider": provider,
		"method":   http.MethodPost,
		"path":     "/posts",
		"body":     body,
	})
	require.NoError(t, err)
	req := agentTaskReq(http.MethodPost, "/api/v1/projects/proj/api/query", string(payload), "proj")
	rec := httptest.NewRecorder()
	srv.AgentQueryAPI(rec, req)
	return decodeQueryResp(t, rec)
}

// The finding itself: a post with no disclosure must not leave the daemon.
func TestAgentQueryAPI_G6B_UndisclosedPublicationWriteIsRefused(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	metrics := NewAgentAPIWriteMetrics(prometheus.NewRegistry())
	srv := disclosureTestServer(t, gw, metrics)

	resp := postToProvider(t, srv, disclosureTestProvider, map[string]any{
		"title": "Vornik ships", "content": "We shipped a thing today.", "submolt": "ai",
	})

	assert.NotEmpty(t, resp.Refusal, "an undisclosed publication write must be refused")
	assert.Empty(t, gw.calls, "the write must not reach the gateway")
	// The refusal is the retry instruction: it must carry the exact text to add.
	assert.Contains(t, resp.Refusal, publicationNoticeText(),
		"the refusal must tell the agent the exact notice to include")
	assert.Equal(t, 1.0, testutil.ToFloat64(
		metrics.WritesTotal.WithLabelValues("all", "USER", "refused")))
}

func TestAgentQueryAPI_G6B_DisclosedPublicationWriteReachesTheGateway(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	metrics := NewAgentAPIWriteMetrics(prometheus.NewRegistry())
	srv := disclosureTestServer(t, gw, metrics)

	resp := postToProvider(t, srv, disclosureTestProvider, map[string]any{
		"title":   "Vornik ships",
		"content": "We shipped a thing today.\n\n" + publicationNoticeText(),
		"submolt": "ai",
	})
	assert.Empty(t, resp.Refusal, "a disclosed write must be permitted: %s", resp.Refusal)
	require.Len(t, gw.calls, 1, "a disclosed write must reach the gateway")
	assert.Equal(t, 1.0, testutil.ToFloat64(
		metrics.WritesTotal.WithLabelValues("all", "USER", "permitted")))
}

// Markdown wrapping must not defeat the check — an agent that hard-wraps the
// trailer has still disclosed.
func TestAgentQueryAPI_G6B_WhitespaceNormalisedMatch(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))

	wrapped := strings.Replace(publicationNoticeText(), " ", "\n  ", 3)
	resp := postToProvider(t, srv, disclosureTestProvider, map[string]any{
		"content": "Post body.\n\n" + wrapped,
	})
	assert.Empty(t, resp.Refusal, "re-wrapped notice must still count as disclosed: %s", resp.Refusal)
	assert.Len(t, gw.calls, 1)
}

// The notice must be in a field the READER sees. Smuggling it into an unlisted
// key is exactly what content_fields exists to prevent.
func TestAgentQueryAPI_G6B_NoticeInAnUnlistedFieldIsRefused(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))

	resp := postToProvider(t, srv, disclosureTestProvider, map[string]any{
		"content":           "We shipped a thing today.",
		"internal_metadata": publicationNoticeText(),
	})
	assert.NotEmpty(t, resp.Refusal, "the notice must appear in a configured content field")
	assert.Empty(t, gw.calls)
}

// Review finding #4: "field absent" and "field present but notice missing" must
// be distinguishable, or a misbehaving agent cannot tell what to fix.
func TestAgentQueryAPI_G6B_AbsentAndPresentFailuresAreDistinguishable(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))

	absent := postToProvider(t, srv, disclosureTestProvider, map[string]any{"title": "no content key"})
	present := postToProvider(t, srv, disclosureTestProvider, map[string]any{"content": "no notice here"})

	require.NotEmpty(t, absent.Refusal)
	require.NotEmpty(t, present.Refusal)
	assert.NotEqual(t, absent.Refusal, present.Refusal,
		"absent-field and missing-notice refusals must differ so the agent can tell them apart")
	assert.Empty(t, gw.calls)
}

// A body the gate cannot inspect is a body it cannot vet. Fail closed.
// Malformed JSON never reaches this layer (the outer decoder rejects it), so the
// uninspectable shapes here are absent, empty, and wrong-typed content.
func TestAgentQueryAPI_G6B_UninspectableBodyIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"nil body", nil},
		{"empty body", map[string]any{}},
		{"content field is not a string", map[string]any{"content": 42}},
		{"content field is blank", map[string]any{"content": "   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))
			resp := postToProvider(t, srv, disclosureTestProvider, tc.body)
			assert.NotEmpty(t, resp.Refusal, "an uninspectable publication body must be refused")
			assert.Empty(t, gw.calls, "nothing may reach the gateway")
		})
	}
}

// No regression for every existing provider: without disclosure.required the
// body is never inspected.
func TestAgentQueryAPI_G6B_NonPublicationProviderIsUnaffected(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))

	resp := postToProvider(t, srv, "maps", map[string]any{"content": "no disclosure anywhere"})
	assert.Empty(t, resp.Refusal, "a non-publication provider must not be gated: %s", resp.Refusal)
	assert.Len(t, gw.calls, 1)
}

// Reads are not publication. A GET must never be gated — the disclosure
// obligation attaches to what this system says, not what it fetches.
func TestAgentQueryAPI_G6B_ReadsAreNotGated(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))

	payload, err := json.Marshal(map[string]any{
		"provider": disclosureTestProvider, "method": http.MethodGet, "path": "/feed",
	})
	require.NoError(t, err)
	req := agentTaskReq(http.MethodPost, "/api/v1/projects/proj/api/query", string(payload), "proj")
	rec := httptest.NewRecorder()
	srv.AgentQueryAPI(rec, req)

	resp := decodeQueryResp(t, rec)
	assert.Empty(t, resp.Refusal, "a read must not be gated: %s", resp.Refusal)
	assert.Len(t, gw.calls, 1)
}

// An unwired disclosure service with a provider that requires disclosure means
// the daemon cannot prove it disclosed. Fail closed rather than publish.
func TestAgentQueryAPI_G6B_UnwiredDisclosureFailsClosed(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))
	srv.aiDisclosure = nil

	resp := postToProvider(t, srv, disclosureTestProvider, map[string]any{
		"content": "We shipped a thing today.",
	})
	assert.NotEmpty(t, resp.Refusal, "an unwired disclosure must refuse a publication write")
	assert.Empty(t, gw.calls)
}

// --- path scoping ---
//
// Not every write to a publication surface publishes text. Moltbook's write set
// includes /posts/{id}/upvote and /agents/{name}/follow, which carry no content
// field and say nothing to a reader. Gating them would refuse the entire
// outreach feed for an obligation that does not apply: Art 50(1) attaches to
// text a human reads, not to a vote. So the gate is scoped to the paths that
// actually publish.

func disclosureTestServerWithPaths(t *testing.T, gw *fakeQueryGateway, paths []string) *Server {
	t.Helper()
	srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))
	p := srv.config.Gateway.Providers[disclosureTestProvider]
	p.Disclosure.Paths = paths
	srv.config.Gateway.Providers[disclosureTestProvider] = p
	return srv
}

func postPathToProvider(t *testing.T, srv *Server, path string, body map[string]any) AgentQueryResponse {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"provider": disclosureTestProvider,
		"method":   http.MethodPost,
		"path":     path,
		"body":     body,
	})
	require.NoError(t, err)
	req := agentTaskReq(http.MethodPost, "/api/v1/projects/proj/api/query", string(payload), "proj")
	rec := httptest.NewRecorder()
	srv.AgentQueryAPI(rec, req)
	return decodeQueryResp(t, rec)
}

// A vote or a follow publishes no text — it must not be gated.
func TestAgentQueryAPI_G6B_NonPublishingWritesOnAGatedProviderArePermitted(t *testing.T) {
	for _, path := range []string{"/posts/abc/upvote", "/posts/abc/downvote", "/agents/someone/follow"} {
		t.Run(path, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServerWithPaths(t, gw, []string{"/posts", "/comments", "/posts/*/comments"})
			resp := postPathToProvider(t, srv, path, nil)
			assert.Empty(t, resp.Refusal,
				"a write that publishes no text must not be gated: %s", resp.Refusal)
			assert.Len(t, gw.calls, 1)
		})
	}
}

// The publishing paths stay gated, including the nested comments route.
func TestAgentQueryAPI_G6B_PublishingPathsRemainGated(t *testing.T) {
	for _, path := range []string{"/posts", "/comments", "/posts/abc/comments"} {
		t.Run(path, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServerWithPaths(t, gw, []string{"/posts", "/comments", "/posts/*/comments"})
			resp := postPathToProvider(t, srv, path, map[string]any{"content": "undisclosed text"})
			assert.NotEmpty(t, resp.Refusal, "a publishing path must stay gated")
			assert.Empty(t, gw.calls)
		})
	}
}

// An empty paths list means "gate every write" — the fail-closed default for a
// provider that does nothing but publish.
func TestAgentQueryAPI_G6B_EmptyPathsGatesEveryWrite(t *testing.T) {
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	srv := disclosureTestServerWithPaths(t, gw, nil)
	resp := postPathToProvider(t, srv, "/posts/abc/upvote", nil)
	assert.NotEmpty(t, resp.Refusal,
		"with no paths configured every write is gated (fail-closed default)")
	assert.Empty(t, gw.calls)
}

// --- path-matcher evasion ---
//
// `path` is an LLM-controlled field. A gate keyed on it must not be evadable by
// spelling the same route differently, or the whole control is advisory. These
// pin the normalisations.
func TestAgentQueryAPI_G6B_PathVariantsCannotEvadeTheGate(t *testing.T) {
	for _, path := range []string{
		"/posts",
		"posts",          // no leading slash
		"/posts/",        // trailing slash
		"//posts",        // duplicated separator
		"/POSTS",         // case
		"/posts?foo=bar", // query smuggled into the path field
		"/posts#frag",    // fragment
		" /posts ",       // padding
	} {
		t.Run(path, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServerWithPaths(t, gw, []string{"/posts", "/comments", "/posts/*/comments"})
			resp := postPathToProvider(t, srv, path, map[string]any{"content": "undisclosed text"})
			assert.NotEmpty(t, resp.Refusal,
				"path %q must still be gated — otherwise the spelling of a route defeats the control", path)
			assert.Empty(t, gw.calls, "nothing may reach the gateway for %q", path)
		})
	}
}

// The exemption must not be evadable in the other direction either: a genuinely
// non-publishing route stays ungated regardless of spelling.
func TestAgentQueryAPI_G6B_NonPublishingPathVariantsStayExempt(t *testing.T) {
	for _, path := range []string{"/posts/abc/upvote", "posts/abc/upvote/", "/POSTS/abc/UPVOTE"} {
		t.Run(path, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServerWithPaths(t, gw, []string{"/posts", "/comments", "/posts/*/comments"})
			resp := postPathToProvider(t, srv, path, nil)
			assert.Empty(t, resp.Refusal, "%q publishes nothing and must stay ungated: %s", path, resp.Refusal)
			assert.Len(t, gw.calls, 1)
		})
	}
}

// A path that normalises to nothing is not a route the operator chose to exempt
// — it is degenerate input. Gate it, so the exemption list can only ever exempt
// routes someone actually named.
func TestAgentQueryAPI_G6B_EmptyPathFailsClosed(t *testing.T) {
	for _, path := range []string{"", "/", "///", "   ", "?x=1"} {
		t.Run("path="+path, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServerWithPaths(t, gw, []string{"/posts", "/comments", "/posts/*/comments"})
			resp := postPathToProvider(t, srv, path, map[string]any{"content": "undisclosed"})
			assert.NotEmpty(t, resp.Refusal,
				"a path that normalises to nothing must fail closed, not fall through the exemption")
			assert.Empty(t, gw.calls)
		})
	}
}

// Percent-encoding in an LLM-supplied path is not a route any operator exempted.
// "/posts%2Fcomments" decodes to a different route than it segments as, so the
// gate cannot reason about it — it fails closed rather than guess.
func TestAgentQueryAPI_G6B_PercentEncodedPathFailsClosed(t *testing.T) {
	for _, path := range []string{
		"/posts%2Fcomments", // encoded separator
		"/%70osts",          // encoded letter spelling a gated route
		"/posts%3Fx=1",      // encoded query marker
		"/posts/abc/upvote%00",
	} {
		t.Run(path, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServerWithPaths(t, gw, []string{"/posts", "/comments", "/posts/*/comments"})
			resp := postPathToProvider(t, srv, path, map[string]any{"content": "undisclosed"})
			assert.NotEmpty(t, resp.Refusal,
				"a percent-encoded path must fail closed: %q", path)
			assert.Empty(t, gw.calls)
		})
	}
}

// The "*" wildcard matches exactly one segment — pinned explicitly, since the
// exemption of /posts/{id}/upvote depends on it not behaving like a prefix.
func TestAgentQueryAPI_G6B_WildcardMatchesExactlyOneSegment(t *testing.T) {
	for _, tc := range []struct {
		path      string
		wantGated bool
	}{
		{"/posts/123/comments", true}, // * matches the id
		{"/posts/any-slug-here/comments", true},
		{"/posts/comments", false},     // too few segments for the 3-seg pattern
		{"/posts/1/2/comments", false}, // too many
		{"/posts/123/upvote", false},   // sibling action stays exempt
	} {
		t.Run(tc.path, func(t *testing.T) {
			gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
			srv := disclosureTestServerWithPaths(t, gw, []string{"/posts/*/comments"})
			resp := postPathToProvider(t, srv, tc.path, map[string]any{"content": "undisclosed"})
			if tc.wantGated {
				assert.NotEmpty(t, resp.Refusal, "%q should be gated by /posts/*/comments", tc.path)
				assert.Empty(t, gw.calls)
			} else {
				assert.Empty(t, resp.Refusal, "%q should not be gated: %s", tc.path, resp.Refusal)
				assert.Len(t, gw.calls, 1)
			}
		})
	}
}

// Review finding #2: with more than one content_field configured, disclosing in
// one and staying silent in the other must refuse. A reader who skips the title
// must still be told.
func TestAgentQueryAPI_G6B_MultiFieldPartialDisclosureIsRefused(t *testing.T) {
	newSrv := func(gw *fakeQueryGateway) *Server {
		srv := disclosureTestServer(t, gw, NewAgentAPIWriteMetrics(prometheus.NewRegistry()))
		p := srv.config.Gateway.Providers[disclosureTestProvider]
		p.Disclosure.ContentFields = []string{"title", "body"}
		srv.config.Gateway.Providers[disclosureTestProvider] = p
		return srv
	}

	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	resp := postToProvider(t, newSrv(gw), disclosureTestProvider, map[string]any{
		"title": "Heads up — " + publicationNoticeText(),
		"body":  "The substance, with no disclosure at all.",
	})
	assert.NotEmpty(t, resp.Refusal, "partial disclosure across content_fields must refuse")
	assert.Contains(t, resp.Refusal, "body", "the refusal should name the offending field")
	assert.Empty(t, gw.calls)

	// Both disclosed → permitted.
	gw2 := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	resp2 := postToProvider(t, newSrv(gw2), disclosureTestProvider, map[string]any{
		"title": "Heads up — " + publicationNoticeText(),
		"body":  "The substance. " + publicationNoticeText(),
	})
	assert.Empty(t, resp2.Refusal, "both fields disclosed must be permitted: %s", resp2.Refusal)
	assert.Len(t, gw2.calls, 1)
}
