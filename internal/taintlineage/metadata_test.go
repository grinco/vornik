package taintlineage

import "testing"

func TestSeverityString(t *testing.T) {
	cases := map[Severity]string{
		SeverityNone: "none", SeverityLow: "low", SeverityHigh: "high",
		SeverityUnknown: "unknown", Severity(99): "invalid",
	}
	for sev, want := range cases {
		if got := sev.String(); got != want {
			t.Errorf("Severity(%d).String() = %q, want %q", sev, got, want)
		}
	}
}

func TestCheckpointMetadataHelpers(t *testing.T) {
	// A taint checkpoint round-trips its kind + hash.
	meta := []byte(`{"decision":{"kind":"untrusted_review","source_set_hash":"h1"}}`)
	if !IsTaintReviewCheckpointMeta(meta) {
		t.Fatalf("must recognize untrusted_review checkpoint")
	}
	if h := CheckpointSourceSetHash(meta); h != "h1" {
		t.Fatalf("hash = %q, want h1", h)
	}
	// A budget checkpoint is not a taint checkpoint; no hash.
	budget := []byte(`{"decision":{"kind":"budget"}}`)
	if IsTaintReviewCheckpointMeta(budget) {
		t.Fatalf("budget must not be a taint checkpoint")
	}
	if h := CheckpointSourceSetHash(budget); h != "" {
		t.Fatalf("budget hash must be empty, got %q", h)
	}
	// Empty / malformed inputs are safe.
	if IsTaintReviewCheckpointMeta(nil) || CheckpointSourceSetHash(nil) != "" {
		t.Fatalf("nil metadata must be safe")
	}
	if IsTaintReviewCheckpointMeta([]byte("{bad")) {
		t.Fatalf("malformed metadata must not parse")
	}
}

func TestLatchMarkerRoundTrip(t *testing.T) {
	meta := LatchMarkerMetadata("abc123")
	h, ok := ParseLatchHash(meta)
	if !ok || h != "abc123" {
		t.Fatalf("latch round-trip failed: h=%q ok=%v", h, ok)
	}
	// Non-latch metadata / empty hash / malformed → not a latch.
	if _, ok := ParseLatchHash([]byte(`{"kind":"other","source_set_hash":"x"}`)); ok {
		t.Fatalf("non-latch kind must not parse as latch")
	}
	if _, ok := ParseLatchHash(LatchMarkerMetadata("")); ok {
		t.Fatalf("empty hash must not parse as a latch")
	}
	if _, ok := ParseLatchHash(nil); ok {
		t.Fatalf("nil must not parse as a latch")
	}
	if _, ok := ParseLatchHash([]byte("{bad")); ok {
		t.Fatalf("malformed must not parse as a latch")
	}
}

func TestStepTaintFromBlob(t *testing.T) {
	// A High row: max severity reconstructs from the source, requires_review set.
	high := StepTaintFromBlob([]byte(`[{"tool":"web_fetch","ref":"u","severity":2}]`), true)
	if !high.Used || !high.RequiresReview || high.MaxSeverity != SeverityHigh {
		t.Fatalf("high row reconstruct wrong: %+v", high)
	}
	// An Unknown-only row: used, NOT requires_review, MaxSeverity=Unknown (F3).
	unk := StepTaintFromBlob([]byte(`[{"tool":"weird","ref":"weird","severity":3}]`), false)
	if !unk.Used || unk.RequiresReview || unk.MaxSeverity != SeverityUnknown {
		t.Fatalf("unknown row reconstruct wrong: %+v", unk)
	}
	// requires_review=true with an empty/low blob defensively implies High.
	defensive := StepTaintFromBlob(nil, true)
	if defensive.MaxSeverity != SeverityHigh {
		t.Fatalf("requires_review=true must imply >= High, got %v", defensive.MaxSeverity)
	}
	// Malformed blob → still Used (row came from the index), no sources.
	mal := StepTaintFromBlob([]byte("{bad"), false)
	if !mal.Used || len(mal.Sources) != 0 {
		t.Fatalf("malformed blob handling wrong: %+v", mal)
	}
}

func TestQueryAPIRef_Variants(t *testing.T) {
	if r := queryAPIRef(`{"path":"/v1/x"}`); r != "/v1/x" {
		t.Fatalf("path-only ref = %q", r)
	}
	if r := queryAPIRef(`{}`); r != "" {
		t.Fatalf("empty json must yield empty ref, got %q", r)
	}
}
