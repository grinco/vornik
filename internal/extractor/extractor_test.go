// Tests for the extractor Registry — the contract every plugin sees
// when it asks "which extractor claims this MIME type?". These are
// the assertions a future MIME-mapping refactor MUST keep green.
package extractor

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// stubExtractor lets the registry tests exercise For() / Register()
// without standing up a real EPUB / PDF parser.
type stubExtractor struct{ name string }

func (s *stubExtractor) Name() string                                    { return s.name }
func (s *stubExtractor) Version() string                                 { return "test" }
func (s *stubExtractor) Extract(context.Context, Source) (Result, error) { return Result{}, nil }

func TestRegistry_ExactMatchWinsOverWildcard(t *testing.T) {
	r := NewRegistry()
	specific := &stubExtractor{name: "audio-mp3"}
	general := &stubExtractor{name: "audio-any"}
	if err := r.Register(general, "audio/*"); err != nil {
		t.Fatalf("register wildcard: %v", err)
	}
	if err := r.Register(specific, "audio/mpeg"); err != nil {
		t.Fatalf("register exact: %v", err)
	}

	got, err := r.For("audio/mpeg")
	if err != nil {
		t.Fatalf("For(audio/mpeg): %v", err)
	}
	if got != specific {
		t.Errorf("exact match must win over wildcard; got %q", got.Name())
	}

	got, err = r.For("audio/wav")
	if err != nil {
		t.Fatalf("For(audio/wav): %v", err)
	}
	if got != general {
		t.Errorf("wildcard must match non-exact subtype; got %q", got.Name())
	}
}

func TestRegistry_CanonicalisesMime(t *testing.T) {
	// Charset parameters and case differences must NOT defeat the
	// match — the email channel hands us "Application/Epub+Zip;
	// charset=binary" verbatim from Content-Type headers.
	r := NewRegistry()
	ext := &stubExtractor{name: "epub"}
	if err := r.Register(ext, "application/epub+zip"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, mt := range []string{
		"application/epub+zip",
		"Application/Epub+Zip",
		"application/epub+zip; charset=utf-8",
		"  application/epub+zip  ",
	} {
		got, err := r.For(mt)
		if err != nil {
			t.Errorf("For(%q): %v", mt, err)
			continue
		}
		if got != ext {
			t.Errorf("For(%q): expected canonical match", mt)
		}
	}
}

func TestRegistry_NoMatch_ReturnsErrNoExtractor(t *testing.T) {
	r := NewRegistry()
	_, err := r.For("application/x-nothing")
	if !errors.Is(err, ErrNoExtractor) {
		t.Errorf("expected ErrNoExtractor; got %v", err)
	}
}

func TestRegistry_InvalidInputs(t *testing.T) {
	r := NewRegistry()
	ext := &stubExtractor{name: "test"}
	for _, mt := range []string{
		"",
		"   ",
		"no-slash",
		"/missing-type",
		"audio/**",    // double wildcard
		"audio/*/bar", // path beyond wildcard
	} {
		if err := r.Register(ext, mt); err == nil {
			t.Errorf("Register(%q) should have failed", mt)
		}
	}
	if err := r.Register(nil, "application/pdf"); err == nil {
		t.Error("Register(nil, ...) should have failed")
	}
	if err := r.Register(ext); err == nil {
		t.Error("Register with no MIME types should have failed")
	}
}

func TestRegistry_LastCallWins(t *testing.T) {
	// Operators override bundled defaults via configs/extractors.yaml.
	// Last-call-wins is the contract.
	r := NewRegistry()
	a := &stubExtractor{name: "a"}
	b := &stubExtractor{name: "b"}
	_ = r.Register(a, "application/pdf")
	_ = r.Register(b, "application/pdf")
	got, _ := r.For("application/pdf")
	if got != b {
		t.Errorf("expected last-registered extractor (b); got %q", got.Name())
	}
}

func TestRegistry_SupportedMimeTypes(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&stubExtractor{name: "pdf"}, "application/pdf")
	_ = r.Register(&stubExtractor{name: "epub"}, "application/epub+zip")
	_ = r.Register(&stubExtractor{name: "audio"}, "audio/*")
	want := []string{"application/epub+zip", "application/pdf", "audio/*"}
	if got := r.SupportedMimeTypes(); !reflect.DeepEqual(got, want) {
		t.Errorf("SupportedMimeTypes = %v; want %v", got, want)
	}
}

func TestMimeFromFilename(t *testing.T) {
	// Lock the small fallback map — emails arrive with no
	// Content-Type sometimes and the filename is all we have. The
	// map deliberately covers only the format set the registry
	// actually supports today.
	cases := map[string]string{
		"book.epub":   "application/epub+zip",
		"BOOK.EPUB":   "application/epub+zip",
		"paper.pdf":   "application/pdf",
		"notes.txt":   "text/plain",
		"README.md":   "text/markdown",
		"page.html":   "text/html",
		"brief.docx":  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"voice.mp3":   "audio/mpeg",
		"clip.mp4":    "video/mp4",
		"photo.jpg":   "image/jpeg",
		"unknown.xyz": "",
		"noext":       "",
	}
	for in, want := range cases {
		if got := MimeFromFilename(in); got != want {
			t.Errorf("MimeFromFilename(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestResult_TotalTextBytes(t *testing.T) {
	r := Result{
		Sections: []Section{
			{Content: "abc"},   // 3
			{Content: ""},      // 0
			{Content: "12345"}, // 5
		},
	}
	if got, want := r.TotalTextBytes(), int64(8); got != want {
		t.Errorf("TotalTextBytes = %d; want %d", got, want)
	}
}
