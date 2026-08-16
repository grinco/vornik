package taskcreate

import (
	"testing"

	"vornik.io/vornik/internal/actor"
)

// Rule 1 (adoption-leaderboard LLD 2026-08-15 §3.1): a task created directly by
// an actor takes that actor. This lives on the canonical creator so every
// operator surface inherits it rather than each remembering.
func TestResolveActor(t *testing.T) {
	t.Run("explicit actor wins over the key", func(t *testing.T) {
		// A UI session resolves to a person even when a key is on the request,
		// and the person is the better answer.
		got := resolveActor(Params{
			Actor:             actor.User("usr-1"),
			CreatedByAPIKeyID: "key-1",
		})
		if got != actor.User("usr-1") {
			t.Errorf("got %v, want user:usr-1", got)
		}
	})

	t.Run("falls back to the authenticating key", func(t *testing.T) {
		// The fallback is why attribution does not stay at 1.2%: a caller that
		// authenticated with a key has already said who it is, and requiring it
		// to ALSO pass an Actor would reproduce the every-surface-must-remember
		// failure the design was written against.
		got := resolveActor(Params{CreatedByAPIKeyID: "key-1"})
		if got != actor.APIKey("key-1") {
			t.Errorf("got %v, want api_key:key-1", got)
		}
	})

	t.Run("no identity at all stays unattributed", func(t *testing.T) {
		// NOT anonymous: anonymous is a positive claim about an auth-off
		// install, whereas this is "we did not record". Conflating them would
		// inflate the coverage figure the dashboard's credibility rests on.
		got := resolveActor(Params{})
		if !got.IsZero() {
			t.Errorf("got %v, want the zero actor so the column is NULL", got)
		}
		if actor.Ptr(got) != nil {
			t.Error("the zero actor must persist as NULL, not an empty string")
		}
	})
}
