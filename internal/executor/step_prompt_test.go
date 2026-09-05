package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// fakePromptRepo stores bodies, optionally "redacting" them the way the seam
// would, and returns the hash of what it stored.
type fakePromptRepo struct {
	redact func(string) string
	saved  map[string]string // hash -> body
	fail   bool
}

func (f *fakePromptRepo) Save(_ context.Context, _ persistence.StepPromptPart, body string) (string, error) {
	if f.fail {
		return "", os.ErrPermission
	}
	if f.redact != nil {
		body = f.redact(body)
	}
	h := persistence.HashStepPrompt(body)
	if f.saved == nil {
		f.saved = map[string]string{}
	}
	f.saved[h] = body
	return h, nil
}
func (f *fakePromptRepo) Get(context.Context, string) (*persistence.StepPrompt, error) {
	return nil, persistence.ErrNotFound
}
func (f *fakePromptRepo) PruneUnreferenced(context.Context) (int64, error) { return 0, nil }

// The double keeps the miss contract the real repositories keep: an absent
// part is ErrNotFound, never (nil, nil).
func TestFakePromptRepo_KeepsTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "StepPromptRepository.Get", (&fakePromptRepo{}).Get)
}

func writePromptFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stepPromptFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The backward-compatibility GATE (design §10, task 4): a container that writes
// no file — every image built before the contract — yields empty hashes and no
// error; an unparseable file yields the same.
func TestReadStepPromptFile_AbsentOrBrokenIsNil(t *testing.T) {
	dir := t.TempDir()
	logger := zerolog.Nop()
	if got := readStepPromptFile(filepath.Join(dir, stepPromptFileName), &logger, "e", "s"); got != nil {
		t.Fatalf("absent file must read as nil, got %+v", got)
	}
	writePromptFile(t, dir, "{not json")
	if got := readStepPromptFile(filepath.Join(dir, stepPromptFileName), &logger, "e", "s"); got != nil {
		t.Fatalf("unparseable file must read as nil, got %+v", got)
	}
	e := &Executor{logger: logger}
	if h := e.persistStepPrompt(context.Background(), "e", "s", nil); h != (persistence.StepPromptHashes{}) {
		t.Fatalf("nil file must yield empty hashes, got %+v", h)
	}
}

func TestPersistStepPrompt_HashesAreOfTheStoredBytes(t *testing.T) {
	dir := t.TempDir()
	sys, usr, tools := "You are the planner.", "Plan it.", `[{"type":"function"}]`
	writePromptFile(t, dir, `{"system":{"sha256":"`+persistence.HashStepPrompt(sys)+`","body":"You are the planner."},`+
		`"user":{"sha256":"`+persistence.HashStepPrompt(usr)+`","body":"Plan it."},`+
		`"tools":{"sha256":"`+persistence.HashStepPrompt(tools)+`","body":"[{\"type\":\"function\"}]"}}`)
	logger := zerolog.Nop()
	f := readStepPromptFile(filepath.Join(dir, stepPromptFileName), &logger, "e", "s")
	if f == nil {
		t.Fatal("file did not read")
	}
	repo := &fakePromptRepo{}
	reg := prometheus.NewRegistry()
	e := &Executor{logger: logger, stepPromptRepo: repo, metrics: NewMetrics(reg)}
	h := e.persistStepPrompt(context.Background(), "e", "s", f)
	want := persistence.StepPromptHashes{System: persistence.HashStepPrompt(sys), User: persistence.HashStepPrompt(usr), Tools: persistence.HashStepPrompt(tools)}
	if h != want {
		t.Fatalf("hashes = %+v, want %+v", h, want)
	}
	if len(repo.saved) != 3 {
		t.Fatalf("stored %d parts, want 3", len(repo.saved))
	}
	if n := testutil.CollectAndCount(e.metrics.PromptHashMismatchTotal); n != 0 {
		t.Fatalf("no mismatch expected, counter has %d series", n)
	}
}

// A redacting seam changes the bytes, so the container's hash and the stored
// hash differ: counted as reason=redacted, the STORED hash wins on the row.
// A container hash that disagrees while the bytes are unchanged is drift.
func TestPersistStepPrompt_MismatchReasons(t *testing.T) {
	logger := zerolog.Nop()
	secret := "token=sk-live-123456"
	f := &stepPromptFile{
		System: stepPromptPart{SHA256: persistence.HashStepPrompt(secret), Body: secret},
		User:   stepPromptPart{SHA256: "0000deadbeef", Body: "plain"},
	}
	repo := &fakePromptRepo{redact: func(s string) string {
		if s == secret {
			return "token=[REDACTED:api_key]"
		}
		return s
	}}
	reg := prometheus.NewRegistry()
	e := &Executor{logger: logger, stepPromptRepo: repo, metrics: NewMetrics(reg)}
	h := e.persistStepPrompt(context.Background(), "e", "s", f)
	if h.System != persistence.HashStepPrompt("token=[REDACTED:api_key]") {
		t.Fatalf("the stored (redacted) hash must win: %+v", h)
	}
	if h.User != persistence.HashStepPrompt("plain") {
		t.Fatalf("user hash: %+v", h)
	}
	if h.Tools != "" {
		t.Fatalf("an empty part is not stored: %+v", h)
	}
	if v := testutil.ToFloat64(e.metrics.PromptHashMismatchTotal.WithLabelValues("redacted")); v != 1 {
		t.Errorf("redacted mismatches = %v, want 1", v)
	}
	if v := testutil.ToFloat64(e.metrics.PromptHashMismatchTotal.WithLabelValues("drift")); v != 1 {
		t.Errorf("drift mismatches = %v, want 1", v)
	}
	// A failing store is a log line and empty hashes — never a step failure.
	e.stepPromptRepo = &fakePromptRepo{fail: true}
	if h := e.persistStepPrompt(context.Background(), "e", "s", f); h != (persistence.StepPromptHashes{}) {
		t.Fatalf("a failing store must yield empty hashes, got %+v", h)
	}
}
