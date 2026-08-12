package membench

import (
	"os"
	"strings"
	"testing"
)

// Destructive-run guard (design §5.8). A benchmark run bulk-writes and clears
// memory. This is the code-level block that stops it ever being pointed at
// production.
//
// Some deployments have a production database whose NAME does not advertise its
// role — the reference deployment's reads like a scratch name, a historical artifact
// of a rename. That mismatch is exactly why the denylist lives in code rather than
// in operator judgement at a command line. Such a name cannot be matched by any
// generic pattern, so it is configured per deployment via DenyDatabasesEnv rather
// than shipped in source; lyingName below stands in for one in these tests.
const lyingName = "acme_scratch_actually_prod"

// TestGuard_DeniedNamesRefused — the whole point.
func TestGuard_DeniedNamesRefused(t *testing.T) {
	t.Setenv(DenyDatabasesEnv, lyingName)
	for _, db := range []string{lyingName, "prod", "production"} {
		if err := CheckDestructiveTarget(db, db); err == nil {
			t.Errorf("CheckDestructiveTarget(%q) allowed a denied database", db)
		}
	}
}

// TestGuard_DeniedNamesRefusedRegardlessOfCase — a denylist that only matches
// one spelling is not a denylist. Postgres identifiers fold to lower case, so
// "PROD" and "Prod" reach the same database.
func TestGuard_DeniedNamesRefusedRegardlessOfCase(t *testing.T) {
	t.Setenv(DenyDatabasesEnv, lyingName)
	for _, db := range []string{strings.ToUpper(lyingName), "Prod", "PRODUCTION"} {
		if err := CheckDestructiveTarget(db, db); err == nil {
			t.Errorf("CheckDestructiveTarget(%q) allowed a denied database by case", db)
		}
	}
}

// TestGuard_DeniedNamesRefusedWithSurroundingWhitespace — a stray space from a
// shell variable must not smuggle a denied name past the check.
func TestGuard_DeniedNamesRefusedWithSurroundingWhitespace(t *testing.T) {
	t.Setenv(DenyDatabasesEnv, lyingName)
	for _, db := range []string{" " + lyingName, lyingName + " ", "\tprod\n"} {
		if err := CheckDestructiveTarget(db, db); err == nil {
			t.Errorf("CheckDestructiveTarget(%q) allowed a denied name padded with whitespace", db)
		}
	}
}

// TestGuard_NoEnvironmentVariableOverride is the round-1 rejection, made
// executable. Review suggested an env-var escape hatch "for local dev"; an
// exported variable persists for a whole shell session and is inherited by every
// child process, which makes it a WEAKER guard than the flag it would bypass.
// This test is the regression anchor for that decision.
func TestGuard_NoEnvironmentVariableOverride(t *testing.T) {
	for _, name := range []string{
		"VORNIK_BENCH_ALLOW_PRODUCTION_DB",
		"VORNIK_BENCH_FORCE",
		"MEMBENCH_ALLOW_PROD",
		"VORNIK_ALLOW_DESTRUCTIVE",
	} {
		t.Setenv(name, "1")
	}
	if err := CheckDestructiveTarget("production", "production"); err == nil {
		t.Error("an environment variable bypassed the denylist; §5.8 rejects an " +
			"env-var override precisely because it persists across a shell and is " +
			"inherited by children")
	}
}

// TestGuard_RequiresMatchingConfirmation — naming the database is the operator's
// acknowledgement. A mismatch means they are not looking at the DSN they think
// they are, which is the 2am failure the guard exists for.
func TestGuard_RequiresMatchingConfirmation(t *testing.T) {
	if err := CheckDestructiveTarget("bench_db", "some_other_db"); err == nil {
		t.Error("a confirmation naming a different database was accepted")
	}
	if err := CheckDestructiveTarget("bench_db", ""); err == nil {
		t.Error("an empty confirmation was accepted; the acknowledgement must be explicit")
	}
}

// TestGuard_AllowsMatchingSafeName — and the happy path stays usable, or
// operators route around the guard.
func TestGuard_AllowsMatchingSafeName(t *testing.T) {
	for _, db := range []string{"bench_db", "vornik_bench_test", "membench_scratch"} {
		if err := CheckDestructiveTarget(db, db); err != nil {
			t.Errorf("CheckDestructiveTarget(%q) refused a safe target: %v", db, err)
		}
	}
}

// TestGuard_EmptyTargetRefused — an unset database name must not sail through as
// "nothing to protect". An empty DSN component often means a default was
// substituted somewhere downstream.
func TestGuard_EmptyTargetRefused(t *testing.T) {
	if err := CheckDestructiveTarget("", ""); err == nil {
		t.Error("an empty database name was accepted")
	}
}

// TestGuard_ErrorNamesTheDatabaseAndWhy — an operator hitting this at 2am needs
// to know which name tripped it and that the block is deliberate, not a bug to
// work around.
func TestGuard_ErrorNamesTheDatabaseAndWhy(t *testing.T) {
	err := CheckDestructiveTarget("production", "production")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "production") {
		t.Errorf("error %q does not name the offending database", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "production") {
		t.Errorf("error %q does not explain WHY the name is denied; without that "+
			"an operator reads it as a bug and looks for a bypass", msg)
	}
}

// TestGuard_DenylistIsNotEmpty — a guard whose list got emptied by a refactor
// would pass every other test in this file while protecting nothing.
func TestGuard_DenylistIsNotEmpty(t *testing.T) {
	if len(deniedDatabases) == 0 {
		t.Fatal("the denylist is empty; the guard protects nothing")
	}
	for _, want := range []string{"prod", "production"} {
		found := false
		for _, d := range deniedDatabases {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not on the shipped denylist", want)
		}
	}
}

// TestGuard_IgnoresUnrelatedEnvironment — the guard must not accidentally depend
// on ambient environment at all, in either direction.
func TestGuard_IgnoresUnrelatedEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/some_other_db")
	if err := CheckDestructiveTarget("bench_db", "bench_db"); err != nil {
		t.Errorf("an unrelated DATABASE_URL affected the decision: %v", err)
	}
	// And confirm the process env genuinely held the value, so this test would
	// notice if t.Setenv stopped working.
	if os.Getenv("DATABASE_URL") == "" {
		t.Fatal("test setup failed: DATABASE_URL not set")
	}
}

// TestGuard_ReasonMatchesTheDeniedName — each refusal must explain ITSELF. A
// configured denial cannot borrow the shipped list's reasoning ("it is a production
// database") because the operator cannot see that name in the source; it has to name
// the setting instead. Conversely a shipped denial must not cite a deployment's
// configured name, which reads as a non-sequitur and invites the reader to dismiss
// the whole message as boilerplate. Observed in a real CLI smoke test.
func TestGuard_ReasonMatchesTheDeniedName(t *testing.T) {
	// A configured denial explains itself by naming the SETTING that produced it,
	// because the operator cannot find the name in the source.
	t.Setenv(DenyDatabasesEnv, lyingName)
	cfgErr := CheckDestructiveTarget(lyingName, lyingName)
	if cfgErr == nil || !strings.Contains(cfgErr.Error(), DenyDatabasesEnv) {
		t.Errorf("configured refusal does not name its source: %v", cfgErr)
	}

	for _, db := range []string{"prod", "production"} {
		err := CheckDestructiveTarget(db, db)
		if err == nil {
			t.Fatalf("%q was accepted", db)
		}
		if strings.Contains(err.Error(), lyingName) {
			t.Errorf("refusing %q mentions a configured name, which reads as a "+
				"non-sequitur: %v", db, err)
		}
		if !strings.Contains(err.Error(), "production database") {
			t.Errorf("refusing %q does not say why: %v", db, err)
		}
	}
}

// Deployment-specific denials (2026-08-11). The CE export's operator-token scan
// caught this package publishing the operator's PRODUCTION database name in a
// shipped source file. A name that lies about its role has to be written down
// somewhere to be denied at all, so the choice was shipped code (which leaks it to
// every CE user) or deployment configuration (which does not). These tests pin the
// resulting contract: the env var may only ever ADD denials.

// TestGuard_EnvMayAddDenials — the mechanism that keeps a deployment-specific name
// protected without shipping it.
func TestGuard_EnvMayAddDenials(t *testing.T) {
	t.Setenv(DenyDatabasesEnv, "acme_live,acme_reporting")
	for _, db := range []string{"acme_live", "acme_reporting"} {
		if err := CheckDestructiveTarget(db, db); err == nil {
			t.Errorf("%q was added to the denylist but accepted", db)
		}
	}
}

// TestGuard_EnvAdditionsCannotRemoveShippedDenials — the asymmetry that makes an
// env var acceptable here at all. §5.8 rejects an env var that LOOSENS the guard;
// tightening carries none of that argument. Setting the variable must not be a way
// to replace the shipped list with a shorter one.
func TestGuard_EnvAdditionsCannotRemoveShippedDenials(t *testing.T) {
	t.Setenv(DenyDatabasesEnv, "something_else")
	for _, db := range []string{"prod", "production"} {
		if err := CheckDestructiveTarget(db, db); err == nil {
			t.Errorf("setting %s replaced the shipped denylist: %q was accepted",
				DenyDatabasesEnv, db)
		}
	}
}

// TestGuard_EnvAdditionsAreCaseAndWhitespaceInsensitive — the value is typed into a
// unit file or shell profile, where stray spacing is normal.
func TestGuard_EnvAdditionsAreCaseAndWhitespaceInsensitive(t *testing.T) {
	t.Setenv(DenyDatabasesEnv, "  Acme_Live , , acme_stage  ")
	for _, db := range []string{"acme_live", "ACME_LIVE", " acme_stage "} {
		if err := CheckDestructiveTarget(db, db); err == nil {
			t.Errorf("%q escaped a configured denial", db)
		}
	}
}

// TestGuard_EnvAdditionRefusalSaysWhereItCameFrom — an operator hitting a denial
// they did not find in the source needs to know which setting produced it, or the
// guard looks like a bug.
func TestGuard_EnvAdditionRefusalSaysWhereItCameFrom(t *testing.T) {
	t.Setenv(DenyDatabasesEnv, "acme_live")
	err := CheckDestructiveTarget("acme_live", "acme_live")
	if err == nil {
		t.Fatal("acme_live was accepted")
	}
	if !strings.Contains(err.Error(), DenyDatabasesEnv) {
		t.Errorf("refusal does not name the setting that caused it: %v", err)
	}
}

// TestGuard_ShipsNoDeploymentSpecificName — the regression itself. No shipped
// denial may be a name specific to one deployment; the CE export's operator-token
// scan fails the build on it, and that scan runs too late to be the only check.
func TestGuard_ShipsNoDeploymentSpecificName(t *testing.T) {
	for _, denied := range deniedDatabases {
		if strings.Contains(strings.ToLower(denied), "swarmd") {
			t.Errorf("shipped denylist contains the deployment-specific name %q; "+
				"configure it via %s instead", denied, DenyDatabasesEnv)
		}
	}
}
