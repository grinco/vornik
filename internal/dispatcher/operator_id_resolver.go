package dispatcher

// Canonical operator ID resolution — collapses a per-channel
// speaker id ("tg:42", "web:abc-hash") onto the canonical
// operator id when the operator has linked their identities via
// /link or `vornikctl operator link`. With no link table wired,
// the resolver is a no-op (returns the speaker id verbatim) so
// the pre-link-feature behaviour is exactly preserved.
//
// See https://docs.vornik.io (Phase A).

import (
	"context"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// resolveCanonicalOperatorID walks the operator_identity_link
// table at most once per turn to find the canonical id for a
// channel-specific speaker id. The resolver is intentionally
// silent on DB errors — falling back to the per-channel id is
// always correct (the linked profile just doesn't apply this
// turn). The dispatcher's debug log captures the failure path
// so operators can investigate without burning the user's turn.
func (a *Agent) resolveCanonicalOperatorID(ctx context.Context, channelSpeakerID string) string {
	if a == nil || a.operatorIdentityLinks == nil || channelSpeakerID == "" {
		return channelSpeakerID
	}
	link, err := a.operatorIdentityLinks.Get(ctx, channelSpeakerID)
	if err != nil {
		// ErrNotFound is the "no link for this speaker yet"
		// path — silent. Any other error surfaces at debug.
		if !errors.Is(err, persistence.ErrNotFound) {
			a.logger.Debug().Err(err).
				Str("channel_speaker_id", channelSpeakerID).
				Msg("dispatcher: identity-link lookup failed; using speaker id verbatim")
		}
		return channelSpeakerID
	}
	if link == nil || link.OperatorID == "" {
		return channelSpeakerID
	}
	return link.OperatorID
}

// resolveCanonicalOperatorID is the ToolExecutor-side mirror of
// the Agent method above. The write tool (update_operator_profile)
// resolves identical to the read path so a linked operator's
// writes land on the same row their reads inject from.
func (te *ToolExecutor) resolveCanonicalOperatorID(ctx context.Context, channelSpeakerID string) string {
	if te == nil || te.operatorIdentityLinks == nil || channelSpeakerID == "" {
		return channelSpeakerID
	}
	link, err := te.operatorIdentityLinks.Get(ctx, channelSpeakerID)
	if err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			te.logger.Debug().Err(err).
				Str("channel_speaker_id", channelSpeakerID).
				Msg("dispatcher: identity-link lookup failed in write tool; using speaker id verbatim")
		}
		return channelSpeakerID
	}
	if link == nil || link.OperatorID == "" {
		return channelSpeakerID
	}
	return link.OperatorID
}
