package dispatcher

import (
	"context"
	"io"
	"strings"
	"testing"
)

// stubSenderRecording captures every SendDocument call for the
// render_document tests below.
type stubSenderRecording struct {
	paths    []string
	captions []string
	err      error
}

// SendArtifactFile implements the channel-agnostic dispatcher.FileSender. It
// records the delivered filename + caption (render_document now passes the
// basename, having opened each produced file itself).
func (s *stubSenderRecording) SendArtifactFile(_ context.Context, name string, _ io.Reader, caption string) error {
	s.paths = append(s.paths, name)
	s.captions = append(s.captions, caption)
	return s.err
}

func TestRenderDocument_RejectsMissingContent(t *testing.T) {
	te := &ToolExecutor{}
	res := te.renderDocument(context.Background(), `{"name":"cv"}`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "content is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestRenderDocument_RejectsMissingName(t *testing.T) {
	te := &ToolExecutor{}
	res := te.renderDocument(context.Background(), `{"content":"# Hi"}`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "name is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestRenderDocument_RejectsBadJSON(t *testing.T) {
	te := &ToolExecutor{}
	res := te.renderDocument(context.Background(), `not json`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestRenderDocument_RejectsMissingFileSender(t *testing.T) {
	te := &ToolExecutor{}
	res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"cv"}`, nil)
	if !strings.Contains(res.Content, "file sending is not configured") {
		t.Errorf("got %q", res.Content)
	}
}

// TestRenderDocument_DeliversMdOnly exercises the happy path with
// formats=["md"]: no external tool needed (md is the source),
// SendDocument is called once with the .md path.
func TestRenderDocument_DeliversMdOnly(t *testing.T) {
	te := &ToolExecutor{}
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(),
		`{"content":"# Hello","name":"hello","formats":["md"]}`, fs)
	if !strings.Contains(res.Content, "Delivered: hello.md") {
		t.Errorf("expected delivery confirmation; got %q", res.Content)
	}
	if len(fs.paths) != 1 {
		t.Fatalf("expected 1 SendDocument call; got %d", len(fs.paths))
	}
	if !strings.HasSuffix(fs.paths[0], "hello.md") {
		t.Errorf("path should end in hello.md; got %q", fs.paths[0])
	}
}

// TestRenderDocument_StripsExtensionFromName pins the contract
// that the LLM can pass either "cv" or "cv.md" — both produce
// hello.md (no double-extension).
func TestRenderDocument_StripsExtensionFromName(t *testing.T) {
	te := &ToolExecutor{}
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(),
		`{"content":"# Hi","name":"cv.md","formats":["md"]}`, fs)
	if !strings.Contains(res.Content, "Delivered: cv.md") {
		t.Errorf("got %q", res.Content)
	}
	if len(fs.paths) != 1 || !strings.HasSuffix(fs.paths[0], "cv.md") {
		t.Errorf("expected single cv.md delivery; got %v", fs.paths)
	}
}

// TestRenderDocument_RejectsInvalidName surfaces safepath rejection
// for paths that try to escape the tmpdir or contain control bytes.
func TestRenderDocument_RejectsInvalidName(t *testing.T) {
	te := &ToolExecutor{}
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(),
		`{"content":"# Hi","name":"../escape","formats":["md"]}`, fs)
	if !strings.Contains(res.Content, "invalid name") {
		t.Errorf("expected invalid name rejection; got %q", res.Content)
	}
}

// TestRenderDocument_NameOnlyExtensionRejected pins the post-trim
// branch: a name like ".md" trims to "" → reuses the same
// "name is required" error so callers don't get a confusing
// "what got rendered" reply.
func TestRenderDocument_NameOnlyExtensionRejected(t *testing.T) {
	te := &ToolExecutor{}
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(),
		`{"content":"# Hi","name":".md","formats":["md"]}`, fs)
	if !strings.Contains(res.Content, "name is required") {
		t.Errorf("expected name-required; got %q", res.Content)
	}
}

// TestRenderDocument_SendDocumentErrorReportedInline pins the
// "delivered some, errored some" branch.
func TestRenderDocument_SendDocumentErrorReportedInline(t *testing.T) {
	te := &ToolExecutor{}
	// First call succeeds, second errors — but the stub returns the
	// same error for every send. We pass just md so a single send
	// occurs; with one delivery erroring → the "nothing delivered" path.
	fs := &stubSenderRecording{err: errSendFailed}
	res := te.renderDocument(context.Background(),
		`{"content":"# Hi","name":"cv","formats":["md"]}`, fs)
	if !strings.Contains(res.Content, "nothing delivered") {
		t.Errorf("expected nothing delivered branch; got %q", res.Content)
	}
}

// TestRenderDocument_HtmlFallbackToInProcess: no pandoc / podman →
// the third-tier in-process renderer kicks in, producing a wrapped
// <pre> HTML file. Verified via the SendDocument capture.
func TestRenderDocument_HtmlFallbackToInProcess(t *testing.T) {
	t.Setenv("PATH", "") // forces LookPath for pandoc + podman to fail
	te := &ToolExecutor{}
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(),
		`{"content":"# Hi","name":"cv","formats":["html"]}`, fs)
	if !strings.Contains(res.Content, "Delivered: cv.html") {
		t.Errorf("expected delivery; got %q", res.Content)
	}
}

// TestRenderDocument_PDFFailsWithoutToolchain: no pandoc / podman →
// renderMarkdownToPDF returns errPodmanUnavailable; the dispatcher
// reports the failure to the caller.
func TestRenderDocument_PDFFailsWithoutToolchain(t *testing.T) {
	t.Setenv("PATH", "")
	te := &ToolExecutor{}
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(),
		`{"content":"# Hi","name":"cv","formats":["pdf"]}`, fs)
	// every requested format failed → that branch surfaces inside
	// the result.
	if !strings.Contains(res.Content, "every requested format failed") {
		t.Errorf("expected every-failed branch; got %q", res.Content)
	}
}

func TestIsPodmanUnavailable(t *testing.T) {
	if !isPodmanUnavailable(errPodmanUnavailable) {
		t.Fatal("recognised sentinel must return true")
	}
	if isPodmanUnavailable(nil) {
		t.Fatal("nil should not be classified as unavailable")
	}
	if isPodmanUnavailable(errSendFailed) {
		t.Fatal("unrelated error should not be classified as unavailable")
	}
}

func TestRunPandocViaPodman_PodmanMissing(t *testing.T) {
	t.Setenv("PATH", "")
	err := runPandocViaPodman(context.Background(), "/tmp/foo.md", "/tmp/foo.html", nil)
	if !isPodmanUnavailable(err) {
		t.Fatalf("expected errPodmanUnavailable; got %v", err)
	}
}

// errSendFailed is a sentinel for the stub sender.
var errSendFailed = stubSenderError("send failed")

type stubSenderError string

func (e stubSenderError) Error() string { return string(e) }
