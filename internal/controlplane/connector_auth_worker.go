package controlplane

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// ConnectorAuthWorker pushes an operator alert when an MCP connector starts
// failing authentication.
//
// WHY THIS EXISTS SEPARATELY FROM THE DOCTOR CHECK. `vornikctl doctor` reports
// the same condition, and that is not enough: nobody runs doctor on a customer
// site between incidents. The 2026-08-25 P0 was found by an operator reading a
// task's result JSON, two days after the Atlassian connector went dark — the
// daemon had logged a warn line per 401 the whole time and nothing pushed. On a
// customer site any connector-backed workflow degrades the same way, and silent
// degradation of an integration is a support incident that starts weeks before
// anyone reports it.
//
// So the condition PUSHES. The same class the doctor check reads
// (tool_audit_log.outcome_class = 'auth', typed by migration 168) drives a
// message to the operator's configured channel.
//
// Design: https://docs.vornik.io §3.4
type ConnectorAuthWorker struct {
	// Failures reports connectors with recent auth-class tool failures.
	// Injected so the worker needs no database and stays testable.
	Failures ConnectorAuthSource
	// Alert pushes one operator alert (subject, body). Nil-safe: an
	// unconfigured notifier means no alert, not a crash.
	//
	// Deliberately NOT an MCP tool. A connector-backed notification would be
	// subject to the very auth state it is reporting on, so a dead Slack
	// connector would swallow the alert saying Slack is dead.
	Alert    func(subject, body string)
	Interval time.Duration

	// Cooldown is how long a given (project, server) stays quiet after an
	// alert. Without it a broken credential produces one message per tool
	// call, which trains the operator to ignore the channel.
	Cooldown time.Duration

	LeaderGate LeaderGate
	Logger     zerolog.Logger

	// alerted is the in-memory dedup state: key -> when we last alerted.
	//
	// In memory DELIBERATELY. A daemon restart re-alerts on the first tick,
	// and that is correct rather than a flaw: if the credential is still
	// broken after a restart the operator should be told again, and if it is
	// not, no alert fires. A table would buy suppression of a message we
	// actually want, at the cost of a migration and a row nobody reads.
	alerted map[string]time.Time
	now     func() time.Time
	stopped chan struct{}
}

// ConnectorAuthFailure is one connector's recent auth-class failure count.
type ConnectorAuthFailure struct {
	ProjectID string
	Server    string
	Count     int
	Last      time.Time
}

// ConnectorAuthSource reports connectors failing authentication right now.
type ConnectorAuthSource interface {
	RecentAuthFailures(ctx context.Context) ([]ConnectorAuthFailure, error)
}

const (
	defaultConnectorAuthInterval = 5 * time.Minute
	defaultConnectorAuthCooldown = time.Hour
)

func (w *ConnectorAuthWorker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return defaultConnectorAuthInterval
}

func (w *ConnectorAuthWorker) cooldown() time.Duration {
	if w.Cooldown > 0 {
		return w.Cooldown
	}
	return defaultConnectorAuthCooldown
}

func (w *ConnectorAuthWorker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Run scans on a ticker until ctx is done. Leader-gated, like every other
// control-plane worker: two daemons must not both alert.
func (w *ConnectorAuthWorker) Run(ctx context.Context) {
	if w == nil || w.Failures == nil {
		return
	}
	if w.alerted == nil {
		w.alerted = map[string]time.Time{}
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)
	w.Logger.Info().Dur("interval", w.interval()).Msg("connector-auth alert worker started")
	defer w.Logger.Info().Msg("connector-auth alert worker stopped")

	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	if w.LeaderGate == nil || w.LeaderGate.IsLeader() {
		w.Tick(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.LeaderGate != nil && !w.LeaderGate.IsLeader() {
				continue
			}
			w.Tick(ctx)
		}
	}
}

// Tick performs one scan. Exported so a test can drive it deterministically
// rather than waiting on a ticker.
func (w *ConnectorAuthWorker) Tick(ctx context.Context) {
	if w == nil || w.Failures == nil {
		return
	}
	if w.alerted == nil {
		w.alerted = map[string]time.Time{}
	}
	failures, err := w.Failures.RecentAuthFailures(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("connector-auth: scan failed")
		return
	}

	now := w.clock()
	w.expireLocked(now)

	// Stable order so a multi-connector outage reads the same way twice.
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].ProjectID != failures[j].ProjectID {
			return failures[i].ProjectID < failures[j].ProjectID
		}
		return failures[i].Server < failures[j].Server
	})

	for _, f := range failures {
		if f.Server == "" || f.Count <= 0 {
			continue
		}
		key := f.ProjectID + "\x00" + f.Server
		if last, ok := w.alerted[key]; ok && now.Sub(last) < w.cooldown() {
			continue
		}
		w.alerted[key] = now

		subject, body := ConnectorAuthAlertText(f)
		// Logged at Error whether or not a notifier is configured, so a
		// deployment with no channel still leaves the condition somewhere an
		// operator will find it.
		w.Logger.Error().
			Str("project", f.ProjectID).
			Str("server", f.Server).
			Int("failures", f.Count).
			Msg("connector-auth: " + subject)
		if w.Alert != nil {
			w.Alert(subject, body)
		}
	}
}

// expireLocked drops cooldown entries that have aged out, so the map does not
// accumulate one entry per connector the deployment has ever had.
func (w *ConnectorAuthWorker) expireLocked(now time.Time) {
	cutoff := now.Add(-w.cooldown())
	for k, at := range w.alerted {
		if at.Before(cutoff) {
			delete(w.alerted, k)
		}
	}
}

// ConnectorAuthAlertText renders the operator message.
//
// It names the connector, the project, how bad it is, and — the part the
// original incident was missing — the command that fixes it. The agent's own
// result payload DID say "re-authenticate the Atlassian MCP connection"; it sat
// unread in a JSON blob for two days. This goes to the operator's channel.
func ConnectorAuthAlertText(f ConnectorAuthFailure) (subject, body string) {
	scope := "daemon scope"
	fix := "vornikctl mcp connect " + f.Server
	if f.ProjectID != "" {
		scope = "project " + f.ProjectID
		fix += " -p " + f.ProjectID
	}
	subject = fmt.Sprintf("connector %q (%s) is failing authentication", f.Server, scope)

	var b strings.Builder
	fmt.Fprintf(&b, "%d tool call(s) were rejected with HTTP 401/403", f.Count)
	if !f.Last.IsZero() {
		fmt.Fprintf(&b, ", most recently at %s", f.Last.UTC().Format(time.RFC3339))
	}
	b.WriteString(".\n\n")
	b.WriteString("Any workflow that depends on this connector is degrading: the calls fail, ")
	b.WriteString("the agents continue, and the outputs look plausible.\n\n")
	fmt.Fprintf(&b, "Fix: %s\n", fix)
	return subject, b.String()
}
