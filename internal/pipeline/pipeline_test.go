package pipeline

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type recLogger struct{ lines []string }

func (l *recLogger) Warn(msg string, args ...any) {
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(strings.TrimSpace(strings.ReplaceAll(sprint(a), "\n", " ")))
	}
	l.lines = append(l.lines, b.String())
}

func sprint(a any) string {
	switch v := a.(type) {
	case string:
		return v
	case error:
		return v.Error()
	default:
		return reflect.ValueOf(a).String()
	}
}

// The closed set is the contract the lint reads; a change here is a design
// change (pipeline-points design §2.1) and must be deliberate.
func TestPoints_AreTheDeclaredFourWithTheirModes(t *testing.T) {
	want := map[string]Mode{
		"dispatcher.pre_tool":     ModeDecide,
		"dispatcher.post_tool":    ModeAround,
		"dispatcher.continuation": ModeDecide,
		"executor.step_outcome":   ModeDecide,
	}
	if len(Points) != len(want) {
		t.Fatalf("Points = %v", Points)
	}
	for _, p := range Points {
		if want[p.Name] != p.Mode {
			t.Errorf("%s: mode %s, want %s", p.Name, p.Mode, want[p.Name])
		}
		if got, ok := Lookup(p.Name); !ok || got != p {
			t.Errorf("Lookup(%s) = %v %t", p.Name, got, ok)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup of an undeclared name must fail")
	}
}

func TestConstructors_PanicOnModeMismatchAndUndeclaredPoint(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected a panic", name)
			}
		}()
		fn()
	}
	mustPanic("NewDecide on an Around point", func() { NewDecide[int](DispatcherPostTool, nil) })
	mustPanic("NewAround on a Decide point", func() { NewAround[int, int](ExecutorStepOutcome, nil) })
	mustPanic("undeclared point", func() { NewDecide[int](Point{Name: "made.up", Mode: ModeDecide}, nil) })
}

func TestRegister_RefusesDuplicateAndEmptyNames(t *testing.T) {
	c := NewDecide[int](DispatcherPreTool, nil)
	c.Register("a", func(context.Context, int) Verdict { return Verdict{} })
	for _, name := range []string{"a", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) must panic", name)
				}
			}()
			c.Register(name, func(context.Context, int) Verdict { return Verdict{} })
		}()
	}
}

func TestDecide_FirstRefusalStopsTheChainAndNamesTheParticipant(t *testing.T) {
	var ran []string
	c := NewDecide[int](ExecutorStepOutcome, nil)
	c.Register("pass", func(_ context.Context, _ int) Verdict { ran = append(ran, "pass"); return Verdict{} })
	c.Register("refuse", func(_ context.Context, _ int) Verdict {
		ran = append(ran, "refuse")
		return Verdict{Refused: true, Reason: "no", Participant: "liar"} // Participant is overwritten by the chain
	})
	c.Register("never", func(_ context.Context, _ int) Verdict { ran = append(ran, "never"); return Verdict{} })

	v := c.Run(context.Background(), 1)
	if !v.Refused || v.Reason != "no" || v.Participant != "refuse" {
		t.Fatalf("verdict %+v", v)
	}
	if !reflect.DeepEqual(ran, []string{"pass", "refuse"}) {
		t.Errorf("ran %v — the participant after the refusal must not run", ran)
	}
	if got := c.Participants(); !reflect.DeepEqual(got, []string{"pass", "refuse", "never"}) {
		t.Errorf("Participants = %v", got)
	}
}

func TestDecide_NoRefusalIsTheZeroVerdict(t *testing.T) {
	c := NewDecide[string](DispatcherPreTool, nil)
	c.Register("observer-shaped", func(context.Context, string) Verdict { return Verdict{} })
	if v := c.Run(context.Background(), "x"); v != (Verdict{}) {
		t.Errorf("verdict %+v", v)
	}
	empty := NewDecide[string](DispatcherPreTool, nil)
	if v := empty.Run(context.Background(), "x"); v != (Verdict{}) {
		t.Errorf("an empty chain refuses nothing: %+v", v)
	}
}

func TestDecide_ContinuationFieldsAreHonouredOnlyAtTheContinuationPoint(t *testing.T) {
	cont := NewDecide[string](DispatcherContinuation, nil)
	cont.Register("retry", func(context.Context, string) Verdict { return Verdict{Retry: "again"} })
	cont.Register("never", func(context.Context, string) Verdict { return Verdict{Refused: true} })
	if v := cont.Run(context.Background(), "x"); v.Retry != "again" || v.Refused || v.Participant != "retry" {
		t.Errorf("Retry must stop the chain like a refusal at the continuation point: %+v", v)
	}
	banner := NewDecide[string](DispatcherContinuation, nil)
	banner.Register("banner", func(context.Context, string) Verdict { return Verdict{Banner: "[warn] "} })
	if v := banner.Run(context.Background(), "x"); v.Banner != "[warn] " || v.Refused {
		t.Errorf("Banner: %+v", v)
	}

	log := &recLogger{}
	wrong := NewDecide[string](DispatcherPreTool, log)
	wrong.Register("bug", func(context.Context, string) Verdict { return Verdict{Retry: "again"} })
	wrong.Register("after", func(context.Context, string) Verdict { return Verdict{Refused: true, Reason: "real"} })
	v := wrong.Run(context.Background(), "x")
	if v.Retry != "" || !v.Refused || v.Participant != "after" {
		t.Errorf("a continuation field elsewhere is read as no refusal and the chain continues: %+v", v)
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "continuation-only field") || !strings.Contains(log.lines[0], "participant bug") {
		t.Errorf("the bug is logged with the participant's name: %v", log.lines)
	}
}

func TestAround_NestsFirstRegisteredOutermostAndNotCallingNextRefuses(t *testing.T) {
	var trace []string
	c := NewAround[string, string](DispatcherPostTool, nil)
	c.Register("outer", func(ctx context.Context, in string, next Next[string, string]) (string, error) {
		trace = append(trace, "outer-in")
		out, err := next(ctx, in+"+o")
		trace = append(trace, "outer-out")
		return out + "|o", err
	})
	c.Register("inner", func(ctx context.Context, in string, next Next[string, string]) (string, error) {
		trace = append(trace, "inner-in")
		out, err := next(ctx, in+"+i")
		trace = append(trace, "inner-out")
		return out + "|i", err
	})
	out, err := c.Run(context.Background(), "x", func(_ context.Context, in string) (string, error) {
		trace = append(trace, "terminal("+in+")")
		return "T", nil
	})
	if err != nil || out != "T|i|o" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !reflect.DeepEqual(trace, []string{"outer-in", "inner-in", "terminal(x+o+i)", "inner-out", "outer-out"}) {
		t.Errorf("trace %v", trace)
	}

	refusing := NewAround[string, string](DispatcherPostTool, nil)
	terminalRan := false
	refusing.Register("gate", func(context.Context, string, Next[string, string]) (string, error) {
		return "", errors.New("refused")
	})
	_, err = refusing.Run(context.Background(), "x", func(context.Context, string) (string, error) { terminalRan = true; return "T", nil })
	if err == nil || terminalRan {
		t.Errorf("a participant that does not call next refuses and the terminal never runs: err=%v ran=%t", err, terminalRan)
	}

	bare := NewAround[string, string](DispatcherPostTool, nil)
	if out, _ := bare.Run(context.Background(), "x", func(_ context.Context, in string) (string, error) { return in, nil }); out != "x" {
		t.Errorf("an empty chain is the terminal: %q", out)
	}
}
