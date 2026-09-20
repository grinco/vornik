package projectdeps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePip writes a shell script that records its argv and simulates pip.
func fakePip(t *testing.T, body string) (bin []string, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	script := filepath.Join(dir, "fake-pip")
	src := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" + body + "\n"
	if err := os.WriteFile(script, []byte(src), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{script}, argvFile
}

func TestPipFetcherInstallsIntoTheSiteDirWithHashesRequired(t *testing.T) {
	bin, argvFile := fakePip(t, "exit 0")
	dir := t.TempDir()

	if err := PipFetcher(bin, "/proj/requirements.lock")(context.Background(), dir); err != nil {
		t.Fatalf("PipFetcher = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, SiteDir)); err != nil {
		t.Fatalf("site dir not created: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(argv)
	// --require-hashes is passed as well as validated: RequireHashPinned
	// refuses a lockfile this design cannot key; pip's flag refuses an
	// artifact whose bytes do not match its pin. Different questions.
	for _, want := range []string{"install", "--require-hashes", "--target", filepath.Join(dir, SiteDir), "--requirement", "/proj/requirements.lock"} {
		if !strings.Contains(got, want+"\n") {
			t.Fatalf("argv = %q, want it to carry %q", got, want)
		}
	}
	for _, arg := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if arg == "-c" || arg == "sh" || arg == "/bin/sh" {
			t.Fatalf("argv = %q, want no shell — the argv is fixed (design §5.1)", got)
		}
	}
}

func TestPipFetcherSurfacesPipsOwnDiagnostics(t *testing.T) {
	// "exit status 1" reads nothing like a hash mismatch, and a hash
	// mismatch is exactly what an operator has to act on.
	bin, _ := fakePip(t, "echo 'ERROR: THESE PACKAGES DO NOT MATCH THE HASHES' >&2; exit 1")

	err := PipFetcher(bin, "/proj/requirements.lock")(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("PipFetcher = nil, want the install failure")
	}
	if !strings.Contains(err.Error(), "DO NOT MATCH THE HASHES") {
		t.Fatalf("err = %q, want pip's own diagnostic carried through", err)
	}
}

func TestPipFetcherDefaultsToTheInterpretersOwnPip(t *testing.T) {
	// A bare `pip` can belong to a different interpreter than the one the
	// agent runs, which materialises into a site dir nothing imports.
	if len(DefaultPipBin) < 3 || DefaultPipBin[0] != "python3" || DefaultPipBin[1] != "-m" || DefaultPipBin[2] != "pip" {
		t.Fatalf("DefaultPipBin = %v, want python3 -m pip", DefaultPipBin)
	}
	// An empty bin must fall back rather than exec "".
	err := PipFetcher(nil, "/nope.lock")(context.Background(), t.TempDir())
	if err != nil && strings.Contains(err.Error(), `exec: "": `) {
		t.Fatalf("empty bin execed an empty argv0: %v", err)
	}
}

func TestTailLinesBoundsTheDiagnostic(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("line\n")
	}
	got := tailLines(b.String(), 40)
	if n := strings.Count(got, "line"); n != 40 {
		t.Fatalf("kept %d lines, want 40", n)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("a truncated diagnostic must say so, got %q", got[:10])
	}
	if short := tailLines("a\nb\n", 40); short != "a\nb" {
		t.Fatalf("tailLines(short) = %q", short)
	}
}
