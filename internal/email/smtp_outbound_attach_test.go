package email

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// TestSendFile_DeliversThreadedAttachment is the Phase-2 end-to-end for the
// email file-delivery seam: after a session is established by inbound mail,
// Channel.SendFile (used by the email FileSender adapter for send_artifact)
// posts a multipart/mixed email threaded onto that conversation, carrying the
// file as a base64 attachment with the caption as the body.
func TestSendFile_DeliversThreadedAttachment(t *testing.T) {
	cfg := validConfig()
	smtp := &fakeSMTP{}
	imap := newFakeIMAP([]RawMessage{
		{UID: "100", Body: sampleEmail("root-id", "alice@ext.test", "Quarterly", "please send the report")},
	})
	cfg.IMAPClient = imap
	cfg.SMTPSender = smtp
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()
	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })

	id, err := ch.SendFile(context.Background(), "root-id", "report.pdf", []byte("%PDF-1.4 fake bytes"), "Here is the report.")
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if id == "" {
		t.Error("SendFile returned empty Message-ID")
	}
	reqs := smtp.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("smtp got %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if len(got.To) != 1 || got.To[0] != "alice@ext.test" {
		t.Errorf("To = %v, want [alice@ext.test] (threaded to the operator)", got.To)
	}
	payload := string(got.Payload)
	for _, want := range []string{
		"In-Reply-To: <root-id>",
		"multipart/mixed",
		`filename="report.pdf"`,
		"Content-Transfer-Encoding: base64",
		"Here is the report.", // caption rendered as the text body part
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %q:\n%s", want, payload)
		}
	}
	cancel()
	<-errCh
}

// getArtifactRepo is a scriptable ArtifactRepository: Get returns the row
// registered for an id (or an error). Embeds fakeArtifactRepo for the rest of
// the interface.
type getArtifactRepo struct {
	*fakeArtifactRepo
	rows   map[string]*persistence.Artifact
	getErr error
}

func (r *getArtifactRepo) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.rows[id], nil
}

func strptr(s string) *string { return &s }

// TestBuildOutboundAttachments_ReadsAndMaps covers the happy path + the
// best-effort skips (no ArtifactID, lookup miss, over-cap) for the
// Channel.Send attachment mapping.
func TestBuildOutboundAttachments_ReadsAndMaps(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reportPath := writeFile("report.md", []byte("# Report\n"))
	bigPath := writeFile("big.bin", make([]byte, 5000))

	repo := &getArtifactRepo{
		fakeArtifactRepo: &fakeArtifactRepo{},
		rows: map[string]*persistence.Artifact{
			"art-report": {ID: "art-report", Name: "report.md", StoragePath: reportPath, MimeType: strptr("text/markdown")},
			"art-big":    {ID: "art-big", Name: "big.bin", StoragePath: bigPath},
		},
	}
	c := &Channel{
		attachmentDeps: persistAttachmentDeps{Repo: repo},
		attachmentCap:  4096, // big.bin (5000) exceeds this
	}

	got := c.buildOutboundAttachments(context.Background(), []conversation.Attachment{
		{ArtifactID: "art-report", Name: "report.md", MimeType: "text/markdown"},
		{Name: "no-id.txt"},         // no ArtifactID → skipped
		{ArtifactID: "art-missing"}, // lookup miss (nil row) → skipped
		{ArtifactID: "art-big"},     // over cap → skipped
	})

	if len(got) != 1 {
		t.Fatalf("want 1 mapped attachment (others skipped), got %d", len(got))
	}
	if got[0].Filename != "report.md" || got[0].ContentType != "text/markdown" {
		t.Fatalf("attachment headers wrong: %+v", got[0])
	}
	if string(got[0].Content) != "# Report\n" {
		t.Fatalf("attachment bytes wrong: %q", got[0].Content)
	}
}

// TestBuildOutboundAttachments_Degrades covers the nil/empty short-circuits
// and the read-error skip — the reply must still be sendable (nil result).
func TestBuildOutboundAttachments_Degrades(t *testing.T) {
	// nil repo → nil.
	c := &Channel{attachmentDeps: persistAttachmentDeps{Repo: nil}}
	if got := c.buildOutboundAttachments(context.Background(), []conversation.Attachment{{ArtifactID: "x"}}); got != nil {
		t.Fatalf("nil repo must yield nil, got %v", got)
	}
	// empty attachments → nil.
	c2 := &Channel{attachmentDeps: persistAttachmentDeps{Repo: &getArtifactRepo{fakeArtifactRepo: &fakeArtifactRepo{}, rows: map[string]*persistence.Artifact{}}}}
	if got := c2.buildOutboundAttachments(context.Background(), nil); got != nil {
		t.Fatalf("empty attachments must yield nil, got %v", got)
	}
	// repo error → skipped (nil), reply still sendable.
	c3 := &Channel{attachmentDeps: persistAttachmentDeps{Repo: &getArtifactRepo{fakeArtifactRepo: &fakeArtifactRepo{}, getErr: errors.New("db down")}}}
	if got := c3.buildOutboundAttachments(context.Background(), []conversation.Attachment{{ArtifactID: "x"}}); got != nil {
		t.Fatalf("repo error must skip (nil), got %v", got)
	}
	// storage path missing on disk → read error → skipped.
	repo := &getArtifactRepo{fakeArtifactRepo: &fakeArtifactRepo{}, rows: map[string]*persistence.Artifact{
		"art-gone": {ID: "art-gone", Name: "gone.txt", StoragePath: "/nonexistent/gone.txt"},
	}}
	c4 := &Channel{attachmentDeps: persistAttachmentDeps{Repo: repo}}
	if got := c4.buildOutboundAttachments(context.Background(), []conversation.Attachment{{ArtifactID: "art-gone"}}); got != nil {
		t.Fatalf("unreadable file must skip (nil), got %v", got)
	}
}
