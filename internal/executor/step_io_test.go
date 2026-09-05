package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// The two files at the container boundary are persisted as content-addressed
// parts beside the prompt (step-I/O persistence design §3): the outcome row
// carries the hash of what was STORED, and the bodies are readable back.
func TestPersistStepIO_HashesAreOfTheStoredBytes(t *testing.T) {
	repo := &fakePromptRepo{saved: map[string]string{}}
	e := &Executor{logger: zerolog.Nop(), stepPromptRepo: repo, metrics: NewMetrics(prometheus.NewRegistry())}
	in, out := []byte(`{"taskId":"t1","context":{"prompt":"do it"}}`), []byte(`{"status":"COMPLETED","message":"done"}`)
	h := e.persistStepIO(context.Background(), "e", "s", stepIOFiles{Input: in, Result: out})
	if h.Input != persistence.HashStepPrompt(string(in)) || h.Result != persistence.HashStepPrompt(string(out)) {
		t.Fatalf("hashes = %+v, want the sha256 of each stored body", h)
	}
	if repo.saved[h.Input] != string(in) || repo.saved[h.Result] != string(out) {
		t.Fatalf("bodies were not stored verbatim: %v", repo.saved)
	}
	if n := testutil.CollectAndCount(e.metrics.StepIOSkippedTotal); n != 0 {
		t.Fatalf("nothing was skipped, counter has %d series", n)
	}
}

// Absent result.json (the container never wrote one) stores nothing and counts
// nothing: the outcome row already says so through container_exit_code. A nil
// repository is the daemon without the store — empty hashes, never an error.
func TestPersistStepIO_AbsentResultAndNoStore(t *testing.T) {
	repo := &fakePromptRepo{saved: map[string]string{}}
	e := &Executor{logger: zerolog.Nop(), stepPromptRepo: repo, metrics: NewMetrics(prometheus.NewRegistry())}
	h := e.persistStepIO(context.Background(), "e", "s", stepIOFiles{Input: []byte(`{}`)})
	if h.Input == "" || h.Result != "" {
		t.Fatalf("input stored, result absent: got %+v", h)
	}
	if n := testutil.CollectAndCount(e.metrics.StepIOSkippedTotal); n != 0 {
		t.Fatalf("an absent result is not a skip, counter has %d series", n)
	}
	e.stepPromptRepo = nil
	if h := e.persistStepIO(context.Background(), "e", "s", stepIOFiles{Input: []byte(`{}`), Result: []byte(`{}`)}); h != (persistence.StepPromptHashes{}) {
		t.Fatalf("no store must yield empty hashes, got %+v", h)
	}
}

// A part over the ceiling is not stored: empty hash, one count with
// reason=too_large and the part's name, and the other part unaffected.
func TestPersistStepIO_CeilingIsCountedNotStored(t *testing.T) {
	repo := &fakePromptRepo{saved: map[string]string{}}
	e := &Executor{logger: zerolog.Nop(), stepPromptRepo: repo, metrics: NewMetrics(prometheus.NewRegistry())}
	huge := []byte(strings.Repeat("x", stepIOMaxBytes+1))
	h := e.persistStepIO(context.Background(), "e", "s", stepIOFiles{Input: huge, Result: []byte(`{"status":"FAILED"}`)})
	if h.Input != "" {
		t.Fatalf("an oversized input must not be stored, got hash %q", h.Input)
	}
	if h.Result == "" {
		t.Fatalf("the result under the ceiling must still be stored: %+v", h)
	}
	if v := testutil.ToFloat64(e.metrics.StepIOSkippedTotal.WithLabelValues("input", "too_large")); v != 1 {
		t.Errorf("too_large count for input = %v, want 1", v)
	}
	if v := testutil.ToFloat64(e.metrics.StepIOSkippedTotal.WithLabelValues("result", "too_large")); v != 0 {
		t.Errorf("too_large count for result = %v, want 0", v)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("stored %d parts, want 1", len(repo.saved))
	}
}

// The seam redacts before storing and the row's hash names the redacted body
// — and that body is still JSON, which the replay harness relies on (design
// §4). A store failure is a log line and an empty hash, never a step failure.
func TestPersistStepIO_RedactedBodyStaysJSONAndFailureIsEmpty(t *testing.T) {
	secret := `{"context":{"prompt":"use token=sk-live-123456 now"}}`
	repo := &fakePromptRepo{saved: map[string]string{}, redact: func(s string) string {
		return strings.ReplaceAll(s, "sk-live-123456", "[REDACTED:api_key]")
	}}
	e := &Executor{logger: zerolog.Nop(), stepPromptRepo: repo, metrics: NewMetrics(prometheus.NewRegistry())}
	h := e.persistStepIO(context.Background(), "e", "s", stepIOFiles{Input: []byte(secret)})
	stored := repo.saved[h.Input]
	if h.Input != persistence.HashStepPrompt(stored) || strings.Contains(stored, "sk-live") {
		t.Fatalf("the stored (redacted) hash must win and the secret must be gone: %q / %q", h.Input, stored)
	}
	if !json.Valid([]byte(stored)) {
		t.Fatalf("the redacted body must still parse as JSON: %s", stored)
	}
	e.stepPromptRepo = &fakePromptRepo{fail: true}
	if h := e.persistStepIO(context.Background(), "e", "s", stepIOFiles{Input: []byte(`{}`), Result: []byte(`{}`)}); h != (persistence.StepPromptHashes{}) {
		t.Fatalf("a failing store must yield empty hashes, got %+v", h)
	}
}
