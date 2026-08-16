// Package actor models WHO caused a task to exist.
//
// The adoption leaderboard needs one identity concept across every entry point
// — companion plugins, REST, A2A, the UI, Telegram, Slack, email, autonomy and
// background workers — and it needs it to survive the hop from task into
// execution into step. See https://docs.vornik.io
//
// TWO PROPERTIES DECIDE EVERYTHING HERE.
//
// First, an Actor records what was OBSERVED, not who we think it was. The
// deployment wants a per-key leaderboard now and a per-person one later; storing
// the key and resolving the person at read time gets both, because the moment a
// key is claimed its entire history rolls up to its owner with no backfill. If
// we instead rewrote keys to users at write time we would destroy the record of
// what actually happened, and could not undo a mapping that turned out wrong.
//
// Second, `system:` is a first-class actor and NOT a null. A NULL means "we
// failed to record"; `system:kg_extraction` means "no human was involved". The
// ~83,000 background-worker rows on this deployment would swamp every real
// person if they were credited to one, and hiding them would make human usage
// look larger than it is. Collapsing the two is exactly how an adoption
// dashboard lies in the flattering direction.
package actor

import (
	"fmt"
	"strings"
)

// Kind is the fixed set of actor kinds. Fixed so a consumer can parse an actor
// without a database lookup, and so an unrecognised kind is a visible bug
// rather than a silent mis-bucket into someone else's row.
type Kind string

const (
	// KindAPIKey is an api_keys.id. Pseudonymous: it identifies a credential,
	// and becomes personal data only once something maps it to a person.
	KindAPIKey Kind = "api_key"
	// KindUser is a users.id, reached via user_identities for channel logins
	// (Telegram, Slack, email, GitHub). Never a parallel person-id space.
	KindUser Kind = "user"
	// KindSystem is machine-initiated work with no human behind it — autonomy,
	// background workers, cross-project calls, replay. The suffix names WHICH
	// machine path, because "system" alone cannot be told apart from a bug.
	KindSystem Kind = "system"
	// KindAnonymous is auth-off, no-key activity: local UI, curl. One bucket per
	// install.
	//
	// Anonymous is NEVER promotable. If that person later authenticates or
	// claims a key, their earlier anonymous work stays anonymous — nothing links
	// it to them, and retro-assigning on a hunch would invent the very
	// attribution this bucket exists to avoid. A key is a fact that can later be
	// resolved to a person; the ABSENCE of a key is not a fact about anyone.
	KindAnonymous Kind = "anonymous"
)

// Actor is a parsed `<kind>:<id>`.
type Actor struct {
	Kind Kind
	ID   string
}

// Well-known system actors. Named constants rather than string literals at the
// call sites, because a typo in one of these does not fail — it silently mints
// a new actor that appears on the leaderboard as its own row.
var (
	// System actors for machine paths (rules 3, 4, 6 of design §3.1).
	Autonomy         = Actor{KindSystem, "autonomy"}
	CrossProjectCall = Actor{KindSystem, "cross_project_call"}
	Counterfactual   = Actor{KindSystem, "counterfactual"}
	KGExtraction     = Actor{KindSystem, "kg_extraction"}
	MemoryTitler     = Actor{KindSystem, "memory_titler"}
	TaskNarrator     = Actor{KindSystem, "task_narrator"}
	MemoryNarrative  = Actor{KindSystem, "memory_narrative"}
)

// APIKey builds an api_key actor. Empty id yields the zero Actor, which String
// renders as "" — callers treat that as "unattributed" rather than writing a
// malformed "api_key:" row.
func APIKey(id string) Actor { return newOrZero(KindAPIKey, id) }

// User builds a user actor from a users.id.
func User(id string) Actor { return newOrZero(KindUser, id) }

// System builds a system actor naming the machine path, e.g. System("webhook").
func System(source string) Actor { return newOrZero(KindSystem, source) }

// Anonymous builds the per-install anonymous bucket.
func Anonymous(install string) Actor { return newOrZero(KindAnonymous, install) }

func newOrZero(k Kind, id string) Actor {
	id = strings.TrimSpace(id)
	if id == "" {
		return Actor{}
	}
	return Actor{Kind: k, ID: id}
}

// IsZero reports whether this is the unattributed actor.
func (a Actor) IsZero() bool { return a.Kind == "" || a.ID == "" }

// IsSystem reports whether this is machine-initiated work. The leaderboard
// shows these, visually distinct, rather than hiding or crediting them.
func (a Actor) IsSystem() bool { return a.Kind == KindSystem }

// Promotable reports whether this actor could later resolve to a person.
// api_key and user can; system and anonymous never can. Read §3.2 before
// changing this — the asymmetry is the whole basis for storing observed actors.
func (a Actor) Promotable() bool {
	return a.Kind == KindAPIKey || a.Kind == KindUser
}

// String renders the stored form, `<kind>:<id>`. The zero Actor renders empty
// so a caller can write NULL rather than a malformed row.
func (a Actor) String() string {
	if a.IsZero() {
		return ""
	}
	return string(a.Kind) + ":" + a.ID
}

// Parse reads the stored form back.
//
// It splits on the FIRST colon only: a system actor may name a sub-source
// ("system:webhook:github" for a signature-verified GitHub webhook), and an id
// is otherwise opaque. Requiring exactly one colon would reject those.
func Parse(s string) (Actor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Actor{}, fmt.Errorf("actor: empty")
	}
	kindStr, id, found := strings.Cut(s, ":")
	if !found {
		return Actor{}, fmt.Errorf("actor: %q has no kind separator", s)
	}
	if id == "" {
		return Actor{}, fmt.Errorf("actor: %q has an empty id", s)
	}
	k := Kind(kindStr)
	switch k {
	case KindAPIKey, KindUser, KindSystem, KindAnonymous:
	default:
		return Actor{}, fmt.Errorf("actor: unknown kind %q in %q", kindStr, s)
	}
	return Actor{Kind: k, ID: id}, nil
}

// Ptr renders an Actor for persistence.Task.CreatedByActor, which is a *string
// so "not recorded" stays distinguishable from "recorded as empty". The zero
// Actor yields nil, so a path with genuinely no actor writes NULL rather than a
// row that claims attribution it does not have.
func Ptr(a Actor) *string {
	if a.IsZero() {
		return nil
	}
	s := a.String()
	return &s
}
