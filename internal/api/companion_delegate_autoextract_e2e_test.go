package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/textfile"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// wireAutoExtract gives srv the minimum extraction surface tryAutoExtract
// needs to produce a non-nil extraction summary.
//
// It leans on tryAutoExtract's cache-hit branch: when extractedDocsRepo
// already holds a row for the artifact whose (ExtractorName, ExtractorVersion,
// Status) match the resolved extractor, the summary is built from that row and
// neither the runner nor the artifact bytes are touched. That keeps the test
// free of artifact-store materialization while still exercising the real
// registry lookup (MIME resolution via extractor.MimeFromFilename → the
// registered textfile extractor).
//
// artifactID must match what the input-artifact store returns for the upload:
// fakeInputArtifactStore mints "art-" + name.
func wireAutoExtract(t *testing.T, srv *Server, artifactID string) {
	t.Helper()

	reg := extractor.NewRegistry()
	require.NoError(t, reg.Register(textfile.New(), "text/plain", "text/markdown", "text/x-markdown"))

	srv.extractorRegistry = reg
	srv.extractorRunner = &extractor.Runner{Repo: &stubDocRepo{}, BasePath: t.TempDir()}
	srv.extractedDocsRepo = &stubDocRepo{
		docs: map[string]*persistence.ExtractedDocument{
			artifactID: {
				ID:               "extdoc-cached",
				ExtractorName:    textfile.Name,
				ExtractorVersion: textfile.Version,
				Status:           persistence.ExtractedDocumentStatusOK,
				SectionCount:     3,
			},
		},
	}
}

// delegateWithArtifact fires a companion delegate() carrying one markdown
// upload and NO skip_auto_extract, then returns the created task's
// context object.
func delegateWithArtifact(t *testing.T, srv *Server, taskRepo *mocks.MockTaskRepository, rawKey, workflow string) map[string]any {
	t.Helper()

	req := mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": workflow,
			"prompt":   "review the attached design doc",
			// Deliberately NOT setting skip_auto_extract — this is the
			// exact shape that produced T-8f69.
			"inputArtifacts": []map[string]any{
				{"name": "design.md", "content": "aGVsbG8="}, // "hello"
			},
		},
	})
	req = withCompanionBearer(req, rawKey)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "delegate must succeed; got: %s", text)

	created := taskRepo.LastCall.Task
	require.NotNil(t, created, "delegate must have created a task")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(created.Payload, &payload))
	taskCtx, _ := payload["context"].(map[string]any)
	require.NotNil(t, taskCtx, "task payload must carry a context object")
	return taskCtx
}

// TestCompanionDelegate_AutoExtractSuppressedForArtifactWorkflows is the
// end-to-end pin for T-8f69 (task_20260725222814_dff3a694415c8f69).
//
// The control case matters as much as the assertion: "wf-alpha" proves the
// extraction path is genuinely live in this test (inputExtractions IS
// produced), so the absence of inputExtractions on "wf-artifacts" is evidence
// that the derivation suppressed it — not that extraction was never wired.
//
// Without the derivation, the artifact-ingesting workflow gets
// inputExtractions, executor.extractTaskInputArtifacts then skips staging the
// raw file, and a role whose allowedTools lacks the document_* MCP tools (the
// companion reviewer) has no readable copy of the artifact at all.
func TestCompanionDelegate_AutoExtractSuppressedForArtifactWorkflows(t *testing.T) {
	t.Run("control: ordinary workflow still auto-extracts", func(t *testing.T) {
		srv, keyRepo, taskRepo := newCompanionMCPServer(t)
		srv.inputArtifactStore = &fakeInputArtifactStore{}
		wireAutoExtract(t, srv, "art-design.md")
		rawKey, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

		taskCtx := delegateWithArtifact(t, srv, taskRepo, rawKey, "wf-alpha")

		require.NotEmpty(t, taskCtx["inputExtractions"],
			"control case is inert: extraction never ran, so this test could not "+
				"distinguish a forced skip from unwired extraction")
	})

	t.Run("artifact-ingesting workflow suppresses auto-extract", func(t *testing.T) {
		srv, keyRepo, taskRepo := newCompanionMCPServer(t)
		srv.inputArtifactStore = &fakeInputArtifactStore{}
		wireAutoExtract(t, srv, "art-design.md")
		rawKey, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-artifacts"})

		taskCtx := delegateWithArtifact(t, srv, taskRepo, rawKey, "wf-artifacts")

		_, hasExtractions := taskCtx["inputExtractions"]
		require.False(t, hasExtractions,
			"require_input_artifacts must force skip_auto_extract: extraction stamps "+
				"inputExtractions, which makes the executor skip staging the raw file — "+
				"and the reviewer role has no document_* tools to fall back to (T-8f69)")
		require.NotEmpty(t, taskCtx["inputFiles"],
			"the raw file path must still reach context so the executor can stage it")
	})
}

// TestCreateTaskREST_AutoExtractSuppressedForArtifactWorkflows closes the other
// door onto the same trap. The companion delegate path is not the only way an
// inputArtifact reaches an artifact-ingesting workflow: POST
// /projects/{id}/tasks accepts inline inputArtifacts too, and it called
// processInputArtifacts with no options — so it ALWAYS auto-extracted,
// reproducing T-8f69 for any REST caller regardless of what the plugins do.
//
// Same derivation, same control-case discipline as the delegate test above.
func TestCreateTaskREST_AutoExtractSuppressedForArtifactWorkflows(t *testing.T) {
	createViaREST := func(t *testing.T, workflowID string) map[string]any {
		t.Helper()
		srv, _, taskRepo := newCompanionMCPServer(t)
		srv.inputArtifactStore = &fakeInputArtifactStore{}
		wireAutoExtract(t, srv, "art-design.md")

		body := `{"taskType":"review","workflowId":"` + workflowID +
			`","inputArtifacts":[{"name":"design.md","content":"aGVsbG8="}]}`
		req := httptest.NewRequest(http.MethodPost, "/projects/alpha/tasks",
			bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.CreateTask(rec, req)
		require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())

		created := taskRepo.LastCall.Task
		require.NotNil(t, created)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(created.Payload, &payload))
		taskCtx, _ := payload["context"].(map[string]any)
		require.NotNil(t, taskCtx)
		return taskCtx
	}

	t.Run("control: ordinary workflow still auto-extracts", func(t *testing.T) {
		require.NotEmpty(t, createViaREST(t, "wf-alpha")["inputExtractions"],
			"control case is inert: extraction never ran via the REST path")
	})

	t.Run("artifact-ingesting workflow suppresses auto-extract", func(t *testing.T) {
		_, has := createViaREST(t, "wf-artifacts")["inputExtractions"]
		require.False(t, has,
			"the REST create-task path must derive skip_auto_extract the same way the "+
				"companion delegate does, or T-8f69 is only half-fixed (T-8f69)")
	})
}
