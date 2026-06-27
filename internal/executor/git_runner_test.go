package executor

import (
	"context"
	"errors"
	"testing"
)

// fakeGitRunner scripts git output per first-arg-after-flags subcommand and
// records the invocations, so git-backed helpers can be exercised without a
// real repository. It is intentionally simple — match on the subcommand
// token (the arg after the leading "-C <dir>" pair, if present).
type fakeGitRunner struct {
	outputs map[string][]byte // keyed by subcommand (e.g. "rev-parse")
	errs    map[string]error
	calls   [][]string
}

func newFakeGitRunner() *fakeGitRunner {
	return &fakeGitRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
}

func (f *fakeGitRunner) subcmd(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-C" {
			i++ // skip the dir
			continue
		}
		return args[i]
	}
	return ""
}

func (f *fakeGitRunner) output(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	sc := f.subcmd(args)
	return f.outputs[sc], f.errs[sc]
}

func (f *fakeGitRunner) combined(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	sc := f.subcmd(args)
	return f.outputs[sc], f.errs[sc]
}

// withGitRunner swaps the package git runner for the duration of a test and
// restores it after. Tests using it must NOT run in parallel (shared global).
func withGitRunner(t *testing.T, r gitRunner) {
	t.Helper()
	old := gitExec
	gitExec = r
	t.Cleanup(func() { gitExec = old })
}

func TestGitHEAD_ReturnsTrimmedSHAFromRunner(t *testing.T) {
	f := newFakeGitRunner()
	f.outputs["rev-parse"] = []byte("abc123def456\n")
	withGitRunner(t, f)

	if got := gitHEAD(context.Background(), "/some/worktree"); got != "abc123def456" {
		t.Fatalf("gitHEAD = %q, want trimmed SHA %q", got, "abc123def456")
	}
	// Sanity: it asked for the HEAD of the given dir.
	if len(f.calls) != 1 || f.calls[0][0] != "-C" || f.calls[0][1] != "/some/worktree" {
		t.Fatalf("unexpected git invocation: %v", f.calls)
	}
}

func TestGitHEAD_EmptyOnError(t *testing.T) {
	f := newFakeGitRunner()
	f.errs["rev-parse"] = errors.New("not a git repository")
	withGitRunner(t, f)

	if got := gitHEAD(context.Background(), "/not/a/repo"); got != "" {
		t.Fatalf("gitHEAD on error = %q, want \"\"", got)
	}
}

func TestGitHEAD_EmptyDirShortCircuits(t *testing.T) {
	f := newFakeGitRunner()
	withGitRunner(t, f)
	if got := gitHEAD(context.Background(), ""); got != "" {
		t.Fatalf("gitHEAD(\"\") = %q, want \"\"", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("gitHEAD(\"\") must not shell out, got calls: %v", f.calls)
	}
}

func TestGitDiffFileCount_CountsNonBlankLines(t *testing.T) {
	f := newFakeGitRunner()
	f.outputs["diff"] = []byte("internal/a.go\ninternal/b.go\n\n")
	withGitRunner(t, f)

	n, ok := gitDiffFileCount(context.Background(), "/proj", "aaa", "bbb")
	if !ok || n != 2 {
		t.Fatalf("gitDiffFileCount = (%d,%v), want (2,true)", n, ok)
	}
}

func TestGitDiffFileCount_FalseOnGitError(t *testing.T) {
	f := newFakeGitRunner()
	f.errs["diff"] = errors.New("bad revision")
	withGitRunner(t, f)

	if n, ok := gitDiffFileCount(context.Background(), "/proj", "aaa", "bbb"); ok || n != 0 {
		t.Fatalf("gitDiffFileCount on error = (%d,%v), want (0,false)", n, ok)
	}
}

func TestGitObjectExists_ViaRunner(t *testing.T) {
	f := newFakeGitRunner()
	withGitRunner(t, f) // cat-file returns no error by default → present
	if !gitObjectExists(context.Background(), "/proj", "deadbeef") {
		t.Fatal("gitObjectExists: want true when cat-file -e exits 0")
	}
	f.errs["cat-file"] = errors.New("exit status 1")
	if gitObjectExists(context.Background(), "/proj", "deadbeef") {
		t.Fatal("gitObjectExists: want false when cat-file -e errors")
	}
	// Empty sha short-circuits without shelling out.
	f2 := newFakeGitRunner()
	withGitRunner(t, f2)
	if gitObjectExists(context.Background(), "/proj", "") {
		t.Fatal("gitObjectExists(\"\"): want false")
	}
	if len(f2.calls) != 0 {
		t.Fatalf("gitObjectExists(\"\") must not shell out, got: %v", f2.calls)
	}
}
