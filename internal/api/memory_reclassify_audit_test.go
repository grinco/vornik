package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMemoryReclassifyLLM_DoesNotLeakRawError guards the fix that the
// reclassify-llm handler returns a generic client message instead of the
// raw err.Error() (which could echo upstream/DB internals to a non-admin,
// project-scoped caller). The detail still goes to the log, not the wire.
func TestMemoryReclassifyLLM_DoesNotLeakRawError(t *testing.T) {
	const secretDetail = "pq: password authentication failed for user \"vornik\" at 10.88.0.7"
	s := newReclassifyServer(&stubClassifyBackfiller{batchErr: errors.New(secretDetail)})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=p", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secretDetail) {
		t.Errorf("raw upstream error leaked to client: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reclassify failed") {
		t.Errorf("expected generic message, got: %s", rec.Body.String())
	}
}
