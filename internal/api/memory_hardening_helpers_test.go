package api

import (
	"encoding/json"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestMarshalEpoch covers the camelCase response shape for a
// CorpusEpoch row. All optional pointer fields are exercised in
// both set and unset form so the JSON envelope stays stable.
func TestMarshalEpoch_BareEpoch(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	e := &persistence.CorpusEpoch{
		ID:                "epoch_1",
		ProjectID:         "p1",
		CreatedAt:         now,
		ChunksAdmitted:    10,
		ChunksQuarantined: 2,
		ChunksVerified:    1,
		ChunksRefuted:     0,
		ChunksSuperseded:  3,
		IsActive:          true,
	}
	got := marshalEpoch(e)
	if got["id"] != "epoch_1" {
		t.Errorf("id: got %v, want epoch_1", got["id"])
	}
	if got["createdAt"] != now.Format(time.RFC3339) {
		t.Errorf("createdAt: got %v, want RFC3339-formatted UTC", got["createdAt"])
	}
	if _, ok := got["closedAt"]; ok {
		t.Error("closedAt: should be absent when ClosedAt is nil")
	}
	if _, ok := got["ingestExecutionId"]; ok {
		t.Error("ingestExecutionId: should be absent when nil")
	}
}

// TestMarshalEpoch_OptionalsPresent — set every pointer field
// and verify they land in the envelope.
func TestMarshalEpoch_OptionalsPresent(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	closedAt := now.Add(2 * time.Hour)
	execID := "exec_xyz"
	notes := "operator-authored note"
	e := &persistence.CorpusEpoch{
		ID:                "epoch_2",
		ProjectID:         "p1",
		CreatedAt:         now,
		ClosedAt:          &closedAt,
		IngestExecutionID: &execID,
		Notes:             &notes,
	}
	got := marshalEpoch(e)
	if got["closedAt"] != closedAt.Format(time.RFC3339) {
		t.Errorf("closedAt: got %v", got["closedAt"])
	}
	if got["ingestExecutionId"] != "exec_xyz" {
		t.Errorf("ingestExecutionId: got %v", got["ingestExecutionId"])
	}
	if got["notes"] != "operator-authored note" {
		t.Errorf("notes: got %v", got["notes"])
	}

	// Smoke check the JSON round-trip — the response is encoded
	// through json.Marshal downstream.
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("marshal round-trip: %v", err)
	}
}

// TestPtrStrToAny — nil → nil, non-nil → dereferenced string.
// Used by handlers that need to render optional-string fields
// in JSON without leaking pointer types.
func TestPtrStrToAny(t *testing.T) {
	if got := ptrStrToAny(nil); got != nil {
		t.Errorf("nil pointer: got %v, want nil any", got)
	}
	s := "hello"
	if got := ptrStrToAny(&s); got != "hello" {
		t.Errorf("non-nil pointer: got %v, want %q", got, "hello")
	}
}
