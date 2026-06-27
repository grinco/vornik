package projectwizard

import (
	"context"
	"strings"
	"testing"
)

// fakeTemplateSource is an in-memory TemplateSource for the
// template-anchored wizard tests. It records the params it was
// asked to materialise so tests can assert proposal→params
// derivation.
type fakeTemplateSource struct {
	spec       TemplateSpec
	exists     bool
	rendered   map[string]string
	matErr     error
	lastParams map[string]string
}

func (f *fakeTemplateSource) Lookup(slug string) (TemplateSpec, bool) {
	if !f.exists {
		return TemplateSpec{}, false
	}
	return f.spec, true
}

func (f *fakeTemplateSource) Materialise(slug string, params map[string]string) (map[string]string, error) {
	f.lastParams = params
	if f.matErr != nil {
		return nil, f.matErr
	}
	return f.rendered, nil
}

// newsFeedSpec mirrors the real news-feed template's parameter
// declaration (the subset the wizard derives against).
func newsFeedSpec() TemplateSpec {
	return TemplateSpec{
		Slug: "news-feed",
		Params: []TemplateParamSpec{
			{Name: "projectId", Type: "string"},
			{Name: "displayName", Type: "string"},
			{Name: "topic", Type: "string"},
			{Name: "interval", Type: "enum", Options: []string{"1h", "4h", "12h", "24h"}},
		},
	}
}

// validRenderedProject is the file set a real template materialise
// would produce — a registry-valid project.yaml plus a swarm file.
func validRenderedProject() map[string]string {
	return map[string]string{
		"projects/acme.yaml":   "projectId: \"acme\"\ndisplayName: \"Acme\"\nswarmId: \"acme-swarm\"\ndefaultWorkflowId: \"adaptive\"\n",
		"swarms/acme-swarm.md": "---\nswarmId: \"acme-swarm\"\nleadRole: lead\n---\n# swarm\n",
	}
}

func TestDeriveTemplateParams(t *testing.T) {
	spec := newsFeedSpec()
	raw := map[string]any{
		"projectId":   "acme",
		"displayName": "Acme Tracker",
		"topic":       "acme pricing",
		"interval":    "6h",       // not in options → must be dropped
		"monitors":    []any{"x"}, // non-scalar, not declared → ignored
		"extraneous":  "ignored",  // not declared → ignored
	}
	got := deriveTemplateParams(spec, raw)

	if got["projectId"] != "acme" || got["displayName"] != "Acme Tracker" || got["topic"] != "acme pricing" {
		t.Errorf("scalar params not carried through: %#v", got)
	}
	if _, ok := got["interval"]; ok {
		t.Errorf("out-of-range enum value should be dropped so the template default wins, got %q", got["interval"])
	}
	if _, ok := got["monitors"]; ok {
		t.Error("non-declared / non-scalar key leaked into params")
	}
	if _, ok := got["extraneous"]; ok {
		t.Error("non-declared key leaked into params")
	}
}

func TestDeriveTemplateParams_ValidEnumKept(t *testing.T) {
	got := deriveTemplateParams(newsFeedSpec(), map[string]any{
		"projectId": "acme",
		"interval":  "12h", // valid option
	})
	if got["interval"] != "12h" {
		t.Errorf("valid enum value should be kept, got %q", got["interval"])
	}
}

func TestCoerceScalar(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"  hi  ", "hi"},
		{true, "true"},
		{float64(6), "6"},     // JSON whole number
		{float64(1.5), "1.5"}, // JSON fractional
		{42, "42"},
		{int64(7), "7"},
		{[]any{1, 2}, ""},      // non-scalar
		{map[string]any{}, ""}, // non-scalar
		{nil, ""},
	}
	for _, c := range cases {
		if got := coerceScalar(c.in); got != c.want {
			t.Errorf("coerceScalar(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateRenderedProject(t *testing.T) {
	if err := validateRenderedProject(validRenderedProject()); err != nil {
		t.Errorf("valid rendered project should pass, got %v", err)
	}

	// Missing swarmId → registry validator rejects.
	bad := map[string]string{
		"projects/acme.yaml": "projectId: \"acme\"\ndefaultWorkflowId: \"adaptive\"\n",
	}
	if err := validateRenderedProject(bad); err == nil || !strings.Contains(err.Error(), "swarmId") {
		t.Errorf("expected swarmId validation error, got %v", err)
	}

	// No project file in the set at all.
	if err := validateRenderedProject(map[string]string{"swarms/x.md": "..."}); err == nil {
		t.Error("expected error when no project YAML present")
	}

	// Unparseable YAML.
	if err := validateRenderedProject(map[string]string{"projects/x.yaml": "::: not yaml :::"}); err == nil {
		t.Error("expected parse error on malformed project YAML")
	}
}

func TestIsProjectTarget(t *testing.T) {
	if !isProjectTarget("projects/acme.yaml") {
		t.Error("projects/acme.yaml should be a project target")
	}
	if isProjectTarget("swarms/acme-swarm.md") {
		t.Error("swarm file is not a project target")
	}
	if isProjectTarget("project-templates/x.yaml") {
		t.Error("project-templates/ must not be mistaken for projects/")
	}
}

// --- Converse + Commit integration with the template anchor ---

func TestConverse_TemplateAnchored_FlipsReadyToCommit(t *testing.T) {
	// LLM proposes a draft that carries only projectId/displayName/topic
	// and names a template — the raw proposal has no swarmId, which used
	// to fail registry validation every turn. With the template anchor
	// it validates against the rendered template instead.
	const ready = `{"message":"Ready?","ready_to_commit":true,"suggested_template":"news-feed","proposal":{"raw":{"projectId":"acme","displayName":"Acme","topic":"acme pricing"}}}`
	w, _, _ := newWizardForTest(chatReply{content: ready})
	w.Templates = &fakeTemplateSource{exists: true, spec: newsFeedSpec(), rendered: validRenderedProject()}

	res, err := w.Converse(context.Background(), "", "op_1", "track acme pricing")
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if !res.Envelope.ReadyToCommit {
		t.Fatalf("expected ready_to_commit to stay true with a valid template anchor, message=%q", res.Envelope.Message)
	}
	if strings.Contains(res.Envelope.Message, "validation:") {
		t.Errorf("unexpected validation error appended: %q", res.Envelope.Message)
	}
}

func TestConverse_TemplateAnchored_BadProjectIDSurfacesError(t *testing.T) {
	// Template materialise fails (e.g. projectId fails the template's
	// pattern) → ready_to_commit is reset and the reason is surfaced.
	const ready = `{"message":"Ready?","ready_to_commit":true,"suggested_template":"news-feed","proposal":{"raw":{"projectId":"Acme","displayName":"Acme","topic":"x"}}}`
	w, _, _ := newWizardForTest(chatReply{content: ready})
	w.Templates = &fakeTemplateSource{exists: true, spec: newsFeedSpec(), matErr: errTestPattern}

	res, err := w.Converse(context.Background(), "", "op_1", "track acme")
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("expected ready_to_commit reset on materialise failure")
	}
	if !strings.Contains(res.Envelope.Message, "validation:") {
		t.Errorf("expected validation reason in message, got %q", res.Envelope.Message)
	}
}

var errTestPattern = &testErr{"parameter \"projectId\": must match pattern"}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }

// multiFileCapturingWriter records WriteFiles calls and also
// satisfies the single-file ProjectWriter so it can be assigned to
// Wizard.Writer.
type multiFileCapturingWriter struct {
	singleCalls int
	multiFiles  map[string]string
	projectID   string
	err         error
}

func (m *multiFileCapturingWriter) Write(_ context.Context, projectID string, _ []byte) (string, error) {
	m.singleCalls++
	return "/ui/projects/" + projectID, m.err
}

func (m *multiFileCapturingWriter) WriteFiles(_ context.Context, projectID string, files map[string]string) (string, error) {
	m.projectID = projectID
	m.multiFiles = files
	if m.err != nil {
		return "", m.err
	}
	return "/ui/projects/" + projectID, nil
}

func TestCommit_TemplateAnchored_WritesRenderedFileSet(t *testing.T) {
	w, store, _ := newWizardForTest()
	writer := &multiFileCapturingWriter{}
	w.Writer = writer
	w.Templates = &fakeTemplateSource{exists: true, spec: newsFeedSpec(), rendered: validRenderedProject()}

	sessionID := pinReadySession(t, store, "op_1", map[string]any{
		"projectId":   "acme",
		"displayName": "Acme",
		"topic":       "acme pricing",
	})
	// Stamp the sticky template slug the way Converse would.
	sess, _ := store.Get(context.Background(), sessionID)
	sess.SuggestedTemplate = "news-feed"
	_ = store.Update(context.Background(), sess)

	res, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.ProjectID != "acme" {
		t.Errorf("projectID = %q, want acme", res.ProjectID)
	}
	if writer.singleCalls != 0 {
		t.Error("template-anchored commit must use the multi-file writer, not single-file Write")
	}
	if len(writer.multiFiles) != 2 {
		t.Fatalf("expected 2 rendered files written, got %d: %#v", len(writer.multiFiles), writer.multiFiles)
	}
	if _, ok := writer.multiFiles["swarms/acme-swarm.md"]; !ok {
		t.Error("swarm file was not part of the committed file set — the swarmId gap is not closed")
	}
}

func TestValidateProposal_TemplateAnchorGuards(t *testing.T) {
	w := &Wizard{Templates: &fakeTemplateSource{exists: true, spec: newsFeedSpec(), rendered: validRenderedProject()}}

	// Empty proposal under a resolvable template.
	if err := w.validateProposal(&ProjectYAML{Raw: map[string]any{}}, "news-feed"); err == nil {
		t.Error("empty proposal should fail")
	}
	// Missing projectId under a resolvable template.
	if err := w.validateProposal(&ProjectYAML{Raw: map[string]any{"displayName": "x"}}, "news-feed"); err == nil {
		t.Error("missing projectId should fail")
	}
	// Unknown template slug → falls back to w.Validator (nil → permissive).
	if err := w.validateProposal(&ProjectYAML{Raw: map[string]any{"projectId": "acme"}}, "does-not-exist"); err != nil {
		t.Errorf("unknown slug should fall back to permissive validator, got %v", err)
	}
}

func TestWriteProject_TemplateButSingleFileWriterFallsBack(t *testing.T) {
	// Template resolves, but the writer implements only the single-file
	// ProjectWriter — commit must fall back to RenderYAML + Write rather
	// than panic or skip the write.
	w, store, _ := newWizardForTest()
	writer := &capturingWriter{}
	w.Writer = writer
	w.Templates = &fakeTemplateSource{exists: true, spec: newsFeedSpec(), rendered: validRenderedProject()}

	// Proposal must itself validate (template anchor needs a rendered
	// project to pass; here it does via the fake's validRenderedProject).
	sessionID := pinReadySession(t, store, "op_1", map[string]any{"projectId": "acme", "displayName": "Acme", "topic": "x"})
	sess, _ := store.Get(context.Background(), sessionID)
	sess.SuggestedTemplate = "news-feed"
	_ = store.Update(context.Background(), sess)

	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(writer.calls) != 1 {
		t.Errorf("expected single-file Write fallback, got %d calls", len(writer.calls))
	}
}

func TestCommit_NoTemplate_FallsBackToSingleFile(t *testing.T) {
	// Without a suggested_template the from-scratch path writes the
	// proposal's own YAML via single-file Write (proposal must carry
	// swarmId itself — minimalValidProposal does).
	w, store, _ := newWizardForTest()
	writer := &multiFileCapturingWriter{}
	w.Writer = writer
	w.Validator = RegistryValidator{}
	w.Templates = &fakeTemplateSource{exists: false}

	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if writer.singleCalls != 1 {
		t.Errorf("expected single-file Write fallback, singleCalls=%d", writer.singleCalls)
	}
	if writer.multiFiles != nil {
		t.Error("multi-file writer should not be used without a template")
	}
}
