package dispatcher

import "testing"

func TestRedactSensitiveToolArgs(t *testing.T) {
	// approval_token redacted, other fields preserved.
	in := `{"mode":"submit","submission_id":"s1","approval_token":"deadbeefdeadbeef"}`
	out := redactSensitiveToolArgs(in)
	if want := `"[redacted]"`; !contains(out, want) {
		t.Fatalf("token not redacted: %s", out)
	}
	if contains(out, "deadbeefdeadbeef") {
		t.Fatalf("raw token leaked: %s", out)
	}
	if !contains(out, `"submission_id":"s1"`) && !contains(out, `"submission_id": "s1"`) {
		t.Fatalf("submission_id lost: %s", out)
	}
	// empty token: unchanged (nothing to redact).
	if got := redactSensitiveToolArgs(`{"approval_token":""}`); contains(got, "[redacted]") {
		t.Fatalf("empty token should not be redacted: %s", got)
	}
	// non-JSON: returned unchanged.
	if got := redactSensitiveToolArgs("not json"); got != "not json" {
		t.Fatalf("non-json mutated: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
