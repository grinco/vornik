package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifySkillSource(t *testing.T) {
	dir := t.TempDir()
	localFile := filepath.Join(dir, "skill.md")
	_ = os.WriteFile(localFile, []byte("x"), 0o644)

	cases := []struct {
		in   string
		want SkillSourceForm
	}{
		{"file:///tmp/x.md", SourceFormLocal},
		{"./relative/skill.md", SourceFormLocal},
		{"/abs/path/skill.md", SourceFormLocal},
		{localFile, SourceFormLocal},
		{"https://example.com/skill.md", SourceFormHTTPS},
		{"git+https://example.com/repo", SourceFormGit},
		{"https://example.com/repo.git", SourceFormGit},
		{"vadim/research", SourceFormHandle},
		{"vadim/research@v1.0.0", SourceFormHandle},
	}
	for _, tc := range cases {
		got, err := ClassifySkillSource(tc.in)
		if err != nil {
			t.Errorf("ClassifySkillSource(%q): unexpected err %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ClassifySkillSource(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseSkillHandle(t *testing.T) {
	cases := []struct {
		in                     string
		handle, skill, version string
		err                    bool
	}{
		{"vadim/research", "vadim", "research", "", false},
		{"vadim/research@v1.0.0", "vadim", "research", "v1.0.0", false},
		{"vadim/research@1.0.0-rc.1", "vadim", "research", "1.0.0-rc.1", false},
		{"no-slash", "", "", "", true},
		{"vadim/", "", "", "", true},
		{"/research", "", "", "", true},
	}
	for _, tc := range cases {
		h, s, v, err := parseSkillHandle(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseSkillHandle(%q) should error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSkillHandle(%q): %v", tc.in, err)
			continue
		}
		if h != tc.handle || s != tc.skill || v != tc.version {
			t.Errorf("parseSkillHandle(%q) = (%q,%q,%q), want (%q,%q,%q)", tc.in, h, s, v, tc.handle, tc.skill, tc.version)
		}
	}
}

func TestResolveSkillSource_Local(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	if err := os.WriteFile(path, []byte("---\nname: foo\n---\nbody"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := ResolveSkillSource(path, nil)
	if err != nil {
		t.Fatalf("resolve local: %v", err)
	}
	if string(res.Bytes) != "---\nname: foo\n---\nbody" {
		t.Errorf("local resolve lost bytes: %q", res.Bytes)
	}
	if !strings.HasPrefix(res.SourceURL, "file://") {
		t.Errorf("local source URL: %q", res.SourceURL)
	}
	if res.Revision != "local" {
		t.Errorf("local revision: %q", res.Revision)
	}
}

func TestResolveSkillSource_HTTPS(t *testing.T) {
	body := "---\nname: https-test\n---\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	res, err := ResolveSkillSource(srv.URL+"/skill.md", nil)
	if err != nil {
		t.Fatalf("resolve https: %v", err)
	}
	if string(res.Bytes) != body {
		t.Errorf("https resolve body lost: %q", res.Bytes)
	}
	if res.Revision != "https" {
		t.Errorf("https revision: %q", res.Revision)
	}
	if res.SourceFilename != "skill.md" {
		t.Errorf("https filename: %q", res.SourceFilename)
	}
}

func TestResolveSkillSource_HTTPS404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := ResolveSkillSource(srv.URL+"/missing.md", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("want HTTP 404 error, got %v", err)
	}
}

func TestResolveSkillSource_HandleRequiresIndex(t *testing.T) {
	_, err := ResolveSkillSource("vadim/research", nil)
	if err == nil || !strings.Contains(err.Error(), "registry index") {
		t.Errorf("want registry-index error, got %v", err)
	}
}

// fakeIndex implements SkillIndexResolver for the handle-source test.
type fakeIndex struct {
	gitURL, rev string
	err         error
}

func (f *fakeIndex) ResolveHandle(handle, skill, version string) (string, string, error) {
	return f.gitURL, f.rev, f.err
}

func TestResolveSkillSource_HandleViaIndex(t *testing.T) {
	idx := &fakeIndex{err: fmt.Errorf("simulated lookup failure")}
	_, err := ResolveSkillSource("vadim/research", idx)
	if err == nil || !strings.Contains(err.Error(), "simulated lookup failure") {
		t.Errorf("index error not surfaced: %v", err)
	}
}

func TestClassifySkillSource_Empty(t *testing.T) {
	_, err := ClassifySkillSource("")
	if err == nil {
		t.Errorf("empty source should error")
	}
}
