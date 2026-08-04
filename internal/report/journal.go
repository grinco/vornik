package report

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Journal tail
// ============
//
// OPERATOR INSTRUCTION 2026-08-03: "make sure the appropriate logs are included".
// The daemon's recent error/panic/fatal lines are the single most useful thing a
// problem report can carry beyond the doctor verdicts, and `vornikctl report` was
// discarding them: the offline doctor collected them into Check.Items and the body
// builder dropped the field.
//
// CLI-ONLY. Its callers are `vornikctl doctor --offline` and `vornikctl report`,
// and nothing else may call it: the daemon's chat/API/A2A paths must not execute a
// program (operator ruling 2026-08-03 — only `vornikctl` may spawn processes; see
// D10/D13 of the design and the SECURITY INVARIANT in
// internal/service/container_problem_report.go). It lives in internal/report rather
// than internal/cli only so the offline doctor and the report body share ONE
// definition of "which lines count as an error and how many we keep" — package
// placement is not permission to call it from the daemon.
//
// Adding a daemon-side caller re-opens the class the ruling closed. Don't.
//
// It is deliberately dumb (exec, filter, cap) and best-effort: no journalctl (a
// container, a non-systemd host) is a warn, never a failure — the daemon inside a
// podman container is the normal CE deployment.
//
// The lines are NOT scrubbed here. Scrubbing is the body's job (AnonymizeBody
// runs every Item through the public scrubber), and doing it twice in two places
// is how the two copies drift.

// journalUnit is the systemd user unit the daemon runs as.
const journalUnit = "vornik"

// journalScanLines is how many recent journal lines are scanned for errors. The
// KEPT lines are capped separately (maxCheckItems) — scanning more than we keep
// means a burst of info lines can't push the errors out of the window.
const journalScanLines = 200

// journalArgs is the journalctl argument vector.
//
// SECURITY INVARIANT (defense in depth — NOT a licence for daemon-side calls; the
// file header is the actual boundary): every element is a compile-time constant
// except `lines`, which is an int we format from journalScanLines — nothing any
// caller can steer. journalctl is run DIRECTLY (execve, no shell, so no
// metacharacter parser exists anywhere in the chain) and takes no unit, filter,
// grep or path parameter. Adding one would create the steerable argv this design
// does not have, so that even a mistaken caller could not turn it into an
// injection. TestJournalArgs_AreConstantAndShellFree guards it.
func journalArgs(lines int) []string {
	return []string{"--user", "-u", journalUnit, "-n", fmt.Sprint(lines), "--no-pager"}
}

// journalRun is a seam so tests drive the tail without a systemd host.
var journalRun = func(ctx context.Context, lines int) ([]byte, error) {
	return exec.CommandContext(ctx, "journalctl", journalArgs(lines)...).CombinedOutput()
}

// JournalTail returns the daemon journal's recent error/panic/fatal lines as a
// Check whose Items are the lines themselves. Statuses are the doctor's own
// vocabulary so the result drops straight into a report or a doctor run.
func JournalTail(ctx context.Context) Check {
	out, err := journalRun(ctx, journalScanLines)
	if err != nil {
		return Check{
			Name: "journal", Status: "warn",
			Message: "journal tail unavailable (skipped): " + strings.TrimSpace(err.Error()),
		}
	}

	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(line)
		if !strings.Contains(l, "panic") && !strings.Contains(l, "fatal") &&
			!strings.Contains(l, `"level":"error"`) {
			continue
		}
		hits = append(hits, strings.TrimSpace(line))
	}
	if len(hits) == 0 {
		return Check{Name: "journal", Status: "ok", Message: "no recent fatal/panic/error lines"}
	}

	seen := len(hits)
	if len(hits) > maxCheckItems {
		// Keep the MOST RECENT: the failure that prompted the report is at the end.
		hits = hits[len(hits)-maxCheckItems:]
	}
	for i, h := range hits {
		if len(h) > maxCheckItemBytes {
			hits[i] = h[:maxCheckItemBytes] + "…"
		}
	}
	return Check{
		Name: "journal", Status: "warn",
		// State what was SEEN, not what was kept: "8 lines" next to 8 rendered
		// lines reads as the whole story when it may not be.
		Message: fmt.Sprintf("%d recent error/fatal line(s) in the daemon journal (showing %d)", seen, len(hits)),
		Items:   hits,
	}
}
