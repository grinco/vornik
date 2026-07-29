package datasubject

import (
	"fmt"
	"net/mail"
	"strings"
)

// Binding is what a binder produces: identifiers that let us recognise the
// subject again, and links to the rows this observation concerns.
//
// Deliberately pure data. Binders do no I/O so their judgement can be tested
// exhaustively — and because a binder's output is the evidence every later
// access or erasure report rests on, it is the part that most needs to be
// examinable in isolation.
type Binding struct {
	Identifiers []Identifier
	Links       []Link
}

// Validate checks a binding is well-formed before anything persists it.
func (b Binding) Validate() error {
	if len(b.Identifiers) == 0 && len(b.Links) == 0 {
		return fmt.Errorf("datasubject: binding is empty")
	}
	for i, id := range b.Identifiers {
		if strings.TrimSpace(id.Kind) == "" || strings.TrimSpace(id.Value) == "" {
			return fmt.Errorf("datasubject: identifier %d needs a kind and a value", i)
		}
		want, err := DefaultConfidence(id.Source)
		if err != nil {
			return fmt.Errorf("datasubject: identifier %d: %w", i, err)
		}
		if !confidenceAtMost(id.Confidence, want) {
			return fmt.Errorf("datasubject: identifier %d: source %q cannot claim confidence %q (maximum %q)",
				i, id.Source, id.Confidence, want)
		}
	}
	for i, l := range b.Links {
		if err := l.Validate(); err != nil {
			return fmt.Errorf("datasubject: link %d: %w", i, err)
		}
	}
	return nil
}

// Identifier kinds. Stable strings — they are persisted and queried.
const (
	KindUserID     = "user_id"
	KindOperatorID = "operator_id"
	KindChannel    = "channel" // "<channel>:<external id>"
	KindEmail      = "email"
	KindKGEntity   = "kg_entity"
)

// BindAuthenticatedIdentity binds a subject from a `user_identities` row: an
// authenticated user and the channel handle they proved control of.
//
// Confidence is `certain` for both identifiers because nothing is inferred —
// the person demonstrated control of the identity. This is the only binder
// entitled to that claim, and keeping the entitlement narrow is what stops
// `certain` becoming meaningless.
//
// see LLD § https://docs.vornik.io §4.2
func BindAuthenticatedIdentity(userID, channel, externalID string) (Binding, error) {
	userID, channel, externalID = strings.TrimSpace(userID), strings.TrimSpace(channel), strings.TrimSpace(externalID)
	if userID == "" {
		return Binding{}, fmt.Errorf("datasubject: user id is required")
	}
	b := Binding{Identifiers: []Identifier{{
		Kind: KindUserID, Value: userID,
		Source: SourceAuthenticated, Confidence: ConfidenceCertain,
	}}}
	// The channel handle is only an identifier when we have both halves; a
	// bare channel name identifies nobody.
	if channel != "" && externalID != "" {
		b.Identifiers = append(b.Identifiers, Identifier{
			Kind: KindChannel, Value: channel + ":" + externalID,
			Source: SourceAuthenticated, Confidence: ConfidenceCertain,
		})
	}
	// The identity row itself is data about this person.
	b.Links = append(b.Links, Link{
		Table: TableUserIdentities, RowID: userID,
		Source: SourceAuthenticated, Confidence: ConfidenceCertain,
		Exclusivity: ExclusiveRow,
	})
	return b, b.Validate()
}

// BindOperatorLink binds a subject from `operator_identity_link`: the operator
// asserted that a channel speaker is a particular operator, whose profile the
// assistant keeps.
//
// The operator_profile row is EXCLUSIVE — a profile is about one person by
// construction — which makes it one of the few rows erasure may delete
// outright without the shared-row decision.
func BindOperatorLink(operatorID, channelSpeakerID string) (Binding, error) {
	operatorID, channelSpeakerID = strings.TrimSpace(operatorID), strings.TrimSpace(channelSpeakerID)
	if operatorID == "" {
		return Binding{}, fmt.Errorf("datasubject: operator id is required")
	}
	b := Binding{Identifiers: []Identifier{{
		Kind: KindOperatorID, Value: operatorID,
		Source: SourceOperatorLink, Confidence: ConfidenceCertain,
	}}}
	if channelSpeakerID != "" {
		b.Identifiers = append(b.Identifiers, Identifier{
			Kind: KindChannel, Value: channelSpeakerID,
			Source: SourceOperatorLink, Confidence: ConfidenceCertain,
		})
	}
	b.Links = append(b.Links, Link{
		Table: TableOperatorProfile, RowID: operatorID,
		Source: SourceOperatorLink, Confidence: ConfidenceCertain,
		Exclusivity: ExclusiveRow,
	})
	return b, b.Validate()
}

// BindEmailEnvelope binds a subject from an inbound message's From: header.
//
// Confidence is `probable`, not `certain`, and the distinction is substantive
// rather than cautious boilerplate: the ADDRESS is a fact off the wire, but the
// claim that this address denotes the same human as some other identifier is an
// inference. A shared mailbox, a delegated assistant, or a role address
// (billing@) makes the address a poor proxy for a person.
//
// The message row is linked as SHARED. An email routinely concerns people other
// than its sender — the recipients, and anyone discussed in the body — so
// asserting exclusivity here would authorise deleting their data on the
// sender's request.
func BindEmailEnvelope(sender, messageRowID, projectID string) (Binding, error) {
	addr, err := NormaliseEmail(sender)
	if err != nil {
		return Binding{}, err
	}
	b := Binding{Identifiers: []Identifier{{
		Kind: KindEmail, Value: addr,
		Source: SourceEmailEnvelope, Confidence: ConfidenceProbable,
	}}}
	if id := strings.TrimSpace(messageRowID); id != "" {
		b.Links = append(b.Links, Link{
			Table: TableTaskMessages, RowID: id, ProjectID: projectID,
			Source: SourceEmailEnvelope, Confidence: ConfidenceProbable,
			Exclusivity: SharedRow,
		})
	}
	return b, b.Validate()
}

// NormaliseEmail lowercases an address and strips any display name, so
// "Jane Doe <Jane.Doe@Example.COM>" and "jane.doe@example.com" resolve to one
// identifier.
//
// It deliberately does NOT canonicalise further. Stripping +tags or dots would
// merge addresses on a provider-specific convention — Gmail treats
// a.b+x@gmail.com as a@gmail.com, most providers do not — and merging two
// identifiers means merging two people's data. On this axis a false merge is
// worse than a false split: a split loses coverage, a merge discloses one
// person's data to another.
func NormaliseEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("datasubject: email address is required")
	}
	if parsed, err := mail.ParseAddress(raw); err == nil {
		raw = parsed.Address
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	at := strings.LastIndex(raw, "@")
	if at <= 0 || at == len(raw)-1 || strings.ContainsAny(raw, " \t") {
		return "", fmt.Errorf("datasubject: %q is not a usable email address", raw)
	}
	return raw, nil
}
