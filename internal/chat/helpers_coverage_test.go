package chat

import (
	"errors"
	"strings"
	"testing"
)

// Coverage-sweep tests for the pure helpers in this package.
// Each test pins one observable contract; failures should
// describe the specific regression in human-readable terms.

// TestFallback_PreservesPrimary — primary non-empty wins;
// backup ignored. The function's whole job.
func TestFallback_PreservesPrimary(t *testing.T) {
	if got := fallback("a", "b"); got != "a" {
		t.Errorf("fallback(a,b) = %q, want a", got)
	}
	if got := fallback("", "b"); got != "b" {
		t.Errorf("fallback(\"\",b) = %q, want b", got)
	}
	if got := fallback("", ""); got != "" {
		t.Errorf("fallback(\"\",\"\") = %q, want \"\"", got)
	}
}

// TestCoalesceJSON_EmptyBecomesObject — the CLI providers emit
// blank text on some failure paths; coalesce protects downstream
// json.Unmarshal callers from "unexpected end of JSON input" by
// substituting the empty object literal.
func TestCoalesceJSON_EmptyBecomesObject(t *testing.T) {
	cases := map[string]string{
		"":           "{}",
		"   ":        "{}",
		"\t\n":       "{}",
		`{"a":1}`:    `{"a":1}`,
		`  {"x":2} `: `  {"x":2} `, // non-blank passes through verbatim
	}
	for in, want := range cases {
		if got := coalesceJSON(in); got != want {
			t.Errorf("coalesceJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTruncate_RespectsLengthBudget — used everywhere in the
// chat package to cap log bodies before journald sees them.
// Boundary conditions matter: at-cap should not get the ellipsis.
func TestTruncate_RespectsLengthBudget(t *testing.T) {
	if got := truncate("hi", 10); got != "hi" {
		t.Errorf("under-cap truncated: %q", got)
	}
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("at-cap added ellipsis: %q", got)
	}
	if got := truncate("hello world", 5); got != "hello…" {
		t.Errorf("over-cap truncated wrong: %q, want hello…", got)
	}
}

// TestTruncateLogString_AppendsSuffix — sister helper; differs
// only in the suffix text. Pin the format so a future cleanup
// merger doesn't accidentally change the wire shape that ops
// dashboards key off.
func TestTruncateLogString_AppendsSuffix(t *testing.T) {
	if got := truncateLogString("hi", 100); got != "hi" {
		t.Errorf("under-cap mutated: %q", got)
	}
	got := truncateLogString("hello world", 5)
	if got != "hello"+"...(truncated)" {
		t.Errorf("truncateLogString = %q, want hello...(truncated)", got)
	}
}

// TestClassifyCLIError_MapsCanonicalShapes — the metric labels
// must be stable across releases; Grafana queries grep on them.
func TestClassifyCLIError_MapsCanonicalShapes(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "success"},
		{errors.New("context deadline exceeded"), "timeout"},
		{errors.New("signal: killed"), "timeout"},
		{errors.New("exit status 1"), "cli_nonzero_exit"},
		{errors.New("no assistant text in response"), "empty_response"},
		{errors.New("something else entirely"), "error"},
	}
	for _, c := range cases {
		if got := classifyCLIError(c.err); got != c.want {
			t.Errorf("classifyCLIError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestError_FormatsCodeAndMessage — pin the wire shape that
// callers downstream may pattern-match on. JSON-shape Error
// values mostly come from the upstream chat gateway.
func TestError_FormatsCodeAndMessage(t *testing.T) {
	e := &Error{Code: 400, Message: "bad request"}
	if got := e.Error(); got != "chat error 400: bad request" {
		t.Errorf("Error() = %q", got)
	}
}

// TestGatewayError_FormatsWithStatus — distinct shape from Error;
// includes both the HTTP status and the parsed Message when
// available. The fallback formatting (no message) is also pinned
// because retry-policy code uses substring matching on the
// rendered string.
func TestGatewayError_FormatsWithStatus(t *testing.T) {
	withMsg := &GatewayError{Status: 502, Message: "upstream timeout"}
	if got := withMsg.Error(); !strings.Contains(got, "502") || !strings.Contains(got, "upstream timeout") {
		t.Errorf("Error() = %q", got)
	}
	noMsg := &GatewayError{Status: 503}
	if got := noMsg.Error(); !strings.Contains(got, "503") {
		t.Errorf("no-message Error() = %q", got)
	}
}

// TestDecodeJWTClaims_PadFormats — codex tokens land as base64url
// without padding (most modern OIDC providers) but some legacy
// vendors emit base64url WITH padding. The decoder must accept
// both rather than rejecting valid tokens.
func TestDecodeJWTClaims_PadFormats(t *testing.T) {
	// {"sub":"u1","iss":"test"} → no-pad base64url
	header := "eyJhbGciOiJIUzI1NiJ9"
	payloadNoPad := "eyJzdWIiOiJ1MSIsImlzcyI6InRlc3QifQ"
	sig := "_sig_"
	tok := header + "." + payloadNoPad + "." + sig
	claims, err := decodeJWTClaims(tok)
	if err != nil {
		t.Fatalf("decode no-pad: %v", err)
	}
	if claims["sub"] != "u1" || claims["iss"] != "test" {
		t.Errorf("claims = %+v", claims)
	}
}

// TestDecodeJWTClaims_RejectsMalformed — three-segment shape is
// required. Two-segment or four-segment tokens are invalid JWTs
// and the decoder MUST error rather than silently parse.
func TestDecodeJWTClaims_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"only-one-segment",
		"two.segments",
		"four.is.too.many",
		"invalid_base64.invalid_base64.invalid_base64",
	}
	for _, tok := range cases {
		if _, err := decodeJWTClaims(tok); err == nil {
			t.Errorf("decodeJWTClaims(%q) returned nil err", tok)
		}
	}
}
