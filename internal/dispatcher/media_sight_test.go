package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/mediakind"
)

// realJPEG / realPNG produce payloads whose magic bytes actually match the
// declared type — the sight path sniffs the content, so fixtures cannot be
// arbitrary strings.
func realJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func realPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type recordingObserver struct {
	attachments []string // "kind/disposition"
	handovers   []string // "kind/reason"
}

func (o *recordingObserver) MediaAttachment(kind, disposition string) {
	o.attachments = append(o.attachments, kind+"/"+disposition)
}
func (o *recordingObserver) MediaHandover(kind, reason string) {
	o.handovers = append(o.handovers, kind+"/"+reason)
}

// stageFile writes bytes to a temp dir and returns (dir, path).
func stageFile(t *testing.T, name string, data []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func imageMsg(name, mime, ref string) conversation.ChannelMessage {
	return conversation.ChannelMessage{
		Text:        "what is this?",
		Attachments: []conversation.Attachment{{Name: name, MimeType: mime, ChannelRef: ref}},
	}
}

func sightReceiver(dir, model string, obs *recordingObserver) *ChannelReceiver {
	return &ChannelReceiver{
		Channel: plainChannel{},
		Media: &MediaSight{
			Model:            model,
			AllowedRoots:     []string{dir},
			MaxBytesPerImage: 5 << 20,
			MaxBytesTotal:    10 << 20,
			MaxImages:        4,
			Metrics:          obs,
		},
	}
}

// A vision-capable model gets the pixels on its own turn.
//
// see LLD § https://docs.vornik.io §4.3
func TestSight_VisionCapableModelGetsImageBlock(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realJPEG(t))
	obs := &recordingObserver{}
	r := sightReceiver(dir, "gemma4:31b", obs)

	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("photo.jpg", "image/jpeg", p))
	if len(outcomes) != 1 || outcomes[0].dataURI == "" {
		t.Fatalf("expected the image to be attached, got %+v", outcomes)
	}
	if !strings.HasPrefix(outcomes[0].dataURI, "data:image/jpeg;base64,") {
		t.Errorf("data URI must be built from the validated MIME constant, got %.40q", outcomes[0].dataURI)
	}

	blocks := mediaBlocks("hello", outcomes)
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "image_url" {
		t.Fatalf("want [text, image_url], got %+v", blocks)
	}
	// The note must tell the model it CAN see, so it does not schedule a
	// redundant task to look at what is already in front of it.
	if !strings.Contains(mediaNotes(outcomes), "ATTACHED TO THIS TURN") {
		t.Errorf("note should say the image is attached: %q", mediaNotes(outcomes))
	}
	r.observeOutcomes(outcomes)
	if len(obs.attachments) != 1 || obs.attachments[0] != "image/inline" {
		t.Errorf("disposition not recorded: %v", obs.attachments)
	}
}

// A blind model hands over, and the note says WHY and forbids guessing.
func TestSight_BlindModelHandsOverAndForbidsGuessing(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realJPEG(t))
	obs := &recordingObserver{}
	r := sightReceiver(dir, "glm-5.2", obs) // the dispatcher model, declared blind

	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("photo.jpg", "image/jpeg", p))
	if len(outcomes) != 1 || outcomes[0].dataURI != "" {
		t.Fatalf("a blind model must not receive pixels: %+v", outcomes)
	}
	if outcomes[0].reason != reasonModelBlind {
		t.Errorf("reason = %q, want %q", outcomes[0].reason, reasonModelBlind)
	}
	notes := mediaNotes(outcomes)
	for _, want := range []string{"CANNOT see", "'vision' workflow", "input_files", "Do NOT describe it from its filename"} {
		if !strings.Contains(notes, want) {
			t.Errorf("handover note missing %q; got: %s", want, notes)
		}
	}
	if mediaBlocks("x", outcomes) != nil {
		t.Error("no blocks may be built when nothing was attached")
	}
	r.observeOutcomes(outcomes)
	if len(obs.handovers) != 1 || obs.handovers[0] != "image/model_blind" {
		t.Errorf("handover not recorded: %v", obs.handovers)
	}
}

// An explicit operator declaration overrides the pattern list, in the
// direction that matters: declaring a pattern-matching model text-only must
// stop the pixels.
func TestSight_ExplicitTextOnlyDeclarationBlocksPixels(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realJPEG(t))
	r := sightReceiver(dir, "gemma4:31b", nil)
	r.Media.Declared = map[string][]mediakind.Modality{"gemma4:31b": {mediakind.ModalityText}}

	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("photo.jpg", "image/jpeg", p))
	if outcomes[0].dataURI != "" {
		t.Error("an explicit text-only declaration must suppress inline pixels")
	}
	if outcomes[0].reason != reasonModelBlind {
		t.Errorf("reason = %q", outcomes[0].reason)
	}
}

// Nil MediaSight is the safe default: everything hands over.
func TestSight_NilConfigHandsOver(t *testing.T) {
	r := &ChannelReceiver{Channel: plainChannel{}}
	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("photo.jpg", "image/jpeg", "/tmp/x.jpg"))
	if len(outcomes) != 1 || outcomes[0].reason != reasonSightDisabled {
		t.Fatalf("want sight_disabled handover, got %+v", outcomes)
	}
}

func TestSight_OverPerImageCapHandsOver(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realJPEG(t))
	r := sightReceiver(dir, "gemma4:31b", nil)
	r.Media.MaxBytesPerImage = 10 // smaller than any real JPEG

	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("photo.jpg", "image/jpeg", p))
	if outcomes[0].dataURI != "" {
		t.Error("an over-cap image must not be attached")
	}
	if outcomes[0].reason != reasonFetchFailed {
		t.Errorf("reason = %q, want the fetch layer's cap refusal", outcomes[0].reason)
	}
}

// The per-turn TOTAL cap catches what the per-image cap cannot: several
// under-cap images that together exceed what should ride on one request.
func TestSight_OverTotalCapHandsOverTheExcess(t *testing.T) {
	jpg := realJPEG(t)
	dir := t.TempDir()
	var atts []conversation.Attachment
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, jpg, 0o600); err != nil {
			t.Fatal(err)
		}
		atts = append(atts, conversation.Attachment{Name: n, MimeType: "image/jpeg", ChannelRef: p})
	}
	obs := &recordingObserver{}
	r := sightReceiver(dir, "gemma4:31b", obs)
	// Room for one image only.
	r.Media.MaxBytesTotal = int64(len(jpg)) + 1

	outcomes := r.buildSightOutcomes(context.Background(),
		conversation.ChannelMessage{Text: "compare these", Attachments: atts})
	inlined, over := 0, 0
	for _, oc := range outcomes {
		if oc.dataURI != "" {
			inlined++
		}
		if oc.reason == reasonOverTotalCap {
			over++
		}
	}
	if inlined != 1 || over != 2 {
		t.Errorf("want 1 inlined and 2 over_total_cap, got %d/%d (%+v)", inlined, over, outcomes)
	}
}

func TestSight_OverCountCapHandsOverTheExcess(t *testing.T) {
	jpg := realJPEG(t)
	dir := t.TempDir()
	var atts []conversation.Attachment
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, jpg, 0o600); err != nil {
			t.Fatal(err)
		}
		atts = append(atts, conversation.Attachment{Name: n, MimeType: "image/jpeg", ChannelRef: p})
	}
	r := sightReceiver(dir, "gemma4:31b", nil)
	r.Media.MaxImages = 2

	outcomes := r.buildSightOutcomes(context.Background(),
		conversation.ChannelMessage{Attachments: atts})
	over := 0
	for _, oc := range outcomes {
		if oc.reason == reasonOverCountCap {
			over++
		}
	}
	if over != 1 {
		t.Errorf("want 1 over_count_cap, got %d (%+v)", over, outcomes)
	}
}

// A declared type that disagrees with the bytes must NOT be attached: we do
// not know what the payload is, so we refuse rather than hand a provider
// something mislabelled.
func TestSight_MIMEMagicByteDisagreementHandsOver(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realPNG(t)) // PNG bytes, claimed JPEG
	r := sightReceiver(dir, "gemma4:31b", nil)

	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("photo.jpg", "image/jpeg", p))
	if outcomes[0].dataURI != "" {
		t.Error("a declared/actual type mismatch must not be attached")
	}
	if outcomes[0].reason != reasonUnsupportedMIME {
		t.Errorf("reason = %q, want %q", outcomes[0].reason, reasonUnsupportedMIME)
	}
}

// An unsupported image format still classifies as an image (so it hands over
// to the vision role) rather than being mistaken for a document.
func TestSight_UnsupportedFormatHandsOver(t *testing.T) {
	dir, p := stageFile(t, "photo.heic", []byte("not really heic but unknown to sniffing"))
	r := sightReceiver(dir, "gemma4:31b", nil)

	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("photo.heic", "image/heic", p))
	if len(outcomes) != 1 {
		t.Fatalf("want one outcome, got %+v", outcomes)
	}
	if outcomes[0].kind != mediakind.KindImage {
		t.Errorf("kind = %v, want image", outcomes[0].kind)
	}
	if outcomes[0].reason != reasonUnsupportedMIME {
		t.Errorf("reason = %q, want %q", outcomes[0].reason, reasonUnsupportedMIME)
	}
}

// Documents are none of this function's business — the extraction and
// input_files paths own them, and emitting a media note would confuse the
// lead about a file that is already handled.
func TestSight_DocumentsIgnored(t *testing.T) {
	dir, p := stageFile(t, "book.epub", []byte("PK..."))
	r := sightReceiver(dir, "gemma4:31b", nil)
	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("book.epub", "application/epub+zip", p))
	if len(outcomes) != 0 {
		t.Errorf("documents must produce no media outcome, got %+v", outcomes)
	}
}

// Audio and video never ride on the dispatcher turn; they get their own
// honest notes rather than a generic image handover.
func TestSight_AudioAndVideoGetTheirOwnNotes(t *testing.T) {
	dir, ap := stageFile(t, "voice.opus", []byte("OggS"))
	r := sightReceiver(dir, "gemma4:31b", nil)
	notes := mediaNotes(r.buildSightOutcomes(context.Background(), imageMsg("voice.opus", "audio/ogg", ap)))
	if !strings.Contains(notes, "audio you cannot hear") || !strings.Contains(notes, "do NOT guess") {
		t.Errorf("audio note wrong: %s", notes)
	}

	dir2, vp := stageFile(t, "clip.mp4", []byte("ftypmp4"))
	r2 := sightReceiver(dir2, "gemma4:31b", nil)
	vnotes := mediaNotes(r2.buildSightOutcomes(context.Background(), imageMsg("clip.mp4", "video/mp4", vp)))
	for _, want := range []string{"video you cannot watch", "'vision' workflow", "visual timing cannot be cited"} {
		if !strings.Contains(vnotes, want) {
			t.Errorf("video note missing %q: %s", want, vnotes)
		}
	}
}

// THE history contract: pixels ride on one turn and are never persisted.
// Without this the session store would write them back as history and every
// later turn would re-pay a flat per-image token charge and bust the cache
// prefix.
//
// see LLD § https://docs.vornik.io §4.3(1)
func TestStripInlineMedia_RemovesPixelsKeepsCoherence(t *testing.T) {
	msgs := []chat.Message{
		{Role: "user", Blocks: []chat.ContentBlock{
			chat.TextBlock("what is this?"),
			chat.ImageBlock("data:image/jpeg;base64,AAAA"),
		}},
		{Role: "assistant", Content: "a red square"},
	}
	got := stripInlineMedia(msgs)

	if len(got[0].Blocks) != 0 {
		t.Error("image blocks must not survive into history")
	}
	if !strings.Contains(got[0].Content, "what is this?") {
		t.Error("the turn's text must survive")
	}
	if !strings.Contains(got[0].Content, "an image was shown on this turn") {
		t.Error("history should record that an image was present, for coherence")
	}
	if strings.Contains(got[0].Content, "base64") {
		t.Error("no base64 payload may remain in history")
	}
	if got[1].Content != "a red square" {
		t.Error("other messages must be untouched")
	}
	// The input slice must not be mutated — callers may still hold it.
	if len(msgs[0].Blocks) != 2 {
		t.Error("stripInlineMedia must not mutate its input")
	}
}

func TestStripInlineMedia_LeavesTextOnlyMessagesAlone(t *testing.T) {
	msgs := []chat.Message{
		{Role: "user", Content: "plain"},
		{Role: "user", Blocks: []chat.ContentBlock{chat.TextBlock("blocks but no image")}},
	}
	got := stripInlineMedia(msgs)
	if got[0].Content != "plain" {
		t.Error("plain message changed")
	}
	if len(got[1].Blocks) != 1 {
		t.Error("a text-only block message must keep its blocks")
	}
}

func TestHumanReason_CoversEveryReason(t *testing.T) {
	for _, reason := range []string{
		reasonModelBlind, reasonOverSizeCap, reasonOverTotalCap, reasonOverCountCap,
		reasonUnsupportedMIME, reasonNoFetchSeam, reasonFetchFailed, reasonSightDisabled,
	} {
		if got := humanReason(reason); got == "" || got == reason {
			t.Errorf("reason %q has no human phrasing (got %q)", reason, got)
		}
	}
	if got := humanReason("something_new"); got != "something_new" {
		t.Errorf("unknown reason should pass through, got %q", got)
	}
}

func TestMediaNotes_EmptyForNoOutcomes(t *testing.T) {
	if mediaNotes(nil) != "" {
		t.Error("no outcomes should produce no notes")
	}
}

func TestSight_NoAttachmentsIsNoop(t *testing.T) {
	r := sightReceiver(t.TempDir(), "gemma4:31b", nil)
	if got := r.buildSightOutcomes(context.Background(), conversation.ChannelMessage{Text: "hi"}); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}

// An unnamed attachment must not produce a malformed note.
func TestMediaNotes_UnnamedAttachment(t *testing.T) {
	notes := mediaNotes([]sightOutcome{{kind: mediakind.KindImage, reason: reasonModelBlind}})
	if !strings.Contains(notes, "(unnamed)") {
		t.Errorf("want an (unnamed) placeholder, got %s", notes)
	}
}

// No fetch seam at all (Slack/GitHub shape) reports its own reason.
func TestSight_NoFetchSeamReason(t *testing.T) {
	r := &ChannelReceiver{
		Channel: plainChannel{},
		Media:   &MediaSight{Model: "gemma4:31b"},
	}
	outcomes := r.buildSightOutcomes(context.Background(),
		conversation.ChannelMessage{Attachments: []conversation.Attachment{
			{Name: "photo.jpg", MimeType: "image/jpeg"},
		}})
	if outcomes[0].reason != reasonNoFetchSeam {
		t.Errorf("reason = %q, want %q", outcomes[0].reason, reasonNoFetchSeam)
	}
}

// An attachment with no declared MIME is sniffed from its bytes rather than
// trusted by extension — and a real JPEG under a misleading name still
// attaches, because the bytes are what the provider will decode.
func TestSight_SniffsWhenChannelDeclaresNoMIME(t *testing.T) {
	dir, p := stageFile(t, "scan.bin", realJPEG(t))
	r := sightReceiver(dir, "gemma4:31b", nil)
	outcomes := r.buildSightOutcomes(context.Background(), imageMsg("scan.bin", "", p))
	if len(outcomes) != 0 {
		// .bin classifies as Unknown, so this is not media at all — the
		// document/input_files path owns it. Asserting the classification
		// boundary here, not the sniff.
		t.Fatalf("an unclassifiable extension is not media: %+v", outcomes)
	}

	// With an image extension but no declared MIME, the sniff decides.
	dir2, p2 := stageFile(t, "scan.png", realPNG(t))
	r2 := sightReceiver(dir2, "gemma4:31b", nil)
	oc := r2.buildSightOutcomes(context.Background(), imageMsg("scan.png", "", p2))
	if len(oc) != 1 || oc[0].dataURI == "" {
		t.Fatalf("a real PNG with no declared MIME should be sniffed and attached: %+v", oc)
	}
	if !strings.HasPrefix(oc[0].dataURI, "data:image/png;base64,") {
		t.Errorf("sniffed type must drive the URI, got %.30q", oc[0].dataURI)
	}
}

// The BYTES decide, always. A channel declaring image/jpeg on PNG bytes must
// hand over rather than emit a data URI whose type is a lie to the provider —
// even though most providers tolerate the mismatch.
func TestSight_DeclaredMIMENeverOverridesTheBytes(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realPNG(t))
	r := sightReceiver(dir, "gemma4:31b", nil)
	oc := r.buildSightOutcomes(context.Background(), imageMsg("photo.jpg", "image/jpeg", p))
	if oc[0].dataURI != "" {
		t.Error("declared jpeg over PNG bytes must not be attached")
	}
	if oc[0].reason != reasonUnsupportedMIME {
		t.Errorf("reason = %q", oc[0].reason)
	}

	// And when they agree, the URI carries the sniffed type.
	dir2, p2 := stageFile(t, "photo.png", realPNG(t))
	r2 := sightReceiver(dir2, "gemma4:31b", nil)
	oc2 := r2.buildSightOutcomes(context.Background(), imageMsg("photo.png", "image/png", p2))
	if len(oc2) != 1 || !strings.HasPrefix(oc2[0].dataURI, "data:image/png;base64,") {
		t.Fatalf("agreeing types should attach as png: %+v", oc2)
	}
}

func TestFetchReasonOf_NonFetchError(t *testing.T) {
	if got := fetchReasonOf(errors.New("plain")); got != reasonFetchFailed {
		t.Errorf("a non-fetchError should map to fetch_failed, got %q", got)
	}
}

// A message whose blocks carry an image but no text block falls back to
// Content for the retained history line.
func TestStripInlineMedia_ImageOnlyBlocksFallBackToContent(t *testing.T) {
	msgs := []chat.Message{{
		Role:    "user",
		Content: "look at this",
		Blocks:  []chat.ContentBlock{chat.ImageBlock("data:image/png;base64,AAA")},
	}}
	got := stripInlineMedia(msgs)
	if !strings.Contains(got[0].Content, "look at this") {
		t.Errorf("Content should be preserved as the fallback text, got %q", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "an image was shown") {
		t.Errorf("marker missing: %q", got[0].Content)
	}
}

// echoAgent returns the request's messages verbatim plus an assistant reply —
// the realistic shape, since Agent.Process builds Result.Messages from the
// request history. Without the echo, a test could pass simply because the
// stub never carried the image forward.
type echoAgent struct{ err error }

func (a echoAgent) Process(_ context.Context, req Request) Result {
	out := make([]chat.Message, 0, len(req.Messages)+1)
	out = append(out, req.Messages...)
	out = append(out, chat.Message{Role: "assistant", Content: "a red square"})
	return Result{Text: "a red square", Messages: out, Err: a.err}
}

func (a echoAgent) ProcessStreaming(ctx context.Context, req Request, _ chat.StreamCallback) Result {
	return a.Process(ctx, req)
}

// recordingSessions captures exactly what would be persisted.
type recordingSessions struct {
	session  Session
	appended []chat.Message
	appendN  int
}

func (s *recordingSessions) Load(context.Context, conversation.ChannelMessage) (Session, error) {
	return s.session, nil
}

func (s *recordingSessions) Append(_ context.Context, _ conversation.ChannelMessage, r Result) error {
	s.appendN++
	s.appended = append(s.appended, r.Messages...)
	return nil
}

// sendingChannel is a minimal Channel that accepts replies.
type sendingChannel struct{ conversation.Channel }

func (sendingChannel) Name() string { return "test" }
func (sendingChannel) Send(context.Context, conversation.ChannelMessage) (string, error) {
	return "1", nil
}

// THE boundary property: for a turn carrying an image, nothing with an
// image_url block may reach the session store — which is what writes history
// back. Asserted at the boundary rather than at the strip call, so the
// guarantee does not depend on one call site staying in the right place.
//
// see LLD § https://docs.vornik.io §4.3(1)
func TestReceive_NoImageBlockEverReachesTheSessionStore(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realJPEG(t))
	sessions := &recordingSessions{}
	r := &ChannelReceiver{
		Channel:  sendingChannel{},
		Agent:    echoAgent{},
		Sessions: sessions,
		Media: &MediaSight{
			Model:            "gemma4:31b",
			AllowedRoots:     []string{dir},
			MaxBytesPerImage: 5 << 20,
			MaxBytesTotal:    10 << 20,
			MaxImages:        4,
		},
	}
	msg := imageMsg("photo.jpg", "image/jpeg", p)
	msg.SessionID = "s1"
	if err := r.Receive(context.Background(), msg); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if sessions.appendN != 1 {
		t.Fatalf("expected exactly one Append, got %d", sessions.appendN)
	}
	sawUser := false
	for _, m := range sessions.appended {
		for _, b := range m.Blocks {
			if b.Type == "image_url" {
				t.Fatal("an image_url block reached the session store")
			}
		}
		if strings.Contains(m.Content, "base64") {
			t.Fatal("base64 payload reached the session store")
		}
		if m.Role == "user" {
			sawUser = true
			// The turn must remain coherent: the question survives and
			// history records that an image was shown.
			if !strings.Contains(m.Content, "what is this?") {
				t.Errorf("user text lost: %q", m.Content)
			}
			if !strings.Contains(m.Content, "an image was shown on this turn") {
				t.Errorf("history should note the image: %q", m.Content)
			}
		}
	}
	if !sawUser {
		t.Error("no user turn was persisted at all")
	}
}

// The agent DID receive the pixels — otherwise the test above would pass on a
// system that never attached them, which is the failure this whole design is
// about.
func TestReceive_AgentActuallyReceivesThePixels(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realJPEG(t))
	var seen []chat.Message
	capture := funcAgent(func(req Request) Result {
		seen = req.Messages
		return Result{Text: "ok", Messages: req.Messages}
	})
	r := &ChannelReceiver{
		Channel: sendingChannel{},
		Agent:   capture,
		Media: &MediaSight{
			Model: "gemma4:31b", AllowedRoots: []string{dir},
			MaxBytesPerImage: 5 << 20, MaxBytesTotal: 10 << 20, MaxImages: 4,
		},
	}
	msg := imageMsg("photo.jpg", "image/jpeg", p)
	msg.SessionID = "s1"
	if err := r.Receive(context.Background(), msg); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	found := false
	for _, m := range seen {
		for _, b := range m.Blocks {
			if b.Type == "image_url" && strings.HasPrefix(b.ImageURL.URL, "data:image/jpeg;base64,") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the agent never received an image_url block — sight is not working")
	}
}

// A dispatcher error must not persist anything, so there is no path where an
// unstripped turn survives a failed turn.
func TestReceive_ErrorPathPersistsNothing(t *testing.T) {
	dir, p := stageFile(t, "photo.jpg", realJPEG(t))
	sessions := &recordingSessions{}
	r := &ChannelReceiver{
		Channel:  sendingChannel{},
		Agent:    echoAgent{err: errAgentBoom},
		Sessions: sessions,
		Media: &MediaSight{
			Model: "gemma4:31b", AllowedRoots: []string{dir},
			MaxBytesPerImage: 5 << 20, MaxBytesTotal: 10 << 20, MaxImages: 4,
		},
	}
	msg := imageMsg("photo.jpg", "image/jpeg", p)
	msg.SessionID = "s1"
	if err := r.Receive(context.Background(), msg); err == nil {
		t.Fatal("expected the dispatcher error to surface")
	}
	if sessions.appendN != 0 {
		t.Errorf("nothing may be persisted on the error path, got %d appends", sessions.appendN)
	}
}

var errAgentBoom = errors.New("agent boom")

// funcAgent adapts a func to Doer.
type funcAgent func(Request) Result

func (f funcAgent) Process(_ context.Context, req Request) Result { return f(req) }
func (f funcAgent) ProcessStreaming(_ context.Context, req Request, _ chat.StreamCallback) Result {
	return f(req)
}
