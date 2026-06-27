package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/persistence"
)

// stubOperatorProfiles implements persistence.OperatorProfileRepository
// for the UI tests. Records lookups + returns programmable rows.
type stubOperatorProfiles struct {
	rows    []*persistence.OperatorProfile
	getRow  *persistence.OperatorProfile
	listErr error
	getErr  error
}

func withAdminUI(req *http.Request) *http.Request {
	return req.WithContext(admin.ContextWithAdmin(req.Context(), "api_key_sha256:test"))
}

func (s *stubOperatorProfiles) Get(_ context.Context, id string) (*persistence.OperatorProfile, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getRow != nil {
		return s.getRow, nil
	}
	for _, r := range s.rows {
		if r.OperatorID == id {
			return r, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func (s *stubOperatorProfiles) Upsert(_ context.Context, _ *persistence.OperatorProfile) error {
	return nil
}
func (s *stubOperatorProfiles) Delete(_ context.Context, _ string) error { return nil }
func (s *stubOperatorProfiles) List(_ context.Context, _ int) ([]*persistence.OperatorProfile, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.rows, nil
}

// TestMemoryOperators_UnwiredRendersHint: SQLite or pre-
// migration-60 deployments leave the repo nil; the page should
// render an explainer hint instead of a stack trace.
func TestMemoryOperators_UnwiredRendersHint(t *testing.T) {
	s := NewServer(WithLogger(quietLogger()))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperators(rec, req)
	assert.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "not wired", "should render explainer")
}

func TestMemoryOperators_RequiresAdmin(t *testing.T) {
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(&stubOperatorProfiles{}))
	req := httptest.NewRequest("GET", "/ui/memory/operators", nil)
	rec := httptest.NewRecorder()
	s.MemoryOperators(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestMemoryOperators_EmptyTableShowsEmpty: repo wired but no
// rows — operator hasn't built up any profiles yet.
func TestMemoryOperators_EmptyTableShowsEmpty(t *testing.T) {
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(&stubOperatorProfiles{}))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperators(rec, req)
	assert.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "No operator profiles yet")
}

// TestMemoryOperators_PopulatedRendersRows: rows come back with
// operator_id, parsed structured keys, notes preview, and the
// updated-at timestamp.
func TestMemoryOperators_PopulatedRendersRows(t *testing.T) {
	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	src := &stubOperatorProfiles{rows: []*persistence.OperatorProfile{
		{OperatorID: "telegram:42", Structured: []byte(`{"tone":"terse","verbosity":"low"}`), Notes: "prefers code blocks", UpdatedAt: now},
		{OperatorID: "webchat:abc", Structured: []byte(`{}`), Notes: "", UpdatedAt: now.Add(-1 * time.Hour)},
	}}
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(src))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperators(rec, req)
	assert.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "telegram:42")
	assert.Contains(t, body, "webchat:abc")
	assert.Contains(t, body, "prefers code blocks")
	// Structured keys render in some form so the operator can
	// see them without drilling into the detail page.
	assert.Contains(t, body, "tone")
}

// TestMemoryOperators_ErrorRenders: a repo error surfaces the
// message inline instead of taking the whole page down.
func TestMemoryOperators_ErrorRenders(t *testing.T) {
	src := &stubOperatorProfiles{listErr: assertErrInternalListErr}
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(src))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperators(rec, req)
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), assertErrInternalListErr.Error())
}

// TestMemoryOperator_RendersDetail: the per-operator detail
// page shows full structured + notes for one id.
func TestMemoryOperator_RendersDetail(t *testing.T) {
	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	src := &stubOperatorProfiles{getRow: &persistence.OperatorProfile{
		OperatorID: "telegram:42",
		Structured: []byte(`{"tone":"terse","time_zone":"Europe/Prague"}`),
		Notes:      "operator prefers code blocks for shell snippets",
		UpdatedAt:  now,
		CreatedAt:  now.Add(-24 * time.Hour),
	}}
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(src))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators/telegram:42", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperator(rec, req, "telegram:42")
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "telegram:42")
	assert.Contains(t, body, "Europe/Prague")
	assert.Contains(t, body, "operator prefers code blocks for shell snippets")
}

// TestMemoryOperator_NotFound: unknown id returns 404.
func TestMemoryOperator_NotFound(t *testing.T) {
	src := &stubOperatorProfiles{}
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(src))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators/telegram:nobody", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperator(rec, req, "telegram:nobody")
	assert.Equal(t, 404, rec.Code)
}

// TestMemoryRouter_RoutesOperators: ensures the existing
// memoryRouter dispatches /memory/operators to MemoryOperators
// and /memory/operators/<id> to MemoryOperator.
func TestMemoryRouter_RoutesOperators(t *testing.T) {
	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	src := &stubOperatorProfiles{rows: []*persistence.OperatorProfile{
		{OperatorID: "telegram:42", Structured: []byte(`{}`), UpdatedAt: now},
	}}
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(src))

	// /operators
	{
		req := withAdminUI(httptest.NewRequest("GET", "/memory/operators", nil))
		rec := httptest.NewRecorder()
		s.memoryRouter(rec, req)
		assert.Equal(t, 200, rec.Code)
		assert.True(t, strings.Contains(rec.Body.String(), "telegram:42"))
	}
	// /operators/<id>
	{
		src.getRow = src.rows[0]
		req := withAdminUI(httptest.NewRequest("GET", "/memory/operators/telegram:42", nil))
		rec := httptest.NewRecorder()
		s.memoryRouter(rec, req)
		assert.Equal(t, 200, rec.Code)
	}
}

var assertErrInternalListErr = simpleErr("connection refused")

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

// TestMemoryOperator_GetErrorRendersBanner — non-ErrNotFound
// failure on Get renders the error banner instead of 404.
// Lets operators see "DB is down" rather than wondering if
// the profile was deleted.
func TestMemoryOperator_GetErrorRendersBanner(t *testing.T) {
	src := &stubOperatorProfiles{getErr: assertErrInternalListErr}
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(src))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators/x", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperator(rec, req, "x")
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), assertErrInternalListErr.Error())
}

// TestMemoryOperator_UnwiredReturns404: with no source wired,
// the detail endpoint 404s (consistent with the list page's
// "not wired" hint — at the URL level, the operator either
// exists or doesn't).
func TestMemoryOperator_UnwiredReturns404(t *testing.T) {
	s := NewServer(WithLogger(quietLogger()))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators/x", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperator(rec, req, "x")
	assert.Equal(t, 404, rec.Code)
}

// TestMemoryOperator_EmptyIDReturns404 — defensive guard.
func TestMemoryOperator_EmptyIDReturns404(t *testing.T) {
	s := NewServer(WithLogger(quietLogger()), WithOperatorProfileSource(&stubOperatorProfiles{}))
	req := withAdminUI(httptest.NewRequest("GET", "/ui/memory/operators/", nil))
	rec := httptest.NewRecorder()
	s.MemoryOperator(rec, req, "")
	assert.Equal(t, 404, rec.Code)
}

// TestOperatorScalarToString_Variants pins every coercion path:
// string trim, integer + float numeric, bool, unsupported nested
// type. Without this the structured-key rendering could drift
// silently between releases.
func TestOperatorScalarToString_Variants(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string trimmed", "  hi  ", "hi"},
		{"empty string", "", ""},
		{"integer-valued float", float64(7), "7"},
		{"true bool", true, "true"},
		{"false bool", false, "false"},
		{"map drops to empty", map[string]any{"a": 1}, ""},
		{"slice drops to empty", []any{"a"}, ""},
		{"nil drops to empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := operatorScalarToString(tc.in); got != tc.want {
				t.Errorf("operatorScalarToString(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPrettyJSON_RoundtripsValidInput formats and pretty-prints.
func TestPrettyJSON_RoundtripsValidInput(t *testing.T) {
	got := prettyJSON([]byte(`{"a":1}`))
	if got == "" {
		t.Errorf("prettyJSON should not be empty on valid JSON")
	}
	// Pretty-printed should contain a newline and indentation.
	if !strings.Contains(got, "\n") {
		t.Errorf("prettyJSON output should be multi-line; got %q", got)
	}
}

// TestPrettyJSON_GarbageReturnsEmpty — malformed input returns
// empty so the detail page hides the raw block cleanly.
func TestPrettyJSON_GarbageReturnsEmpty(t *testing.T) {
	if got := prettyJSON([]byte("not json")); got != "" {
		t.Errorf("prettyJSON(garbage) = %q, want empty", got)
	}
	if got := prettyJSON(nil); got != "" {
		t.Errorf("prettyJSON(nil) = %q, want empty", got)
	}
}

// TestFormatInt64_Boundaries — zero / positive / negative.
func TestFormatInt64_Boundaries(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{-7, "-7"},
		{12345, "12345"},
	}
	for _, tc := range cases {
		if got := formatInt64(tc.in); got != tc.want {
			t.Errorf("formatInt64(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatFloat_DecimalRounding pins the 2-decimal renderer
// the structured-key rendering uses. A subtle change here could
// break the operator-visible "verbosity: 0.55" display.
func TestFormatFloat_DecimalRounding(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.50, "0.50"},
		{0.555, "0.56"},  // rounds up
		{-0.25, "-0.25"}, // negative rounding
		{1.0, "1.00"},
	}
	for _, tc := range cases {
		if got := formatFloat(tc.in); got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPadFrac_LeadingZeros — fractional part needs leading-zero
// padding to match the decimals width.
func TestPadFrac_LeadingZeros(t *testing.T) {
	cases := []struct {
		frac  int64
		width int
		want  string
	}{
		{5, 2, "05"},
		{55, 2, "55"},
		{1, 3, "001"},
		{0, 2, "00"},
	}
	for _, tc := range cases {
		if got := padFrac(tc.frac, tc.width); got != tc.want {
			t.Errorf("padFrac(%d, %d) = %q, want %q", tc.frac, tc.width, got, tc.want)
		}
	}
}
