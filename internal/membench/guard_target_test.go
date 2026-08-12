package membench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The guard used to validate --database and --i-know-this-wipes as STRINGS and
// nothing else. On 2026-08-12 that let a run naming a freshly-created throwaway
// write twelve fixture documents into the PRODUCTION corpus: the named database
// was left with zero tables, because the vornik adapter reaches the running daemon
// over companion MCP and the daemon writes to whatever database IT is configured
// with.
//
// The production database's real name is deliberately NOT written here. It is
// operator-identifying, this file ships to the public repository, and the release
// leak scan is denylist-sourced with zero exemptions. A placeholder costs the test
// nothing: the assertion is that BOTH names reach the operator, not what they are.
//
// That is worse than having no guard. The flag name tells an operator the blast
// radius is bounded, so they stop looking.
//
// These tests pin the fix: the guard must ask the system under test which database
// it actually writes to, and it must FAIL CLOSED when it cannot get an answer.

// reportingSystem is a MemorySystem that can name its write target.
type reportingSystem struct {
	*fakeSystem
	db     string
	dbErr  error
	called int
}

func (r *reportingSystem) WriteTargetDatabase(_ context.Context) (string, error) {
	r.called++
	if r.dbErr != nil {
		return "", r.dbErr
	}
	return r.db, nil
}

func TestVerifyWriteTarget_AgreesWhenTheDaemonNamesTheSameDatabase(t *testing.T) {
	sys := &reportingSystem{fakeSystem: newFakeSystem("vornik"), db: "bench_db"}
	if err := VerifyWriteTarget(context.Background(), sys, "bench_db"); err != nil {
		t.Errorf("a matching target was refused: %v", err)
	}
	if sys.called == 0 {
		t.Error("the guard did not ask the system at all — which is the whole defect")
	}
}

// TestVerifyWriteTarget_RefusesTheActualIncident is the regression test, using the
// real names from 2026-08-12.
func TestVerifyWriteTarget_RefusesTheActualIncident(t *testing.T) {
	sys := &reportingSystem{fakeSystem: newFakeSystem("vornik"), db: "the-production-database"}

	err := VerifyWriteTarget(context.Background(), sys, "vornik_retrieval_gate")
	if err == nil {
		t.Fatal("the guard accepted a run that would write to a different database than " +
			"the one named — this is exactly the 2026-08-12 production write")
	}
	// Both names must appear, or the operator cannot see what happened.
	if !strings.Contains(err.Error(), "vornik_retrieval_gate") ||
		!strings.Contains(err.Error(), "the-production-database") {
		t.Errorf("error %q must name BOTH the database asked for and the one the system "+
			"actually writes", err)
	}
}

// TestVerifyWriteTarget_FailsClosedWhenTheSystemCannotAnswer is the central
// decision. "Could not verify" must refuse, because the alternative is the false
// assurance that caused the incident: a guard that passes when it learned nothing
// is indistinguishable from no guard, while still reading like protection.
func TestVerifyWriteTarget_FailsClosedWhenTheSystemCannotAnswer(t *testing.T) {
	sys := &reportingSystem{fakeSystem: newFakeSystem("vornik"), dbErr: errors.New("404 not found")}

	err := VerifyWriteTarget(context.Background(), sys, "bench_db")
	if err == nil {
		t.Fatal("the guard passed without learning the write target; 'cannot verify' must " +
			"refuse, or the flag promises containment it never checked")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q should carry the underlying reason, or an operator cannot tell "+
			"a stale daemon from a broken token", err)
	}
}

// TestVerifyWriteTarget_FailsClosedOnASystemThatCannotReport: an adapter with no
// reporting capability at all is the same situation as one that errored — refuse.
// An older daemon is not a licence to skip the check.
func TestVerifyWriteTarget_FailsClosedOnASystemThatCannotReport(t *testing.T) {
	err := VerifyWriteTarget(context.Background(), newFakeSystem("legacy"), "bench_db")
	if err == nil {
		t.Fatal("a system that cannot report its write target was allowed to proceed")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cannot report") &&
		!strings.Contains(strings.ToLower(err.Error()), "does not report") {
		t.Errorf("error %q should say the system cannot report its write target", err)
	}
}

// TestVerifyWriteTarget_RefusesAnEmptyAnswer: a system that returns "" has not
// named a database, and treating empty as a match would re-open the hole for any
// deployment whose reporting is misconfigured.
func TestVerifyWriteTarget_RefusesAnEmptyAnswer(t *testing.T) {
	sys := &reportingSystem{fakeSystem: newFakeSystem("vornik"), db: ""}
	if err := VerifyWriteTarget(context.Background(), sys, "bench_db"); err == nil {
		t.Error("an empty write-target answer was treated as agreement")
	}
}

// TestVerifyWriteTarget_IgnoresSurroundingWhitespace keeps the check about the
// database rather than about typing.
func TestVerifyWriteTarget_IgnoresSurroundingWhitespace(t *testing.T) {
	sys := &reportingSystem{fakeSystem: newFakeSystem("vornik"), db: " bench_db\n"}
	if err := VerifyWriteTarget(context.Background(), sys, "bench_db "); err != nil {
		t.Errorf("whitespace defeated the comparison: %v", err)
	}
}
