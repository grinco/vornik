package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestServer_CallMCPTool_RejectsOversizedBody is a regression test for the
// unbounded-heap-allocation finding: CallMCPTool used to read the request
// body with no size cap, letting an agent/unauthenticated caller force an
// arbitrarily large allocation. The body is now wrapped in
// http.MaxBytesReader(maxMCPArgsBytes), so a body over the cap fails to
// decode and the existing 400 branch fires.
func TestServer_CallMCPTool_RejectsOversizedBody(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "ok"}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f))

	// Construct a syntactically valid JSON body whose size exceeds the
	// cap. The oversized read is what must be rejected, not the JSON
	// shape, so the payload itself is well-formed.
	args := strings.Repeat("x", maxMCPArgsBytes)
	body := `{"name":"mcp__gmail__search_emails","arguments":{"q":"` + args + `"}}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/alice/mcp/tools/call",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"oversized MCP tool body must be rejected with 400")
	assert.Empty(t, f.lastTool,
		"executor must not be invoked when the body exceeds the size cap")
}

// TestServer_CallMCPTool_AcceptsBodyUnderCap guards the lower boundary: a
// payload comfortably under maxMCPArgsBytes still decodes and reaches the
// executor, so the cap doesn't regress legitimate (possibly large) tool
// arguments.
func TestServer_CallMCPTool_AcceptsBodyUnderCap(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "ok"}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f))

	args := strings.Repeat("x", maxMCPArgsBytes/2)
	body := `{"name":"mcp__gmail__search_emails","arguments":{"q":"` + args + `"}}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/alice/mcp/tools/call",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code,
		"a body under the cap must still be accepted")
	assert.Equal(t, "mcp__gmail__search_emails", f.lastTool)
}

func TestServer_CallMCPTool_RejectsTaskHeaderMismatch(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "must not run"}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f))
	row := &persistence.APIKey{Name: "agent:task_task-bound"}
	id := &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: row}}
	ctx := context.WithValue(context.Background(), identityKey, id)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/mcp/tools/call",
		strings.NewReader(`{"name":"mcp__broker__place_order"}`)).WithContext(ctx)
	req.Header.Set("X-Task-ID", "task-spoofed")
	rec := httptest.NewRecorder()

	server.CallMCPTool(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, f.lastTool)
}

func TestServer_CallMCPTool_GateFailureBlocksDispatch(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "must not run"}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, errors.New("database unavailable")
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f), WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/mcp/tools/call",
		strings.NewReader(`{"name":"mcp__broker__place_order"}`))
	req.Header.Set("X-Task-ID", "task-replay")
	rec := httptest.NewRecorder()

	server.CallMCPTool(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, f.lastTool)
}
