package dispatcher

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/mediakind"
)

// MediaSight configures whether the dispatcher may perceive media on its own
// turn, and how much of it. A nil *MediaSight disables the behaviour
// entirely — every media attachment then hands over, which is the
// pre-existing behaviour and a safe default.
//
// see LLD § https://docs.vornik.io §4.3
type MediaSight struct {
	// Model is the model id the dispatcher's turn will run on. Its
	// declared or inferred capabilities decide whether pixels are
	// attached at all.
	Model string
	// Declared holds chat.model_capabilities. Nil falls back to
	// mediakind's id patterns, then to text-only (fail-closed).
	Declared map[string][]mediakind.Modality
	// Store reads artifact-backed attachment bytes. Nil is fine —
	// attachments then resolve via ChannelRef or the channel's fetcher.
	Store AttachmentStore
	// AllowedRoots bounds which host paths a channel-supplied
	// ChannelRef may be read from.
	AllowedRoots []string
	// MaxBytesPerImage / MaxBytesTotal / MaxImages bound one turn. Zero
	// means unbounded for the byte caps and "no limit" for the count.
	MaxBytesPerImage int64
	MaxBytesTotal    int64
	MaxImages        int
	// Metrics observes dispositions. Nil is fine.
	Metrics MediaObserver
}

// MediaObserver records what happened to each media attachment. Implemented
// by the metrics layer; every call site is nil-safe.
//
// The two label sets answer "why did my photo go to a task instead of being
// answered inline", which is otherwise invisible to an operator.
type MediaObserver interface {
	MediaAttachment(kind, disposition string)
	MediaHandover(kind, reason string)
}

// Dispositions and handover reasons. Metric label values — keep stable.
const (
	dispositionInline      = "inline"
	dispositionHandover    = "handover"
	dispositionTranscribed = "transcribed"

	reasonModelBlind      = "model_blind"
	reasonOverSizeCap     = "over_size_cap"
	reasonOverTotalCap    = "over_total_cap"
	reasonOverCountCap    = "over_count_cap"
	reasonUnsupportedMIME = "unsupported_mime"
	reasonSightDisabled   = "sight_disabled"
)

// inlineImageMIMEs is the allowlist of types wrapped into a data URI.
//
// Narrower than mediakind's notion of an image on purpose: mediakind says
// what a file IS, and this says what we are willing to hand a provider. A
// HEIC classifies as an image and therefore hands over rather than being
// mistaken for a document — the right outcome, since providers reject it.
//
// The data URI is assembled from the CONSTANT matched here plus base64 of
// the bytes, never from a channel-supplied string, so a crafted MIME cannot
// inject structure into the prompt. Mirrors mimeFromExt in
// cmd/agent-helper/main.go, whose comment states the same intent.
var inlineImageMIMEs = map[string]string{
	"image/jpeg": "image/jpeg",
	"image/jpg":  "image/jpeg",
	"image/png":  "image/png",
	"image/gif":  "image/gif",
	"image/webp": "image/webp",
}

// sightOutcome is the per-attachment decision, carried out of the loop so
// the caller can build both the text notes and the content blocks.
type sightOutcome struct {
	attachment conversation.Attachment
	kind       mediakind.Kind
	// dataURI is non-empty only when the pixels are being attached.
	dataURI string
	// reason is non-empty when this attachment handed over.
	reason string
}

// buildSightOutcomes decides, per attachment on this turn, whether the
// dispatcher can perceive it directly or must hand it to a specialist.
//
// Fail-closed at every branch: anything unresolved is a handover with a
// named reason, never a silent omission and never a partial attach. The one
// thing this function will not do is attach something it is unsure about —
// a degraded or wrong image produces a confident wrong reading, which is
// worse than the honest round-trip through the vision role.
func (r *ChannelReceiver) buildSightOutcomes(ctx context.Context, msg conversation.ChannelMessage) []sightOutcome {
	if len(msg.Attachments) == 0 {
		return nil
	}
	out := make([]sightOutcome, 0, len(msg.Attachments))

	cfg := r.Media
	var caps mediakind.Set
	if cfg != nil {
		caps = mediakind.Capabilities(cfg.Model, cfg.Declared)
	}

	var totalBytes int64
	inlined := 0
	for _, a := range msg.Attachments {
		kind := mediakind.Classify(a.Name, a.MimeType)
		oc := sightOutcome{attachment: a, kind: kind}

		switch {
		case kind == mediakind.KindDocument || kind == mediakind.KindUnknown:
			// Not media: the existing extraction / input_files paths
			// own these. No disposition is recorded — this function
			// only speaks about media.
			continue

		case cfg == nil:
			oc.reason = reasonSightDisabled

		case kind != mediakind.KindImage:
			// Audio and video do not ride on the dispatcher turn.
			// Audio is transcribed upstream (§4.4) and video reduces
			// to keyframes plus transcript in the extractor, so both
			// reach the lead as text or hand over.
			oc.reason = reasonModelBlind

		case !caps.Can(mediakind.ModalityVision):
			oc.reason = reasonModelBlind

		case cfg.MaxImages > 0 && inlined >= cfg.MaxImages:
			oc.reason = reasonOverCountCap

		default:
			// Fetch once, then decide from the BYTES. An earlier revision
			// short-circuited on a declared MIME and sniffed separately,
			// which read the same file twice.
			data, err := fetchAttachmentBytes(ctx, r.Channel, cfg.Store, a, cfg.AllowedRoots, cfg.MaxBytesPerImage)
			if err != nil {
				oc.reason = fetchReasonOf(err)
				break
			}
			// The payload's magic bytes are the authority, ALWAYS — not the
			// declared type, and not the extension. A channel could declare
			// image/jpeg on a PNG, and a data URI carrying the wrong type is
			// a lie to the provider even when it happens to be tolerated.
			mime, ok := detectInlineMIME(data)
			if !ok {
				oc.reason = reasonUnsupportedMIME
				break
			}
			// A declared type that disagrees with the bytes means we do not
			// actually know what this file is, so we decline to guess.
			if declared, declaredOK := inlineImageMIMEs[strings.ToLower(strings.TrimSpace(a.MimeType))]; declaredOK && declared != mime {
				oc.reason = reasonUnsupportedMIME
				break
			}
			if cfg.MaxBytesTotal > 0 && totalBytes+int64(len(data)) > cfg.MaxBytesTotal {
				// The per-turn budget the per-image cap cannot catch:
				// four under-cap images still add up to a request the
				// provider will reject, and the local token estimate
				// charges a flat per-image figure so nothing upstream
				// would notice.
				oc.reason = reasonOverTotalCap
				break
			}
			totalBytes += int64(len(data))
			inlined++
			oc.dataURI = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		}
		out = append(out, oc)
	}
	return out
}

// detectInlineMIME reports the allowlisted image type of a payload, based on
// its magic bytes. Returns false for anything not on the allowlist —
// including a valid image type providers reject.
func detectInlineMIME(data []byte) (string, bool) {
	sniffed := http.DetectContentType(data)
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = sniffed[:i]
	}
	mime, ok := inlineImageMIMEs[strings.ToLower(strings.TrimSpace(sniffed))]
	return mime, ok
}

// fetchReasonOf maps a fetch failure to its stable metric reason.
func fetchReasonOf(err error) string {
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe.Reason()
	}
	return reasonFetchFailed
}

// mediaBlocks converts outcomes into the content blocks for THIS turn,
// prefixed by the turn's text. Returns nil when nothing is being attached,
// so the caller keeps the plain-Content message shape.
func mediaBlocks(text string, outcomes []sightOutcome) []chat.ContentBlock {
	var blocks []chat.ContentBlock
	for _, oc := range outcomes {
		if oc.dataURI == "" {
			continue
		}
		blocks = append(blocks, chat.ImageBlock(oc.dataURI))
	}
	if len(blocks) == 0 {
		return nil
	}
	return append([]chat.ContentBlock{chat.TextBlock(text)}, blocks...)
}

// mediaNotes renders the per-attachment routing directive appended to the
// turn text.
//
// Generated from the branch actually taken, which is the point: the previous
// hardcoded Telegram trailer asserted that images "are forwarded to
// vision-capable models as multimodal content automatically" while no such
// path existed. A sentence derived from the decision cannot describe a
// decision that was not made.
func mediaNotes(outcomes []sightOutcome) string {
	if len(outcomes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, oc := range outcomes {
		name := oc.attachment.Name
		if name == "" {
			name = "(unnamed)"
		}
		if oc.dataURI != "" {
			fmt.Fprintf(&b, "\n[MEDIA: %s (%s) is ATTACHED TO THIS TURN — you can see it. Answer from what you observe; do not schedule a task to look at it.]",
				name, oc.kind)
			continue
		}
		switch oc.kind {
		case mediakind.KindAudio:
			fmt.Fprintf(&b, "\n[MEDIA: %s is audio you cannot hear. If a transcript is available above, answer from it; otherwise say so — do NOT guess at its contents.]", name)
		case mediakind.KindVideo:
			fmt.Fprintf(&b, "\n[MEDIA: %s is video you cannot watch. Schedule a task with the 'vision' workflow and pass this file in input_files. Frames are sampled at a fixed interval, so visual timing cannot be cited precisely.]", name)
		default:
			fmt.Fprintf(&b, "\n[MEDIA: %s is an image you CANNOT see (%s). Schedule a task with the 'vision' workflow and pass this file in input_files verbatim. Do NOT describe it from its filename, size, or what this request implies is in it.]",
				name, humanReason(oc.reason))
		}
	}
	return b.String()
}

// humanReason turns a metric label into a phrase for the prompt, so the
// model is told WHY it cannot see rather than merely that it cannot.
func humanReason(reason string) string {
	switch reason {
	case reasonModelBlind:
		return "this model has no vision capability"
	case reasonOverSizeCap:
		return "the file is over the inline size limit"
	case reasonOverTotalCap:
		return "this turn is already at its image-data limit"
	case reasonOverCountCap:
		return "this turn is already at its image-count limit"
	case reasonUnsupportedMIME:
		return "the format is not one that can be attached"
	case reasonNoFetchSeam:
		return "this channel cannot supply the file's bytes"
	case reasonFetchFailed:
		return "the file's bytes could not be read"
	case reasonSightDisabled:
		return "inline media is disabled on this deployment"
	default:
		return reason
	}
}

// observeOutcomes records dispositions. Audio arriving already transcribed
// is reported as "transcribed" by the caller, not here.
func (r *ChannelReceiver) observeOutcomes(outcomes []sightOutcome) {
	if r.Media == nil || r.Media.Metrics == nil {
		return
	}
	for _, oc := range outcomes {
		kind := oc.kind.String()
		if oc.dataURI != "" {
			r.Media.Metrics.MediaAttachment(kind, dispositionInline)
			continue
		}
		r.Media.Metrics.MediaAttachment(kind, dispositionHandover)
		r.Media.Metrics.MediaHandover(kind, oc.reason)
	}
}

// stripInlineMedia replaces image blocks with a short text marker.
//
// Applied to the post-turn message slice before it is persisted, because
// the session store writes result.Messages back as the new history — so
// without this, every image would be REPLAYED on every later turn of the
// conversation. That costs a flat per-image token charge forever
// (chat/conversation.go accounts image blocks at a fixed figure regardless
// of payload size) and invalidates the stable cache prefix on each turn.
//
// A marker rather than a deletion, so the history stays coherent: the model
// can see that it was shown an image and answered about it, without the
// pixels riding along.
func stripInlineMedia(msgs []chat.Message) []chat.Message {
	out := make([]chat.Message, len(msgs))
	copy(out, msgs)
	for i, m := range out {
		if len(m.Blocks) == 0 {
			continue
		}
		hasImage := false
		var text strings.Builder
		for _, b := range m.Blocks {
			switch b.Type {
			case "image_url":
				hasImage = true
			case "text":
				text.WriteString(b.Text)
			}
		}
		if !hasImage {
			continue
		}
		body := text.String()
		if body == "" {
			body = m.Content
		}
		out[i].Blocks = nil
		out[i].Content = body + "\n[an image was shown on this turn; it is not retained in history]"
	}
	return out
}
