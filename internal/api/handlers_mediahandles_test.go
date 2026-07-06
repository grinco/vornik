package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/mediahandles"
)

const mhEncodeTool = "mcp__scraper__encode_image"
const mhPublishTool = "mcp__pagedrop__pagedrop_publish_page"

func mhServer(f *fakeMCPExecutor) *Server {
	store := mediahandles.New(mediahandles.Options{
		Sources: []string{mhEncodeTool},
		Sinks:   []mediahandles.Sink{{Tool: mhPublishTool, HTMLArg: "html", ImagesArg: "images"}},
	})
	return NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f), WithMediaHandles(store))
}

func mhCall(t *testing.T, server *Server, taskID, name, argsJSON string) string {
	t.Helper()
	body := `{"name":"` + name + `","arguments":` + argsJSON + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/assistant/mcp/tools/call", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Task-ID", taskID)
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Text
}

// End-to-end through the handler: encode_image's data URI is stashed and the
// agent receives only a handle; a later publish call has the payload injected
// into its images arg — the base64 never appears in either agent-facing text.
func TestCallMCPTool_MediaHandles_Roundtrip(t *testing.T) {
	f := &fakeMCPExecutor{}
	server := mhServer(f)

	// 1. encode_image returns a data URI; the handler stashes it.
	f.executeRet = `{"data_uri":"data:image/jpeg;base64,ZZZZZZZZ","content_type":"image/jpeg","bytes":120,"width":800,"height":600}`
	out := mhCall(t, server, "task-1", mhEncodeTool, `{"url":"https://example.com/a.jpg"}`)

	assert.NotContains(t, out, "ZZZZZZZZ", "base64 must not reach the agent")
	var hr struct {
		MediaHandle string `json:"media_handle"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &hr))
	require.NotEmpty(t, hr.MediaHandle)

	// 2. publish referencing the handle: the payload is injected into images.
	f.executeRet = `Published page: ok`
	html := `<h1>x</h1><img src=\"cid:` + hr.MediaHandle + `\">`
	_ = mhCall(t, server, "task-1", mhPublishTool, `{"title":"P","html":"`+html+`"}`)

	assert.Contains(t, f.lastArgsJSON, `"images"`, "images arg must be injected")
	assert.Contains(t, f.lastArgsJSON, "data:image/jpeg;base64,ZZZZZZZZ", "the stashed data URI must reach the sink")
	assert.Contains(t, f.lastArgsJSON, hr.MediaHandle, "image id must be the handle")
}

// A publish referencing a handle that was never stashed fails fast as a
// tool-result error the agent can read (not a transport error), and the
// executor is not invoked.
func TestCallMCPTool_MediaHandles_DanglingRef(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "must not run"}
	server := mhServer(f)

	out := mhCall(t, server, "task-1", mhPublishTool, `{"title":"P","html":"<img src=\"cid:deadbeef\">"}`)
	assert.Contains(t, out, "deadbeef")
	assert.Contains(t, out, "MCP error")
	assert.Empty(t, f.lastTool, "executor must not run when a media handle is missing")
}
