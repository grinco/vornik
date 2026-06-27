package email

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeArtifactRepo is a minimal in-memory ArtifactRepository fake
// for attachment-persistence tests. We only need Create — the email
// channel doesn't use the other methods on the ingestion path.
type fakeArtifactRepo struct {
	created   []*persistence.Artifact
	createErr error
}

func (f *fakeArtifactRepo) Create(_ context.Context, a *persistence.Artifact) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, a)
	return nil
}

func (f *fakeArtifactRepo) Get(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (f *fakeArtifactRepo) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (f *fakeArtifactRepo) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, nil
}
func (f *fakeArtifactRepo) Delete(_ context.Context, _ string) error              { return nil }
func (f *fakeArtifactRepo) DeleteByExecutionID(_ context.Context, _ string) error { return nil }
func (f *fakeArtifactRepo) UpdateTaskID(_ context.Context, _, _ string) error     { return nil }

// multipartMixedWithAttachment builds a multipart/mixed payload
// with one text/plain body part and one named application/pdf
// attachment. Used to exercise the attachment walk.
func multipartMixedWithAttachment(boundary, textBody, attachName, attachContent string) []byte {
	// base64-encode the attachment content
	encoded := []byte(attachContent)
	// quick base64 manual encode using stdlib through the wire would
	// require an import — we use base64 via decodeTransfer in tests.
	// Instead we simply emit 8bit so we exercise the same code path.
	var b strings.Builder
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(textBody + "\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString(fmt.Sprintf(`Content-Type: application/pdf; name="%s"`, attachName) + "\r\n")
	b.WriteString(fmt.Sprintf(`Content-Disposition: attachment; filename="%s"`, attachName) + "\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.Write(encoded)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return []byte(b.String())
}

// ---- parseMessageBody: text + attachments ----

func TestParseMessageBody_ExtractsAttachmentAlongsideText(t *testing.T) {
	body := multipartMixedWithAttachment("MIXED", "Please review the PDF.", "report.pdf", "PDF-PAYLOAD-BYTES")
	res := parseMessageBody(`multipart/mixed; boundary="MIXED"`, "", body)
	if !strings.Contains(res.Text, "Please review the PDF.") {
		t.Errorf("Text = %q, want to contain plaintext part", res.Text)
	}
	if len(res.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(res.Attachments))
	}
	att := res.Attachments[0]
	if att.Filename != "report.pdf" {
		t.Errorf("Filename = %q, want report.pdf", att.Filename)
	}
	if att.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf", att.ContentType)
	}
	if string(att.Content) != "PDF-PAYLOAD-BYTES" {
		t.Errorf("Content = %q, want PDF-PAYLOAD-BYTES", string(att.Content))
	}
	if att.SizeBytes != int64(len("PDF-PAYLOAD-BYTES")) {
		t.Errorf("SizeBytes = %d, want %d", att.SizeBytes, len("PDF-PAYLOAD-BYTES"))
	}
}

func TestParseMessageBody_NoAttachmentsOnPlainText(t *testing.T) {
	res := parseMessageBody("text/plain", "", []byte("hi"))
	if res.Text != "hi" {
		t.Errorf("Text = %q", res.Text)
	}
	if len(res.Attachments) != 0 {
		t.Errorf("Attachments = %v, want none", res.Attachments)
	}
}

func TestParseMessageBody_AttachmentWithoutFilenameUsesDefault(t *testing.T) {
	// No filename param on the attachment part — extractor must
	// synthesise a stable default rather than panicking.
	body := []byte(
		"--B\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"body\r\n" +
			"--B\r\n" +
			"Content-Type: application/octet-stream\r\n" +
			"Content-Disposition: attachment\r\n" +
			"\r\n" +
			"BINARY\r\n" +
			"--B--\r\n",
	)
	res := parseMessageBody(`multipart/mixed; boundary="B"`, "", body)
	if len(res.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(res.Attachments))
	}
	if res.Attachments[0].Filename == "" {
		t.Errorf("Filename should be synthesised, got empty")
	}
}

func TestParseMessageBody_NestedMultipartCollectsAttachments(t *testing.T) {
	// multipart/mixed wrapping multipart/alternative + attachment.
	body := []byte(
		"--OUTER\r\n" +
			`Content-Type: multipart/alternative; boundary="INNER"` + "\r\n\r\n" +
			"--INNER\r\n" +
			"Content-Type: text/html\r\n\r\n" +
			"<p>html body</p>\r\n" +
			"--INNER\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"plain body\r\n" +
			"--INNER--\r\n" +
			"--OUTER\r\n" +
			`Content-Type: image/png; name="logo.png"` + "\r\n" +
			`Content-Disposition: attachment; filename="logo.png"` + "\r\n" +
			"Content-Transfer-Encoding: 8bit\r\n" +
			"\r\n" +
			"PNG\r\n" +
			"--OUTER--\r\n",
	)
	res := parseMessageBody(`multipart/mixed; boundary="OUTER"`, "", body)
	if !strings.Contains(res.Text, "plain body") {
		t.Errorf("Text = %q, want plain body", res.Text)
	}
	if len(res.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(res.Attachments))
	}
	if res.Attachments[0].Filename != "logo.png" {
		t.Errorf("Filename = %q", res.Attachments[0].Filename)
	}
}

func TestParseMessageBody_InlineImagesAreNotTreatedAsAttachments(t *testing.T) {
	// Content-Disposition: inline + cid:... reference. The slice-2
	// rule: inline images are skipped (they're part of the HTML body,
	// not user-uploaded artifacts). Only `attachment` disposition or
	// non-text parts without `inline` disposition land in Attachments.
	body := []byte(
		"--B\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"hi\r\n" +
			"--B\r\n" +
			`Content-Type: image/png; name="inline-logo.png"` + "\r\n" +
			`Content-Disposition: inline; filename="inline-logo.png"` + "\r\n" +
			"Content-ID: <logo@cid>\r\n" +
			"\r\n" +
			"PNG\r\n" +
			"--B--\r\n",
	)
	res := parseMessageBody(`multipart/mixed; boundary="B"`, "", body)
	if len(res.Attachments) != 0 {
		t.Errorf("inline image must not be treated as attachment, got %d", len(res.Attachments))
	}
}

// ---- PersistAttachments: artifact repo + filesystem ----

func TestPersistAttachments_WritesBytesAndRecordsArtifact(t *testing.T) {
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	atts := []ParsedAttachment{
		{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("PDFBYTES"), SizeBytes: 8},
	}
	persisted, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  dir,
		ProjectID: "proj-1",
		MessageID: "msg-1",
	}, atts)
	if err != nil {
		t.Fatalf("PersistAttachments: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted = %d, want 1", len(persisted))
	}
	if persisted[0].StoragePath == "" {
		t.Fatal("persisted[0].StoragePath empty")
	}
	gotBytes, err := os.ReadFile(persisted[0].StoragePath)
	if err != nil {
		t.Fatalf("read storage: %v", err)
	}
	if string(gotBytes) != "PDFBYTES" {
		t.Errorf("disk bytes = %q, want PDFBYTES", string(gotBytes))
	}
	if len(repo.created) != 1 {
		t.Fatalf("artifact repo got %d Create calls, want 1", len(repo.created))
	}
	art := repo.created[0]
	if art.Name != "report.pdf" {
		t.Errorf("Artifact.Name = %q", art.Name)
	}
	if art.ProjectID != "proj-1" {
		t.Errorf("Artifact.ProjectID = %q", art.ProjectID)
	}
	if art.ArtifactClass != persistence.ArtifactClassInput {
		t.Errorf("ArtifactClass = %q, want INPUT", art.ArtifactClass)
	}
	if art.MimeType == nil || *art.MimeType != "application/pdf" {
		t.Errorf("MimeType = %v, want application/pdf", art.MimeType)
	}
	if art.SizeBytes == nil || *art.SizeBytes != 8 {
		t.Errorf("SizeBytes = %v, want 8", art.SizeBytes)
	}
}

func TestPersistAttachments_NilRepoReturnsNoOp(t *testing.T) {
	dir := t.TempDir()
	atts := []ParsedAttachment{
		{Filename: "x.txt", ContentType: "text/plain", Content: []byte("data"), SizeBytes: 4},
	}
	// nil Repo + empty StoreDir: tolerate gracefully — channel's
	// "no artifact wiring" mode just drops attachments with a warning.
	persisted, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      nil,
		StoreDir:  dir,
		ProjectID: "p",
		MessageID: "m",
	}, atts)
	if err != nil {
		t.Fatalf("PersistAttachments: %v", err)
	}
	if len(persisted) != 0 {
		t.Errorf("persisted = %d, want 0 when repo is nil", len(persisted))
	}
}

func TestPersistAttachments_EmptyStoreDirReturnsNoOp(t *testing.T) {
	repo := &fakeArtifactRepo{}
	atts := []ParsedAttachment{
		{Filename: "x.txt", ContentType: "text/plain", Content: []byte("data"), SizeBytes: 4},
	}
	persisted, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  "", // no dir means "drop"
		ProjectID: "p",
		MessageID: "m",
	}, atts)
	if err != nil {
		t.Fatalf("PersistAttachments: %v", err)
	}
	if len(persisted) != 0 {
		t.Errorf("persisted = %d, want 0 with empty StoreDir", len(persisted))
	}
	if len(repo.created) != 0 {
		t.Errorf("repo got %d Create calls, want 0", len(repo.created))
	}
}

func TestPersistAttachments_PropagatesRepoError(t *testing.T) {
	dir := t.TempDir()
	repo := &fakeArtifactRepo{createErr: errors.New("DB fail")}
	atts := []ParsedAttachment{
		{Filename: "x.txt", ContentType: "text/plain", Content: []byte("ok"), SizeBytes: 2},
	}
	_, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  dir,
		ProjectID: "p",
		MessageID: "m",
	}, atts)
	if err == nil || !strings.Contains(err.Error(), "DB fail") {
		t.Errorf("err = %v, want to wrap repo error", err)
	}
}

func TestPersistAttachments_SafePathSanitisesFilename(t *testing.T) {
	// A hostile filename ("../escape.txt") must not write outside
	// the store dir.
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	atts := []ParsedAttachment{
		{Filename: "../escape.txt", ContentType: "text/plain", Content: []byte("safe"), SizeBytes: 4},
	}
	persisted, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  dir,
		ProjectID: "proj-1",
		MessageID: "msg-1",
	}, atts)
	if err != nil {
		t.Fatalf("PersistAttachments: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted = %d", len(persisted))
	}
	// Resolve symlinks on both sides — macOS /var → /private/var means a
	// raw HasPrefix check fails even when the path is well inside the dir.
	resolvedDir, _ := filepath.EvalSymlinks(dir)
	resolvedPath, _ := filepath.EvalSymlinks(persisted[0].StoragePath)
	if !strings.HasPrefix(resolvedPath, resolvedDir) {
		t.Errorf("storage path %q escaped %q", resolvedPath, resolvedDir)
	}
}

func TestPersistAttachments_DuplicateFilenamesDoNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	atts := []ParsedAttachment{
		{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("FIRST"), SizeBytes: 5},
		{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("SECOND"), SizeBytes: 6},
	}
	persisted, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  dir,
		ProjectID: "proj-1",
		MessageID: "msg-1",
	}, atts)
	if err != nil {
		t.Fatalf("PersistAttachments: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted = %d, want 2", len(persisted))
	}
	if persisted[0].StoragePath == persisted[1].StoragePath {
		t.Fatalf("duplicate filenames reused one storage path: %q", persisted[0].StoragePath)
	}
	first, err := os.ReadFile(persisted[0].StoragePath)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	second, err := os.ReadFile(persisted[1].StoragePath)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if string(first) != "FIRST" || string(second) != "SECOND" {
		t.Fatalf("attachment bytes overwritten: first=%q second=%q", string(first), string(second))
	}
	if repo.created[0].Name != "report.pdf" || repo.created[1].Name != "report-2.pdf" {
		t.Fatalf("artifact names = %q, %q; want report.pdf, report-2.pdf",
			repo.created[0].Name, repo.created[1].Name)
	}
}

// TestPersistAttachments_PathLayoutDoesNotDoubleNest pins the
// on-disk shape: <StoreDir>/<safe-message-id>/<filename>. The
// service-container layer is responsible for project + email-inbound
// segments before it hands the dir in (or passes the operator-set
// path verbatim) — re-appending them here produced the double-nested
// .../email-inbound/<projectID>/<projectID>/email-inbound/<msg-id>/...
// shape operators saw on artifact paths through 2026-05-21. Anchor
// the contract so a future refactor doesn't reintroduce the bug.
func TestPersistAttachments_PathLayoutDoesNotDoubleNest(t *testing.T) {
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	atts := []ParsedAttachment{
		{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("X"), SizeBytes: 1},
	}
	persisted, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  dir,
		ProjectID: "assistant",
		MessageID: "abc123@mail.example",
	}, atts)
	if err != nil {
		t.Fatalf("PersistAttachments: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted = %d, want 1", len(persisted))
	}
	resolvedDir, _ := filepath.EvalSymlinks(dir)
	resolvedPath, _ := filepath.EvalSymlinks(persisted[0].StoragePath)
	rel, err := filepath.Rel(resolvedDir, resolvedPath)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	// Want: <safe-msg-id>/report.pdf — exactly two segments under StoreDir.
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 {
		t.Fatalf("path layout = %q; want <msg-id>/<file>, got %d segments", rel, len(parts))
	}
	if parts[0] == "assistant" || parts[0] == "email-inbound" {
		t.Errorf("path %q re-nests projectID or email-inbound under StoreDir", rel)
	}
	if parts[1] != "report.pdf" {
		t.Errorf("filename segment = %q, want report.pdf", parts[1])
	}
	// The artifact metadata still carries the project id even though
	// the path no longer encodes it — that's the resolver's job.
	if repo.created[0].ProjectID != "assistant" {
		t.Errorf("Artifact.ProjectID = %q, want assistant", repo.created[0].ProjectID)
	}
}

func TestPersistAttachments_RejectsProjectIDPathTraversal(t *testing.T) {
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	atts := []ParsedAttachment{
		{Filename: "x.txt", ContentType: "text/plain", Content: []byte("data"), SizeBytes: 4},
	}
	_, err := PersistAttachments(context.Background(), persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  dir,
		ProjectID: "../escape",
		MessageID: "msg-1",
	}, atts)
	if err == nil || !strings.Contains(err.Error(), "invalid attachment project id") {
		t.Fatalf("err = %v, want invalid project id", err)
	}
	if len(repo.created) != 0 {
		t.Fatalf("repo Create called despite invalid project id")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "escape")); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected escaped path created: %v", statErr)
	}
}

// ---- caps and limits ----

func TestEnforceAttachmentCap_RefusesOverCap(t *testing.T) {
	atts := []ParsedAttachment{
		{Filename: "a.bin", SizeBytes: 10 * 1024 * 1024}, // 10 MiB
		{Filename: "b.bin", SizeBytes: 20 * 1024 * 1024}, // 20 MiB
	}
	if err := enforceAttachmentCap(atts, 25*1024*1024); err == nil {
		t.Error("expected cap violation error for 30 MiB > 25 MiB cap")
	}
}

func TestEnforceAttachmentCap_AllowsUnderCap(t *testing.T) {
	atts := []ParsedAttachment{
		{Filename: "a.bin", SizeBytes: 5 * 1024 * 1024},
	}
	if err := enforceAttachmentCap(atts, 25*1024*1024); err != nil {
		t.Errorf("under-cap rejected: %v", err)
	}
}

func TestEnforceAttachmentCap_ZeroCapMeansNoLimit(t *testing.T) {
	atts := []ParsedAttachment{
		{Filename: "a.bin", SizeBytes: 100 * 1024 * 1024 * 1024}, // 100 GiB
	}
	if err := enforceAttachmentCap(atts, 0); err != nil {
		t.Errorf("zero cap should mean unlimited, got %v", err)
	}
}

// ---- internal helper coverage ----

func TestSafeMessageDir_EmptyFallsBackToUnknown(t *testing.T) {
	if got := safeMessageDir(""); got != "unknown" {
		t.Errorf("safeMessageDir(empty) = %q, want unknown", got)
	}
}

func TestSafeMessageDir_PreservesAlphaNum(t *testing.T) {
	got := safeMessageDir("abc-123.xyz@host.com")
	if !strings.Contains(got, "abc-123.xyz") || !strings.Contains(got, "_at_host.com") {
		t.Errorf("safeMessageDir lost characters: %q", got)
	}
}

func TestSafeMessageDir_ReplacesUnsafeChars(t *testing.T) {
	got := safeMessageDir("a/b\\c<d>e")
	if strings.ContainsAny(got, "/\\<>") {
		t.Errorf("safeMessageDir kept unsafe char: %q", got)
	}
}

func TestSafeMessageDir_TruncatesLongInput(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := safeMessageDir(long)
	if len(got) > 120 {
		t.Errorf("safeMessageDir didn't truncate: len = %d", len(got))
	}
}

func TestSafeAttachmentFilename_EmptyFallsBack(t *testing.T) {
	if got := safeAttachmentFilename("", 3); got != "attachment-3.bin" {
		t.Errorf("safeAttachmentFilename(empty) = %q", got)
	}
}

func TestSafeAttachmentFilename_DotDotFallsBack(t *testing.T) {
	if got := safeAttachmentFilename("..", 1); got != "attachment-1.bin" {
		t.Errorf(".. should fall back, got %q", got)
	}
	if got := safeAttachmentFilename(".", 1); got != "attachment-1.bin" {
		t.Errorf(". should fall back, got %q", got)
	}
}

func TestSafeAttachmentFilename_StripsControlChars(t *testing.T) {
	in := "evil\x00\x01file.txt"
	got := safeAttachmentFilename(in, 0)
	for _, c := range got {
		if c < 0x20 {
			t.Errorf("safeAttachmentFilename kept control char: %q", got)
		}
	}
}

func TestSynthesiseAnonymousFilename_UsesSubtype(t *testing.T) {
	got := synthesiseAnonymousFilename(2, "application/pdf")
	if !strings.Contains(got, "attachment-2") || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("synthesiseAnonymousFilename = %q", got)
	}
}

func TestSynthesiseAnonymousFilename_NoSubtypeFallsBackToBin(t *testing.T) {
	got := synthesiseAnonymousFilename(0, "")
	if !strings.HasSuffix(got, ".bin") {
		t.Errorf("synthesiseAnonymousFilename = %q, expected .bin fallback", got)
	}
}

func TestParseContentDisposition_Empty(t *testing.T) {
	disp, params := parseContentDisposition("")
	if disp != "" {
		t.Errorf("disposition = %q, want empty", disp)
	}
	if params == nil {
		t.Error("params must be non-nil even on empty input")
	}
}

func TestParseContentDisposition_Malformed(t *testing.T) {
	disp, _ := parseContentDisposition(";;;notvalid")
	if disp != "" {
		t.Errorf("malformed disposition = %q, want empty fallback", disp)
	}
}

func TestParseMessageBody_DecodesBase64Attachment(t *testing.T) {
	// "Hello, world!" base64-encoded is SGVsbG8sIHdvcmxkIQ==.
	body := []byte(
		"--B\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"see attachment\r\n" +
			"--B\r\n" +
			`Content-Type: application/octet-stream; name="data.bin"` + "\r\n" +
			`Content-Disposition: attachment; filename="data.bin"` + "\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			"SGVsbG8sIHdvcmxkIQ==\r\n" +
			"--B--\r\n",
	)
	res := parseMessageBody(`multipart/mixed; boundary="B"`, "", body)
	if len(res.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(res.Attachments))
	}
	if string(res.Attachments[0].Content) != "Hello, world!" {
		t.Errorf("base64 decode failed: %q", string(res.Attachments[0].Content))
	}
}

func TestParseMessageBody_DecodesEncodedFilenameHeader(t *testing.T) {
	// Filename with RFC 2047 encoded-word (e.g. UTF-8 base64).
	// "Käse.pdf" base64-encoded.
	body := []byte(
		"--B\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"body\r\n" +
			"--B\r\n" +
			"Content-Type: application/pdf\r\n" +
			`Content-Disposition: attachment; filename="=?UTF-8?B?S8Okc2UucGRm?="` + "\r\n\r\n" +
			"PDF\r\n" +
			"--B--\r\n",
	)
	res := parseMessageBody(`multipart/mixed; boundary="B"`, "", body)
	if len(res.Attachments) != 1 {
		t.Fatalf("Attachments = %d", len(res.Attachments))
	}
	if !strings.Contains(res.Attachments[0].Filename, "Käse") {
		t.Errorf("encoded filename not decoded: %q", res.Attachments[0].Filename)
	}
}
