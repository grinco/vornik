package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Interaction is the parsed, SIGNATURE-VERIFIED subset of a Slack block_actions
// payload that the answer handler needs.
//
// This type is the seam between the two halves of the button path
// (https://docs.vornik.io §v1.2/2): this package
// owns transport — verifying the signature and parsing the payload — while
// internal/api owns authorization and the steering.Answerer call, because that
// is where the repositories live. Nothing here touches persistence, which keeps
// internal/slack the pure transport package it is deliberately built as.
//
// Every field is taken from the SIGNED body. UserID in particular is the §3a
// identity: it is Slack's assertion of who clicked, not something a caller can
// set.
type Interaction struct {
	TeamID      string
	UserID      string
	ChannelID   string
	MessageTS   string
	ResponseURL string
	// ActionValue is the button's opaque token, as produced by the steering
	// notifier (`steer:<action>:<payload>`). Parsing it is the answer handler's
	// business, not this package's.
	ActionValue string
}

// ErrNotAnInteraction rejects a payload this endpoint does not serve — a
// non-block_actions interaction type, an unknown workspace, or a body with no
// action.
var ErrNotAnInteraction = errors.New("slack: not a steering interaction")

// ParseInteraction verifies the request signature and returns the typed
// interaction.
//
// Order is the security property: the signature is checked over the RAW body
// before anything is parsed or trusted, matching the events path. A caller must
// treat any error as "refuse and do nothing" — there is no partial success.
func (c *Channel) ParseInteraction(r *http.Request, now time.Time) (Interaction, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		return Interaction{}, fmt.Errorf("slack: read interaction body: %w", err)
	}
	if len(body) > maxWebhookBodyBytes {
		return Interaction{}, errors.New("slack: interaction body too large")
	}
	// Signature FIRST, over the raw bytes, before the body is interpreted.
	if err := c.verifySignature(r, body, now); err != nil {
		return Interaction{}, err
	}

	form, err := url.ParseQuery(string(body))
	if err != nil {
		return Interaction{}, fmt.Errorf("slack: parse interaction form: %w", err)
	}
	raw := strings.TrimSpace(form.Get("payload"))
	if raw == "" {
		return Interaction{}, ErrNotAnInteraction
	}

	var p struct {
		Type string `json:"type"`
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
		ResponseURL string `json:"response_url"`
		Actions     []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Interaction{}, fmt.Errorf("slack: decode interaction payload: %w", err)
	}
	// Only button taps. A view_submission or shortcut is a different contract
	// and must not be coerced into this one.
	if p.Type != "block_actions" || len(p.Actions) == 0 {
		return Interaction{}, ErrNotAnInteraction
	}
	// The signing secret is per-installation, so a payload naming a workspace
	// this deployment does not serve is not ours to act on — the same team gate
	// the event and slash paths apply.
	if _, ok := c.installationsByID[p.Team.ID]; !ok {
		return Interaction{}, fmt.Errorf("%w: unknown team %q", ErrNotAnInteraction, p.Team.ID)
	}

	value := p.Actions[0].Value
	if value == "" {
		value = p.Actions[0].ActionID
	}
	return Interaction{
		TeamID:      p.Team.ID,
		UserID:      p.User.ID,
		ChannelID:   p.Channel.ID,
		MessageTS:   p.Message.TS,
		ResponseURL: p.ResponseURL,
		ActionValue: value,
	}, nil
}

// WriteInteractionAck writes the empty 200 Slack needs within three seconds.
//
// Deliberately its own function so the ack contract is testable and hard to
// drift: the body stays EMPTY and the confirmation reaches the operator through
// response_url instead. The caller must invoke this immediately after parsing —
// before authorization, before answering — because anything that runs first can
// blow the budget, and a blown budget is how the 2026-07-30 event-path bug
// killed in-flight turns.
func WriteInteractionAck(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
}

// InteractionParserMux verifies an interaction against several channels — one
// per project with a `slack` block, mirroring how MuxHandler fans the single
// events route out.
//
// It tries each channel in turn and returns the first that VERIFIES, rather
// than reading team_id from the unparsed body to pick a channel. Routing on
// unverified content would mean choosing which signing secret to trust based on
// a field the sender controls; trying each keeps signature verification the
// only thing that decides. The cost is one extra HMAC per configured
// workspace — negligible at the handful of channels a daemon serves, and the
// team gate inside ParseInteraction still rejects a payload naming a workspace
// the matching channel does not serve.
type InteractionParserMux struct {
	channels []*Channel
}

// NewInteractionParserMux builds the multi-channel parser.
func NewInteractionParserMux(channels []*Channel) *InteractionParserMux {
	return &InteractionParserMux{channels: channels}
}

// ParseInteraction returns the first channel's successful parse.
//
// The body must be re-readable per attempt, so it is buffered once here rather
// than consumed by the first channel — a subtlety that would otherwise make
// every channel after the first see an empty body and fail.
func (m *InteractionParserMux) ParseInteraction(r *http.Request, now time.Time) (Interaction, error) {
	if m == nil || len(m.channels) == 0 {
		return Interaction{}, ErrNotAnInteraction
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		return Interaction{}, fmt.Errorf("slack: read interaction body: %w", err)
	}
	var lastErr = ErrNotAnInteraction
	for _, ch := range m.channels {
		clone := r.Clone(r.Context())
		clone.Body = io.NopCloser(bytes.NewReader(body))
		got, err := ch.ParseInteraction(clone, now)
		if err == nil {
			return got, nil
		}
		lastErr = err
	}
	return Interaction{}, lastErr
}
