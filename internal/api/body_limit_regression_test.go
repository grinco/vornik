package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLimitJSONBody_CapsOversizePayload is the regression for the
// 2026-06-04 bug sweep: several mutating handlers decoded
// json.NewDecoder(r.Body).Decode directly with no MaxBytesReader, so an
// authenticated caller could force the daemon to buffer an arbitrarily
// large JSON value (memory-pressure DoS). limitJSONBody now caps the
// body; an oversize payload fails to decode instead of being buffered
// whole.
func TestLimitJSONBody_CapsOversizePayload(t *testing.T) {
	// A single JSON string value well over the cap.
	huge := fmt.Sprintf(`{"reason":%q}`, strings.Repeat("A", defaultJSONBodyLimit*2))
	r := httptest.NewRequest("POST", "/x", strings.NewReader(huge))
	w := httptest.NewRecorder()

	limitJSONBody(w, r)

	var dst struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&dst); err == nil {
		t.Fatal("decode of an oversize body should fail once limitJSONBody caps it")
	}
}

// TestLimitJSONBody_AllowsNormalPayload — a payload under the cap still
// decodes normally, so the bound doesn't break legitimate requests.
func TestLimitJSONBody_AllowsNormalPayload(t *testing.T) {
	body := `{"reason":"a normal reason"}`
	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	w := httptest.NewRecorder()

	limitJSONBody(w, r)

	var dst struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&dst); err != nil {
		t.Fatalf("decode of a normal body should succeed: %v", err)
	}
	if dst.Reason != "a normal reason" {
		t.Fatalf("Reason = %q, want %q", dst.Reason, "a normal reason")
	}
}
