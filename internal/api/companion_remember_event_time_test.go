package api

import (
	"encoding/json"
	"testing"
	"time"
)

// Slice 0a of 2026-08-10-memory-benchmark-harness-design.md §4.1: the
// companion remember tool gains an optional event_time so a caller depositing
// dated material can say when the content PERTAINS TO, rather than having it
// silently inherit "now".
//
// Without this, IngestTextAt has no external caller and the event-time column
// is unreachable from outside the process — the read path would COALESCE every
// chunk back to ingest time forever.

// TestRememberArgs_ParsesEventTime pins the wire contract: RFC3339 in, a real
// instant out.
func TestRememberArgs_ParsesEventTime(t *testing.T) {
	var args rememberArgs
	raw := `{"content":"Alice moved to Berlin","event_time":"2023-05-14T09:30:00Z"}`
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := args.parseEventTime()
	if err != nil {
		t.Fatalf("parseEventTime: %v", err)
	}
	want := time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("event_time = %v, want %v", got, want)
	}
}

// TestRememberArgs_AbsentEventTimeIsZero pins the default. Zero means
// "unknown", which the write path persists as NULL so the read path's COALESCE
// falls back to ingest time — i.e. exactly today's behaviour for every caller
// that doesn't opt in.
func TestRememberArgs_AbsentEventTimeIsZero(t *testing.T) {
	var args rememberArgs
	if err := json.Unmarshal([]byte(`{"content":"no date here"}`), &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := args.parseEventTime()
	if err != nil {
		t.Fatalf("parseEventTime: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("absent event_time = %v, want zero (unknown)", got)
	}
}

// TestRememberArgs_MalformedEventTimeRejected — a garbled date must be a loud
// error, not a silent fallback to now. Silently defaulting would file the
// deposit under the wrong clock and be invisible at the call site, which is the
// exact failure mode the event-time work exists to remove.
func TestRememberArgs_MalformedEventTimeRejected(t *testing.T) {
	for _, bad := range []string{"14/05/2023", "yesterday", "2023-13-45T99:99:99Z", "1684056600"} {
		var args rememberArgs
		raw, err := json.Marshal(map[string]string{"content": "x", "event_time": bad})
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("unmarshal %q: %v", bad, err)
		}
		if _, err := args.parseEventTime(); err == nil {
			t.Errorf("event_time %q accepted; want a rejection so the caller "+
				"learns the date was not understood", bad)
		}
	}
}

// TestRememberArgs_DateOnlyEventTimeAccepted — the memory_search tool already
// accepts a bare YYYY-MM-DD for its bounds, so remember accepting only full
// RFC3339 would be a gratuitous inconsistency across two tools the same model
// uses in one turn.
func TestRememberArgs_DateOnlyEventTimeAccepted(t *testing.T) {
	var args rememberArgs
	if err := json.Unmarshal([]byte(`{"content":"x","event_time":"2023-05-14"}`), &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := args.parseEventTime()
	if err != nil {
		t.Fatalf("parseEventTime(date-only): %v", err)
	}
	want := time.Date(2023, 5, 14, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("date-only event_time = %v, want %v", got, want)
	}
}

// TestParseRecallDateBound_AcceptsDateOnly is a regression test for a live-daemon
// bug found by a benchmark smoke run: the recall tool's own JSON schema documents
// "ISO date (YYYY-MM-DD) or full RFC3339", and the agent-side memory_search tool
// accepts both, but the companion path required RFC3339 and rejected exactly the
// input its schema advertises:
//
//	from_date: parsing time "2023-01-01" as "2006-01-02T15:04:05Z07:00"
func TestParseRecallDateBound_AcceptsDateOnly(t *testing.T) {
	got, err := parseRecallDateBound("2023-01-01", false)
	if err != nil {
		t.Fatalf("date-only lower bound rejected: %v", err)
	}
	want := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("lower bound = %v, want %v", got, want)
	}
}

// TestParseRecallDateBound_UpperBoundCoversWholeDay — an inclusive "to
// 2023-12-31" must cover that entire day. Taking midnight would silently exclude
// everything stamped later on the boundary day, which is the subtle half of the
// bug: the query looks right and quietly loses a day of content.
func TestParseRecallDateBound_UpperBoundCoversWholeDay(t *testing.T) {
	got, err := parseRecallDateBound("2023-12-31", true)
	if err != nil {
		t.Fatalf("date-only upper bound rejected: %v", err)
	}
	want := time.Date(2023, 12, 31, 23, 59, 59, 999999999, time.UTC)
	if !got.Equal(want) {
		t.Errorf("upper bound = %v, want end of day %v", got, want)
	}
}

// TestParseRecallDateBound_AcceptsRFC3339 — the previously-working form must keep
// working; this fix widens the contract rather than replacing it.
func TestParseRecallDateBound_AcceptsRFC3339(t *testing.T) {
	got, err := parseRecallDateBound("2023-05-14T09:30:00Z", false)
	if err != nil {
		t.Fatalf("RFC3339 rejected: %v", err)
	}
	if !got.Equal(time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("RFC3339 parsed to %v", got)
	}
}

// TestParseRecallDateBound_RejectsGarbage — a malformed bound must error rather
// than silently becoming the zero time, which the searcher reads as "no bound"
// and would widen the search instead of narrowing it.
func TestParseRecallDateBound_RejectsGarbage(t *testing.T) {
	for _, bad := range []string{"yesterday", "14/05/2023", "2023-13-45", ""} {
		if _, err := parseRecallDateBound(bad, false); err == nil {
			t.Errorf("accepted %q as a date bound", bad)
		}
	}
}
