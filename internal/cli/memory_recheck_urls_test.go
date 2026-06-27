package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// stubRecheckRunner implements recheckURLsRunner for the CLI body
// test. Captures the args and returns a fixed outcome.
type stubRecheckRunner struct {
	gotProject string
	gotLimit   int
	out        memory.RecheckOutcome
	err        error
}

func (s *stubRecheckRunner) RecheckProject(_ context.Context, projectID string, limit int) (memory.RecheckOutcome, error) {
	s.gotProject = projectID
	s.gotLimit = limit
	return s.out, s.err
}

// TestDoMemoryRecheckURLs_HumanReadable — the human-readable path
// surfaces the counts the operator cares about (alive vs dead).
func TestDoMemoryRecheckURLs_HumanReadable(t *testing.T) {
	runner := &stubRecheckRunner{
		out: memory.RecheckOutcome{
			ChunksScanned:   12,
			ChunksWithURLs:  8,
			URLsChecked:     11,
			URLsAlive:       7,
			URLsDead:        4,
			ChunksConfirmed: 5,
			ChunksFlagged:   3,
		},
	}
	var buf bytes.Buffer
	if err := doMemoryRecheckURLs(context.Background(), runner, "assistant", 100, false, &buf); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if runner.gotProject != "assistant" || runner.gotLimit != 100 {
		t.Fatalf("runner called with wrong args: project=%q limit=%d", runner.gotProject, runner.gotLimit)
	}
	got := buf.String()
	wantContains := []string{
		`assistant`,
		`chunks scanned`,
		`alive=7`,
		`dead=4`,
		`chunks flagged:    3`,
	}
	for _, sub := range wantContains {
		if !strings.Contains(got, sub) {
			t.Fatalf("output missing %q:\n%s", sub, got)
		}
	}
}

// TestDoMemoryRecheckURLs_JSON — JSON mode round-trips the counts
// cleanly so downstream tooling can parse them.
func TestDoMemoryRecheckURLs_JSON(t *testing.T) {
	runner := &stubRecheckRunner{
		out: memory.RecheckOutcome{
			ChunksScanned:   2,
			ChunksWithURLs:  1,
			URLsChecked:     1,
			URLsAlive:       0,
			URLsDead:        1,
			ChunksConfirmed: 0,
			ChunksFlagged:   1,
		},
	}
	var buf bytes.Buffer
	if err := doMemoryRecheckURLs(context.Background(), runner, "p", 0, true, &buf); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if parsed["project"] != "p" {
		t.Fatalf("project mismatch: %v", parsed["project"])
	}
	// JSON numbers come back as float64.
	if parsed["urls_dead"].(float64) != 1 {
		t.Fatalf("urls_dead mismatch: %v", parsed["urls_dead"])
	}
	if parsed["chunks_flagged"].(float64) != 1 {
		t.Fatalf("chunks_flagged mismatch: %v", parsed["chunks_flagged"])
	}
}

// TestDoMemoryRecheckURLs_PropagatesRunnerErr — when the underlying
// liveness checker errors (e.g. DB unreachable), the CLI surfaces it.
func TestDoMemoryRecheckURLs_PropagatesRunnerErr(t *testing.T) {
	runner := &stubRecheckRunner{err: errors.New("boom")}
	var buf bytes.Buffer
	err := doMemoryRecheckURLs(context.Background(), runner, "p", 0, false, &buf)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected wrapped runner error, got %v", err)
	}
}

// TestDoMemoryRecheckURLs_RejectsEmptyProject — the CLI guards the
// surface separately from the runner so the runner's no-op error
// can't be masked by a typo.
func TestDoMemoryRecheckURLs_RejectsEmptyProject(t *testing.T) {
	runner := &stubRecheckRunner{}
	var buf bytes.Buffer
	if err := doMemoryRecheckURLs(context.Background(), runner, "", 0, false, &buf); err == nil {
		t.Fatal("expected error on empty project")
	}
	if runner.gotProject != "" {
		t.Fatal("runner must NOT be invoked when project is empty")
	}
}

// TestMemoryRecheckURLsCommand_FlagsRegistered guards against a
// future refactor accidentally dropping the operator-facing flags.
func TestMemoryRecheckURLsCommand_FlagsRegistered(t *testing.T) {
	cmd := memoryRecheckURLsCmd
	wantFlags := []string{"project", "limit", "timeout", "json"}
	for _, f := range wantFlags {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("flag --%s is missing", f)
		}
	}
	// --project must be marked required so an empty invocation
	// errors fast on the Cobra side.
	flag := cmd.Flags().Lookup("project")
	if flag == nil {
		t.Fatal("--project flag missing")
	}
	req := flag.Annotations[cobraBashCompOneRequiredFlag]
	if len(req) == 0 || req[0] != "true" {
		t.Error("--project must be required")
	}
}

// cobraBashCompOneRequiredFlag is the annotation key Cobra uses to
// remember which flags were marked required. Copying the value
// here avoids depending on cobra's unexported constants.
const cobraBashCompOneRequiredFlag = "cobra_annotation_bash_completion_one_required_flag"
