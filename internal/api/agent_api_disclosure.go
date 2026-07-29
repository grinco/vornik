package api

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/aidisclosure"
)

// Art 50(1) enforcement for gateway providers that publish to humans
// (G6 finding B, 2026-07-29).
//
// The channel chokepoint in dispatcher/channel_receiver.go covers channels. It
// does not cover a query_api write to a third-party API whose readers are
// people — the `moltbook` provider posts autonomously to a public social
// platform on a 6h/24h cadence. That path carried no code-enforced disclosure
// at all; the obligation rested on a prompt instruction in a knowledge skill.
//
// The gateway cannot INJECT the notice: that needs per-provider, per-route
// knowledge of which JSON field holds human-facing prose, and it would be wrong
// the moment the third party changes its API. So it enforces by REFUSAL. The
// agent composes the content; a write whose content does not carry the notice
// does not leave the daemon. Refusals return to the agent as the tool result
// (the house pattern — see the taint gate in AgentQueryAPI), so the agent can
// fix the body and retry within the same task rather than losing the work.
//
// Design: https://docs.vornik.io §5

// PublicationDiscloser supplies the notice an authored artifact must carry.
// A one-method interface so this package depends on the policy rather than on
// the service that also owns per-session channel state.
type PublicationDiscloser interface {
	PublicationNotice() aidisclosure.Notice
}

// publicationDisclosureFields returns the body fields that must carry the notice
// for a write to this provider and path, or nil when the write is not gated —
// either the provider is not a publication surface (the common case: no
// inspection, no cost) or the path is not one that publishes text.
func (s *Server) publicationDisclosureFields(provider, path string) []string {
	if s.config == nil {
		return nil
	}
	p, ok := s.config.Gateway.Providers[provider]
	if !ok || !p.Disclosure.Required {
		return nil
	}
	if !disclosurePathMatches(p.Disclosure.Paths, path) {
		return nil
	}
	return p.Disclosure.ContentFields
}

// disclosurePathMatches reports whether the gate covers this request path.
//
// An EMPTY pattern list matches everything: for a provider whose whole purpose is
// publishing, no path list is needed, and an operator who omits one gets
// over-enforcement rather than a silent hole.
//
// Patterns match SEGMENT-WISE, with "*" matching exactly one segment, so a
// resource id can be wildcarded without dragging in its sibling actions:
// "/posts/*/comments" gates a comment on any post, while "/posts/*/upvote" stays
// ungated. Prefix matching would be wrong here — "/posts" as a prefix also
// swallows "/posts/{id}/upvote", which publishes nothing. Pattern ORDER does not
// matter; any match gates.
//
// MATCHING CONTRACT, and why each rule exists. `path` is LLM-supplied, so a gate
// keyed on it is only as strong as its normalisation. All of these are pinned by
// TestAgentQueryAPI_G6B_PathVariantsCannotEvadeTheGate and its siblings:
//
//   - case-insensitive          — "/POSTS" must not evade a gate on "/posts"
//   - query + fragment stripped — "/posts?draft=1" is the same route
//   - empty segments dropped    — "//posts/" is the same route
//   - percent-encoding → GATED  — cannot be reasoned about, so never exempted
//   - empty path → GATED        — degenerate input is not an operator exemption
//
// The three "→ GATED" rules mean the exemption list can only ever exempt a route
// somebody deliberately named. Every ambiguity resolves toward enforcement: an
// over-gated call is a refused tool call the agent retries, while an under-gated
// one is undisclosed AI text in front of a person.
func disclosurePathMatches(patterns []string, path string) bool {
	if len(patterns) == 0 {
		return true
	}
	// Percent-encoding in an LLM-supplied path is never something an operator
	// exempted. "/posts%2Fcomments" segments as one thing and decodes as
	// another, so the gate cannot reason about which route the upstream will
	// actually serve — and decoding first would just move the ambiguity. Refuse
	// to guess: gate it.
	if strings.Contains(path, "%") {
		return true
	}
	segs := disclosurePathSegments(path)
	// A path that normalises to nothing is degenerate input, not a route the
	// operator chose to exempt. Gate it, so the exemption list can only ever
	// exempt routes someone actually named.
	if len(segs) == 0 {
		return true
	}
	for _, pat := range patterns {
		if pat = strings.TrimSpace(pat); pat == "" {
			continue
		}
		patSegs := disclosurePathSegments(pat)
		if len(patSegs) != len(segs) {
			continue
		}
		matched := true
		for i, ps := range patSegs {
			if ps != "*" && ps != segs[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// disclosurePathSegments normalises a request path (or a configured pattern) into
// comparable segments.
//
// `path` is an LLM-controlled field, so a gate keyed on it must not be evadable
// by spelling the same route differently. Without these normalisations
// "/posts?x=1", "/POSTS", "/posts#f" and " /posts " all slipped past a gate that
// caught "/posts" — verified by TestAgentQueryAPI_G6B_PathVariantsCannotEvadeTheGate.
//
// Case folding can over-gate a genuinely case-sensitive upstream route. That is
// the safe direction: over-enforcement is a refused tool call, under-enforcement
// is undisclosed AI text in front of a person.
func disclosurePathSegments(p string) []string {
	p = strings.TrimSpace(p)
	// A query or fragment is not part of the route. Cut them before matching so
	// they cannot be used to disguise one.
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.ToLower(strings.Trim(p, "/"))
	// Drop empty segments so "//posts" and "posts//" compare equal to "posts".
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s = strings.TrimSpace(s); s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// normaliseForMatch collapses all whitespace runs to single spaces so a notice
// the agent hard-wrapped, indented inside markdown, or split across lines still
// matches the canonical text. Without this the gate would refuse writes that had
// genuinely disclosed, and train the agent to fight it.
func normaliseForMatch(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// agentPublicationRefusal vets a write to a publication-surface provider and
// returns the refusal reason, or "" when the write may proceed.
//
// Every failure direction refuses. A body that cannot be inspected is a body
// that cannot be vetted, and publishing undisclosed AI-authored text to a human
// is the non-conformity that carries the Art 99 exposure — a refused tool call
// is merely a retry.
//
// body is the already-decoded request body (AgentQueryRequest.Body), so
// malformed JSON never reaches here: the outer decoder rejects it first. The
// uninspectable case at this layer is an absent or fieldless body.
func (s *Server) agentPublicationRefusal(provider, path string, body map[string]any) string {
	fields := s.publicationDisclosureFields(provider, path)
	if len(fields) == 0 {
		return "" // not a publication surface
	}

	// Fail closed when the notice itself is unavailable: an unwired disclosure
	// means the daemon cannot prove it disclosed.
	if s.aiDisclosure == nil {
		return "This write was refused: " + provider + " is configured as a publication surface " +
			"but the AI-disclosure service is not wired, so the required EU AI Act Art 50(1) " +
			"notice cannot be determined. This is a daemon misconfiguration — report it to the operator."
	}
	notice := s.aiDisclosure.PublicationNotice().Text

	// Distinguish "no inspectable field" from "field present, notice missing":
	// the two need different fixes, and an agent that cannot tell them apart
	// cannot correct itself (review finding #4).
	var present []string
	for _, f := range fields {
		if v, ok := body[f]; ok {
			if str, isStr := v.(string); isStr && strings.TrimSpace(str) != "" {
				present = append(present, f)
			}
		}
	}
	if len(present) == 0 {
		return fmt.Sprintf(
			"This write was refused: %s is a publication surface and the request body carries none of "+
				"the fields the disclosure gate inspects (%s). Put the published text in one of those "+
				"fields, and include this notice verbatim within it:\n\n%s",
			provider, strings.Join(fields, ", "), notice)
	}

	// Every field a reader will see must carry the notice. Checking all present
	// fields rather than any one of them means a two-part post cannot disclose
	// in the title and stay silent in the body.
	wantNotice := normaliseForMatch(notice)
	for _, f := range present {
		if !strings.Contains(normaliseForMatch(body[f].(string)), wantNotice) {
			return fmt.Sprintf(
				"This write was refused: %s publishes to human readers, so field %q must carry the "+
					"EU AI Act Art 50(1) disclosure and does not. Add this text verbatim to it and "+
					"retry:\n\n%s",
				provider, f, notice)
		}
	}
	return ""
}
