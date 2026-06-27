package slack

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// MuxHandler fans inbound Slack Events API deliveries out to the
// matching per-project Channel. Necessary because the daemon mounts
// a single HTTP route (`/api/v1/slack/webhook`) but per-project
// routing produces one Channel per workspace; each Channel knows
// its own signing secret + bot token + allowlists.
//
// Dispatch shape:
//
//   - URL-verification handshakes are routed to any one channel
//     (every channel's verifier will accept the URL-verification
//     envelope since it has no team_id — the handshake's signature
//     is verified against that channel's signing secret).
//   - All other deliveries peek at payload.team_id and dispatch to
//     the channel whose installations map includes that team_id.
//   - Unknown team_ids return HTTP 200 + audit log (Slack retries on
//     non-200 — silent drop preserves the rest of the codebase's
//     contract).
//
// The mux clones the request body before peeking so the downstream
// channel's HandleWebhook still sees an intact stream. Otherwise
// we'd consume the body twice.
type MuxHandler struct {
	channels []*Channel
	byTeam   map[string]*Channel
	logger   zerolog.Logger
}

// NewMuxHandler constructs a MuxHandler from the supplied channels.
// Each channel's installationsByID is folded into the mux's per-team
// lookup so HandleWebhook is O(1). Channels with overlapping team_ids
// are a config error (buildSlackChannels enforces no duplicates) so
// a later channel can't shadow an earlier one — but we defensively
// keep the first one seen if the operator manually constructs a mux
// with duplicates.
func NewMuxHandler(channels []*Channel, logger zerolog.Logger) *MuxHandler {
	m := &MuxHandler{
		channels: channels,
		byTeam:   make(map[string]*Channel),
		logger:   logger,
	}
	for _, ch := range channels {
		for teamID := range ch.installationsByID {
			if _, exists := m.byTeam[teamID]; exists {
				logger.Warn().
					Str("team_id", teamID).
					Msg("slack mux: duplicate team_id across channels; keeping first")
				continue
			}
			m.byTeam[teamID] = ch
		}
	}
	return m
}

// ServeHTTP implements http.Handler so the daemon's API server can
// mount MuxHandler directly. Reads + buffers the body so the
// dispatch-target channel still gets an intact request.
func (m *MuxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxWebhookBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Peek the envelope. We don't fully decode here — just enough to
	// pull out type + team_id. Errors fall through to the URL-
	// verification / route-by-team_id branches below; we let the
	// downstream channel surface the proper 400 if the body really
	// is malformed.
	var peek struct {
		Type   string `json:"type"`
		TeamID string `json:"team_id"`
	}
	_ = json.Unmarshal(body, &peek)

	if peek.Type == "url_verification" {
		m.handleURLVerification(w, r, body)
		return
	}

	target := m.resolveTarget(peek.Type, peek.TeamID)
	if target == nil {
		// No channel can handle this delivery. Slack would retry on
		// non-200; ack + audit-log to break the retry loop.
		m.logger.Warn().
			Str("type", peek.Type).
			Str("team_id", peek.TeamID).
			Msg("slack mux: no channel matches; dropping delivery with 200")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Rewind the body for the downstream handler. http.NewRequest
	// + http.Request.Body is an io.ReadCloser; reset via a bytes
	// buffer so HandleWebhook reads exactly what arrived.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	target.HandleWebhook(w, r)
}

// handleURLVerification picks the channel whose signing secret
// validates the handshake. Slack's URL-verification envelope carries
// no team_id, so multi-project daemons with distinct Slack apps cannot
// route by workspace yet. Trying each configured verifier lets every
// app complete endpoint registration without weakening the normal
// event_callback route.
func (m *MuxHandler) handleURLVerification(w http.ResponseWriter, r *http.Request, body []byte) {
	for _, ch := range m.channels {
		now := time.Now()
		if ch.clock != nil {
			now = ch.clock()
		}
		if err := ch.verifySignature(r, body, now); err != nil {
			continue
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		ch.HandleWebhook(w, r)
		return
	}
	m.logger.Warn().Msg("slack mux: url_verification signature did not match any channel; dropping delivery with 200")
	w.WriteHeader(http.StatusOK)
}

// resolveTarget picks the channel to handle a delivery. Deliveries
// route by team_id; unknown returns nil so ServeHTTP can audit-log
// and 200 to avoid Slack retry storms.
func (m *MuxHandler) resolveTarget(typ, teamID string) *Channel {
	if teamID == "" {
		return nil
	}
	return m.byTeam[teamID]
}
