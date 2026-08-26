package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type fakeAuthSource struct {
	out []ConnectorAuthFailure
	err error
	n   int
}

func (f *fakeAuthSource) RecentAuthFailures(context.Context) ([]ConnectorAuthFailure, error) {
	f.n++
	return f.out, f.err
}

type sentAlert struct{ subject, body string }

func newWorker(src ConnectorAuthSource) (*ConnectorAuthWorker, *[]sentAlert, *time.Time) {
	sent := &[]sentAlert{}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	w := &ConnectorAuthWorker{
		Failures: src,
		Alert:    func(s, b string) { *sent = append(*sent, sentAlert{s, b}) },
		Logger:   zerolog.Nop(),
		now:      func() time.Time { return now },
	}
	return w, sent, &now
}

// The alert must reach the operator's channel and carry the fix. The original
// incident's own payload said "re-authenticate" and sat unread in a JSON blob
// for two days — that is the failure this worker exists to prevent.
func TestAlertsOnAuthFailures(t *testing.T) {
	src := &fakeAuthSource{out: []ConnectorAuthFailure{
		{ProjectID: "vornik-marketing", Server: "atlassian", Count: 14, Last: time.Now().UTC()},
	}}
	w, sent, _ := newWorker(src)
	w.Tick(context.Background())

	if len(*sent) != 1 {
		t.Fatalf("want 1 alert, got %d", len(*sent))
	}
	a := (*sent)[0]
	if !strings.Contains(a.subject, "atlassian") || !strings.Contains(a.subject, "vornik-marketing") {
		t.Errorf("subject does not identify the connector: %q", a.subject)
	}
	for _, want := range []string{"14", "401", "vornikctl mcp connect atlassian -p vornik-marketing"} {
		if !strings.Contains(a.body, want) {
			t.Errorf("body does not contain %q:\n%s", want, a.body)
		}
	}
}

// A broken credential produces ONE message, not one per tool call. Alert spam
// trains an operator to ignore the channel, which is the same outcome as no
// alert at all.
func TestCooldownSuppressesRepeats(t *testing.T) {
	src := &fakeAuthSource{out: []ConnectorAuthFailure{
		{ProjectID: "p", Server: "atlassian", Count: 5},
	}}
	w, sent, now := newWorker(src)

	w.Tick(context.Background())
	w.Tick(context.Background())
	w.Tick(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("want 1 alert inside the cooldown, got %d", len(*sent))
	}

	*now = now.Add(defaultConnectorAuthCooldown + time.Minute)
	w.Tick(context.Background())
	if len(*sent) != 2 {
		t.Fatalf("want a re-alert after the cooldown, got %d", len(*sent))
	}
}

// Two broken connectors are two findings — the cooldown is per connector, not
// global, or the second outage hides behind the first.
func TestCooldownIsPerConnector(t *testing.T) {
	src := &fakeAuthSource{out: []ConnectorAuthFailure{
		{ProjectID: "p", Server: "atlassian", Count: 2},
		{ProjectID: "p", Server: "slack", Count: 3},
	}}
	w, sent, _ := newWorker(src)
	w.Tick(context.Background())
	if len(*sent) != 2 {
		t.Fatalf("want one alert per connector, got %d", len(*sent))
	}
}

func TestNoFailuresNoAlert(t *testing.T) {
	w, sent, _ := newWorker(&fakeAuthSource{})
	w.Tick(context.Background())
	if len(*sent) != 0 {
		t.Fatalf("a healthy deployment must be silent, got %d alerts", len(*sent))
	}
}

func TestScanErrorDoesNotAlert(t *testing.T) {
	w, sent, _ := newWorker(&fakeAuthSource{err: errors.New("db down")})
	w.Tick(context.Background())
	if len(*sent) != 0 {
		t.Fatalf("a failed scan must not manufacture an alert, got %d", len(*sent))
	}
}

// An unconfigured notifier must not crash the worker — the condition still
// reaches the log.
func TestNilAlerterIsSafe(_ *testing.T) {
	w := &ConnectorAuthWorker{
		Failures: &fakeAuthSource{out: []ConnectorAuthFailure{{Server: "atlassian", Count: 1}}},
		Logger:   zerolog.Nop(),
	}
	w.Tick(context.Background()) // must not panic
}

// A daemon-scope connector has no project flag to pass.
func TestDaemonScopeAlertOmitsProjectFlag(t *testing.T) {
	subject, body := ConnectorAuthAlertText(ConnectorAuthFailure{Server: "atlassian", Count: 1})
	if !strings.Contains(subject, "daemon scope") {
		t.Errorf("subject: %q", subject)
	}
	if strings.Contains(body, "-p ") {
		t.Errorf("a daemon-scope connector takes no -p flag:\n%s", body)
	}
}

// The cooldown map must not accumulate one entry per connector the deployment
// has ever had.
func TestCooldownStateExpires(t *testing.T) {
	src := &fakeAuthSource{out: []ConnectorAuthFailure{{ProjectID: "p", Server: "atlassian", Count: 1}}}
	w, _, now := newWorker(src)
	w.Tick(context.Background())
	if len(w.alerted) != 1 {
		t.Fatalf("want 1 cooldown entry, got %d", len(w.alerted))
	}

	src.out = nil
	*now = now.Add(defaultConnectorAuthCooldown * 2)
	w.Tick(context.Background())
	if len(w.alerted) != 0 {
		t.Fatalf("aged-out cooldown state must be dropped, got %d entries", len(w.alerted))
	}
}
