package chatauth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
)

type fakeBurner struct {
	live  map[string]bool
	fail  error
	calls []string
}

func (f *fakeBurner) BurnExposedLinkCode(_ context.Context, code string) (bool, error) {
	f.calls = append(f.calls, code)
	if f.fail != nil {
		return false, f.fail
	}
	return f.live[code], nil
}

func guard(t *testing.T, b Burner) (*ExposureGuard, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return NewExposureGuard(b, NewExposureMetrics(reg), zerolog.Nop()), reg
}

func counted(t *testing.T, reg *prometheus.Registry, channel, outcome string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "vornik_chat_link_code_exposed_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var gotChannel, gotOutcome string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "channel":
					gotChannel = l.GetValue()
				case "outcome":
					gotOutcome = l.GetValue()
				}
			}
			if gotChannel == channel && gotOutcome == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The whole point: the token never reaches the model.
func TestScrub_RemovesTheCodeBeforeDispatch(t *testing.T) {
	code := authz.MarkLinkCode("ACDE2346")
	b := &fakeBurner{live: map[string]bool{code: true}}
	g, reg := guard(t, b)

	out := g.Scrub(context.Background(), "slack", "hey use "+code+" thanks")

	if strings.Contains(out, code) {
		t.Fatalf("the code survived into the dispatched text: %q", out)
	}
	if len(b.calls) != 1 {
		t.Fatalf("want the code burned once, got %d calls", len(b.calls))
	}
	if counted(t, reg, "slack", "revoked") != 1 {
		t.Fatal("a live code's revocation was not counted")
	}
}

// A code-shaped token that is NOT live still gets scrubbed, and the outcome is
// distinguishable in the metric without being distinguishable to the speaker.
func TestScrub_NonLiveCodeIsStillRemoved(t *testing.T) {
	code := authz.MarkLinkCode("ACDE2346")
	g, reg := guard(t, &fakeBurner{live: map[string]bool{}})

	out := g.Scrub(context.Background(), "telegram", code)

	if strings.Contains(out, code) {
		t.Fatalf("a non-live code-shaped token was dispatched: %q", out)
	}
	if counted(t, reg, "telegram", "scrubbed") != 1 {
		t.Fatal("the scrub of a non-live token was not counted")
	}
}

// An unreachable store must not put the token back in front of the model.
// Keeping it away is the half that needs no store.
func TestScrub_BurnFailureStillScrubs(t *testing.T) {
	code := authz.MarkLinkCode("ACDE2346")
	g, reg := guard(t, &fakeBurner{fail: errors.New("db down")})

	out := g.Scrub(context.Background(), "slack", "here "+code)

	if strings.Contains(out, code) {
		t.Fatalf("a burn failure let the code through: %q", out)
	}
	if counted(t, reg, "slack", "burn_failed") != 1 {
		t.Fatal("the burn failure was not counted")
	}
}

// A deployment with no identity wiring scrubs anyway.
func TestScrub_NilBurnerStillScrubs(t *testing.T) {
	code := authz.MarkLinkCode("ACDE2346")
	g, _ := guard(t, nil)
	if out := g.Scrub(context.Background(), "slack", code); strings.Contains(out, code) {
		t.Fatalf("no burner meant no scrub: %q", out)
	}
}

// An ordinary prompt must pass through byte for byte. Prompt-swallowing is the
// failure §5.2 calls worse than the one being fixed.
func TestScrub_OrdinaryPromptIsUntouched(t *testing.T) {
	b := &fakeBurner{live: map[string]bool{}}
	g, reg := guard(t, b)
	text := "summarise ACDE2345 and file it under VLK"

	if out := g.Scrub(context.Background(), "slack", text); out != text {
		t.Fatalf("an ordinary prompt was altered: %q", out)
	}
	if len(b.calls) != 0 {
		t.Fatalf("an ordinary prompt consulted the code store: %v", b.calls)
	}
	if counted(t, reg, "slack", "scrubbed") != 0 {
		t.Fatal("an ordinary prompt was counted as an exposure")
	}
}

// A nil guard is the pre-wiring path and must not panic or alter text.
func TestScrub_NilGuardIsTransparent(t *testing.T) {
	var g *ExposureGuard
	if out := g.Scrub(context.Background(), "slack", "hello"); out != "hello" {
		t.Fatalf("nil guard altered text: %q", out)
	}
}
